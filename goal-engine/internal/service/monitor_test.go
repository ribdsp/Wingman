package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// The fixtures put "now" one third of the way through a monthly period, which is
// where pace arithmetic is easiest to reason about: 35% elapsed, so a goal that
// has moved 10% of the way to its target is unambiguously behind.
var (
	testNow         = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	testPeriodStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	testPeriodEnd   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

const testMetric = "mrr.total"

func fixedClock(t time.Time) Clock { return func() time.Time { return t } }

func float64Ptr(v float64) *float64 { return &v }

// testGoal is an active "reach 100 by the end of the month" goal with its
// baseline already captured, which is the steady state the monitor mostly sees.
func testGoal() repository.GoalRecord {
	return repository.GoalRecord{
		Goal: domain.Goal{
			ID:                   "goal-1",
			Product:              "acme",
			Title:                "Reach 100M MRR",
			SourceText:           "Get MRR to 100 million by the end of September.",
			MetricKey:            testMetric,
			Comparator:           domain.ComparatorGTE,
			TargetValue:          100,
			BaselineValue:        float64Ptr(0),
			PeriodStart:          testPeriodStart,
			PeriodEnd:            testPeriodEnd,
			Status:               domain.GoalStatusActive,
			ToleranceRatio:       0.05,
			TriggerCooldown:      6 * time.Hour,
			MaxTriggersPerPeriod: 5,
			BotID:                "bot-1",
			ChannelID:            "chan-1",
		},
	}
}

// monitorFixture wires a monitor to fakes a test can reach into.
type monitorFixture struct {
	monitor     *Monitor
	goals       *fakeGoals
	samples     *fakeSamples
	evaluations *fakeEvaluations
	dispatches  *fakeDispatches
	flags       *fakeFlags
	audit       *fakeAudit
	metrics     *fakeMetrics
	sampler     *fakeSampler
	tasks       *fakeTasks
	notifier    *fakeNotifier
}

// newMonitorFixture builds a monitor observing `observed` for every due goal.
func newMonitorFixture(t *testing.T, observed float64, due ...repository.GoalRecord) *monitorFixture {
	t.Helper()
	f := &monitorFixture{
		goals:       newFakeGoals(due...),
		samples:     &fakeSamples{},
		evaluations: &fakeEvaluations{},
		dispatches:  &fakeDispatches{},
		flags:       &fakeFlags{},
		audit:       &fakeAudit{},
		metrics:     newFakeMetrics(testMetric),
		sampler:     &fakeSampler{value: observed, at: testNow},
		tasks:       &fakeTasks{response: core.TaskResponse{TaskID: "task-9", StatusCode: 201}},
		notifier:    &fakeNotifier{},
	}

	monitor, err := NewMonitor(MonitorDeps{
		Goals:        f.goals,
		Samples:      f.samples,
		LatestSample: f.samples,
		Evaluations:  f.evaluations,
		Dispatches:   f.dispatches,
		Flags:        f.flags,
		Audit:        f.audit,
		Metrics:      f.metrics,
		Sampler:      f.sampler,
		Tasks:        f.tasks,
		Notices:      NewNotices(NoticesDeps{Notifier: f.notifier, ConsoleBaseURL: "https://console.wingman.test"}),
		Clock:        fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a monitor, got %v", err)
	}
	f.monitor = monitor
	return f
}

func TestNewMonitorNamesEveryMissingDependency(t *testing.T) {
	// A monitor runs unattended, so a wiring mistake has to fail at startup rather
	// than as a nil dereference three hours later.
	_, err := NewMonitor(MonitorDeps{})
	if err == nil {
		t.Fatal("expected an incomplete monitor to be refused")
	}
	for _, name := range []string{"Goals", "Samples", "LatestSample", "Evaluations", "Dispatches", "Flags", "Audit", "Metrics", "Sampler", "Tasks"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %s to be named in %v", name, err)
		}
	}
}

func TestNewMonitorFillsInItsOptionalDefaults(t *testing.T) {
	f := newMonitorFixture(t, 10)
	if f.monitor.batchSize != defaultBatchSize {
		t.Fatalf("expected the default batch size, got %d", f.monitor.batchSize)
	}
	if f.monitor.location != time.UTC {
		t.Fatalf("expected UTC by default, got %v", f.monitor.location)
	}
	if f.monitor.maxSampleAge != defaultMaxSampleAge {
		t.Fatalf("expected the default staleness limit, got %v", f.monitor.maxSampleAge)
	}

	monitor, err := NewMonitor(MonitorDeps{
		Goals: f.goals, Samples: f.samples, LatestSample: f.samples,
		Evaluations: f.evaluations,
		Dispatches:  f.dispatches, Flags: f.flags, Audit: f.audit,
		Metrics: f.metrics, Sampler: f.sampler, Tasks: f.tasks,
		BatchSize: -5, MaxSampleAge: -time.Hour,
	})
	if err != nil {
		t.Fatalf("expected a monitor, got %v", err)
	}
	if monitor.batchSize != defaultBatchSize || monitor.clock == nil {
		t.Fatalf("expected meaningless values to be replaced, got %+v", monitor)
	}
	// A negative staleness limit would make every reported value stale, which would
	// stop every push-based goal rather than loosen anything.
	if monitor.maxSampleAge != defaultMaxSampleAge {
		t.Fatalf("expected a negative staleness limit to be replaced, got %v", monitor.maxSampleAge)
	}
}

func TestTickWakesAnAgentWhenAGoalIsBehindPace(t *testing.T) {
	// Arrange: 35% of the period gone, 10% of the way to target.
	f := newMonitorFixture(t, 10, testGoal())

	// Act
	result, err := f.monitor.Tick(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Checked != 1 || result.Triggered != 1 || result.Failed != 0 {
		t.Fatalf("unexpected result %+v", result)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionTrigger {
		t.Fatalf("expected a trigger decision, got %q", got)
	}
	if len(f.samples.inserted) != 1 || f.samples.inserted[0].MetricKey != testMetric {
		t.Fatalf("expected the observation to be stored, got %+v", f.samples.inserted)
	}
	if len(f.tasks.requests) != 1 {
		t.Fatalf("expected exactly one agent task, got %d", len(f.tasks.requests))
	}
	if len(f.dispatches.created) != 1 || len(f.dispatches.sent) != 1 {
		t.Fatalf("expected the dispatch to be recorded and marked sent, got %+v / %v",
			f.dispatches.created, f.dispatches.sent)
	}
	if !f.audit.has(ActionTriggerDispatch) {
		t.Fatalf("expected a dispatch audit entry, got %v", f.audit.actions())
	}
}

func TestTickSendsABriefThatNamesTheGoalAndTheNumbers(t *testing.T) {
	// The brief is the whole interface to the agent: if it does not carry the gap,
	// the agent is being woken with nothing to act on.
	f := newMonitorFixture(t, 10, testGoal())

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	brief := f.tasks.last().Brief
	for _, want := range []string{"Reach 100M MRR", "acme", testMetric, "approval API"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("expected %q in the brief:\n%s", want, brief)
		}
	}
	if f.tasks.last().Metadata["goalId"] != "goal-1" {
		t.Fatalf("expected the goal id in the metadata, got %+v", f.tasks.last().Metadata)
	}
}

func TestTickLeavesAGoalAloneWhenItIsOnPace(t *testing.T) {
	// 35% elapsed, 35% of the way to target.
	f := newMonitorFixture(t, 35, testGoal())

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Triggered != 0 || len(f.tasks.requests) != 0 {
		t.Fatalf("expected no agent to be woken, got %+v", result)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionNoop {
		t.Fatalf("expected a noop decision, got %q", got)
	}
	// The observation is still recorded: the point of the time series is that it
	// covers the quiet periods too.
	if len(f.samples.inserted) != 1 || len(f.evaluations.inserted) != 1 {
		t.Fatal("expected the quiet check to still be recorded")
	}
}

func TestTickRefusesToRunAtAllWhenTheKillSwitchIsEngaged(t *testing.T) {
	// Halted means paused, not "carry on quietly": no goal is even read, so nothing
	// can be dispatched by a later bug in this function.
	f := newMonitorFixture(t, 10, testGoal())
	f.flags.engaged = true

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected a halted tick to be reported without an error, got %v", err)
	}
	if !result.Halted || result.Checked != 0 {
		t.Fatalf("expected a halted tick, got %+v", result)
	}
	if f.goals.listCalls != 0 {
		t.Fatal("expected no goals to be read while halted")
	}
	if len(f.tasks.requests) != 0 {
		t.Fatal("expected no agent to be woken while halted")
	}
}

func TestTickAbortsWhenTheKillSwitchCannotBeRead(t *testing.T) {
	// An unreadable kill switch is not "off". Reading a failure as false would let
	// a database blip re-enable autonomous action the operator had disabled.
	f := newMonitorFixture(t, 10, testGoal())
	f.flags.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err == nil {
		t.Fatal("expected an unreadable kill switch to abort the tick")
	}
	if result.Checked != 0 || f.goals.listCalls != 0 || len(f.tasks.requests) != 0 {
		t.Fatalf("expected nothing to happen, got %+v", result)
	}
}

func TestTickAbortsWhenTheGoalListCannotBeRead(t *testing.T) {
	f := newMonitorFixture(t, 10, testGoal())
	f.goals.listErr = errBoom

	if _, err := f.monitor.Tick(context.Background()); err == nil {
		t.Fatal("expected an unreadable goal list to abort the tick")
	}
}

func TestTickCapturesTheBaselineFromTheFirstObservation(t *testing.T) {
	goal := testGoal()
	goal.BaselineValue = nil
	f := newMonitorFixture(t, 42, goal)

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got, ok := f.goals.baselines["goal-1"]; !ok || got != 42 {
		t.Fatalf("expected the baseline to be stored as 42, got %v (present=%v)", got, ok)
	}
	if !f.audit.has(ActionBaselineCaptured) {
		t.Fatalf("expected the baseline capture to be audited, got %v", f.audit.actions())
	}
	if f.evaluations.last().BaselineValue != 42 {
		t.Fatalf("expected the evaluation to use the captured baseline, got %v", f.evaluations.last().BaselineValue)
	}
}

func TestTickWithholdsATriggerOnTheTickThatCapturedTheBaseline(t *testing.T) {
	// With one observation and no history, "no progress" restates the baseline
	// rather than reporting a trend. A backdated goal must not wake an agent before
	// a second observation exists to compare against.
	goal := testGoal()
	goal.BaselineValue = nil
	goal.TargetValue = 100
	f := newMonitorFixture(t, 0, goal)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionTrigger {
		t.Fatalf("expected the evaluation to still record the trigger decision, got %q", got)
	}
	if result.Triggered != 0 || len(f.tasks.requests) != 0 || len(f.dispatches.created) != 0 {
		t.Fatalf("expected no agent to be woken, got %+v", result)
	}
}

func TestTickAbortsAGoalWhenTheBaselineCannotBeStored(t *testing.T) {
	// Evaluating against a baseline that was not persisted would mean the next tick
	// measures progress from a different starting line.
	goal := testGoal()
	goal.BaselineValue = nil
	f := newMonitorFixture(t, 42, goal)
	f.goals.baselineErr = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.evaluations.inserted) != 0 {
		t.Fatalf("expected the goal to fail before being evaluated, got %+v", result)
	}
}

func TestTickSettlesAGoalThatReachedItsTarget(t *testing.T) {
	f := newMonitorFixture(t, 120, testGoal())

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Settled != 1 || result.Triggered != 0 {
		t.Fatalf("expected the goal to settle, got %+v", result)
	}
	if f.goals.statuses["goal-1"] != domain.GoalStatusAchieved {
		t.Fatalf("expected an achieved goal, got %q", f.goals.statuses["goal-1"])
	}
	if !f.audit.has(ActionGoalSettled) {
		t.Fatalf("expected the settlement to be audited, got %v", f.audit.actions())
	}
	if len(f.tasks.requests) != 0 {
		t.Fatal("expected no agent to be woken for a goal that is already met")
	}
}

func TestTickSettlesAGoalThatMissedItsDeadline(t *testing.T) {
	f := newMonitorFixture(t, 50, testGoal())
	// Move the clock past the period end.
	f.monitor.clock = fixedClock(testPeriodEnd.Add(time.Hour))

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Settled != 1 {
		t.Fatalf("expected the goal to settle, got %+v", result)
	}
	if f.goals.statuses["goal-1"] != domain.GoalStatusMissed {
		t.Fatalf("expected a missed goal, got %q", f.goals.statuses["goal-1"])
	}
	if len(f.tasks.requests) != 0 {
		t.Fatal("expected no agent to be woken after the deadline has passed")
	}
}

func TestTickReportsAGoalItCouldNotSettle(t *testing.T) {
	f := newMonitorFixture(t, 120, testGoal())
	f.goals.statusErr = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || result.Settled != 0 {
		t.Fatalf("expected the goal to be reported as failed, got %+v", result)
	}
}

func TestTickRefusesToDecideOnABrokenSample(t *testing.T) {
	// Every float comparison against NaN is false, so an unguarded evaluator reads
	// a broken metric as "off track" and wakes an agent on nothing.
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		f := newMonitorFixture(t, value, testGoal())

		result, err := f.monitor.Tick(context.Background())
		if err != nil {
			t.Fatalf("expected the tick to succeed for %v, got %v", value, err)
		}
		if got := f.evaluations.last().Decision; got != domain.DecisionSkippedInvalidSample {
			t.Fatalf("expected %v to be skipped as an invalid sample, got %q", value, got)
		}
		if result.Triggered != 0 || len(f.tasks.requests) != 0 {
			t.Fatalf("expected no agent to be woken on %v", value)
		}
	}
}

func TestTickKeepsGoingWhenOneGoalFails(t *testing.T) {
	// One broken metric query must not stop every other goal from being watched.
	broken := testGoal()
	broken.ID = "goal-broken"
	broken.MetricKey = "not.declared"
	second := testGoal()
	second.ID = "goal-2"

	f := newMonitorFixture(t, 10, testGoal(), broken, second)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Checked != 3 || result.Failed != 1 || result.Triggered != 2 {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "goal-broken") {
		t.Fatalf("expected the failing goal to be named, got %v", result.Errors)
	}
}

func TestTickReportsASamplerFailureWithoutRecordingAnEvaluation(t *testing.T) {
	// An unread metric is not an observation of zero, and writing an evaluation for
	// it would put a number in the audit trail that was never measured.
	f := newMonitorFixture(t, 10, testGoal())
	f.sampler.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.evaluations.inserted) != 0 || len(f.samples.inserted) != 0 {
		t.Fatalf("expected nothing to be recorded, got %+v", result)
	}
}

func TestTickRecordsTheDispatchBeforeCallingCore(t *testing.T) {
	// The ordering is the guarantee that the engine cannot act invisibly: if core
	// is called first and the process dies, an agent is working with no record of
	// why.
	f := newMonitorFixture(t, 10, testGoal())
	f.tasks.err = &core.Error{StatusCode: 503, Retryable: true, Body: "unavailable"}

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if len(f.dispatches.created) != 1 {
		t.Fatalf("expected the dispatch to have been recorded before the call, got %+v", f.dispatches.created)
	}
	if len(f.dispatches.failed) != 1 || len(f.dispatches.sent) != 0 {
		t.Fatalf("expected the dispatch to be marked failed, got failed=%v sent=%v",
			f.dispatches.failed, f.dispatches.sent)
	}
	if !strings.Contains(f.dispatches.failures[0], "503") {
		t.Fatalf("expected the status in the stored failure, got %q", f.dispatches.failures[0])
	}
	if !f.audit.has(ActionTriggerFailed) {
		t.Fatalf("expected the failure to be audited, got %v", f.audit.actions())
	}
	if result.Failed != 1 || result.Triggered != 0 {
		t.Fatalf("expected the goal to be reported as failed, got %+v", result)
	}
}

func TestTickTreatsADuplicateDispatchAsWorkAlreadyUnderWay(t *testing.T) {
	// A conflict means this evaluation was already dispatched. Sending it again
	// would double the work the agent does.
	f := newMonitorFixture(t, 10, testGoal())
	f.dispatches.createErr = repository.ErrConflict

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Failed != 0 {
		t.Fatalf("expected a duplicate not to be a failure, got %+v", result)
	}
	if len(f.tasks.requests) != 0 {
		t.Fatal("expected no second task to be sent")
	}
}

func TestTickDerivesTheIdempotencyKeyFromTheEvaluation(t *testing.T) {
	// Retrying one decision must not create a second task, while a genuinely new
	// decision must get its own key.
	f := newMonitorFixture(t, 10, testGoal())

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	key := f.tasks.last().IdempotencyKey
	if !strings.HasPrefix(key, dispatchKeyPrefix) {
		t.Fatalf("expected a namespaced key, got %q", key)
	}
	if f.dispatches.created[0].IdempotencyKey != key {
		t.Fatalf("expected the stored key to match the sent one, got %q and %q",
			f.dispatches.created[0].IdempotencyKey, key)
	}

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the second tick to succeed, got %v", err)
	}
	if f.tasks.last().IdempotencyKey == key {
		t.Fatal("expected a new evaluation to produce a new key")
	}
}

func TestTickRefusesToDispatchWhenNoBotWouldRunTheTask(t *testing.T) {
	// A task addressed to nobody is worse than no task: it looks handled.
	goal := testGoal()
	goal.BotID = ""
	f := newMonitorFixture(t, 10, goal)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.tasks.requests) != 0 || len(f.dispatches.created) != 0 {
		t.Fatalf("expected the dispatch to be refused, got %+v", result)
	}
	if !strings.Contains(result.Errors[0], "no bot") {
		t.Fatalf("expected the reason to be plain, got %v", result.Errors)
	}
}

func TestTickFallsBackToTheConfiguredDefaultBot(t *testing.T) {
	goal := testGoal()
	goal.BotID = ""
	goal.ChannelID = ""
	f := newMonitorFixture(t, 10, goal)
	f.monitor.defaultBotID = "fallback-bot"
	f.monitor.defaultChannelID = "fallback-chan"

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got := f.tasks.last(); got.BotID != "fallback-bot" || got.ChannelID != "fallback-chan" {
		t.Fatalf("expected the default bot to be used, got %+v", got)
	}
}

func TestTickStopsWhenTheContextIsCancelled(t *testing.T) {
	// Shutdown must not mean hammering a database that is going away.
	f := newMonitorFixture(t, 35, testGoal(), testGoal(), testGoal())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := f.monitor.Tick(ctx)
	if err != nil {
		t.Fatalf("expected the tick to report rather than fail, got %v", err)
	}
	if result.Checked != 1 {
		t.Fatalf("expected the tick to stop after the first goal, got %+v", result)
	}
}

func TestTickSurvivesAnAuditFailure(t *testing.T) {
	// The action has already happened by the time the audit entry is written.
	// Failing the tick would make the monitor retry something that must not repeat.
	f := newMonitorFixture(t, 10, testGoal())
	f.audit.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Triggered != 1 {
		t.Fatalf("expected the agent still to be woken, got %+v", result)
	}
}

func TestTickReportsAFailureToMarkADispatchSent(t *testing.T) {
	f := newMonitorFixture(t, 10, testGoal())
	f.dispatches.sentErr = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("expected the bookkeeping failure to be reported, got %+v", result)
	}
}

func TestTickBoundsTheErrorsItCarriesBack(t *testing.T) {
	// A tick that fails on a thousand goals must not return a thousand strings.
	due := make([]repository.GoalRecord, 0, maxTickErrors+5)
	for i := 0; i < maxTickErrors+5; i++ {
		goal := testGoal()
		goal.ID = "goal-" + itoa(i)
		goal.MetricKey = "not.declared"
		due = append(due, goal)
	}
	f := newMonitorFixture(t, 10, due...)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != maxTickErrors+5 {
		t.Fatalf("expected every goal to be counted, got %+v", result)
	}
	if len(result.Errors) != maxTickErrors {
		t.Fatalf("expected the error list to be capped at %d, got %d", maxTickErrors, len(result.Errors))
	}
}

func TestTickHonoursACooldownRatherThanRedispatching(t *testing.T) {
	f := newMonitorFixture(t, 10, testGoal())
	recent := testNow.Add(-time.Hour)
	f.dispatches.history = repository.DispatchHistory{LastTriggeredAt: &recent, CountThisPeriod: 1}

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionCooldownSkipped {
		t.Fatalf("expected the cooldown to hold, got %q", got)
	}
	if result.Triggered != 0 || len(f.tasks.requests) != 0 {
		t.Fatalf("expected no agent to be woken during a cooldown, got %+v", result)
	}
}

func TestTickStopsAtTheTriggerBudget(t *testing.T) {
	// This is the anti-task-storm limit: a goal that stays behind all month must
	// not wake an agent every minute for a month.
	f := newMonitorFixture(t, 10, testGoal())
	f.dispatches.history = repository.DispatchHistory{CountThisPeriod: 5}

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionTriggerBudgetExhausted {
		t.Fatalf("expected the budget to be exhausted, got %q", got)
	}
	if result.Triggered != 0 {
		t.Fatalf("expected no dispatch, got %+v", result)
	}
}

func TestTickReportsAGoalWhoseHistoryCannotBeRead(t *testing.T) {
	// Without the history the cooldown and the trigger budget are both unknown, so
	// deciding anyway would mean deciding without the rate limits.
	f := newMonitorFixture(t, 10, testGoal())
	f.dispatches.historyErr = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.evaluations.inserted) != 0 {
		t.Fatalf("expected the goal to fail before being evaluated, got %+v", result)
	}
}

func TestTickReportsAGoalWhoseObservationCannotBeStored(t *testing.T) {
	f := newMonitorFixture(t, 10, testGoal())
	f.samples.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.tasks.requests) != 0 {
		t.Fatalf("expected the goal to fail before any dispatch, got %+v", result)
	}
}

func TestTickReportsAGoalWhoseEvaluationCannotBeStored(t *testing.T) {
	// The evaluation row is the justification for waking an agent. Dispatching
	// without it would leave an action nobody can explain afterwards.
	f := newMonitorFixture(t, 10, testGoal())
	f.evaluations.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 || len(f.tasks.requests) != 0 {
		t.Fatalf("expected no dispatch without a stored evaluation, got %+v", result)
	}
}

func TestTickCountsEveryDecisionItProduced(t *testing.T) {
	behind := testGoal()
	onPace := testGoal()
	onPace.ID = "goal-2"
	f := newMonitorFixture(t, 10, behind, onPace)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Decisions[domain.DecisionTrigger] != 2 {
		t.Fatalf("expected both decisions to be counted, got %+v", result.Decisions)
	}
	if result.Duration < 0 || result.StartedAt.IsZero() {
		t.Fatalf("expected the timing to be reported, got %+v", result)
	}
}

func TestTickSkipsAGoalThatIsNoLongerActive(t *testing.T) {
	goal := testGoal()
	goal.Status = domain.GoalStatusPaused
	f := newMonitorFixture(t, 10, goal)

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if got := f.evaluations.last().Decision; got != domain.DecisionSkippedInactive {
		t.Fatalf("expected a paused goal to be skipped, got %q", got)
	}
	if result.Triggered != 0 {
		t.Fatalf("expected no dispatch for a paused goal, got %+v", result)
	}
}

// --- notifications ---
//
// The last thing a dispatch does, and the only part of it nobody depends on.

func TestTickNotifiesAHumanThatAnAgentIsNowWorkingUnattended(t *testing.T) {
	f := newMonitorFixture(t, 10, testGoal())

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if len(f.notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %+v", f.notifier.sent)
	}
	sent := f.notifier.sent[0]
	if sent.Kind != core.NotifyTrigger {
		t.Fatalf("expected a trigger notification, got %q", sent.Kind)
	}
	if sent.SubjectID != "goal-1" {
		t.Fatalf("expected the notification to name the goal, got %q", sent.SubjectID)
	}
	if sent.Link != "https://console.wingman.test/goals/goal-1" {
		t.Fatalf("expected a link to the goal, got %q", sent.Link)
	}
	// Not the gap, not the observed value, not the target. Those are on the screen
	// the link opens, where the evaluation history is next to them.
	for _, r := range sent.Headline {
		if r >= '0' && r <= '9' {
			t.Fatalf("expected no figure in a headline, got %q", sent.Headline)
		}
	}
}

func TestTickNotifiesNobodyAboutAQuietCheck(t *testing.T) {
	// A goal on pace is the normal case and happens every tick. Messaging it would
	// make the notification channel worthless within a day.
	f := newMonitorFixture(t, 35, testGoal())

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if len(f.notifier.sent) != 0 {
		t.Fatalf("expected no notification, got %+v", f.notifier.sent)
	}
}

func TestTickNotifiesNobodyWhenTheDispatchItselfFailed(t *testing.T) {
	// No agent was woken, so there is nothing to tell anybody they are now watching.
	// The failure is in the dispatch row and the tick result instead.
	f := newMonitorFixture(t, 10, testGoal())
	f.tasks.err = errBoom

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if len(f.notifier.sent) != 0 {
		t.Fatalf("expected no notification, got %+v", f.notifier.sent)
	}
}

func TestTickDispatchesIdenticallyWhenTheNotifierFails(t *testing.T) {
	// A chat platform being down must not turn a dispatched trigger into a failed
	// goal: the agent is already running and the console already shows it.
	f := newMonitorFixture(t, 10, testGoal())
	f.notifier.err = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick to succeed, got %v", err)
	}
	if result.Triggered != 1 || result.Failed != 0 {
		t.Fatalf("expected the dispatch to stand, got %+v", result)
	}
	if len(f.dispatches.sent) != 1 {
		t.Fatalf("expected the dispatch still marked sent, got %v", f.dispatches.sent)
	}
	if !f.audit.has(ActionTriggerDispatch) || f.audit.has(ActionTriggerFailed) {
		t.Fatalf("expected the audit trail unchanged, got %v", f.audit.actions())
	}
}

func TestFailDispatchRecordsATransportFailureWithNoStatus(t *testing.T) {
	// A transport failure has no HTTP status at all, and a stored zero would read
	// as one.
	f := newMonitorFixture(t, 10, testGoal())
	f.tasks.err = &core.Error{Retryable: true, Err: errors.New("connection refused")}

	if _, err := f.monitor.Tick(context.Background()); err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if len(f.dispatches.failed) != 1 {
		t.Fatalf("expected the dispatch to be marked failed, got %v", f.dispatches.failed)
	}
	if !strings.Contains(f.dispatches.failures[0], "connection refused") {
		t.Fatalf("expected the cause to be stored, got %q", f.dispatches.failures[0])
	}
}

func TestFailDispatchSurvivesBeingUnableToRecordTheFailure(t *testing.T) {
	// Losing the bookkeeping must not lose the error the caller is already
	// returning.
	f := newMonitorFixture(t, 10, testGoal())
	f.tasks.err = errBoom
	f.dispatches.failErr = errBoom

	result, err := f.monitor.Tick(context.Background())
	if err != nil {
		t.Fatalf("expected the tick itself to survive, got %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("expected the dispatch failure to be reported, got %+v", result)
	}
}
