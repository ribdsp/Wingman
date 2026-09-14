package domain

import (
	"testing"
	"time"
)

// fixedNow is an arbitrary instant. Decide never reads a clock — now is a
// parameter — so the value only has to be stable enough to assert on.
var fixedNow = time.Date(2026, 9, 11, 10, 30, 0, 0, time.UTC)

// healthyRun is an input that continues: defaults filled, a few iterations spent,
// a readable ledger with room in it.
//
// Every test below changes exactly one field of it. That is the point: a test that
// fails names the branch it broke, and a branch that stops being reachable shows up
// as a failure here rather than as coverage quietly dropping.
func healthyRun() ContinuationInput {
	return ContinuationInput{
		Limits: RunLimits{}.WithDefaults(),
		State:  RunState{Iterations: 3, ToolCalls: 5, TokensUsed: 10_000},
		Ledger: Ledger{Readable: true, TokensToday: 50_000},
	}
}

func assertStopped(t *testing.T, got Continuation, want StopReason) {
	t.Helper()
	if got.Continue {
		t.Fatalf("run continued; expected it to stop with %q", want)
	}
	if got.Stop != want {
		t.Fatalf("stopped with %q; want %q (reason given: %s)", got.Stop, want, got.Reason)
	}
	if got.Reason == "" {
		t.Error("stopped without a reason; the audit row would say nothing")
	}
}

func TestDecide_healthyRun_continues(t *testing.T) {
	// Arrange
	in := healthyRun()

	// Act
	got := Decide(in, fixedNow)

	// Assert
	if !got.Continue {
		t.Fatalf("run stopped with %q (%s); expected it to continue", got.Stop, got.Reason)
	}
	if got.Stop != "" {
		t.Errorf("a continuing run carries stop reason %q; want empty", got.Stop)
	}
	if got.RemainingTokens != 240_000 {
		t.Errorf("remaining tokens = %d; want 240000 (the per-run allowance, which is the tighter of the two)", got.RemainingTokens)
	}
	if !got.DecidedAt.Equal(fixedNow) {
		t.Errorf("decided at %s; want the now it was given, %s", got.DecidedAt, fixedNow)
	}
}

// Branch 1.

func TestDecide_modelFinished_reportsCompleted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.ModelFinished = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopCompleted)
	if !got.Stop.Succeeded() {
		t.Error("completed does not count as success; it is the only reason that should")
	}
}

// TestDecide_modelFinishedWhileKillSwitchEngaged_reportsCompleted locks the one
// place this ladder departs from the obvious ordering.
//
// A run whose model has already answered will spend nothing further, so filing it
// as halted would record a finished run — including whatever its tool calls already
// did — under a word that means "nothing happened". If someone reorders the ladder
// to put halted first, this is the test that says so.
func TestDecide_modelFinishedWhileKillSwitchEngaged_reportsCompleted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.ModelFinished = true
	in.KillSwitchEngaged = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopCompleted)
}

// Branch 2.

func TestDecide_cancelled_reportsCancelled(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Cancelled = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopCancelled)
}

// A cancelled run is somebody's explicit instruction about this run. Recording it
// as halted would attribute it to the kill switch instead, and the two need
// different follow-up: one is done with, the other resumes when the switch clears.
func TestDecide_cancelledWhileKillSwitchEngaged_reportsCancelled(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Cancelled = true
	in.KillSwitchEngaged = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopCancelled)
}

// Branch 3.

func TestDecide_providerFailed_reportsProviderError(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.ProviderFailed = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopProviderError)
	if got.Stop.Succeeded() {
		t.Error("a provider failure counted as success")
	}
}

// Branch 4.

func TestDecide_killSwitchEngaged_reportsHalted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.KillSwitchEngaged = true

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopHalted)
}

// The kill switch outranks every budget check. An operator who engages it wants the
// run stopped for that reason, recorded under that word — not attributed to a
// coincidental cap that would have stopped it a moment later anyway.
func TestDecide_killSwitchEngagedWithExhaustedBudget_reportsHalted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.KillSwitchEngaged = true
	in.State.TokensUsed = in.Limits.MaxTokensPerRun

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopHalted)
}

// Branch 5.

func TestDecide_unreadableLedger_reportsBudgetUnreadable(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Ledger = Ledger{Readable: false, TokensToday: 0}

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopBudgetUnreadable)
	if got.RemainingTokens != 0 {
		t.Errorf("remaining tokens = %d; an unanswerable budget question must yield 0, not a number derived from a missing reading", got.RemainingTokens)
	}
}

// A counter that has gone backwards is broken, not low.
func TestDecide_negativeTokensToday_reportsBudgetUnreadable(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Ledger = Ledger{Readable: true, TokensToday: -1}

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopBudgetUnreadable)
}

// An unreadable ledger stops the run even when every cap has room. This is the
// fail-closed half of the rule; without it a broken ledger reads exactly like a
// budget nobody has touched.
func TestDecide_unreadableLedgerWithRoomInEveryCap_reportsBudgetUnreadable(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State = RunState{}
	in.Ledger.Readable = false

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopBudgetUnreadable)
}

// Branch 6.

func TestDecide_runTokenAllowanceSpent_reportsRunBudgetExhausted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State.TokensUsed = in.Limits.MaxTokensPerRun

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopRunBudgetExhausted)
}

// A zero per-run allowance is an unset limit, and an unset limit stops. Reading it
// as "no ceiling" would make a forgotten WithDefaults call the most expensive kind
// of bug there is.
func TestDecide_unsetRunTokenAllowance_reportsRunBudgetExhausted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Limits.MaxTokensPerRun = 0

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopRunBudgetExhausted)
}

// Branch 7.

func TestDecide_userDailyAllowanceSpent_reportsUserBudgetExhausted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Ledger.TokensToday = in.Limits.MaxTokensPerUserDay

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopUserBudgetExhausted)
}

func TestDecide_unsetUserDailyAllowance_reportsUserBudgetExhausted(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Limits.MaxTokensPerUserDay = 0

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopUserBudgetExhausted)
}

// NoUserDailyCap is the only way to switch the daily check off, and switching it
// off must actually work — otherwise an operator who meant it goes looking for
// another way, and finds a worse one.
func TestDecide_userDailyCapRemoved_continuesPastAnyDailySpend(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.Limits.MaxTokensPerUserDay = NoUserDailyCap
	in.Ledger.TokensToday = 9_000_000_000

	// Act
	got := Decide(in, fixedNow)

	// Assert
	if !got.Continue {
		t.Fatalf("run stopped with %q (%s); the daily cap was removed explicitly", got.Stop, got.Reason)
	}
	if got.RemainingTokens != 240_000 {
		t.Errorf("remaining tokens = %d; want 240000 — with no daily cap only the per-run allowance bounds the next call", got.RemainingTokens)
	}
}

// Branch 8.

func TestDecide_iterationsUsedUp_reportsIterationCap(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State.Iterations = in.Limits.MaxIterations

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopIterationCap)
}

// The cap is a ceiling on iterations taken, not on iterations begun: at one below
// the limit there is exactly one left.
func TestDecide_oneIterationBelowTheCap_continues(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State.Iterations = in.Limits.MaxIterations - 1

	// Act
	got := Decide(in, fixedNow)

	// Assert
	if !got.Continue {
		t.Fatalf("run stopped with %q (%s); one iteration remained", got.Stop, got.Reason)
	}
}

// Branch 9.

func TestDecide_toolCallsUsedUp_reportsToolCallCap(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State.ToolCalls = in.Limits.MaxToolCalls

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopToolCallCap)
}

// Both caps reached at once resolves to the iteration cap, because that is the
// order. An operator tuning limits reads the recorded reason as "which ceiling do I
// raise", so the two must not be interchangeable.
func TestDecide_bothCapsReached_reportsIterationCap(t *testing.T) {
	// Arrange
	in := healthyRun()
	in.State.Iterations = in.Limits.MaxIterations
	in.State.ToolCalls = in.Limits.MaxToolCalls

	// Act
	got := Decide(in, fixedNow)

	// Assert
	assertStopped(t, got, StopIterationCap)
}

// Every stop reason the ladder can produce, checked against the closed set. Two are
// not the ladder's: StopToolDenied is decided per call by ClassifyTool, and
// StopAbandoned is recorded by the sweep that finds runs whose worker died.
func TestDecide_reachesEveryLadderStopReason(t *testing.T) {
	// Arrange
	perturb := map[StopReason]func(*ContinuationInput){
		StopCompleted:           func(in *ContinuationInput) { in.ModelFinished = true },
		StopCancelled:           func(in *ContinuationInput) { in.Cancelled = true },
		StopProviderError:       func(in *ContinuationInput) { in.ProviderFailed = true },
		StopHalted:              func(in *ContinuationInput) { in.KillSwitchEngaged = true },
		StopBudgetUnreadable:    func(in *ContinuationInput) { in.Ledger.Readable = false },
		StopRunBudgetExhausted:  func(in *ContinuationInput) { in.State.TokensUsed = in.Limits.MaxTokensPerRun },
		StopUserBudgetExhausted: func(in *ContinuationInput) { in.Ledger.TokensToday = in.Limits.MaxTokensPerUserDay },
		StopIterationCap:        func(in *ContinuationInput) { in.State.Iterations = in.Limits.MaxIterations },
		StopToolCallCap:         func(in *ContinuationInput) { in.State.ToolCalls = in.Limits.MaxToolCalls },
	}
	decidedElsewhere := map[StopReason]bool{
		StopToolDenied: true,
		StopAbandoned:  true,
	}

	// Assert every reason in the closed set is accounted for. Without this, adding a
	// constant and forgetting to decide where it comes from would leave a reason that
	// can be stored but can never be produced.
	for _, reason := range AllStopReasons() {
		if _, ok := perturb[reason]; !ok && !decidedElsewhere[reason] {
			t.Errorf("stop reason %q is in AllStopReasons but nothing produces it", reason)
		}
	}

	for want, apply := range perturb {
		t.Run(string(want), func(t *testing.T) {
			// Act
			in := healthyRun()
			apply(&in)
			got := Decide(in, fixedNow)

			// Assert
			assertStopped(t, got, want)
		})
	}
}

func TestRemainingTokens_takesTheTighterOfTheTwoAllowances(t *testing.T) {
	// Arrange
	limits := RunLimits{MaxTokensPerRun: 100_000, MaxTokensPerUserDay: 120_000}
	state := RunState{TokensUsed: 10_000}
	ledger := Ledger{Readable: true, TokensToday: 90_000}

	// Act
	got := RemainingTokens(limits, state, ledger)

	// Assert
	// 90_000 left in the run, 30_000 left in the day.
	if got != 30_000 {
		t.Errorf("remaining = %d; want 30000, the daily allowance being the tighter one", got)
	}
}

func TestRemainingTokens_overspentRun_returnsZeroNotANegative(t *testing.T) {
	// Arrange
	limits := RunLimits{MaxTokensPerRun: 1_000, MaxTokensPerUserDay: NoUserDailyCap}
	state := RunState{TokensUsed: 5_000}
	ledger := Ledger{Readable: true}

	// Act
	got := RemainingTokens(limits, state, ledger)

	// Assert
	// A negative ceiling handed to a provider is either an error or, worse, ignored.
	if got != 0 {
		t.Errorf("remaining = %d; want 0", got)
	}
}

func TestRemainingTokens_unreadableLedger_returnsZero(t *testing.T) {
	// Arrange
	limits := RunLimits{}.WithDefaults()
	state := RunState{}
	ledger := Ledger{Readable: false}

	// Act
	got := RemainingTokens(limits, state, ledger)

	// Assert
	if got != 0 {
		t.Errorf("remaining = %d; want 0 — the per-run allowance is not an answer to a question about today's spend", got)
	}
}

func TestStopReason_onlyCompletedCountsAsSuccess(t *testing.T) {
	// Arrange
	all := AllStopReasons()

	// Act & Assert
	succeeded := 0
	for _, reason := range all {
		if reason.Succeeded() {
			succeeded++
			if reason != StopCompleted {
				t.Errorf("%q counts as success", reason)
			}
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d reasons count as success; want exactly 1", succeeded, len(all))
	}
}
