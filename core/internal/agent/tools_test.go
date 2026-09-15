package agent

import (
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The per-call gate. What a tool call is allowed to do depends on the operator's grant
// and on whether anybody is watching the run, and those two facts produce five
// different outcomes — run it, refuse it and carry on, file it with the goal engine,
// stop the run, or tell the model it got the arguments wrong. One test each.

// unattended is a run the goal engine started because a metric moved. Nobody is
// waiting, which is what makes a write a different question from the same write typed
// into a chat.
func unattended() Input {
	in := testInput()
	in.Task.Source = domain.TaskSourceGoalEngine
	return in
}

func TestRun_anAttendedWrite_runs(t *testing.T) {
	// Arrange
	// Somebody is at the other end and can see what was done, which is the whole
	// difference between this test and the next one.
	h := newHarness(t,
		asksFor(toolCall("call_1", "send_email", map[string]any{"to": "supplier@example.test"})),
		answered("asked the supplier for the invoices"),
	)

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	if got := h.tools.names(); len(got) != 1 || got[0] != "send_email" {
		t.Errorf("tools called = %v; want [send_email]", got)
	}
}

func TestRun_anUnattendedWrite_isRefusedAndIsNotFiledWithTheMoneyGate(t *testing.T) {
	// Arrange
	h := newHarness(t,
		asksFor(toolCall("call_1", "send_email", map[string]any{"to": "supplier@example.test"})),
		answered("I would have emailed the supplier; that needs a person to approve"),
	)

	// Act
	outcome := h.run(t, unattended())

	// Assert
	// Refused for this run rather than filed. The goal engine's gate decides amounts of
	// money, and a write has no amount: filing one there would come back denied for
	// having no positive amount, which is an audit row that blames the number instead
	// of the situation.
	if len(h.gate.requests) != 0 {
		t.Errorf("the spending gate was asked about a write: %+v", h.gate.requests)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
	// The run is not over. Whatever it did manage is still worth reporting.
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}

	// And the model was told, because a refusal said by omission is one it tries again.
	results := lastToolResults(t, h)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v; want one error result", results)
	}
	if !strings.Contains(results[0].Content, "not called") {
		t.Errorf("result content = %q; want it to say the tool was not called", results[0].Content)
	}
}

func TestRun_everyCallGetsAResult_evenTheRefusedOne(t *testing.T) {
	// Arrange
	// Both vendors reject the following turn when a call it made has no answer, so a
	// batch that is part-permitted still has to answer all of it.
	h := newHarness(t,
		asksFor(
			toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"}),
			toolCall("call_2", "send_email", map[string]any{"to": "supplier@example.test"}),
		),
		answered("read the file; the email needs a person"),
	)

	// Act
	outcome := h.run(t, unattended())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	results := lastToolResults(t, h)
	if len(results) != 2 {
		t.Fatalf("tool results = %+v; want one per call", results)
	}
	if results[0].CallID != "call_1" || results[0].IsError {
		t.Errorf("result 1 = %+v; want a successful answer to call_1", results[0])
	}
	if results[1].CallID != "call_2" || !results[1].IsError {
		t.Errorf("result 2 = %+v; want a refusal answering call_2", results[1])
	}
}

func TestRun_aSpend_isFiledWithTheGoalEngineAndRunsWhenAutoApproved(t *testing.T) {
	// Arrange
	h := newHarness(t,
		asksFor(toolCall("call_1", "pay_invoice", map[string]any{"invoice": "INV-1", "amount": 1500})),
		answered("paid INV-1"),
	)

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	if len(h.gate.requests) != 1 {
		t.Fatalf("the gate was asked %d times; want once", len(h.gate.requests))
	}

	// The request is the operator's declared contract plus the model's amount. Nothing
	// in it is the model's choice except the number, and the gate re-checks that.
	got := h.gate.requests[0]
	if got.ActionType != "supplier.invoice" {
		t.Errorf("actionType = %q; want the operator's declared action", got.ActionType)
	}
	if got.Amount != 1500 {
		t.Errorf("amount = %v; want 1500", got.Amount)
	}
	if got.Currency != "USD" {
		t.Errorf("currency = %q; want USD, as the gate compares it", got.Currency)
	}
	if got.RunID != "run_1" || got.ToolName != "pay_invoice" {
		t.Errorf("request = %+v; want the run and tool named", got)
	}
	// Derived from the run and the call, so a retried step files the same request
	// instead of a second one against the same daily cap.
	if !strings.Contains(got.IdempotencyKey, "run_1") || !strings.Contains(got.IdempotencyKey, "call_1") {
		t.Errorf("idempotencyKey = %q; want it derived from the run and the call", got.IdempotencyKey)
	}

	if names := h.tools.names(); len(names) != 1 || names[0] != "pay_invoice" {
		t.Errorf("tools called = %v; want [pay_invoice]", names)
	}
}

func TestRun_aSpendTheGatePutInFrontOfAHuman_doesNotWait(t *testing.T) {
	// Arrange
	// A run parked against somebody's attention holds a worker, a sandbox and a place
	// in that user's daily budget for as long as nobody looks at the queue.
	h := newHarness(t,
		asksFor(toolCall("call_1", "pay_invoice", map[string]any{"invoice": "INV-1", "amount": 1500})),
		answered("the payment is waiting for approval; everything else is done"),
	)
	h.gate.decision = SpendDecision{
		Outcome: domain.ApprovalPending,
		Reason:  "no approval policy configured for action \"supplier.invoice\"",
	}

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all — nothing was approved", len(h.tools.calls))
	}
	results := lastToolResults(t, h)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v; want one error result", results)
	}
	if !strings.Contains(results[0].Content, "did not wait") {
		t.Errorf("result content = %q; want it to say a human was asked and the run did not wait", results[0].Content)
	}
}

func TestRun_aSpendTheGateDenied_stopsTheRun(t *testing.T) {
	// Arrange
	// The gate denies a cap breach rather than escalating it, and a run told no about
	// money has no business continuing to try.
	h := newHarness(t, asksFor(toolCall("call_1", "pay_invoice", map[string]any{"amount": 900000})))
	h.gate.decision = SpendDecision{
		Outcome: domain.ApprovalDenied,
		Reason:  "amount 900000.00 exceeds the hard cap of 500000.00 USD",
	}

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopToolDenied {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopToolDenied)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
	got := h.assertFinishedOnce(t, domain.StopToolDenied)
	if !strings.Contains(got.reason, "hard cap") {
		t.Errorf("reason = %q; want the gate's own words", got.reason)
	}
}

func TestRun_theSpendingGateCannotBeReached_refusesAndStops(t *testing.T) {
	// Arrange
	// Fail closed: the thing that permits could not be asked. An unreachable goal
	// engine also makes the kill switch unreadable, so the next iteration would halt
	// anyway — stopping here says which of the two it was.
	h := newHarness(t, asksFor(toolCall("call_1", "pay_invoice", map[string]any{"amount": 1500})))
	h.gate.err = errFake

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopToolDenied {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopToolDenied)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
}

func TestRun_noSpendingGateIsConfigured_refusesTheSpend(t *testing.T) {
	// Arrange
	// An instance with no goal engine is an instance where nothing may spend money.
	// A nil gate is not "no gate to pass".
	h := newHarness(t,
		asksFor(toolCall("call_1", "pay_invoice", map[string]any{"amount": 1500})),
		answered("I cannot pay anything on this instance"),
	)
	h.loop.cfg.Gate = nil

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	results := lastToolResults(t, h)
	if len(results) != 1 || !strings.Contains(results[0].Content, "no approval gate is configured") {
		t.Errorf("tool results = %+v; want the model told why nothing was spent", results)
	}
}

func TestRun_aSpendWithNoAmountInItsArguments_isNotFiled(t *testing.T) {
	// Arrange
	// The model's own arguments, so this is a mistake it can correct. What must not
	// happen is a zero reaching the gate: zero is below every threshold an operator
	// would write.
	h := newHarness(t,
		asksFor(toolCall("call_1", "pay_invoice", map[string]any{"invoice": "INV-1"})),
		answered("I need the amount before I can pay that"),
	)

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if len(h.gate.requests) != 0 {
		t.Errorf("the gate was asked to decide a spend with no amount: %+v", h.gate.requests)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	results := lastToolResults(t, h)
	if len(results) != 1 || !strings.Contains(results[0].Content, "amount") {
		t.Errorf("tool results = %+v; want the model told which argument is missing", results)
	}
}

func TestRun_aTurnAskingForMoreCallsThanTheCapAllows_stopsPartWayThrough(t *testing.T) {
	// Arrange
	// The ladder checks the cap between iterations. It cannot check it inside one, and
	// a model that asks for three calls in a single turn would otherwise put the cap
	// behind the run.
	in := testInput()
	in.Run.Limits.MaxToolCalls = 2
	h := newHarness(t, asksFor(
		toolCall("call_1", "read_file", map[string]any{"path": "a"}),
		toolCall("call_2", "read_file", map[string]any{"path": "b"}),
		toolCall("call_3", "read_file", map[string]any{"path": "c"}),
	))

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopToolCallCap {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopToolCallCap)
	}
	if got := h.tools.names(); len(got) != 2 {
		t.Errorf("tools called = %v; want exactly the two the cap allowed", got)
	}
}

func TestRun_aToolThatFails_isReportedToTheModelAndTheRunCarriesOn(t *testing.T) {
	// Arrange
	// Working around a command that did not work is what the model is there for. Ending
	// the run on one missing file would throw away everything already established.
	h := newHarness(t,
		asksFor(toolCall("call_1", "read_file", map[string]any{"path": "missing.csv"})),
		answered("that file is not there; here is what I found elsewhere"),
	)
	h.tools.call = func(_ tool.Invocation) (tool.Result, error) {
		return tool.Result{}, errFake
	}

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	results := lastToolResults(t, h)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v; want one error result", results)
	}
	// The transcript carries the failure separately from the content, so it can be read
	// for what went wrong without parsing tool output.
	steps := h.transcript.toolSteps()
	if len(steps) != 1 || steps[0].Err == "" {
		t.Errorf("tool steps = %+v; want one step carrying the failure", steps)
	}
}

func TestRun_aToolReportingItsOwnFailure_isMarkedButNotFatal(t *testing.T) {
	// Arrange
	// The tool ran and said no — a non-zero exit, a 404 from a declared API. That is
	// output, not an outage.
	h := newHarness(t,
		asksFor(toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"})),
		answered("the command failed; I will try another way"),
	)
	h.tools.call = func(_ tool.Invocation) (tool.Result, error) {
		return tool.Result{Content: "exit status 1: permission denied", IsError: true}, nil
	}

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	results := lastToolResults(t, h)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("tool results = %+v; want the failure marked", results)
	}
	if !strings.Contains(results[0].Content, "permission denied") {
		t.Errorf("result content = %q; want the tool's own output", results[0].Content)
	}
	steps := h.transcript.toolSteps()
	if len(steps) != 1 || steps[0].Err == "" {
		t.Errorf("tool steps = %+v; want the step to record that the tool failed", steps)
	}
}

func TestRun_truncatedToolOutput_saysSoInTheTranscriptAndToTheModel(t *testing.T) {
	// Arrange
	// A reply built on half a file has to be recognisable as one later.
	h := newHarness(t,
		asksFor(toolCall("call_1", "read_file", map[string]any{"path": "huge.log"})),
		answered("the log is longer than I could read"),
	)
	h.tools.call = func(_ tool.Invocation) (tool.Result, error) {
		return tool.Result{Content: "line one\n", Truncated: true}, nil
	}

	// Act
	h.run(t, testInput())

	// Assert
	results := lastToolResults(t, h)
	if len(results) != 1 || !strings.Contains(results[0].Content, "[output truncated]") {
		t.Errorf("tool results = %+v; want the truncation marked", results)
	}
	steps := h.transcript.toolSteps()
	if len(steps) != 1 || !strings.Contains(steps[0].Content, "[output truncated]") {
		t.Errorf("tool steps = %+v; want the truncation in the transcript too", steps)
	}
}

func TestRun_aTurnThatAsksForAToolAndNamesNone_isAProviderFailure(t *testing.T) {
	// Arrange
	// The adapter said tool_use and gave nothing to call. Sending the same conversation
	// again gets the same answer.
	h := newHarness(t, answer{response: provider.Response{
		Text:   "let me look that up",
		Finish: provider.FinishToolUse,
		Usage:  provider.Usage{TokensIn: 10, TokensOut: 5},
	}})

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
}

func TestSpendAmount_readsWhatAGateCanDecideAndRefusesTheRest(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      float64
		wantError bool
	}{
		{name: "a number", input: `{"amount": 1500.5}`, want: 1500.5},
		// A model asked for a number will sometimes send "1500.00". Refusing that would
		// turn a formatting habit into a run that cannot pay anything, and the gate
		// re-checks the value either way.
		{name: "a number as a string", input: `{"amount": "1500.00"}`, want: 1500},
		{name: "a padded string", input: `{"amount": " 1500 "}`, want: 1500},
		{name: "no arguments at all", input: ``, wantError: true},
		{name: "not an object", input: `[1500]`, wantError: true},
		{name: "the field is absent", input: `{"invoice": "INV-1"}`, wantError: true},
		{name: "the field is not a number", input: `{"amount": "a lot"}`, wantError: true},
		{name: "the field is null", input: `{"amount": null}`, wantError: true},
		// Zero and negative are refused here rather than at the gate. The gate denies
		// them too, but a denial recorded against an amount nobody meant to send is an
		// audit row that misleads.
		{name: "zero", input: `{"amount": 0}`, wantError: true},
		{name: "negative", input: `{"amount": -20}`, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act
			got, err := spendAmount([]byte(test.input), "amount")

			// Assert
			if test.wantError {
				if err == nil {
					t.Fatalf("spendAmount(%q) = %v, nil; want an error", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("spendAmount(%q) = %v; want %v", test.input, err, test.want)
			}
			if got != test.want {
				t.Errorf("spendAmount(%q) = %v; want %v", test.input, got, test.want)
			}
		})
	}
}

func TestToolSchemas_offersEveryToolTheRunWasGiven(t *testing.T) {
	// Arrange
	tools := &fakeTools{definitions: []tool.Definition{
		{Name: "read_file", Description: "read a file", InputSchema: map[string]any{"type": "object"}},
		{Name: "send_email", Description: "send an email", InputSchema: map[string]any{"type": "object"}},
	}}

	// Act
	got := toolSchemas(tools)

	// Assert
	if len(got) != 2 {
		t.Fatalf("toolSchemas() returned %d schemas; want 2", len(got))
	}
	if got[0].Name != "read_file" || got[0].Description != "read a file" {
		t.Errorf("schema = %+v; want the definition copied as it stands", got[0])
	}
	// The schema is a third party's contract — an MCP server's or an API's — and is
	// passed through rather than rewritten.
	if got[1].InputSchema["type"] != "object" {
		t.Errorf("schema = %+v; want the input schema carried through", got[1])
	}
}

// lastToolResults is the results the loop sent back with the most recent request.
func lastToolResults(t *testing.T, h *harness) []provider.ToolResult {
	t.Helper()
	if len(h.provider.requests) < 2 {
		t.Fatalf("the provider was called %d times; the loop never sent tool results back", len(h.provider.requests))
	}
	last := h.provider.requests[len(h.provider.requests)-1]
	return last.Messages[len(last.Messages)-1].ToolResults
}
