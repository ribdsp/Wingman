package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// fakeRunner is a tool source with no server behind it.
type fakeRunner struct {
	label       string
	definitions []Definition
	listErr     error
	// result and callErr are what Call answers with.
	result  Result
	callErr error
	// calls records what was asked of it, so a test can assert that a dropped tool
	// was never reached rather than only that its output was ignored.
	calls []Invocation
}

func (f *fakeRunner) Label() string { return f.label }

func (f *fakeRunner) Tools(context.Context) ([]Definition, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.definitions, nil
}

func (f *fakeRunner) Call(_ context.Context, invocation Invocation) (Result, error) {
	f.calls = append(f.calls, invocation)
	return f.result, f.callErr
}

// definition is a tool a fake runner offers.
func definition(name string) Definition {
	return Definition{
		Name:        name,
		Description: name + " does something",
		InputSchema: map[string]any{"type": "object"},
		Source:      SourceMCP,
		Origin:      "reports",
	}
}

// grantsFor declares every name as an enabled read tool.
func grantsFor(names ...string) *Grants {
	granted := make([]domain.ToolGrant, 0, len(names))
	for _, name := range names {
		granted = append(granted, domain.ToolGrant{Name: name, Class: domain.ToolClassRead, Enabled: true})
	}
	return NewGrants(granted...)
}

func TestRegistry_offer_showsTheModelOnlyWhatTheOperatorGranted(t *testing.T) {
	// Arrange
	// The server offers three tools. One was granted, one was never written down, and
	// one was granted and switched off.
	runner := &fakeRunner{
		label: "mcp:reports",
		definitions: []Definition{
			definition("read_report"),
			definition("delete_report"),
			definition("email_report"),
		},
	}
	grants := NewGrants(
		domain.ToolGrant{Name: "read_report", Class: domain.ToolClassRead, Enabled: true},
		domain.ToolGrant{Name: "email_report", Class: domain.ToolClassWrite, Enabled: false},
	)

	// Act
	offering, err := NewRegistry(grants, runner).Offer(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}
	// Not showing an ungranted tool is not the same defence as denying a call to it —
	// ClassifyTool would deny anyway. It is what stops the model planning around a
	// capability it cannot use and reporting the run as blocked.
	if names := strings.Join(offering.Names(), ","); names != "read_report" {
		t.Errorf("offered %q; want read_report alone", names)
	}
	if offering.Has("delete_report") {
		t.Error("a tool nobody granted is reachable")
	}
	if offering.Has("email_report") {
		t.Error("a tool that was switched off is reachable")
	}
}

func TestRegistry_offer_sortsTheToolListSoTwoIdenticalRunsSendTheSamePrompt(t *testing.T) {
	// Arrange
	// Map order would make one prompt several prompts as far as caching is concerned,
	// and it would make two identical runs produce different transcripts.
	runner := &fakeRunner{
		label: "mcp:reports",
		definitions: []Definition{
			definition("read_report"),
			definition("archive_report"),
			definition("send_report"),
		},
	}

	// Act
	offering, err := NewRegistry(grantsFor("read_report", "archive_report", "send_report"), runner).
		Offer(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}
	want := "archive_report,read_report,send_report"
	if got := strings.Join(offering.Names(), ","); got != want {
		t.Errorf("offered %q; want %q", got, want)
	}
}

func TestRegistry_offer_withdrawsANameTwoSourcesClaim(t *testing.T) {
	// Arrange
	// The grant says what class the tool has, and therefore whether it needs a human.
	// It does not say whose implementation runs. Letting the first source win would
	// let one added later quietly take over a name the operator approved for another.
	first := &fakeRunner{label: "mcp:reports", definitions: []Definition{definition("read_report")}}
	second := &fakeRunner{
		label:       "openapi:billing",
		definitions: []Definition{definition("read_report"), definition("read_invoice")},
	}

	// Act
	offering, err := NewRegistry(grantsFor("read_report", "read_invoice"), first, second).Offer(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}
	if offering.Has("read_report") {
		t.Error("a contested name is still callable")
	}
	// The tool that only one source claims is unaffected: one conflict withdraws one
	// name, not the source.
	if !offering.Has("read_invoice") {
		t.Error("an uncontested tool from the same source was withdrawn too")
	}

	conflicts := offering.Conflicting()
	if len(conflicts) != 1 {
		t.Fatalf("recorded %d conflicts; want 1", len(conflicts))
	}
	if conflicts[0].Name != "read_report" {
		t.Errorf("conflict is over %q; want read_report", conflicts[0].Name)
	}
	// Both sources are named, because "one of your servers is shadowing another" is
	// unactionable without knowing which two.
	if got := strings.Join(conflicts[0].Labels, ","); got != "mcp:reports,openapi:billing" {
		t.Errorf("conflict labels = %q; want both sources", got)
	}
}

func TestRegistry_offer_aSourceThatIsDownDoesNotStopTheRun(t *testing.T) {
	// Arrange
	// One unreachable MCP server halting every run on the instance is a worse failure
	// than an agent that cannot do one thing and says so.
	down := &fakeRunner{label: "mcp:reports", listErr: errors.New("dial unix /tmp/reports.sock: connect: no such file")}
	up := &fakeRunner{label: "builtin:shell", definitions: []Definition{definition("shell")}}

	// Act
	offering, err := NewRegistry(grantsFor("shell"), down, up).Offer(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("offer failed because one source was down: %v", err)
	}
	if !offering.Has("shell") {
		t.Error("the source that was up was not offered")
	}

	unavailable := offering.Unavailable()
	if len(unavailable) != 1 {
		t.Fatalf("recorded %d unavailable sources; want 1", len(unavailable))
	}
	// Nothing is silent: the name goes on the run step, so "the agent did not use the
	// reports server" has an answer.
	if unavailable[0].Label != "mcp:reports" {
		t.Errorf("unavailable source = %q; want mcp:reports", unavailable[0].Label)
	}
	if unavailable[0].Err == nil {
		t.Error("the failure was recorded without saying what it was")
	}
}

func TestRegistry_offer_isASnapshotAndNotALiveViewOfTheServer(t *testing.T) {
	// Arrange
	// A server may add a tool at any moment. A run that re-listed before every call
	// could be handed one halfway through that it was never planned with, and the set
	// it is judged against would not be the set it was given.
	runner := &fakeRunner{label: "mcp:reports", definitions: []Definition{definition("read_report")}}
	registry := NewRegistry(grantsFor("read_report", "wire_money"), runner)

	offering, err := registry.Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	runner.definitions = append(runner.definitions, definition("wire_money"))

	// Assert
	if offering.Has("wire_money") {
		t.Error("a tool the server added after the run started is callable inside it")
	}
	// A later run sees it, because the snapshot is per run and not per process.
	later, err := registry.Offer(context.Background())
	if err != nil {
		t.Fatalf("second offer failed: %v", err)
	}
	if !later.Has("wire_money") {
		t.Error("a new tool never becomes visible")
	}
}

func TestOffering_definitions_cannotBeEditedByWhoeverReceivesThem(t *testing.T) {
	// Arrange
	runner := &fakeRunner{label: "mcp:reports", definitions: []Definition{definition("read_report")}}
	offering, err := NewRegistry(grantsFor("read_report"), runner).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	// A provider adapter is the caller here. One that renamed a tool in place would
	// change what every later reader of this offering sees.
	taken := offering.Definitions()
	taken[0].Name = "wire_money"

	// Assert
	if offering.Definitions()[0].Name != "read_report" {
		t.Error("editing the returned definitions changed the offering")
	}
}

func TestOffering_call_routesToTheSourceThatDeclaredTheTool(t *testing.T) {
	// Arrange
	reports := &fakeRunner{
		label:       "mcp:reports",
		definitions: []Definition{definition("read_report")},
		result:      Result{Content: "revenue: 41.2m"},
	}
	shell := &fakeRunner{
		label:       "builtin:shell",
		definitions: []Definition{definition("shell")},
		result:      Result{Content: "total 0"},
	}
	offering, err := NewRegistry(grantsFor("read_report", "shell"), reports, shell).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	result, err := offering.Call(context.Background(), Invocation{
		Name:      "read_report",
		Input:     []byte(`{"period":"today"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if result.Content != "revenue: 41.2m" {
		t.Errorf("content = %q; want the reports server's answer", result.Content)
	}
	if len(shell.calls) != 0 {
		t.Error("the other source was called as well")
	}
	if len(reports.calls) != 1 {
		t.Fatalf("the reports server was called %d times; want 1", len(reports.calls))
	}
	// The workspace travels with the invocation rather than being held on a runner,
	// because a runner is shared by every run on the instance.
	if reports.calls[0].Workspace != "/workspaces/u1" {
		t.Errorf("workspace = %q; want the run's own", reports.calls[0].Workspace)
	}
}

func TestOffering_call_boundsWhatOneToolCanCharge(t *testing.T) {
	// Arrange
	// Truncation lives here rather than in each runner, so there is one place that
	// decides how much of a tool's output a run pays for and a source added later
	// cannot forget to do it.
	runner := &fakeRunner{
		label:       "mcp:reports",
		definitions: []Definition{definition("read_report")},
		result:      Result{Content: strings.Repeat("x", MaxOutputBytes*2)},
	}
	offering, err := NewRegistry(grantsFor("read_report"), runner).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	result, err := offering.Call(context.Background(), Invocation{Name: "read_report", Workspace: "/w"})

	// Assert
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if !result.Truncated {
		t.Error("a reply twice the cap was not marked as truncated")
	}
	if len(result.Content) > MaxOutputBytes+200 {
		t.Errorf("kept %d bytes; the cap is %d", len(result.Content), MaxOutputBytes)
	}
}

func TestOffering_call_refusesAToolThisRunWasNeverOffered(t *testing.T) {
	// Arrange
	runner := &fakeRunner{label: "mcp:reports", definitions: []Definition{definition("read_report")}}
	offering, err := NewRegistry(grantsFor("read_report"), runner).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	_, err = offering.Call(context.Background(), Invocation{Name: "wire_money", Workspace: "/w"})

	// Assert
	// Not a model mistake — the loop checks the grant before it gets here — so this is
	// an error rather than output the model is invited to learn from.
	if err == nil {
		t.Fatal("a tool outside the offering was called")
	}
	if !strings.Contains(err.Error(), "wire_money") {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

func TestOffering_call_saysWhichSourceFailedWhenOneDoes(t *testing.T) {
	// Arrange
	runner := &fakeRunner{
		label:       "mcp:reports",
		definitions: []Definition{definition("read_report")},
		callErr:     errors.New("connection reset by peer"),
	}
	offering, err := NewRegistry(grantsFor("read_report"), runner).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	_, err = offering.Call(context.Background(), Invocation{Name: "read_report", Workspace: "/w"})

	// Assert
	if err == nil {
		t.Fatal("a failed tool call was reported as a success")
	}
	// The label, because "a tool call failed" sends an operator to the wrong server.
	for _, want := range []string{"read_report", "mcp:reports", "connection reset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}
}

func TestOffering_call_trimsTheNameSoAStraySpaceIsNotAnUnknownTool(t *testing.T) {
	// Arrange
	runner := &fakeRunner{
		label:       "mcp:reports",
		definitions: []Definition{definition("read_report")},
		result:      Result{Content: "ok"},
	}
	offering, err := NewRegistry(grantsFor("read_report"), runner).Offer(context.Background())
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}

	// Act
	_, err = offering.Call(context.Background(), Invocation{Name: " read_report ", Workspace: "/w"})

	// Assert
	if err != nil {
		t.Fatalf("a padded tool name was refused: %v", err)
	}
	// The runner is handed the trimmed name, not the padded one, or an MCP server
	// would be asked for a tool it does not have.
	if runner.calls[0].Name != "read_report" {
		t.Errorf("the runner was asked for %q", runner.calls[0].Name)
	}
}

func TestNewRegistry_withNoGrantsOffersNothingRatherThanCrashing(t *testing.T) {
	// Arrange
	// A caller that forgot to load the file should get a run where nothing is
	// callable, not a nil map dereference four tool calls into the loop.
	runner := &fakeRunner{label: "mcp:reports", definitions: []Definition{definition("read_report")}}

	// Act
	offering, err := NewRegistry(nil, runner).Offer(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("offer failed: %v", err)
	}
	if len(offering.Names()) != 0 {
		t.Errorf("offered %v with no grants loaded", offering.Names())
	}
}

func TestRegistry_grants_areReachableForThePerCallCheck(t *testing.T) {
	// Arrange
	// The loop needs both: the registry decides what the model sees, ClassifyTool
	// decides what may run. Exposing the grants here means the loop does not hold a
	// second reference that can drift from this one.
	grants := grantsFor("read_report")

	// Act
	registry := NewRegistry(grants, &fakeRunner{label: "mcp:reports"})

	// Assert
	if !registry.Grants().Classify("read_report", false).Allowed {
		t.Error("the registry's grants are not the ones it was built with")
	}
}
