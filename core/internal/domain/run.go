// Package domain holds Wingman core's agent-run rules.
//
// Everything in this package is pure: no database, no HTTP, no model call, no
// clock reads. Time, budgets and observed counters are passed in; decisions come
// out. That is deliberate — these rules decide how long an autonomous agent may
// keep working and what it may spend, so they have to be exhaustively testable
// without infrastructure, and the test has to be fast enough that nobody is
// tempted to skip it.
package domain

import "time"

// StopReason is why an agent run stopped. Every run records exactly one, and the
// set is closed: a run that ended for a reason not listed here is a bug, not a
// new value to add quietly.
type StopReason string

const (
	// StopCompleted means the model answered without asking for another tool
	// call. It is the only reason that describes success.
	StopCompleted StopReason = "completed"
	// StopCancelled means a human asked this run to stop.
	StopCancelled StopReason = "cancelled"
	// StopProviderError means the last model call failed in a way the loop
	// cannot retry.
	StopProviderError StopReason = "provider_error"
	// StopHalted means the kill switch is engaged, or could not be read at all.
	StopHalted StopReason = "halted"
	// StopBudgetUnreadable means the spend ledger gave no answer. It is separate
	// from an exhausted budget on purpose: "I do not know what has been spent"
	// and "the allowance is used up" are different facts, and a run stopped for
	// the first one is waiting on a repair, not on tomorrow.
	StopBudgetUnreadable StopReason = "budget_unreadable"
	// StopRunBudgetExhausted means this run has spent its token allowance.
	StopRunBudgetExhausted StopReason = "run_budget_exhausted"
	// StopUserBudgetExhausted means the caller has spent their day's allowance.
	StopUserBudgetExhausted StopReason = "user_budget_exhausted"
	// StopIterationCap means the loop has gone round as many times as it may.
	StopIterationCap StopReason = "iteration_cap"
	// StopToolCallCap means the run has made as many tool calls as it may.
	StopToolCallCap StopReason = "tool_call_cap"
	// StopToolDenied means the run needed a tool it is not permitted to use.
	// Unlike the rest, this one is decided per tool call by ClassifyTool rather
	// than by the continuation ladder.
	StopToolDenied StopReason = "tool_denied"
	// StopAbandoned means the process running this run stopped before it finished —
	// a crash, a kill, a deploy in the middle of a run.
	//
	// It is neither a ladder outcome nor a tool decision: nothing inside a run can
	// conclude this about itself, so it is recorded by the recovery sweep that finds
	// runs left in flight by a worker that is no longer there. It exists as its own
	// reason rather than being filed under provider_error because those send an
	// operator to two different places — one to the vendor's status page, and this
	// one to their own logs.
	StopAbandoned StopReason = "abandoned"
)

// AllStopReasons is the closed set, in the order the enum in migration 000001
// declares it plus the two reasons the ladder does not produce.
//
// It exists so the set has one definition. Two tests and one migration have to agree
// on it, and a reason added to the constants but not to a list somewhere is a reason
// that gets stored and then read back as a run that ended for no known cause.
func AllStopReasons() []StopReason {
	return []StopReason{
		StopCompleted,
		StopCancelled,
		StopProviderError,
		StopHalted,
		StopBudgetUnreadable,
		StopRunBudgetExhausted,
		StopUserBudgetExhausted,
		StopIterationCap,
		StopToolCallCap,
		StopToolDenied,
		StopAbandoned,
	}
}

// Succeeded reports whether the run finished its work. Exactly one reason does.
func (s StopReason) Succeeded() bool { return s == StopCompleted }

// NoUserDailyCap disables the per-user daily token allowance.
//
// It is spelled out rather than left as a zero because a limit that is zero by
// omission and a limit that is absent by choice must not look the same to the
// evaluator. Everywhere else in this package a zero limit stops the run: an
// unset bound is a misconfiguration, and stopping is the safe reading of one.
const NoUserDailyCap int64 = -1

// RunLimits bounds one agent run. Every field is a ceiling, and a zero in any of
// them stops the run rather than releasing it — see NoUserDailyCap.
//
// WithDefaults is the only place that turns an omitted value into a working
// default, so a zero reaching Decide means something upstream forgot to call it.
type RunLimits struct {
	// MaxIterations is how many times the loop may ask the model. This is the
	// main defence against an agent that talks itself in circles.
	MaxIterations int
	// MaxToolCalls bounds side effects rather than thought. A run can reach its
	// iteration cap having done nothing; reaching this one means it acted.
	MaxToolCalls int
	// MaxTokensPerRun is the token allowance for this run alone.
	MaxTokensPerRun int64
	// MaxTokensPerUserDay is the caller's allowance across every run today, or
	// NoUserDailyCap.
	MaxTokensPerUserDay int64
	// StepTimeout bounds one model call.
	StepTimeout time.Duration
	// SandboxTimeout bounds one tool execution inside the sandbox.
	SandboxTimeout time.Duration
}

// RunState is what the run has consumed so far. The caller supplies it; nothing
// in this package counts anything for itself.
type RunState struct {
	Iterations int
	ToolCalls  int
	TokensUsed int64
}

// Ledger is what is known about the caller's spending today.
type Ledger struct {
	// Readable is false when the spend ledger could not be read. A false here is
	// not "nothing spent" — it is "no answer", and the ladder refuses on it. The
	// alternative is deciding a budget question against a number that is missing,
	// which reads exactly like a budget that is untouched.
	Readable bool
	// TokensToday is the caller's spend so far today. A negative value is
	// treated as unreadable: a counter that has gone backwards is broken.
	TokensToday int64
}

// ContinuationInput is everything Decide needs.
type ContinuationInput struct {
	Limits RunLimits
	State  RunState
	Ledger Ledger
	// KillSwitchEngaged halts the run. An unreadable flag must be passed as
	// true — unreadable means halt, the same rule the goal engine applies.
	KillSwitchEngaged bool
	// Cancelled is set when the user or an operator asked this run to stop.
	Cancelled bool
	// ProviderFailed reports that the last model call failed in a way the loop
	// cannot retry. A retryable failure is the loop's business, not the ladder's.
	ProviderFailed bool
	// ModelFinished reports that the last model response asked for no further
	// tool call — the model considers the task answered.
	ModelFinished bool
}

// Continuation is the immutable record of one decision to keep going or to stop.
//
// Every check is recorded, not only the one that ended the run: a loop that logs
// nothing until it stops cannot answer "how close to the cap was it?", which is
// the question an operator asks when deciding whether a limit is set right.
type Continuation struct {
	Continue bool
	// Stop is empty while Continue is true.
	Stop   StopReason
	Reason string
	// RemainingTokens is what the next iteration may spend. The agent passes it
	// to the provider as a response ceiling, so a run cannot overshoot its
	// budget by one expensive call.
	RemainingTokens int64
	DecidedAt       time.Time
}
