package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// A push metric is one the engine cannot read: the number lives in somebody's
// dashboard or somebody's head, and arrives by being reported. These tests cover
// what a tick does with a goal measured on one, where the interesting cases are
// not the happy path but the ways a feed can quietly die.

// newPushFixture is the monitor fixture with its metric redeclared as push, and
// with a reported value already on file unless the test removes it.
func newPushFixture(t *testing.T, reported *repository.SampleRecord, due ...repository.GoalRecord) *monitorFixture {
	t.Helper()
	f := newMonitorFixture(t, 999, due...)
	f.metrics.declare(testMetric, metrics.SourcePush)
	f.samples.latest = reported
	return f
}

// reportedValue is a value posted an hour ago, which is what a healthy feed looks
// like.
func reportedValue(value float64) *repository.SampleRecord {
	return &repository.SampleRecord{
		ID:         77,
		MetricKey:  testMetric,
		Value:      value,
		ObservedAt: testNow.Add(-time.Hour),
		Source:     string(metrics.SourcePush),
	}
}

func TestATickJudgesAPushGoalOnTheLastReportedValue(t *testing.T) {
	// Arrange: 35% of the period gone, 10% of the way to target, and the 10 arrived
	// by being reported rather than read. The sampler is primed with 999 so that a
	// tick which pulled by mistake would reach a different conclusion.
	f := newPushFixture(t, reportedValue(10), testGoal())

	// Act
	result, err := f.monitor.Tick(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("expected the tick to run, got %v", err)
	}
	if result.Failed != 0 || result.Triggered != 1 {
		t.Fatalf("expected one trigger and no failure, got %+v", result)
	}
	if len(f.sampler.observed) != 0 {
		t.Fatalf("expected nothing to be pulled for a reported metric, got %v", f.sampler.observed)
	}
	if len(f.samples.inserted) != 0 {
		// Re-storing the same reported value once per tick would turn one observation
		// into a flat line, and a dead feed would read as a steady one.
		t.Fatalf("expected no new observation to be stored, got %+v", f.samples.inserted)
	}
	if len(f.evaluations.inserted) != 1 {
		t.Fatalf("expected one evaluation, got %d", len(f.evaluations.inserted))
	}
	if f.evaluations.inserted[0].ObservedValue != 10 {
		t.Fatalf("expected the reported value to be judged, got %v", f.evaluations.inserted[0].ObservedValue)
	}
	if id := f.evaluations.sampleIDs[0]; id == nil || *id != 77 {
		// The evaluation has to point at the exact reading it was made from, or
		// "why did it trigger" has no answer.
		t.Fatalf("expected the evaluation to cite the reported observation, got %v", id)
	}
}

func TestAPushGoalWithNothingReportedYetIsAVisibleFailure(t *testing.T) {
	// A goal nobody has fed cannot be judged. Skipping it quietly would leave an
	// operator believing it is being watched.
	f := newPushFixture(t, nil, testGoal())

	result, err := f.monitor.Tick(context.Background())

	if err != nil {
		t.Fatalf("expected one goal's problem not to abort the tick, got %v", err)
	}
	if result.Failed != 1 || result.Triggered != 0 {
		t.Fatalf("expected a reported failure and no trigger, got %+v", result)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], testMetric) {
		t.Fatalf("expected the metric to be named in the error, got %v", result.Errors)
	}
	if len(f.evaluations.inserted) != 0 {
		t.Fatal("expected no evaluation to be recorded from a value that does not exist")
	}
}

func TestAStaleReportedValueStopsTheGoalRatherThanFreezingIt(t *testing.T) {
	// This is the failure the staleness limit exists for. If the job feeding a metric
	// dies, the last number stays where it was: the goal looks exactly as on-track as
	// it did the moment the feed stopped, and nothing would be triggered again. A
	// failed check says so; a stale reading would not.
	stale := reportedValue(10)
	stale.ObservedAt = testNow.Add(-3 * 24 * time.Hour)
	f := newPushFixture(t, stale, testGoal())

	result, err := f.monitor.Tick(context.Background())

	if err != nil {
		t.Fatalf("expected the tick to run, got %v", err)
	}
	if result.Failed != 1 || result.Triggered != 0 {
		t.Fatalf("expected a reported failure and no trigger, got %+v", result)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "stale") {
		t.Fatalf("expected staleness to be named in the error, got %v", result.Errors)
	}
	if len(f.evaluations.inserted) != 0 {
		t.Fatal("expected no evaluation from a stale value")
	}
	if len(f.tasks.requests) != 0 {
		t.Fatal("expected no agent to be woken on a number nobody stands behind")
	}
}

func TestTheStalenessLimitToleratesAFeedThatRanLate(t *testing.T) {
	// A daily job that ran a few hours late is not a dead feed, and treating it as
	// one would make the engine cry wolf every morning.
	late := reportedValue(10)
	late.ObservedAt = testNow.Add(-25 * time.Hour)
	f := newPushFixture(t, late, testGoal())

	result, err := f.monitor.Tick(context.Background())

	if err != nil {
		t.Fatalf("expected the tick to run, got %v", err)
	}
	if result.Failed != 0 || result.Triggered != 1 {
		t.Fatalf("expected the late value to still be judged, got %+v (%v)", result, result.Errors)
	}
}

func TestAnUnreadableReportedValueFailsTheGoalNotTheTick(t *testing.T) {
	// One goal's storage problem must not stop every other goal from being watched.
	f := newPushFixture(t, nil, testGoal())
	f.samples.latestErr = errors.New("connection reset")

	result, err := f.monitor.Tick(context.Background())

	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("expected the goal to be reported as failed, got %+v", result)
	}
}

func TestAPushGoalCapturesItsBaselineFromTheReportedValue(t *testing.T) {
	// A goal created before anything was reported still needs a starting point, and
	// it has to come from the same place every later reading does.
	goal := testGoal()
	goal.Goal.BaselineValue = nil
	f := newPushFixture(t, reportedValue(30), goal)

	result, err := f.monitor.Tick(context.Background())

	if err != nil {
		t.Fatalf("expected the tick to run, got %v", err)
	}
	if result.Failed != 0 {
		t.Fatalf("expected no failure, got %v", result.Errors)
	}
	if f.goals.baselines[goal.ID] != 30 {
		t.Fatalf("expected the reported value to become the baseline, got %+v", f.goals.baselines)
	}
	if !f.audit.has(ActionBaselineCaptured) {
		t.Fatal("expected the captured baseline to be audited")
	}
}

func TestAPulledGoalStillStoresWhatItRead(t *testing.T) {
	// The push path must not have changed how an ordinary metric behaves: a value the
	// engine read itself is stored every tick, which is what makes the time series.
	f := newMonitorFixture(t, 10, testGoal())

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to run, got %v", err)
	}

	if len(f.samples.inserted) != 1 {
		t.Fatalf("expected the reading to be stored, got %d", len(f.samples.inserted))
	}
	if got := f.samples.inserted[0].Source; got != string(metrics.SourceSQL) {
		t.Fatalf("expected the metric's own source to be recorded, got %q", got)
	}
	if len(f.sampler.observed) != 1 {
		t.Fatalf("expected the metric to be pulled once, got %v", f.sampler.observed)
	}
}
