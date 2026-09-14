package domain

import (
	"fmt"
	"time"
)

// Decide reports whether an agent run may take another iteration, and if not,
// why it stopped.
//
// The order of the checks is the safety model, so it is worth reading as one:
//
//	 1  the model asked for no further tool call        completed
//	 2  a human cancelled the run                       cancelled
//	 3  the last call failed unrecoverably              provider_error
//	 4  the kill switch is engaged, or unreadable       halted
//	 5  the spend ledger gave no answer                 budget_unreadable
//	 6  this run has spent its allowance                run_budget_exhausted
//	 7  the caller has spent today's allowance          user_budget_exhausted
//	 8  the loop has gone round enough times            iteration_cap
//	 9  the run has made enough tool calls              tool_call_cap
//	10  anything left                                   continue
//
// Rows 4 to 9 are the guards on *continuing*. They are checked after rows 1 to 3
// on purpose: they answer "may this run do more work?", and that is not a question
// worth asking about a run that is already over.
//
// Which is why `completed` sits above `halted` — the one place this ladder departs
// from the obvious ordering, and the one worth arguing for. A run whose model has
// already answered will spend nothing further, so filing it as `halted` would
// record a finished run, including whatever its tool calls already did, under a
// word that means "nothing happened". Concealing side effects that did happen is
// the worse failure of the two. The kill switch loses nothing by it: it is checked
// once before the first iteration, where nothing has run yet and `halted` is the
// plain truth, and from then on it stops every iteration of every run that is not
// already finished.
//
// Rows 5 and 6 mirror the goal engine's spending gate: caps deny rather than
// escalate, and no budget question is decided against a ledger that cannot be
// read. Rows 8 and 9 are separate because reaching an iteration cap and reaching
// a tool-call cap describe different runs — one talked too long, the other acted
// too much — and an operator tuning limits needs to know which.
func Decide(in ContinuationInput, now time.Time) Continuation {
	remaining := RemainingTokens(in.Limits, in.State, in.Ledger)

	stop := func(reason StopReason, why string) Continuation {
		return Continuation{
			Stop:            reason,
			Reason:          why,
			RemainingTokens: remaining,
			DecidedAt:       now,
		}
	}

	if in.ModelFinished {
		return stop(StopCompleted, "the model answered without asking for another tool call")
	}
	if in.Cancelled {
		return stop(StopCancelled, "a human cancelled the run")
	}
	if in.ProviderFailed {
		return stop(StopProviderError, "the model provider failed in a way the loop cannot retry")
	}
	if in.KillSwitchEngaged {
		return stop(StopHalted, "the kill switch is engaged")
	}

	// A ledger that cannot be read, or one whose counter has gone backwards, is
	// not a ledger reading zero.
	if !in.Ledger.Readable || in.Ledger.TokensToday < 0 {
		return stop(StopBudgetUnreadable, "the spend ledger could not be read")
	}

	if in.Limits.MaxTokensPerRun <= 0 || in.State.TokensUsed >= in.Limits.MaxTokensPerRun {
		return stop(StopRunBudgetExhausted, fmt.Sprintf(
			"the run has used %d of its %d token allowance",
			in.State.TokensUsed, in.Limits.MaxTokensPerRun))
	}

	// NoUserDailyCap is the only way to switch this check off, and it has to be
	// written down. A zero here is an unset limit, and an unset limit stops.
	if in.Limits.MaxTokensPerUserDay != NoUserDailyCap {
		if in.Limits.MaxTokensPerUserDay <= 0 || in.Ledger.TokensToday >= in.Limits.MaxTokensPerUserDay {
			return stop(StopUserBudgetExhausted, fmt.Sprintf(
				"the caller has used %d of their %d tokens for today",
				in.Ledger.TokensToday, in.Limits.MaxTokensPerUserDay))
		}
	}

	if in.State.Iterations >= in.Limits.MaxIterations {
		return stop(StopIterationCap, fmt.Sprintf(
			"the loop has taken %d of %d permitted iterations",
			in.State.Iterations, in.Limits.MaxIterations))
	}
	if in.State.ToolCalls >= in.Limits.MaxToolCalls {
		return stop(StopToolCallCap, fmt.Sprintf(
			"the run has made %d of %d permitted tool calls",
			in.State.ToolCalls, in.Limits.MaxToolCalls))
	}

	return Continuation{
		Continue:        true,
		Reason:          fmt.Sprintf("iteration %d of %d, %d tokens left", in.State.Iterations+1, in.Limits.MaxIterations, remaining),
		RemainingTokens: remaining,
		DecidedAt:       now,
	}
}

// RemainingTokens is how many tokens the next iteration may spend.
//
// The agent passes this to the provider as the response ceiling, so a run cannot
// overshoot its budget by one expensive call. Capping at the provider is exact;
// the alternative — estimating what the next call will cost and refusing in
// advance — is a guess, and a guess that runs high refuses runs that would have
// fitted comfortably.
//
// It returns zero whenever the answer is "none", including when the ledger cannot
// be read. Zero is the safe answer to an unanswerable budget question, and a
// caller that passes it to a provider gets a refusal rather than a surprise.
func RemainingTokens(limits RunLimits, state RunState, ledger Ledger) int64 {
	if !ledger.Readable || ledger.TokensToday < 0 {
		return 0
	}

	remaining := limits.MaxTokensPerRun - state.TokensUsed
	if limits.MaxTokensPerUserDay != NoUserDailyCap {
		if daily := limits.MaxTokensPerUserDay - ledger.TokensToday; daily < remaining {
			remaining = daily
		}
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}
