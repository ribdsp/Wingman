package agent

import (
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// One test per stop reason the loop can produce, the way
// goal-engine/internal/domain/evaluate_test.go covers its ladder branch by branch.
//
// domain/decide_test.go already proves the ladder's order in isolation. What these
// prove is the other half: that the loop asks it the right questions, that it asks
// again before every iteration rather than once, and that a run ends with exactly one
// reason on its row. A later change that reorders the ladder breaks decide_test.go; one
// that stops consulting it breaks these.

func TestRun_theModelAnswers_completesTheRun(t *testing.T) {
	// Arrange
	h := newHarness(t, answered("the invoices were late because the supplier changed portals"))

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopCompleted)
	}
	h.assertFinishedOnce(t, domain.StopCompleted)
	if h.provider.calls != 1 {
		t.Errorf("the provider was called %d times; want once", h.provider.calls)
	}
}

func TestRun_cancelled_stopsBeforeCallingTheModel(t *testing.T) {
	// Arrange
	h := newHarness(t)
	h.runs.cancelled = true

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCancelled {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopCancelled)
	}
	// The point of checking before the call rather than after: a cancelled run must
	// not spend one more model call discovering it was cancelled.
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
	h.assertFinishedOnce(t, domain.StopCancelled)
}

func TestRun_theCancellationFlagIsUnreadable_stopsTheRun(t *testing.T) {
	// Arrange
	// Unreadable is not "nobody cancelled". Stopping costs the work done so far;
	// carrying on costs whatever the run does next, which somebody asked it not to do.
	h := newHarness(t)
	h.runs.cancelErr = errFake

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCancelled {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopCancelled)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
}

func TestRun_theProviderFailsUnrecoverably_stopsWithProviderError(t *testing.T) {
	// Arrange
	// A 401 is not retryable: a key that is wrong now is wrong in a minute.
	h := newHarness(t, answer{err: &provider.Error{Provider: "fake", Status: 401, Kind: "authentication_error"}})

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
	if h.provider.calls != 1 {
		t.Errorf("the provider was called %d times; want once, with no retry", h.provider.calls)
	}
	// The failure is on the run row, because "provider_error" on its own does not tell
	// an operator which provider or why.
	got := h.assertFinishedOnce(t, domain.StopProviderError)
	if !strings.Contains(got.reason, "401") {
		t.Errorf("reason = %q; want the provider's status in it", got.reason)
	}
	// And in the transcript, as a model step that bought nothing.
	if step := h.transcript.last(); step.Kind != domain.StepKindModel || step.Err == "" {
		t.Errorf("last step = %+v; want a model step carrying the failure", step)
	}
}

func TestRun_theKillSwitchIsEngaged_haltsBeforeCallingTheModel(t *testing.T) {
	// Arrange
	h := newHarness(t)
	h.halt.engaged = true

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopHalted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopHalted)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
	h.assertFinishedOnce(t, domain.StopHalted)
}

func TestRun_theKillSwitchIsUnreadable_haltsAnyway(t *testing.T) {
	// Arrange
	// The rule the goal engine applies to its own flag: unreadable means engaged. A
	// switch nobody can read is not a switch that is off, and this is the branch that
	// makes an unreachable goal engine stop agent runs instead of freeing them.
	h := newHarness(t)
	h.halt.err = errFake

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopHalted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopHalted)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
}

func TestRun_theKillSwitchIsEngagedMidRun_stopsTheNextIteration(t *testing.T) {
	// Arrange
	// The reason the ladder is consulted before every iteration instead of once. The
	// first turn asks for a tool; the switch is engaged while that tool runs.
	h := newHarness(t, asksFor(toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"})))
	h.tools.call = func(_ tool.Invocation) (tool.Result, error) {
		h.halt.engaged = true
		return tool.Result{Content: "3 invoices"}, nil
	}

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopHalted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopHalted)
	}
	// One turn happened and its tool ran. The halt stopped the second turn, not the
	// first — a run already under way is not unwound.
	if h.provider.calls != 1 {
		t.Errorf("the provider was called %d times; want once", h.provider.calls)
	}
	if len(h.tools.calls) != 1 {
		t.Errorf("the tool ran %d times; want once", len(h.tools.calls))
	}
}

func TestRun_theSpendLedgerCannotBeRead_stopsWithBudgetUnreadable(t *testing.T) {
	// Arrange
	// No budget question is decided against a number that is missing, because a
	// missing number reads exactly like a budget that is untouched.
	h := newHarness(t)
	h.budget.ledgerErr = errFake

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopBudgetUnreadable {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopBudgetUnreadable)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
	h.assertFinishedOnce(t, domain.StopBudgetUnreadable)
}

func TestRun_theRunHasSpentItsAllowance_stopsWithRunBudgetExhausted(t *testing.T) {
	// Arrange
	in := testInput()
	in.Run.State.TokensUsed = in.Run.Limits.MaxTokensPerRun
	h := newHarness(t)

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopRunBudgetExhausted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopRunBudgetExhausted)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
}

func TestRun_theUserHasSpentTodaysAllowance_stopsWithUserBudgetExhausted(t *testing.T) {
	// Arrange
	in := testInput()
	h := newHarness(t)
	// A cap that binds across every run the user starts today, not just this one.
	h.budget.ledger = domain.Ledger{Readable: true, TokensToday: in.Run.Limits.MaxTokensPerUserDay}

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopUserBudgetExhausted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopUserBudgetExhausted)
	}
	if h.provider.calls != 0 {
		t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
	}
}

func TestRun_theModelKeepsGoing_stopsAtTheIterationCap(t *testing.T) {
	// Arrange
	// Two turns permitted, and a model that never stops asking for tools. This is the
	// defence against an agent that talks itself in circles, and it only works if the
	// loop is counting its own turns.
	in := testInput()
	in.Run.Limits.MaxIterations = 2

	call := toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"})
	h := newHarness(t, asksFor(call), asksFor(call))

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopIterationCap {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopIterationCap)
	}
	// Exactly the permitted number of turns. A third would have been unscripted and
	// the fake would have failed it.
	if h.provider.calls != 2 {
		t.Errorf("the provider was called %d times; want 2", h.provider.calls)
	}
	h.assertFinishedOnce(t, domain.StopIterationCap)
}

func TestRun_theRunHasMadeEnoughToolCalls_stopsAtTheToolCallCap(t *testing.T) {
	// Arrange
	// One tool call permitted, spent in the first turn. The cap is reached between
	// iterations here; tools_test.go covers the same cap reached inside one.
	in := testInput()
	in.Run.Limits.MaxToolCalls = 1
	h := newHarness(t, asksFor(toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"})))

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopToolCallCap {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopToolCallCap)
	}
	if len(h.tools.calls) != 1 {
		t.Errorf("the tool ran %d times; want once", len(h.tools.calls))
	}
}

func TestRun_theModelCallsAToolItWasNotGranted_stopsWithToolDenied(t *testing.T) {
	// Arrange
	// The model was shown exactly what it may use, so a name outside that set is not a
	// mistake to negotiate over. Being told "no, try again" teaches an agent to guess.
	h := newHarness(t, asksFor(toolCall("call_1", "wire_transfer", map[string]any{"to": "someone"})))

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopToolDenied {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopToolDenied)
	}
	if len(h.tools.calls) != 0 {
		t.Errorf("the tool ran %d times; want not at all", len(h.tools.calls))
	}
	// The attempt is in the transcript, so an operator can see what it tried to call.
	steps := h.transcript.toolSteps()
	if len(steps) != 1 || steps[0].ToolName != "wire_transfer" || steps[0].Err == "" {
		t.Errorf("tool steps = %+v; want one denied step naming wire_transfer", steps)
	}
	h.assertFinishedOnce(t, domain.StopToolDenied)
}

func TestRun_aGrantThatIsSwitchedOff_stopsWithToolDenied(t *testing.T) {
	// Arrange
	// enabled: false denies. An operator who switched a tool off did not mean "ask me".
	h := newHarness(t, asksFor(toolCall("call_1", "delete_everything", nil)))

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

func TestRun_aWholeRun_readsAToolAndAnswers(t *testing.T) {
	// Arrange
	// Dispatch to stop reason with no infrastructure: a stubbed provider, stubbed
	// tools, fake repositories. This is the test that proves the pieces are wired to
	// each other and not merely to their own fakes.
	h := newHarness(t,
		asksFor(toolCall("call_1", "read_file", map[string]any{"path": "invoices.csv"})),
		answered("three invoices are late; the supplier moved to a new portal"),
	)
	h.tools.call = func(invocation tool.Invocation) (tool.Result, error) {
		return tool.Result{Content: "INV-1,late\nINV-2,late\nINV-3,late"}, nil
	}
	in := testInput()

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	h.assertFinishedOnce(t, domain.StopCompleted)

	// The transcript is the run: the model's reasoning, then what the tool returned,
	// then the answer. In that order, because a run killed between two steps should
	// leave the reasoning behind rather than a gap.
	wantKinds := []domain.StepKind{domain.StepKindModel, domain.StepKindTool, domain.StepKindModel}
	got := h.transcript.kinds()
	if len(got) != len(wantKinds) {
		t.Fatalf("transcript = %v; want %v", got, wantKinds)
	}
	for i := range wantKinds {
		if got[i] != wantKinds[i] {
			t.Errorf("step %d is %q; want %q", i+1, got[i], wantKinds[i])
		}
	}
	// Indexes start at 1 and do not repeat: repository.AppendStep refuses a zero, and
	// a repeated index is a transcript that cannot be ordered.
	for i, step := range h.transcript.steps {
		if step.Index != i+1 {
			t.Errorf("step %d has index %d; want %d", i+1, step.Index, i+1)
		}
		if step.RunID != in.Run.ID {
			t.Errorf("step %d has run id %q; want %q", i+1, step.RunID, in.Run.ID)
		}
		if step.At.IsZero() {
			t.Errorf("step %d has no timestamp", i+1)
		}
	}

	// The tool call carried this user's workspace, and nothing else's.
	if len(h.tools.calls) != 1 {
		t.Fatalf("the tool ran %d times; want once", len(h.tools.calls))
	}
	if h.tools.calls[0].Workspace != in.Workspace {
		t.Errorf("workspace = %q; want %q", h.tools.calls[0].Workspace, in.Workspace)
	}

	// Both model calls were charged to the owner, and the tool call was not.
	if len(h.budget.charges) != 2 {
		t.Fatalf("charges = %+v; want one per model call", h.budget.charges)
	}
	for _, charge := range h.budget.charges {
		if charge.UserID != in.Run.OwnerUserID || charge.RunID != in.Run.ID {
			t.Errorf("charge = %+v; want it booked to the run's owner", charge)
		}
		if charge.Provider != "fake" || charge.Model != "test-model" {
			t.Errorf("charge = %+v; want the provider and model that answered", charge)
		}
	}

	// The second request carried the conversation forward: the opening brief, the
	// assistant turn with its tool call, and the result answering that call by id.
	if len(h.provider.requests) != 2 {
		t.Fatalf("the provider was called %d times; want 2", len(h.provider.requests))
	}
	second := h.provider.requests[1]
	if len(second.Messages) != 3 {
		t.Fatalf("the second call carried %d messages; want 3", len(second.Messages))
	}
	results := second.Messages[2].ToolResults
	if len(results) != 1 || results[0].CallID != "call_1" {
		t.Errorf("tool results = %+v; want one answering call_1", results)
	}
	if results[0].IsError {
		t.Error("the tool result is marked an error; the tool succeeded")
	}
	// Every request must survive the port's own validation, which is what both vendors
	// would otherwise reject at the wire.
	for i, request := range h.provider.requests {
		if err := request.Validate(); err != nil {
			t.Errorf("request %d: %v", i+1, err)
		}
	}
}

// ---------------------------------------------------------------------------
// the answer, which is the one part of a run a person reads

func TestRun_theOutcomeCarriesTheModelsLastWords(t *testing.T) {
	// Arrange
	h := newHarness(t, answered("  the supplier changed portals  "))

	// Act
	outcome := h.run(t, testInput())

	// Assert — the runner puts this in the chat as the assistant's reply. An outcome that
	// answered and reported nothing would leave the person who asked with a system note
	// saying their run stopped.
	if outcome.Answer != "the supplier changed portals" {
		t.Errorf("answer = %q; want the model's reply, trimmed", outcome.Answer)
	}
}

func TestRun_aRunStoppedBeforeTheModelSpokeCarriesNoAnswer(t *testing.T) {
	// Arrange
	h := newHarness(t)
	h.runs.cancelled = true

	// Act
	outcome := h.run(t, testInput())

	// Assert — an empty answer is what makes the runner write a system note naming the stop
	// reason, rather than an empty assistant message that reads as the agent having nothing
	// to say.
	if outcome.Answer != "" {
		t.Errorf("answer = %q; want none", outcome.Answer)
	}
	if outcome.Reason == "" {
		t.Error("the outcome has no reason; a run with no answer is the one that needs one")
	}
}

func TestLastAssistantText_skipsAClosingTurnThatOnlyAskedForATool(t *testing.T) {
	// Arrange — the last thing the model did was call a tool, and the run ended on the
	// ladder before it could speak again.
	messages := []provider.Message{
		{Role: provider.RoleUser, Text: "find out why the invoices are late"},
		{Role: provider.RoleAssistant, Text: "the supplier changed portals"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{toolCall("call_1", "read_file", nil)}},
		{Role: provider.RoleUser, ToolResults: []provider.ToolResult{{CallID: "call_1", Content: "ok"}}},
	}

	// Act
	got := lastAssistantText(messages)

	// Assert — scanned backwards for a non-empty turn, so a tool call does not blank out
	// the sentence before it.
	if got != "the supplier changed portals" {
		t.Errorf("answer = %q; want the last thing the model actually said", got)
	}
}

func TestLastAssistantText_isEmptyWhenTheModelNeverSaidAnything(t *testing.T) {
	// Arrange — halted or cancelled before the first reply.
	messages := []provider.Message{{Role: provider.RoleUser, Text: "find out why the invoices are late"}}

	// Act
	got := lastAssistantText(messages)

	// Assert — the brief must never come back as the agent's own answer.
	if got != "" {
		t.Errorf("answer = %q; want none", got)
	}
}
