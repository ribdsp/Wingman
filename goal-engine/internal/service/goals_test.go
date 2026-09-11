package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

type goalsFixture struct {
	goals    *Goals
	registry *fakeRegistry
	metrics  *fakeMetrics
	audit    *fakeAudit
}

func newGoalsFixture(t *testing.T, record repository.GoalRecord) *goalsFixture {
	t.Helper()
	f := &goalsFixture{
		registry: newFakeRegistry(record),
		metrics:  newFakeMetrics(testMetric),
		audit:    &fakeAudit{},
	}
	goals, err := NewGoals(GoalsDeps{
		Goals:   f.registry,
		Metrics: f.metrics,
		Audit:   f.audit,
		Clock:   fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a goals service, got %v", err)
	}
	f.goals = goals
	return f
}

// testActor is an operator asking for something through the API.
func testActor() Actor {
	return Actor{Type: repository.ActorUser, ID: "ops", RequestID: "req-1"}
}

// testBot is an agent in Wingman core asking for something through the API.
func testBot() Actor {
	return Actor{Type: repository.ActorBot, ID: "bot-growth", RequestID: "req-1"}
}

// changesFromAudit pulls the before/after diff out of the last audit entry.
func changesFromAudit(t *testing.T, f *goalsFixture) map[string]any {
	t.Helper()
	if len(f.audit.events) == 0 {
		t.Fatal("expected an audit entry")
	}
	var detail struct {
		Changes map[string]any `json:"changes"`
	}
	last := f.audit.events[len(f.audit.events)-1]
	if err := json.Unmarshal([]byte(last.Detail), &detail); err != nil {
		t.Fatalf("audit detail is not JSON: %v (%s)", err, last.Detail)
	}
	return detail.Changes
}

func TestNewGoalsNamesEveryMissingDependency(t *testing.T) {
	_, err := NewGoals(GoalsDeps{})
	if err == nil {
		t.Fatal("expected construction to fail")
	}
	for _, name := range []string{"Goals", "Metrics", "Audit"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %q in %q", name, err)
		}
	}
}

func TestNewGoalsDefaultsTheClock(t *testing.T) {
	goals, err := NewGoals(GoalsDeps{
		Goals:   newFakeRegistry(testGoal()),
		Metrics: newFakeMetrics(testMetric),
		Audit:   &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("expected a goals service, got %v", err)
	}
	if goals.clock == nil {
		t.Fatal("expected a default clock")
	}
}

func TestCreateStoresTheGoalAndRecordsWhoAskedFor(t *testing.T) {
	// The audit entry is the whole point of the registry: a target nobody can trace
	// back to a person is a target nobody owns.
	f := newGoalsFixture(t, testGoal())

	record, err := f.goals.Create(context.Background(), testGoal().Goal, testActor())
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if record.Title != "Reach 100M MRR" || record.CreatedBy != "ops" {
		t.Fatalf("unexpected record: %+v", record)
	}
	if len(f.registry.created) != 1 {
		t.Fatalf("expected one stored goal, got %d", len(f.registry.created))
	}
	if len(f.audit.events) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(f.audit.events))
	}
	event := f.audit.events[0]
	if event.Action != ActionGoalCreated || event.SubjectType != SubjectGoal {
		t.Fatalf("unexpected audit entry: %+v", event)
	}
	if event.ActorType != repository.ActorUser || event.ActorID != "ops" || event.RequestID != "req-1" {
		t.Fatalf("expected the actor on the entry, got %+v", event)
	}
	for _, want := range []string{"targetValue", "metricKey", "periodEnd", "triggerCooldown"} {
		if !strings.Contains(event.Detail, want) {
			t.Fatalf("expected %q in the audit detail: %s", want, event.Detail)
		}
	}
}

func TestCreateFillsTheSafetyLimitsALeftBlankGoalWouldOtherwiseLack(t *testing.T) {
	// A goal created with blank limits must not become the most aggressive goal in
	// the registry.
	f := newGoalsFixture(t, testGoal())
	goal := testGoal().Goal
	goal.ToleranceRatio = 0
	goal.TriggerCooldown = 0
	goal.MaxTriggersPerPeriod = 0
	goal.Comparator = ""
	goal.Status = ""

	record, err := f.goals.Create(context.Background(), goal, testActor())
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if record.ToleranceRatio != domain.DefaultToleranceRatio {
		t.Fatalf("expected the default tolerance, got %v", record.ToleranceRatio)
	}
	if record.TriggerCooldown != domain.DefaultTriggerCooldown {
		t.Fatalf("expected the default cooldown, got %v", record.TriggerCooldown)
	}
	if record.MaxTriggersPerPeriod != domain.DefaultMaxTriggersPerPeriod {
		t.Fatalf("expected the default trigger budget, got %d", record.MaxTriggersPerPeriod)
	}
	if record.Comparator != domain.ComparatorGTE || record.Status != domain.GoalStatusActive {
		t.Fatalf("unexpected defaults: %+v", record.Goal)
	}
}

func TestCreateStartsAGoalNowWhenNoStartIsGiven(t *testing.T) {
	// "Reach 100M by October" is a sentence about the time between saying it and
	// October.
	f := newGoalsFixture(t, testGoal())
	goal := testGoal().Goal
	goal.PeriodStart = time.Time{}

	record, err := f.goals.Create(context.Background(), goal, testActor())
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if !record.PeriodStart.Equal(testNow) {
		t.Fatalf("expected the period to start now, got %s", record.PeriodStart)
	}
}

func TestCreateRefusesAGoalPointingAtAMetricNobodyDeclared(t *testing.T) {
	// Metrics are declared by the operator in config. A goal naming one that does
	// not exist would fail on every tick, forever.
	f := newGoalsFixture(t, testGoal())
	goal := testGoal().Goal
	goal.MetricKey = "revenue.invented"

	_, err := f.goals.Create(context.Background(), goal, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if field := ValidationFields(err)["metricKey"]; field == "" {
		t.Fatalf("expected the error to name metricKey, got %v", err)
	}
	if len(f.registry.created) != 0 {
		t.Fatal("expected nothing to be stored")
	}
	if len(f.audit.events) != 0 {
		t.Fatal("expected no audit entry for a rejected goal")
	}
}

func TestCreateReportsEveryStructuralProblemAtOnce(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	goal := testGoal().Goal
	goal.Product = ""
	goal.Title = ""

	_, err := f.goals.Create(context.Background(), goal, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	fields := ValidationFields(err)
	if fields["product"] == "" || fields["title"] == "" {
		t.Fatalf("expected both fields reported, got %v", fields)
	}
	if len(f.registry.created) != 0 {
		t.Fatal("expected nothing to be stored")
	}
}

func TestCreateRefusesAGoalWhoseDeadlineHasAlreadyPassed(t *testing.T) {
	// Structurally fine, but it would be settled as missed on the first tick. That
	// is a typo, not an intention.
	f := newGoalsFixture(t, testGoal())
	goal := testGoal().Goal
	goal.PeriodStart = testNow.Add(-30 * 24 * time.Hour)
	goal.PeriodEnd = testNow.Add(-24 * time.Hour)

	_, err := f.goals.Create(context.Background(), goal, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "past") {
		t.Fatalf("expected the reason to mention the past, got %v", err)
	}
	if len(f.registry.created) != 0 {
		t.Fatal("expected nothing to be stored")
	}
}

func TestCreateRefusesAnUnattributedGoal(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	_, err := f.goals.Create(context.Background(), testGoal().Goal, Actor{Type: repository.ActorUser, ID: "  "})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if len(f.registry.created) != 0 {
		t.Fatal("expected nothing to be stored")
	}
}

func TestCreateTreatsAnUnknownActorTypeAsABot(t *testing.T) {
	// An unnamed caller gets the least privileged reading. Recording a bot as an
	// operator would be the mistake that matters.
	f := newGoalsFixture(t, testGoal())

	_, err := f.goals.Create(context.Background(), testGoal().Goal, Actor{Type: "superuser", ID: "who"})
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if got := f.audit.events[0].ActorType; got != repository.ActorBot {
		t.Fatalf("expected the actor recorded as a bot, got %q", got)
	}
}

func TestCreateReportsAStorageFailureWithoutClaimingSuccess(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.createErr = errors.New("unique violation")

	_, err := f.goals.Create(context.Background(), testGoal().Goal, testActor())
	if err == nil || !strings.Contains(err.Error(), "unique violation") {
		t.Fatalf("expected the storage error, got %v", err)
	}
	if len(f.audit.events) != 0 {
		t.Fatal("expected no audit entry for a goal that was not stored")
	}
}

func TestCreateSucceedsEvenWhenTheAuditWriteFails(t *testing.T) {
	// The goal is already stored. Failing the caller now would leave them believing
	// it was not.
	f := newGoalsFixture(t, testGoal())
	f.audit.err = errors.New("audit table is full")

	if _, err := f.goals.Create(context.Background(), testGoal().Goal, testActor()); err != nil {
		t.Fatalf("expected the create to stand, got %v", err)
	}
}

func TestGetRejectsABlankID(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	if _, err := f.goals.Get(context.Background(), "  "); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if f.registry.getCalls != 0 {
		t.Fatal("expected no query for a blank id")
	}
}

func TestGetTranslatesAMissingRowIntoTheServiceSentinel(t *testing.T) {
	// The repository's own errors stop at this boundary, so a handler never has to
	// know what storage this service uses.
	f := newGoalsFixture(t, testGoal())
	f.registry.getErr = repository.ErrNotFound

	_, err := f.goals.Get(context.Background(), "goal-1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if errors.Is(err, repository.ErrNotFound) {
		t.Fatal("expected the repository error not to leak")
	}
}

func TestGetReportsAnUnexpectedStorageFailure(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.getErr = errors.New("connection reset")

	_, err := f.goals.Get(context.Background(), "goal-1")
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected the storage error, got %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a broken connection is not a missing goal")
	}
}

func TestListClampsThePageSizeSoNobodyCanAskForTheWholeTable(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		offset     int
		wantLimit  int
		wantOffset int
	}{
		{"no limit given", 0, 0, defaultPageLimit, 0},
		{"limit above the ceiling", maxPageLimit + 500, 0, maxPageLimit, 0},
		{"negative limit", -5, 0, defaultPageLimit, 0},
		{"negative offset", 10, -20, 10, 0},
		{"a reasonable page", 25, 50, 25, 50},
	}
	for _, c := range cases {
		f := newGoalsFixture(t, testGoal())
		if _, _, err := f.goals.List(context.Background(), repository.GoalFilter{Limit: c.limit, Offset: c.offset}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := f.registry.listed[0]
		if got.Limit != c.wantLimit || got.Offset != c.wantOffset {
			t.Fatalf("%s: got limit %d offset %d, want %d/%d", c.name, got.Limit, got.Offset, c.wantLimit, c.wantOffset)
		}
	}
}

func TestListRejectsAStatusThatIsNotAStatus(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	_, _, err := f.goals.List(context.Background(), repository.GoalFilter{Status: "nearly"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if len(f.registry.listed) != 0 {
		t.Fatal("expected no query for an impossible filter")
	}
}

func TestListPassesTheFilterThroughAndReturnsTheTotal(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.listTotal = 42

	records, total, err := f.goals.List(context.Background(), repository.GoalFilter{
		Product: "acme",
		Status:  domain.GoalStatusActive,
		Search:  "MRR",
	})
	if err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	if len(records) != 1 || total != 42 {
		t.Fatalf("expected 1 record of 42, got %d of %d", len(records), total)
	}
	got := f.registry.listed[0]
	if got.Product != "acme" || got.Status != domain.GoalStatusActive || got.Search != "MRR" {
		t.Fatalf("filter did not survive: %+v", got)
	}
}

func TestListReportsAStorageFailure(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.listErr = errors.New("statement timeout")

	if _, _, err := f.goals.List(context.Background(), repository.GoalFilter{}); err == nil ||
		!strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("expected the storage error, got %v", err)
	}
}

func TestPatchRefusesAPatchThatChangesNothing(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	_, err := f.goals.Patch(context.Background(), "goal-1", repository.GoalPatch{}, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if f.registry.getCalls != 0 {
		t.Fatal("expected no read for an empty patch")
	}
}

func TestPatchRefusesToLetAnAgentRewriteTheGoalItIsMeasuredAgainst(t *testing.T) {
	// An agent may state a new goal — that loop is the point of Wingman — but not
	// move the line it is being judged by. Pausing the goal you are failing and
	// lowering the target you cannot reach are the same move, so both are the
	// operator's.
	f := newGoalsFixture(t, testGoal())

	for _, patch := range []struct {
		name  string
		patch repository.GoalPatch
	}{
		{"lower the target", repository.GoalPatch{TargetValue: float64Ptr(1)}},
		{"pause the goal", repository.GoalPatch{Status: statusPtr(domain.GoalStatusPaused)}},
		{"move the deadline", repository.GoalPatch{PeriodEnd: timePtr(testNow.Add(8760 * time.Hour))}},
	} {
		t.Run(patch.name, func(t *testing.T) {
			_, err := f.goals.Patch(context.Background(), "goal-1", patch.patch, testBot())
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected ErrForbidden, got %v", err)
			}
		})
	}

	if len(f.registry.patchedIDs) != 0 {
		t.Fatalf("expected nothing written, got %v", f.registry.patchedIDs)
	}
	if len(f.audit.events) != 0 {
		t.Fatalf("expected no audit entry for a refused patch, got %v", f.audit.actions())
	}
}

func TestPatchRecordsWhatTheValueWasBeforeItChanged(t *testing.T) {
	// "ops updated goal 7" answers nothing. "ops moved targetValue from
	// 100,000,000 to 40,000,000" is the entry somebody needs six weeks later.
	f := newGoalsFixture(t, testGoal())

	record, err := f.goals.Patch(context.Background(), "goal-1", repository.GoalPatch{
		TargetValue: float64Ptr(40),
		Title:       stringPtr("Reach 40M MRR"),
	}, testActor())
	if err != nil {
		t.Fatalf("expected the patch to apply, got %v", err)
	}
	if record.TargetValue != 40 || record.Title != "Reach 40M MRR" {
		t.Fatalf("unexpected record: %+v", record.Goal)
	}
	if len(f.registry.patchedIDs) != 1 || f.registry.patchedIDs[0] != "goal-1" {
		t.Fatalf("expected the patch to reach goal-1, got %v", f.registry.patchedIDs)
	}
	if event := f.audit.events[0]; event.SubjectID != "goal-1" {
		t.Fatalf("expected the entry to name the goal, got %q", event.SubjectID)
	}

	changes := changesFromAudit(t, f)
	target, ok := changes["targetValue"].(map[string]any)
	if !ok {
		t.Fatalf("expected a targetValue diff, got %v", changes)
	}
	if target["from"] != float64(100) || target["to"] != float64(40) {
		t.Fatalf("expected 100 -> 40, got %v", target)
	}
	if _, ok := changes["title"]; !ok {
		t.Fatalf("expected the title change recorded, got %v", changes)
	}
	if len(changes) != 2 {
		t.Fatalf("expected exactly the two changed fields, got %v", changes)
	}
	if event := f.audit.events[0]; event.Action != ActionGoalUpdated || event.SubjectType != SubjectGoal {
		t.Fatalf("unexpected audit entry: %+v", event)
	}
}

func TestPatchDoesNotWriteWhenEveryFieldAlreadyHoldsThatValue(t *testing.T) {
	// An audit entry recording no change is worse than none: it makes the log look
	// like the target moved when it did not.
	f := newGoalsFixture(t, testGoal())

	record, err := f.goals.Patch(context.Background(), "goal-1", repository.GoalPatch{
		TargetValue: float64Ptr(100),
		BotID:       stringPtr("bot-1"),
	}, testActor())
	if err != nil {
		t.Fatalf("expected the no-op to succeed, got %v", err)
	}
	if record.TargetValue != 100 {
		t.Fatalf("expected the goal unchanged, got %+v", record.Goal)
	}
	if len(f.registry.patched) != 0 {
		t.Fatal("expected no write for a patch that changes nothing")
	}
	if len(f.audit.events) != 0 {
		t.Fatalf("expected no audit entry, got %+v", f.audit.events)
	}
}

func TestPatchRefusesToMarkAGoalAchievedByHand(t *testing.T) {
	// achieved and missed are the monitor's verdicts, reached from recorded
	// observations. Assigning them by hand empties the record of meaning.
	for _, status := range []domain.GoalStatus{domain.GoalStatusAchieved, domain.GoalStatusMissed} {
		f := newGoalsFixture(t, testGoal())

		_, err := f.goals.Patch(context.Background(), "goal-1",
			repository.GoalPatch{Status: statusPtr(status)}, testActor())
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: expected a validation error, got %v", status, err)
		}
		if !strings.Contains(err.Error(), "engine") {
			t.Fatalf("%s: expected the reason to say who decides, got %v", status, err)
		}
		if len(f.registry.patched) != 0 {
			t.Fatalf("%s: expected no write", status)
		}
	}
}

func TestPatchAllowsTheStatusesAnOperatorOwns(t *testing.T) {
	for _, status := range []domain.GoalStatus{
		domain.GoalStatusPaused, domain.GoalStatusArchived,
	} {
		f := newGoalsFixture(t, testGoal())

		record, err := f.goals.Patch(context.Background(), "goal-1",
			repository.GoalPatch{Status: statusPtr(status)}, testActor())
		if err != nil {
			t.Fatalf("%s: expected the patch to apply, got %v", status, err)
		}
		if record.Status != status {
			t.Fatalf("%s: expected the status applied, got %q", status, record.Status)
		}
	}
}

func TestPatchWillNotRewriteASettledGoalInPlace(t *testing.T) {
	// "The goal was missed and then the target was lowered" has to stay legible in
	// the log instead of arriving as one edit.
	for _, status := range []domain.GoalStatus{
		domain.GoalStatusAchieved, domain.GoalStatusMissed, domain.GoalStatusArchived,
	} {
		settled := testGoal()
		settled.Status = status
		f := newGoalsFixture(t, settled)

		_, err := f.goals.Patch(context.Background(), "goal-1",
			repository.GoalPatch{TargetValue: float64Ptr(40)}, testActor())
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: expected a validation error, got %v", status, err)
		}
		if !strings.Contains(err.Error(), "reopen") {
			t.Fatalf("%s: expected the error to say how to proceed, got %v", status, err)
		}
		if len(f.registry.patched) != 0 {
			t.Fatalf("%s: expected no write", status)
		}
	}
}

func TestPatchLetsASettledGoalBeReopenedOnItsOwn(t *testing.T) {
	settled := testGoal()
	settled.Status = domain.GoalStatusMissed
	f := newGoalsFixture(t, settled)

	record, err := f.goals.Patch(context.Background(), "goal-1",
		repository.GoalPatch{Status: statusPtr(domain.GoalStatusActive)}, testActor())
	if err != nil {
		t.Fatalf("expected the reopen to succeed, got %v", err)
	}
	if record.Status != domain.GoalStatusActive {
		t.Fatalf("expected an active goal, got %q", record.Status)
	}
	changes := changesFromAudit(t, f)
	if _, ok := changes["status"]; !ok {
		t.Fatalf("expected the status change recorded, got %v", changes)
	}
}

func TestPatchRefusesToTurnAValidGoalIntoAnInvalidOne(t *testing.T) {
	// The patch is applied to a copy and validated before anything is written, so a
	// single field cannot leave a goal the monitor can never evaluate.
	cases := []struct {
		name  string
		patch repository.GoalPatch
		field string
	}{
		{"deadline before the start", repository.GoalPatch{
			PeriodEnd: timePtr(testPeriodStart.Add(-24 * time.Hour)),
		}, "periodEnd"},
		{"tolerance so wide nothing is late", repository.GoalPatch{
			ToleranceRatio: float64Ptr(0.9),
		}, "toleranceRatio"},
		{"cooldown below the floor", repository.GoalPatch{
			TriggerCooldown: durationPtr(time.Minute),
		}, "triggerCooldown"},
		{"negative trigger budget", repository.GoalPatch{
			MaxTriggersPerPeriod: intPtr(-1),
		}, "maxTriggersPerPeriod"},
		{"blank title", repository.GoalPatch{
			Title: stringPtr("   "),
		}, "title"},
	}
	for _, c := range cases {
		f := newGoalsFixture(t, testGoal())

		_, err := f.goals.Patch(context.Background(), "goal-1", c.patch, testActor())
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: expected a validation error, got %v", c.name, err)
		}
		if field := ValidationFields(err)[c.field]; field == "" {
			t.Fatalf("%s: expected the error to name %q, got %v", c.name, c.field, ValidationFields(err))
		}
		if len(f.registry.patched) != 0 {
			t.Fatalf("%s: expected no write", c.name)
		}
	}
}

func TestPatchRefusesToMoveADeadlineIntoThePast(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	_, err := f.goals.Patch(context.Background(), "goal-1", repository.GoalPatch{
		PeriodEnd: timePtr(testNow.Add(-24 * time.Hour)),
	}, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "past") {
		t.Fatalf("expected the reason to mention the past, got %v", err)
	}
	if len(f.registry.patched) != 0 {
		t.Fatal("expected no write")
	}
}

func TestPatchAllowsRemovingACooldownDeliberately(t *testing.T) {
	// Zero is reachable on an existing goal, where it is an explicit choice, unlike
	// a blank field at creation.
	f := newGoalsFixture(t, testGoal())

	record, err := f.goals.Patch(context.Background(), "goal-1", repository.GoalPatch{
		TriggerCooldown: durationPtr(0),
	}, testActor())
	if err != nil {
		t.Fatalf("expected the patch to apply, got %v", err)
	}
	if record.TriggerCooldown != 0 {
		t.Fatalf("expected the cooldown removed, got %v", record.TriggerCooldown)
	}
	if _, ok := changesFromAudit(t, f)["triggerCooldown"]; !ok {
		t.Fatal("expected removing a safety limit to be audited")
	}
}

func TestPatchTranslatesAMissingGoalAndRequiresAnActorAndAnID(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.getErr = repository.ErrNotFound
	patch := repository.GoalPatch{TargetValue: float64Ptr(40)}

	if _, err := f.goals.Patch(context.Background(), "goal-1", patch, testActor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	f = newGoalsFixture(t, testGoal())
	if _, err := f.goals.Patch(context.Background(), "goal-1", patch, Actor{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected an actor to be required, got %v", err)
	}
	if _, err := f.goals.Patch(context.Background(), " ", patch, testActor()); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected an id to be required, got %v", err)
	}
	if f.registry.getCalls != 0 {
		t.Fatal("expected no read before the request was understood")
	}
}

func TestPatchReportsAWriteFailureAndTranslatesALostRow(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.registry.patchErr = errors.New("deadlock detected")

	_, err := f.goals.Patch(context.Background(), "goal-1",
		repository.GoalPatch{TargetValue: float64Ptr(40)}, testActor())
	if err == nil || !strings.Contains(err.Error(), "deadlock detected") {
		t.Fatalf("expected the storage error, got %v", err)
	}
	if len(f.audit.events) != 0 {
		t.Fatal("expected no audit entry for a write that did not happen")
	}

	// A goal deleted between the read and the write is a missing goal, not a broken
	// engine.
	f = newGoalsFixture(t, testGoal())
	f.registry.patchErr = repository.ErrNotFound
	_, err = f.goals.Patch(context.Background(), "goal-1",
		repository.GoalPatch{TargetValue: float64Ptr(40)}, testActor())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPatchRejectsAStatusThatIsNotAStatus(t *testing.T) {
	f := newGoalsFixture(t, testGoal())

	_, err := f.goals.Patch(context.Background(), "goal-1",
		repository.GoalPatch{Status: statusPtr("nearly")}, testActor())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
}

func TestPatchSucceedsEvenWhenTheAuditWriteFails(t *testing.T) {
	f := newGoalsFixture(t, testGoal())
	f.audit.err = errors.New("audit table is full")

	if _, err := f.goals.Patch(context.Background(), "goal-1",
		repository.GoalPatch{TargetValue: float64Ptr(40)}, testActor()); err != nil {
		t.Fatalf("expected the patch to stand, got %v", err)
	}
	if len(f.registry.patched) != 1 {
		t.Fatal("expected the write to have happened")
	}
}

func TestApplyGoalPatchRecordsThatTheTextChangedRatherThanASecondCopyOfIt(t *testing.T) {
	// The source text can be a pasted document. The log records that it moved, not
	// the document twice.
	goal := testGoal().Goal
	long := strings.Repeat("a", 500)

	_, changes := applyGoalPatch(goal, repository.GoalPatch{SourceText: stringPtr(long)})

	diff, ok := changes["sourceText"].(map[string]any)
	if !ok {
		t.Fatalf("expected a sourceText diff, got %v", changes)
	}
	if diff["to"] != 500 {
		t.Fatalf("expected the new length, got %v", diff["to"])
	}
}

func TestApplyGoalPatchTrimsTextAndLeavesUntouchedFieldsAlone(t *testing.T) {
	goal := testGoal().Goal

	patched, changes := applyGoalPatch(goal, repository.GoalPatch{
		Title:     stringPtr("  Reach 40M MRR  "),
		BotID:     stringPtr(" bot-2 "),
		ChannelID: stringPtr("chan-1"), // unchanged once trimmed
	})

	if patched.Title != "Reach 40M MRR" || patched.BotID != "bot-2" {
		t.Fatalf("expected trimmed values, got %q / %q", patched.Title, patched.BotID)
	}
	if _, ok := changes["channelId"]; ok {
		t.Fatalf("expected no diff for an unchanged channel, got %v", changes)
	}
	if patched.MetricKey != goal.MetricKey || patched.Comparator != goal.Comparator {
		t.Fatal("expected fields outside the patch to be untouched")
	}
}

func TestValidationFieldsReadsBothErrorShapesAndIgnoresOthers(t *testing.T) {
	// The handler layer renders per-field messages without knowing how the service
	// builds its errors.
	single := fieldError("metricKey", "is not a declared metric")
	if got := ValidationFields(single)["metricKey"]; got != "is not a declared metric" {
		t.Fatalf("expected the single field, got %v", ValidationFields(single))
	}
	if !errors.Is(single, ErrValidation) {
		t.Fatal("expected a field error to match the validation sentinel")
	}

	goal := testGoal().Goal
	goal.Product = ""
	many := goal.Validate()
	if got := ValidationFields(many)["product"]; got == "" {
		t.Fatalf("expected the domain fields, got %v", ValidationFields(many))
	}

	if got := ValidationFields(errors.New("connection reset")); got != nil {
		t.Fatalf("expected no fields, got %v", got)
	}
	if got := ValidationFields(nil); got != nil {
		t.Fatalf("expected no fields for a nil error, got %v", got)
	}
}

func TestSystemActorIsTheEngineActingOnItsOwnSchedule(t *testing.T) {
	actor := SystemActor()
	if actor.Type != repository.ActorSystem || actor.ID == "" {
		t.Fatalf("unexpected system actor: %+v", actor)
	}
	if err := actor.validate(); err != nil {
		t.Fatalf("expected the system actor to be usable, got %v", err)
	}
}
