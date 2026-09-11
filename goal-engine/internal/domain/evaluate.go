package domain

import (
	"fmt"
	"math"
	"time"
)

// Evaluate applies Wingman's pace rules to a single goal observation and
// returns the decision together with the numbers that justify it.
//
// The order of the checks is the safety model, so it is worth stating plainly:
// the kill switch outranks everything, then goal lifecycle, then sample
// validity, then the period window, then pace, and only then rate limits.
// DecisionTrigger is the single outcome that wakes an agent, and it is the last
// thing the ladder can produce — anything ambiguous stops short of it.
func Evaluate(in EvaluationInput) Evaluation {
	g := in.Goal

	baseline := in.Observed
	if g.BaselineValue != nil {
		baseline = *g.BaselineValue
	}

	ev := Evaluation{
		GoalID:        g.ID,
		ObservedValue: in.Observed,
		TargetValue:   g.TargetValue,
		BaselineValue: baseline,
		EvaluatedAt:   in.Now,
		ElapsedRatio:  elapsedRatio(g.PeriodStart, g.PeriodEnd, in.Now),
	}
	ev.ExpectedValue = expectedValue(g, baseline, ev.ElapsedRatio)
	ev.ProgressRatio = progressRatio(g, baseline, in.Observed)
	ev.PaceRatio = paceRatio(ev.ProgressRatio, ev.ElapsedRatio)
	ev.TargetMet = targetMet(g.Comparator, in.Observed, g.TargetValue)
	ev.OnTrack = onTrack(g, in.Observed, ev.ExpectedValue)

	return decide(in, ev)
}

// decide walks the safety ladder and stamps the decision onto the evaluation.
func decide(in EvaluationInput, ev Evaluation) Evaluation {
	g := in.Goal

	if in.KillSwitchEngaged {
		return ev.with(DecisionHalted, "kill switch engaged: autonomous action suspended")
	}

	if g.Status != GoalStatusActive {
		return ev.with(DecisionSkippedInactive, fmt.Sprintf("goal status is %q, not %q", g.Status, GoalStatusActive))
	}

	if !isFinite(in.Observed) {
		return ev.with(DecisionSkippedInvalidSample,
			fmt.Sprintf("observed value %v is not a finite number: refusing to decide on a broken sample", in.Observed))
	}

	if in.Now.Before(g.PeriodStart) {
		return ev.with(DecisionSkippedNotStarted,
			fmt.Sprintf("goal period opens at %s", g.PeriodStart.Format(time.RFC3339)))
	}

	periodClosed := !in.Now.Before(g.PeriodEnd)

	// A "reach" goal is done the moment it is reached. A "stay under" goal can
	// still regress, so it only settles when its period closes.
	if ev.TargetMet && (g.Comparator == ComparatorGTE || periodClosed) {
		return ev.with(DecisionAchieved,
			fmt.Sprintf("target met: observed %.4g vs target %.4g", ev.ObservedValue, ev.TargetValue))
	}

	if periodClosed {
		return ev.with(DecisionMissed,
			fmt.Sprintf("period closed at %s with observed %.4g vs target %.4g",
				g.PeriodEnd.Format(time.RFC3339), ev.ObservedValue, ev.TargetValue))
	}

	if ev.OnTrack {
		return ev.with(DecisionNoop,
			fmt.Sprintf("on pace: observed %.4g vs expected %.4g at %.0f%% elapsed",
				ev.ObservedValue, ev.ExpectedValue, ev.ElapsedRatio*100))
	}

	offPace := fmt.Sprintf("off pace: observed %.4g vs expected %.4g at %.0f%% elapsed (pace %.2f)",
		ev.ObservedValue, ev.ExpectedValue, ev.ElapsedRatio*100, ev.PaceRatio)

	if g.MaxTriggersPerPeriod > 0 && in.TriggersThisPeriod >= g.MaxTriggersPerPeriod {
		return ev.with(DecisionTriggerBudgetExhausted,
			fmt.Sprintf("%s, but %d of %d triggers for this period are already used",
				offPace, in.TriggersThisPeriod, g.MaxTriggersPerPeriod))
	}

	if in.LastTriggeredAt != nil && in.Now.Sub(*in.LastTriggeredAt) < g.TriggerCooldown {
		return ev.with(DecisionCooldownSkipped,
			fmt.Sprintf("%s, but the last trigger was %s ago and the cooldown is %s",
				offPace, in.Now.Sub(*in.LastTriggeredAt).Round(time.Minute), g.TriggerCooldown))
	}

	return ev.with(DecisionTrigger, offPace)
}

// with returns a copy carrying the decision and its explanation. Evaluations
// are never mutated in place; each one is an audit record.
func (e Evaluation) with(d Decision, reason string) Evaluation {
	e.Decision = d
	e.Reason = reason
	return e
}

// elapsedRatio is the fraction of the goal period that has passed, clamped to
// [0,1]. A misconfigured period of zero length reads as fully elapsed so that
// it settles instead of dividing by zero.
func elapsedRatio(start, end, now time.Time) float64 {
	total := end.Sub(start)
	if total <= 0 {
		return 1
	}
	return math.Min(math.Max(now.Sub(start).Seconds()/total.Seconds(), 0), 1)
}

// expectedValue is where the metric should sit right now if progress were
// perfectly linear from baseline to target across the period.
func expectedValue(g Goal, baseline, elapsed float64) float64 {
	// When the baseline already satisfies the target there is no ramp to walk,
	// so the expectation is the target line itself: a "keep churn under 2%"
	// goal that starts at 1% is held to 2%, not to 1%.
	if targetMet(g.Comparator, baseline, g.TargetValue) {
		return g.TargetValue
	}
	return baseline + (g.TargetValue-baseline)*elapsed
}

// progressRatio is how far the metric has travelled from baseline to target,
// where 1 means the target was reached.
func progressRatio(g Goal, baseline, observed float64) float64 {
	distance := g.TargetValue - baseline
	if distance == 0 {
		if targetMet(g.Comparator, observed, g.TargetValue) {
			return 1
		}
		return 0
	}
	return (observed - baseline) / distance
}

// paceRatio compares achieved progress against elapsed time. Below 1 means
// behind schedule. Nothing can be late at the instant a period opens, so a zero
// elapsed ratio reads as exactly on pace.
func paceRatio(progress, elapsed float64) float64 {
	if elapsed <= 0 {
		return 1
	}
	return progress / elapsed
}

// onTrack applies the goal's tolerance band around the expected value. The band
// is taken from the magnitude of the expectation so that it behaves sanely for
// negative metrics too.
func onTrack(g Goal, observed, expected float64) bool {
	tolerance := g.ToleranceRatio
	if tolerance <= 0 {
		tolerance = DefaultToleranceRatio
	}
	band := math.Abs(expected) * tolerance

	if g.Comparator == ComparatorLTE {
		return observed <= expected+band
	}
	return observed >= expected-band
}

// targetMet reports whether the raw target condition holds.
func targetMet(c Comparator, observed, target float64) bool {
	if c == ComparatorLTE {
		return observed <= target
	}
	return observed >= target
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
