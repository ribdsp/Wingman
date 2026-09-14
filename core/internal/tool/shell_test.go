package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSandbox records what it was asked to run and answers with what a test set up.
type fakeSandbox struct {
	result ExecResult
	err    error

	workspace string
	script    string
	calls     int
	// deadline is whether the context that reached Exec carried one.
	deadline bool
}

func (f *fakeSandbox) Exec(ctx context.Context, workspace, script string) (ExecResult, error) {
	f.calls++
	f.workspace = workspace
	f.script = script
	_, f.deadline = ctx.Deadline()
	return f.result, f.err
}

func TestShell_tools_offersOneToolWithASchemaBothProvidersAccept(t *testing.T) {
	// Arrange
	shell := NewShell(&fakeSandbox{})

	// Act
	definitions, err := shell.Tools(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("listing the built-in tool failed: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("offered %d tools; want 1", len(definitions))
	}

	definition := definitions[0]
	if definition.Name != ShellToolName {
		t.Errorf("name = %q; want %q", definition.Name, ShellToolName)
	}
	if !ValidName(definition.Name) {
		t.Errorf("the built-in tool's own name is one the providers reject: %q", definition.Name)
	}
	if definition.Source != SourceBuiltin {
		t.Errorf("source = %q; want builtin", definition.Source)
	}
	// The description is written for the model, not the operator: one that does not
	// know the sandbox may have no network spends a billed tool call finding out.
	if !strings.Contains(strings.ToLower(definition.Description), "network") {
		t.Errorf("the description does not mention the network: %q", definition.Description)
	}

	properties, ok := definition.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the schema has no properties object: %v", definition.InputSchema)
	}
	if _, declared := properties["script"]; !declared {
		t.Error("the schema does not declare a script argument")
	}
	if definition.InputSchema["additionalProperties"] != false {
		t.Error("the schema does not tell the model to stop inventing arguments")
	}
}

func TestShell_call_runsTheScriptInTheRunsOwnWorkspace(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{result: ExecResult{Stdout: "total 0\n"}}
	shell := NewShell(sandbox)

	// Act
	result, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"ls -la"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("a well-formed call failed: %v", err)
	}
	if result.IsError {
		t.Error("a command that exited 0 was reported as an error")
	}
	if result.Content != "total 0" {
		t.Errorf("content = %q; want the command's output with its trailing newline gone", result.Content)
	}
	if sandbox.script != "ls -la" {
		t.Errorf("the sandbox ran %q", sandbox.script)
	}
	// Per user, never shared, and it arrives per call rather than being held on the
	// runner — the runner is shared by every run on the instance.
	if sandbox.workspace != "/workspaces/u1" {
		t.Errorf("the sandbox used workspace %q", sandbox.workspace)
	}
}

func TestShell_call_refusesToRunWithoutAWorkspace(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{}
	shell := NewShell(sandbox)

	// Act
	_, err := shell.Call(context.Background(), Invocation{
		Name:  ShellToolName,
		Input: []byte(`{"script":"rm -rf ."}`),
	})

	// Assert
	// Refused rather than defaulted to the working directory. This process's working
	// directory is the service's own installation, so a missing workspace would turn
	// a sandboxed tool call into a command on the host.
	if err == nil {
		t.Fatal("a call with no workspace was run")
	}
	if sandbox.calls != 0 {
		t.Error("the script reached the sandbox anyway")
	}
}

func TestShell_call_aMissingScriptArgumentIsTheModelsMistakeToFix(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{}
	shell := NewShell(sandbox)

	// Act
	// Arguments the tool does not take, from a model that can correct itself if it is
	// told. A Go error here would stop the whole run over a fixable mistake.
	cases := map[string]string{
		"no script":      `{}`,
		"a number":       `{"script":42}`,
		"an empty one":   `{"script":"   "}`,
		"the wrong name": `{"command":"ls"}`,
	}

	// Assert
	for label, input := range cases {
		result, err := shell.Call(context.Background(), Invocation{
			Name:      ShellToolName,
			Input:     []byte(input),
			Workspace: "/workspaces/u1",
		})
		if err != nil {
			t.Errorf("%s stopped the run: %v", label, err)
			continue
		}
		if !result.IsError {
			t.Errorf("%s was reported as a success", label)
		}
		if !strings.Contains(result.Content, "script") {
			t.Errorf("%s does not tell the model which argument: %q", label, result.Content)
		}
	}
	if sandbox.calls != 0 {
		t.Error("a call with unusable arguments reached the sandbox")
	}
}

func TestShell_call_refusesArgumentsThatAreNotJSON(t *testing.T) {
	// Arrange
	shell := NewShell(&fakeSandbox{})

	// Act
	_, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	// Unlike a missing argument, this is not something the model can be told about:
	// its own arguments were JSON on the way in, so bytes that are not JSON mean
	// something rewrote them in between.
	if err == nil {
		t.Fatal("truncated arguments were accepted")
	}
}

func TestShell_call_reportsAFailedCommandAsOutputRatherThanAsAnError(t *testing.T) {
	// Arrange
	// The command ran and failed. The model is supposed to read the failure and try
	// something else, which it cannot do if the run stops.
	sandbox := &fakeSandbox{result: ExecResult{
		Stderr:   "ls: cannot access 'reports': No such file or directory\n",
		ExitCode: 2,
	}}
	shell := NewShell(sandbox)

	// Act
	result, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"ls reports"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("a command that exited non-zero stopped the run: %v", err)
	}
	if !result.IsError {
		t.Error("a command that exited 2 was reported as a success")
	}
	// Labelled, because the two streams mean different things and a model that cannot
	// tell them apart reads a warning as a result.
	if !strings.Contains(result.Content, "[stderr]") {
		t.Errorf("stderr is not labelled: %q", result.Content)
	}
	if !strings.Contains(result.Content, "[exit status 2]") {
		t.Errorf("the exit status is missing: %q", result.Content)
	}
}

func TestShell_call_saysSomethingWhenAFailingCommandPrintedNothing(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{result: ExecResult{ExitCode: 1}}
	shell := NewShell(sandbox)

	// Act
	result, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"test -f reports/today.csv"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	// Otherwise a silent failure and a silent success are the same empty string, and
	// the model reads the failure as done.
	if result.Content != "[exit status 1]" {
		t.Errorf("content = %q; want the exit status alone", result.Content)
	}
	if !result.IsError {
		t.Error("a silent failure was reported as a success")
	}
}

func TestShell_call_keepsBothStreamsWhenACommandUsesBoth(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{result: ExecResult{
		Stdout: "revenue,41200000\n",
		Stderr: "warning: 3 rows skipped\n",
	}}
	shell := NewShell(sandbox)

	// Act
	result, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"./report.sh"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	want := "revenue,41200000\n[stderr]\nwarning: 3 rows skipped"
	if result.Content != want {
		t.Errorf("content = %q; want %q", result.Content, want)
	}
	if result.IsError {
		t.Error("a command that warned and exited 0 was reported as an error")
	}
}

func TestShell_call_aSandboxThatCouldNotRunTheCommandStopsTheStep(t *testing.T) {
	// Arrange
	// Not a command that failed — a sandbox that never ran one. The model cannot
	// correct a container that will not start.
	sandbox := &fakeSandbox{err: errors.New("docker: cannot connect to the daemon")}
	shell := NewShell(sandbox)

	// Act
	_, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"ls"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err == nil {
		t.Fatal("a sandbox failure was reported as tool output")
	}
	if !strings.Contains(err.Error(), ShellToolName) {
		t.Errorf("the error does not name the tool: %v", err)
	}
}

func TestShell_call_passesTheStepDeadlineToTheSandbox(t *testing.T) {
	// Arrange
	// The deadline is the only thing standing between `sleep infinity` and a worker
	// that never returns, so it has to reach Exec rather than being replaced.
	sandbox := &fakeSandbox{result: ExecResult{Stdout: "ok"}}
	shell := NewShell(sandbox)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Act
	if _, err := shell.Call(ctx, Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"sleep 1"}`),
		Workspace: "/workspaces/u1",
	}); err != nil {
		t.Fatalf("call failed: %v", err)
	}

	// Assert
	if !sandbox.deadline {
		t.Error("the sandbox was handed a context with no deadline")
	}
}

func TestShell_call_refusesAToolThatIsNotItsOwn(t *testing.T) {
	// Arrange
	sandbox := &fakeSandbox{}
	shell := NewShell(sandbox)

	// Act
	_, err := shell.Call(context.Background(), Invocation{
		Name:      "read_report",
		Workspace: "/workspaces/u1",
	})

	// Assert
	// A routing mistake in the registry, not something a model did. Running the
	// script anyway would execute one tool's arguments as another tool's command.
	if err == nil {
		t.Fatal("the shell ran a call addressed to another tool")
	}
	if sandbox.calls != 0 {
		t.Error("the sandbox was used for another tool's call")
	}
}

func TestShell_call_withNoSandboxWiredSaysSoInsteadOfPanicking(t *testing.T) {
	// Arrange
	shell := NewShell(nil)

	// Act
	_, err := shell.Call(context.Background(), Invocation{
		Name:      ShellToolName,
		Input:     []byte(`{"script":"ls"}`),
		Workspace: "/workspaces/u1",
	})

	// Assert
	if err == nil {
		t.Fatal("a shell with no sandbox reported a successful call")
	}
}

func TestShell_label_namesTheSourceForTheAuditTrail(t *testing.T) {
	// Arrange, Act
	label := NewShell(&fakeSandbox{}).Label()

	// Assert
	// Stable and specific: it is what an operator reads when a source is unavailable
	// or two of them claim one name.
	if label != "builtin:shell" {
		t.Errorf("label = %q; want builtin:shell", label)
	}
}
