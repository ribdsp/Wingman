package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The fakes. One per port, each holding what it was asked and answering what the test
// set — no scripting language, no expectations engine. The loop is worth testing this
// way because every safety property it has is "what did it do when this answer came
// back", and that is a struct field.

var (
	// testNow is fixed so a finished run's timestamp is an assertion rather than a
	// window.
	testNow = time.Date(2026, 9, 12, 10, 30, 0, 0, time.UTC)

	// errFake stands in for any infrastructure failure. It is deliberately not a
	// provider.Error: an unrecognised error is the case the loop has to be careful
	// with.
	errFake = errors.New("fake failure")
)

type fakeTranscript struct {
	steps     []domain.Step
	indexErr  error
	appendErr error
}

func (f *fakeTranscript) NextStepIndex(_ context.Context, _ string) (int, error) {
	if f.indexErr != nil {
		return 0, f.indexErr
	}
	return len(f.steps) + 1, nil
}

func (f *fakeTranscript) AppendStep(_ context.Context, step domain.Step) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.steps = append(f.steps, step)
	return nil
}

// kinds is the transcript as a shape, for asserting the order a turn wrote things in.
func (f *fakeTranscript) kinds() []domain.StepKind {
	out := make([]domain.StepKind, 0, len(f.steps))
	for _, step := range f.steps {
		out = append(out, step.Kind)
	}
	return out
}

func (f *fakeTranscript) toolSteps() []domain.Step {
	var out []domain.Step
	for _, step := range f.steps {
		if step.Kind == domain.StepKindTool {
			out = append(out, step)
		}
	}
	return out
}

func (f *fakeTranscript) last() domain.Step {
	if len(f.steps) == 0 {
		return domain.Step{}
	}
	return f.steps[len(f.steps)-1]
}

type finishedRun struct {
	runID  string
	stop   domain.StopReason
	reason string
	at     time.Time
}

type fakeRuns struct {
	cancelled bool
	cancelErr error
	finishErr error
	// finished records every Finish call, not the last one. A run must end exactly
	// once, and a slice is the only way a test can tell that it did.
	finished []finishedRun
}

func (f *fakeRuns) CancelRequested(_ context.Context, _ string) (bool, error) {
	if f.cancelErr != nil {
		return false, f.cancelErr
	}
	return f.cancelled, nil
}

func (f *fakeRuns) Finish(_ context.Context, runID string, stop domain.StopReason, reason string, at time.Time) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	f.finished = append(f.finished, finishedRun{runID: runID, stop: stop, reason: reason, at: at})
	return nil
}

type fakeBudget struct {
	ledger    domain.Ledger
	ledgerErr error
	charges   []Charge
	chargeErr error
}

func (f *fakeBudget) Ledger(_ context.Context, _ string, _ time.Time) (domain.Ledger, error) {
	if f.ledgerErr != nil {
		// The repository's own rule: an unreadable ledger is not a zero one.
		return domain.Ledger{}, f.ledgerErr
	}
	return f.ledger, nil
}

func (f *fakeBudget) Charge(_ context.Context, charge Charge) error {
	if f.chargeErr != nil {
		return f.chargeErr
	}
	f.charges = append(f.charges, charge)
	return nil
}

type fakeHalt struct {
	engaged bool
	err     error
}

func (f *fakeHalt) Engaged(_ context.Context) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.engaged, nil
}

type fakeGate struct {
	decision SpendDecision
	err      error
	requests []SpendRequest
}

func (f *fakeGate) Request(_ context.Context, req SpendRequest) (SpendDecision, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return SpendDecision{}, f.err
	}
	return f.decision, nil
}

// answer is one scripted model reply.
type answer struct {
	response provider.Response
	err      error
}

type fakeProvider struct {
	name     string
	answers  []answer
	requests []provider.Request
	calls    int
}

func (f *fakeProvider) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f *fakeProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	f.requests = append(f.requests, req)
	index := f.calls
	f.calls++

	if index >= len(f.answers) {
		// Not a canned reply. A loop that asked for a turn the test did not script is
		// either looping or being retried, and both are things a test should see rather
		// than absorb.
		return provider.Response{}, fmt.Errorf("fake provider: call %d was not scripted", index+1)
	}
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	return f.answers[index].response, f.answers[index].err
}

type fakeTools struct {
	definitions []tool.Definition
	unavailable []tool.RunnerFailure
	conflicting []tool.Conflict
	// call answers an invocation. Nil means an empty successful result, which is what
	// most tests want: they are about the gate, not about the output.
	call  func(invocation tool.Invocation) (tool.Result, error)
	calls []tool.Invocation
}

func (f *fakeTools) Definitions() []tool.Definition    { return f.definitions }
func (f *fakeTools) Unavailable() []tool.RunnerFailure { return f.unavailable }
func (f *fakeTools) Conflicting() []tool.Conflict      { return f.conflicting }
func (f *fakeTools) names() []string {
	out := make([]string, 0, len(f.calls))
	for _, invocation := range f.calls {
		out = append(out, invocation.Name)
	}
	return out
}

func (f *fakeTools) Call(_ context.Context, invocation tool.Invocation) (tool.Result, error) {
	f.calls = append(f.calls, invocation)
	if f.call == nil {
		return tool.Result{Content: "ok"}, nil
	}
	return f.call(invocation)
}

// harness is a loop and everything it was built from, so a test can assert on both.
type harness struct {
	loop       *Loop
	provider   *fakeProvider
	transcript *fakeTranscript
	runs       *fakeRuns
	budget     *fakeBudget
	halt       *fakeHalt
	gate       *fakeGate
	tools      *fakeTools
	grants     *tool.Grants
}

// newHarness builds a loop whose every dependency answers the permissive thing, so a
// test only has to say what is different about its case.
func newHarness(t *testing.T, answers ...answer) *harness {
	t.Helper()

	h := &harness{
		provider:   &fakeProvider{name: "fake", answers: answers},
		transcript: &fakeTranscript{},
		runs:       &fakeRuns{},
		budget:     &fakeBudget{ledger: domain.Ledger{Readable: true}},
		halt:       &fakeHalt{},
		gate:       &fakeGate{decision: SpendDecision{Outcome: domain.ApprovalAutoApproved, Reason: "below the threshold"}},
		tools: &fakeTools{definitions: []tool.Definition{
			{Name: "read_file", Description: "read a file", InputSchema: map[string]any{"type": "object"}},
			{Name: "send_email", Description: "send an email", InputSchema: map[string]any{"type": "object"}},
			{Name: "pay_invoice", Description: "pay an invoice", InputSchema: map[string]any{"type": "object"}},
		}},
		grants: testGrants(),
	}

	loop, err := New(Config{
		Provider:   h.provider,
		Model:      "test-model",
		Transcript: h.transcript,
		Runs:       h.runs,
		Budget:     h.budget,
		Halt:       h.halt,
		Gate:       h.gate,
		Grants:     h.grants,
		Logger:     zerolog.Nop(),
		Now:        func() time.Time { return testNow },
		// Retries are not what these tests are timing.
		RetryPause: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() = %v; want a loop", err)
	}
	h.loop = loop
	return h
}

// testGrants is the operator's declared tool list used throughout: one read tool, one
// write tool, one spending tool with a contract, and one switched off. Four grants
// cover every branch of the per-call gate.
func testGrants() *tool.Grants {
	return tool.NewGrants(
		domain.ToolGrant{Name: "read_file", Class: domain.ToolClassRead, Enabled: true},
		domain.ToolGrant{Name: "send_email", Class: domain.ToolClassWrite, Enabled: true},
		domain.ToolGrant{
			Name:    "pay_invoice",
			Class:   domain.ToolClassSpend,
			Enabled: true,
			Spend: &domain.ToolSpend{
				ActionType:     "supplier.invoice",
				AmountArgument: "amount",
				Currency:       "USD",
			},
		},
		domain.ToolGrant{Name: "delete_everything", Class: domain.ToolClassWrite, Enabled: false},
	)
}

// testInput is a run that the ladder will let continue: real defaults, nothing spent.
func testInput() Input {
	return Input{
		Run: domain.Run{
			ID:          "run_1",
			TaskID:      "task_1",
			OwnerUserID: "user_1",
			Limits:      domain.RunLimits{}.WithDefaults(),
			StartedAt:   testNow,
		},
		Task: domain.Task{
			ID:          "task_1",
			OwnerUserID: "user_1",
			Source:      domain.TaskSourceUser,
			Brief:       "find out why last week's invoices are late",
			Status:      domain.TaskStatusRunning,
		},
		Workspace: "/workspaces/user_1",
	}
}

// answered is a model reply that ends the run.
func answered(text string) answer {
	return answer{response: provider.Response{
		Text:   text,
		Finish: provider.FinishAnswered,
		Usage:  provider.Usage{TokensIn: 100, TokensOut: 20},
		Model:  "test-model",
	}}
}

// asksFor is a model reply that calls tools.
func asksFor(calls ...provider.ToolCall) answer {
	return answer{response: provider.Response{
		Text:      "working on it",
		ToolCalls: calls,
		Finish:    provider.FinishToolUse,
		Usage:     provider.Usage{TokensIn: 100, TokensOut: 20},
		Model:     "test-model",
	}}
}

func toolCall(id, name string, arguments map[string]any) provider.ToolCall {
	input := json.RawMessage(nil)
	if arguments != nil {
		encoded, err := json.Marshal(arguments)
		if err != nil {
			panic(err)
		}
		input = encoded
	}
	return provider.ToolCall{ID: id, Name: name, Input: input}
}

// run drives the loop with the harness's own tools attached.
func (h *harness) run(t *testing.T, in Input) Outcome {
	t.Helper()
	in.Tools = h.tools
	outcome, err := h.loop.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run() = %v; want a run that ended with a reason", err)
	}
	return outcome
}

// assertFinishedOnce is the invariant every one of these tests shares: exactly one
// stop reason reached the run row.
func (h *harness) assertFinishedOnce(t *testing.T, stop domain.StopReason) finishedRun {
	t.Helper()
	if len(h.runs.finished) != 1 {
		t.Fatalf("the run was finished %d times; want exactly once", len(h.runs.finished))
	}
	got := h.runs.finished[0]
	if got.stop != stop {
		t.Errorf("stop = %q (%q); want %q", got.stop, got.reason, stop)
	}
	if got.reason == "" {
		t.Error("the run was finished with no reason; the transcript would not say why")
	}
	return got
}
