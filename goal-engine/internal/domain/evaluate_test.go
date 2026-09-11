package domain

import (
	"math"
	"testing"
	"time"
)

// Period used by most cases: a 7-day goal window.
var (
	periodStart = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	midPeriod   = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) // exactly 50%
)

func ptr(v float64) *float64 { return &v }

func timePtr(t time.Time) *time.Time { return &t }

// reachGoal is "get active users from 5k to 10k this week".
func reachGoal() Goal {
	return Goal{
		ID:                   "goal-1",
		Product:              "acme",
		Title:                "10k active users this week",
		MetricKey:            "activeUsersWeekly",
		Comparator:           ComparatorGTE,
		TargetValue:          10000,
		BaselineValue:        ptr(5000),
		PeriodStart:          periodStart,
		PeriodEnd:            periodEnd,
		Status:               GoalStatusActive,
		TriggerCooldown:      6 * time.Hour,
		MaxTriggersPerPeriod: 3,
		BotID:                "bot-growth",
		ChannelID:            "chan-acme",
	}
}

// keepUnderGoal is "keep weekly churn under 2%, starting from 5%".
func keepUnderGoal() Goal {
	g := reachGoal()
	g.ID = "goal-2"
	g.Title = "Keep churn under 2%"
	g.MetricKey = "churnRateWeekly"
	g.Comparator = ComparatorLTE
	g.TargetValue = 2
	g.BaselineValue = ptr(5)
	return g
}

func TestEvaluateReturnsNoopWhenGoalIsExactlyOnPace(t *testing.T) {
	// Arrange: halfway through the period, halfway from 5k to 10k.
	in := EvaluationInput{Goal: reachGoal(), Observed: 7500, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionNoop {
		t.Fatalf("decision = %q, want %q (reason: %s)", got.Decision, DecisionNoop, got.Reason)
	}
	if !got.OnTrack {
		t.Error("OnTrack = false, want true")
	}
	if got.ExpectedValue != 7500 {
		t.Errorf("ExpectedValue = %v, want 7500", got.ExpectedValue)
	}
	if math.Abs(got.PaceRatio-1) > 1e-9 {
		t.Errorf("PaceRatio = %v, want 1", got.PaceRatio)
	}
	if math.Abs(got.ElapsedRatio-0.5) > 1e-9 {
		t.Errorf("ElapsedRatio = %v, want 0.5", got.ElapsedRatio)
	}
}

func TestEvaluateTriggersWhenGoalFallsBehindPaceBeyondTolerance(t *testing.T) {
	// Arrange: halfway through, only 6000 of the 7500 needed. Pace 0.2.
	in := EvaluationInput{Goal: reachGoal(), Observed: 6000, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
	if got.OnTrack {
		t.Error("OnTrack = true, want false")
	}
	if !got.ShouldDispatch() {
		t.Error("ShouldDispatch() = false, want true")
	}
	if math.Abs(got.ProgressRatio-0.2) > 1e-9 {
		t.Errorf("ProgressRatio = %v, want 0.2", got.ProgressRatio)
	}
}

func TestEvaluateStaysOnTrackWhenShortfallIsWithinTolerance(t *testing.T) {
	// Arrange: expected 7500, tolerance 5% => floor is 7125.
	in := EvaluationInput{Goal: reachGoal(), Observed: 7200, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionNoop {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionNoop)
	}
}

func TestEvaluateTriggersWhenShortfallIsJustOutsideTolerance(t *testing.T) {
	// Arrange: 7100 is below the 7125 tolerance floor.
	in := EvaluationInput{Goal: reachGoal(), Observed: 7100, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateHonoursCustomToleranceRatio(t *testing.T) {
	// Arrange: a 20% tolerance makes 6200 acceptable against expected 7500.
	g := reachGoal()
	g.ToleranceRatio = 0.20
	in := EvaluationInput{Goal: g, Observed: 6200, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionNoop {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionNoop)
	}
}

func TestEvaluateSkipsTriggerWhileCooldownIsActive(t *testing.T) {
	// Arrange: behind pace, but a trigger fired one hour ago (cooldown 6h).
	in := EvaluationInput{
		Goal:               reachGoal(),
		Observed:           6000,
		Now:                midPeriod,
		LastTriggeredAt:    timePtr(midPeriod.Add(-1 * time.Hour)),
		TriggersThisPeriod: 1,
	}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionCooldownSkipped {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionCooldownSkipped)
	}
	if got.ShouldDispatch() {
		t.Error("ShouldDispatch() = true, want false during cooldown")
	}
}

func TestEvaluateTriggersOnceCooldownHasElapsed(t *testing.T) {
	// Arrange: last trigger was 7 hours ago, cooldown is 6 hours.
	in := EvaluationInput{
		Goal:               reachGoal(),
		Observed:           6000,
		Now:                midPeriod,
		LastTriggeredAt:    timePtr(midPeriod.Add(-7 * time.Hour)),
		TriggersThisPeriod: 1,
	}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateStopsTriggeringAfterPerPeriodBudgetIsExhausted(t *testing.T) {
	// Arrange: 3 of 3 triggers already used this period.
	in := EvaluationInput{
		Goal:               reachGoal(),
		Observed:           6000,
		Now:                midPeriod,
		LastTriggeredAt:    timePtr(midPeriod.Add(-24 * time.Hour)),
		TriggersThisPeriod: 3,
	}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionTriggerBudgetExhausted {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTriggerBudgetExhausted)
	}
}

func TestEvaluateAllowsUnlimitedTriggersWhenBudgetIsZero(t *testing.T) {
	// Arrange: MaxTriggersPerPeriod 0 means unlimited.
	g := reachGoal()
	g.MaxTriggersPerPeriod = 0
	in := EvaluationInput{
		Goal:               g,
		Observed:           6000,
		Now:                midPeriod,
		LastTriggeredAt:    timePtr(midPeriod.Add(-24 * time.Hour)),
		TriggersThisPeriod: 99,
	}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateHaltsEverythingWhenKillSwitchIsEngaged(t *testing.T) {
	// Arrange: badly behind pace, but the kill switch is on.
	in := EvaluationInput{
		Goal:              reachGoal(),
		Observed:          10,
		Now:               midPeriod,
		KillSwitchEngaged: true,
	}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionHalted {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionHalted)
	}
	if got.ShouldDispatch() {
		t.Error("ShouldDispatch() = true, want false while halted")
	}
}

func TestEvaluateSettlesReachGoalAsSoonAsTargetIsReached(t *testing.T) {
	// Arrange: 10k reached at the halfway mark.
	in := EvaluationInput{Goal: reachGoal(), Observed: 10000, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionAchieved {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionAchieved)
	}
	if !got.TargetMet {
		t.Error("TargetMet = false, want true")
	}
}

func TestEvaluateDoesNotSettleStayUnderGoalMidPeriod(t *testing.T) {
	// Arrange: churn is already under the 2% ceiling, but the week is not over
	// so it can still regress.
	in := EvaluationInput{Goal: keepUnderGoal(), Observed: 1.5, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionNoop {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionNoop)
	}
	if !got.TargetMet {
		t.Error("TargetMet = false, want true (1.5 <= 2)")
	}
}

func TestEvaluateSettlesStayUnderGoalWhenPeriodClosesWithinTarget(t *testing.T) {
	// Arrange: period is over and churn ended under the ceiling.
	in := EvaluationInput{Goal: keepUnderGoal(), Observed: 1.9, Now: periodEnd}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionAchieved {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionAchieved)
	}
}

func TestEvaluateTracksPaceForStayUnderGoalUsingDescendingLine(t *testing.T) {
	// Arrange: churn must fall 5% -> 2%; halfway the line sits at 3.5%.
	// 4.0 is above the 3.675 tolerance ceiling, so it is off track.
	in := EvaluationInput{Goal: keepUnderGoal(), Observed: 4.0, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.ExpectedValue != 3.5 {
		t.Errorf("ExpectedValue = %v, want 3.5", got.ExpectedValue)
	}
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateMarksGoalMissedWhenPeriodClosesShort(t *testing.T) {
	// Arrange: the week is over at 9k of 10k.
	in := EvaluationInput{Goal: reachGoal(), Observed: 9000, Now: periodEnd.Add(time.Minute)}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionMissed {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionMissed)
	}
	if got.ShouldDispatch() {
		t.Error("ShouldDispatch() = true, want false after the period closed")
	}
}

func TestEvaluateSkipsGoalsWhosePeriodHasNotStarted(t *testing.T) {
	// Arrange
	in := EvaluationInput{Goal: reachGoal(), Observed: 5000, Now: periodStart.Add(-time.Hour)}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionSkippedNotStarted {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionSkippedNotStarted)
	}
}

func TestEvaluateSkipsGoalsThatAreNotActive(t *testing.T) {
	// Arrange
	for _, status := range []GoalStatus{GoalStatusPaused, GoalStatusArchived, GoalStatusAchieved, GoalStatusMissed} {
		g := reachGoal()
		g.Status = status
		in := EvaluationInput{Goal: g, Observed: 10, Now: midPeriod}

		// Act
		got := Evaluate(in)

		// Assert
		if got.Decision != DecisionSkippedInactive {
			t.Errorf("status %q: decision = %q, want %q", status, got.Decision, DecisionSkippedInactive)
		}
	}
}

func TestEvaluateUsesObservedValueAsBaselineWhenBaselineIsUnset(t *testing.T) {
	// Arrange: no baseline captured yet, so the first observation is the
	// baseline and nothing can be behind pace.
	g := reachGoal()
	g.BaselineValue = nil
	in := EvaluationInput{Goal: g, Observed: 4200, Now: periodStart}

	// Act
	got := Evaluate(in)

	// Assert
	if got.BaselineValue != 4200 {
		t.Errorf("BaselineValue = %v, want 4200", got.BaselineValue)
	}
	if got.Decision != DecisionNoop {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionNoop)
	}
}

func TestEvaluateTreatsBaselineAlreadyPastTargetAsAchieved(t *testing.T) {
	// Arrange: baseline 11k already clears a 10k target.
	g := reachGoal()
	g.BaselineValue = ptr(11000)
	in := EvaluationInput{Goal: g, Observed: 11500, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.Decision != DecisionAchieved {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionAchieved)
	}
}

func TestEvaluateUsesFlatCeilingWhenStayUnderGoalStartsCompliant(t *testing.T) {
	// Arrange: churn starts at 1% against a 2% ceiling. There is nothing to
	// descend, so the expectation is the ceiling itself and 2.5% is off track.
	g := keepUnderGoal()
	g.BaselineValue = ptr(1)
	in := EvaluationInput{Goal: g, Observed: 2.5, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.ExpectedValue != 2 {
		t.Errorf("ExpectedValue = %v, want 2 (flat ceiling)", got.ExpectedValue)
	}
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateClampsElapsedRatioAndSurvivesZeroLengthPeriod(t *testing.T) {
	// Arrange: a misconfigured goal whose period has no duration.
	g := reachGoal()
	g.PeriodEnd = g.PeriodStart
	in := EvaluationInput{Goal: g, Observed: 6000, Now: g.PeriodStart}

	// Act
	got := Evaluate(in)

	// Assert: it must not divide by zero, and a closed period that fell short
	// is a miss.
	if got.ElapsedRatio != 1 {
		t.Errorf("ElapsedRatio = %v, want 1", got.ElapsedRatio)
	}
	if got.Decision != DecisionMissed {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionMissed)
	}
}

func TestEvaluateSurvivesBaselineEqualToTarget(t *testing.T) {
	// Arrange: a zero-distance goal must not divide by zero.
	g := reachGoal()
	g.BaselineValue = ptr(10000)
	in := EvaluationInput{Goal: g, Observed: 9000, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if math.IsNaN(got.ProgressRatio) || math.IsInf(got.ProgressRatio, 0) {
		t.Errorf("ProgressRatio = %v, want a finite number", got.ProgressRatio)
	}
	if got.Decision != DecisionTrigger {
		t.Fatalf("decision = %q, want %q", got.Decision, DecisionTrigger)
	}
}

func TestEvaluateRefusesToActOnNonFiniteObservation(t *testing.T) {
	// Arrange: a broken metric query yields NaN or ±Inf. Because every float
	// comparison against NaN is false, an unguarded evaluator would read this
	// as "behind pace" and dispatch a task off garbage data.
	for _, observed := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		in := EvaluationInput{Goal: reachGoal(), Observed: observed, Now: midPeriod}

		// Act
		got := Evaluate(in)

		// Assert
		if got.Decision != DecisionSkippedInvalidSample {
			t.Errorf("observed %v: decision = %q, want %q", observed, got.Decision, DecisionSkippedInvalidSample)
		}
		if got.ShouldDispatch() {
			t.Errorf("observed %v: ShouldDispatch() = true, want false", observed)
		}
	}
}

func TestEvaluateAlwaysRecordsEvaluationMetadata(t *testing.T) {
	// Arrange
	in := EvaluationInput{Goal: reachGoal(), Observed: 6000, Now: midPeriod}

	// Act
	got := Evaluate(in)

	// Assert
	if got.GoalID != "goal-1" {
		t.Errorf("GoalID = %q, want %q", got.GoalID, "goal-1")
	}
	if !got.EvaluatedAt.Equal(midPeriod) {
		t.Errorf("EvaluatedAt = %v, want %v", got.EvaluatedAt, midPeriod)
	}
	if got.Reason == "" {
		t.Error("Reason is empty; every decision must be explainable in the audit log")
	}
	if got.TargetValue != 10000 {
		t.Errorf("TargetValue = %v, want 10000", got.TargetValue)
	}
}
