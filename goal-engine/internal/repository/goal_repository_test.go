package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

var (
	testPeriodStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	testPeriodEnd   = time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
)

// goalSQLRows returns an empty result set shaped like the goals table.
func goalSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "product", "title", "source_text", "metric_key", "comparator",
		"target_value", "baseline_value", "period_start", "period_end", "status",
		"tolerance_ratio", "trigger_cooldown_seconds", "max_triggers_per_period",
		"bot_id", "channel_id", "created_by", "created_at", "updated_at",
	})
}

// addGoalRow appends a row with the given baseline, which is the one nullable
// column that maps to a pointer.
func addGoalRow(rows *sqlmock.Rows, baseline any) *sqlmock.Rows {
	return rows.AddRow(
		"11111111-1111-1111-1111-111111111111", "acme", "Grow MRR",
		"get MRR to 50M this month", "billing.mrr.idr", "gte",
		50000000.0, baseline, testPeriodStart, testPeriodEnd, "active",
		0.05, 21600, 3, "bot-1", "chan-1", "ops", testPeriodStart, testPeriodStart,
	)
}

func testGoal() domain.Goal {
	return domain.Goal{
		Product:              "acme",
		Title:                "Grow MRR",
		SourceText:           "get MRR to 50M this month",
		MetricKey:            "billing.mrr.idr",
		Comparator:           domain.ComparatorGTE,
		TargetValue:          50000000,
		PeriodStart:          testPeriodStart,
		PeriodEnd:            testPeriodEnd,
		Status:               domain.GoalStatusActive,
		ToleranceRatio:       0.05,
		TriggerCooldown:      6 * time.Hour,
		MaxTriggersPerPeriod: 3,
		BotID:                "bot-1",
		ChannelID:            "chan-1",
	}
}

func TestGoalRepositoryCreateStoresAndMapsBack(t *testing.T) {
	// Arrange
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectQuery("INSERT INTO goals").
		WithArgs("acme", "Grow MRR", "get MRR to 50M this month", "billing.mrr.idr",
			"gte", 50000000.0, nil, testPeriodStart, testPeriodEnd, "active",
			0.05, 21600, 3, "bot-1", "chan-1", "ops").
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	// Act
	record, err := repo.Create(context.Background(), testGoal(), "ops")

	// Assert
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if record.ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("unexpected id %q", record.ID)
	}
	if record.TriggerCooldown != 6*time.Hour {
		t.Fatalf("expected the cooldown to round-trip as 6h, got %s", record.TriggerCooldown)
	}
	if record.BaselineValue != nil {
		t.Fatalf("expected a NULL baseline to map to nil, got %v", *record.BaselineValue)
	}
	if record.CreatedBy != "ops" {
		t.Fatalf("unexpected creator %q", record.CreatedBy)
	}
}

func TestGoalRepositoryCreateAppliesDefaults(t *testing.T) {
	// An empty status and a zero tolerance must not be written as-is: the domain
	// treats zero tolerance as "use the default", and the two layers have to
	// agree or a goal would trigger on any deviation at all.
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	goal := testGoal()
	goal.Status = ""
	goal.ToleranceRatio = 0

	mock.ExpectQuery("INSERT INTO goals").
		WithArgs("acme", "Grow MRR", "get MRR to 50M this month", "billing.mrr.idr",
			"gte", 50000000.0, nil, testPeriodStart, testPeriodEnd, "active",
			domain.DefaultToleranceRatio, 21600, 3, "bot-1", "chan-1", "ops").
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	if _, err := repo.Create(context.Background(), goal, "ops"); err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
}

func TestGoalRepositoryCreatePassesBaselineWhenSet(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	goal := testGoal()
	baseline := 32000000.0
	goal.BaselineValue = &baseline

	mock.ExpectQuery("INSERT INTO goals").
		WithArgs("acme", "Grow MRR", "get MRR to 50M this month", "billing.mrr.idr",
			"gte", 50000000.0, 32000000.0, testPeriodStart, testPeriodEnd, "active",
			0.05, 21600, 3, "bot-1", "chan-1", "ops").
		WillReturnRows(addGoalRow(goalSQLRows(), 32000000.0))

	record, err := repo.Create(context.Background(), goal, "ops")
	if err != nil {
		t.Fatalf("expected the goal to be created, got %v", err)
	}
	if record.BaselineValue == nil || *record.BaselineValue != 32000000.0 {
		t.Fatalf("expected the baseline to round-trip, got %v", record.BaselineValue)
	}
}

func TestGoalRepositoryGetByIDReportsAMissingGoal(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectQuery("SELECT .+ FROM goals WHERE id = \\$1").
		WithArgs("missing").
		WillReturnRows(goalSQLRows())

	_, err := repo.GetByID(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGoalRepositoryListNumbersPlaceholdersConsistently(t *testing.T) {
	// An off-by-one between the filter args and the paging args would silently
	// return the wrong page rather than fail, so the numbering is asserted.
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM goals WHERE 1 = 1 AND product = \\$1 AND status = \\$2").
		WithArgs("acme", "active").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery("LIMIT \\$3 OFFSET \\$4").
		WithArgs("acme", "active", 10, 20).
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	records, total, err := repo.List(context.Background(), GoalFilter{
		Product: "acme",
		Status:  domain.GoalStatusActive,
		Limit:   10,
		Offset:  20,
	})
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 7 {
		t.Fatalf("expected 7 matches, got %d", total)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record on the page, got %d", len(records))
	}
}

func TestGoalRepositoryListReusesOnePlaceholderForSearch(t *testing.T) {
	// The search term is one value matched against two columns. Appending it
	// twice would shift every later placeholder by one.
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectQuery("\\(title ILIKE \\$1 OR source_text ILIKE \\$1\\)").
		WithArgs("%mrr%").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("LIMIT \\$2 OFFSET \\$3").
		WithArgs("%mrr%", defaultPageLimit, 0).
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	if _, _, err := repo.List(context.Background(), GoalFilter{Search: "  mrr  "}); err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
}

func TestGoalRepositoryListDueSelectsStartedActiveGoals(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery("WHERE status = 'active' AND period_start <= \\$1").
		WithArgs(now, 5).
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	records, err := repo.ListDue(context.Background(), now, 5)
	if err != nil {
		t.Fatalf("expected due goals, got %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 due goal, got %d", len(records))
	}
}

func TestGoalRepositoryListDueFallsBackToADefaultLimit(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	now := time.Now()
	mock.ExpectQuery("FROM goals").
		WithArgs(now, defaultPageLimit).
		WillReturnRows(goalSQLRows())

	if _, err := repo.ListDue(context.Background(), now, 0); err != nil {
		t.Fatalf("expected due goals, got %v", err)
	}
}

func TestGoalRepositorySetBaselineOnlyWritesOnce(t *testing.T) {
	// A baseline that moved would rewrite the definition of progress mid-period,
	// so the guard belongs in the statement rather than in a caller's discipline.
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectExec("SET baseline_value = \\$2 WHERE id = \\$1 AND baseline_value IS NULL").
		WithArgs("goal-1", 32000000.0).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetBaseline(context.Background(), "goal-1", 32000000.0); err != nil {
		t.Fatalf("expected the baseline to be set, got %v", err)
	}
}

func TestGoalRepositoryUpdateStatusReportsAMissingGoal(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectExec("UPDATE goals SET status = \\$2").
		WithArgs("missing", "achieved").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.UpdateStatus(context.Background(), "missing", domain.GoalStatusAchieved)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGoalRepositoryPatchOnlyTouchesNamedFields(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	title := "Grow MRR faster"
	cooldown := 2 * time.Hour
	mock.ExpectQuery("UPDATE goals SET title = \\$2, trigger_cooldown_seconds = \\$3 WHERE id = \\$1").
		WithArgs("goal-1", title, 7200).
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	if _, err := repo.Patch(context.Background(), "goal-1", GoalPatch{
		Title:           &title,
		TriggerCooldown: &cooldown,
	}); err != nil {
		t.Fatalf("expected the patch to apply, got %v", err)
	}
}

func TestGoalRepositoryPatchWithNothingToChangeJustReads(t *testing.T) {
	// An empty patch must not emit `UPDATE goals SET  WHERE`, which is a syntax
	// error waiting for the first caller who sends an empty body.
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	mock.ExpectQuery("SELECT .+ FROM goals WHERE id = \\$1").
		WithArgs("goal-1").
		WillReturnRows(addGoalRow(goalSQLRows(), nil))

	if _, err := repo.Patch(context.Background(), "goal-1", GoalPatch{}); err != nil {
		t.Fatalf("expected a plain read, got %v", err)
	}
}

func TestGoalRepositoryPatchReportsAMissingGoal(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewGoalRepository(db)
	status := domain.GoalStatusPaused
	mock.ExpectQuery("UPDATE goals SET").
		WithArgs("missing", "paused").
		WillReturnRows(goalSQLRows())

	_, err := repo.Patch(context.Background(), "missing", GoalPatch{Status: &status})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGoalPatchIsEmptyDetectsAnyField(t *testing.T) {
	if !(GoalPatch{}).IsEmpty() {
		t.Fatal("expected a zero patch to be empty")
	}
	title := "x"
	if (GoalPatch{Title: &title}).IsEmpty() {
		t.Fatal("expected a patch with a title to be non-empty")
	}
	channel := "c"
	if (GoalPatch{ChannelID: &channel}).IsEmpty() {
		t.Fatal("expected a patch with a channel to be non-empty")
	}
}

// --- samples ---

func sampleSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "metric_key", "value", "observed_at", "source", "duration_ms"})
}

func TestSampleRepositoryInsertRecordsAnObservation(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSampleRepository(db)
	observedAt := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO metric_samples").
		WithArgs("billing.mrr.idr", 41000000.0, observedAt, "sql", 250).
		WillReturnRows(sampleSQLRows().AddRow(1, "billing.mrr.idr", 41000000.0, observedAt, "sql", 250))

	record, err := repo.Insert(context.Background(), SampleInput{
		MetricKey:  "billing.mrr.idr",
		Value:      41000000,
		ObservedAt: observedAt,
		Source:     "sql",
		Duration:   250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected the sample to be stored, got %v", err)
	}
	if record.ID != 1 || record.Value != 41000000 {
		t.Fatalf("unexpected record %+v", record)
	}
	if record.DurationMS == nil || *record.DurationMS != 250 {
		t.Fatalf("expected the duration to round-trip, got %v", record.DurationMS)
	}
}

func TestSampleRepositoryInsertRefusesBrokenValuesWithoutAskingTheDatabase(t *testing.T) {
	// No expectation is registered: reaching the database at all would fail the
	// test, which is the point. A NaN sample must never be stored, because every
	// comparison against it is false and that reads as "off track".
	db, _ := newTestDB(t)
	repo := NewSampleRepository(db)

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := repo.Insert(context.Background(), SampleInput{MetricKey: "billing.mrr.idr", Value: value}); err == nil {
			t.Fatalf("expected %v to be refused", value)
		}
	}
}

func TestSampleRepositoryInsertDefaultsSourceAndDuration(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSampleRepository(db)
	mock.ExpectQuery("INSERT INTO metric_samples").
		WithArgs("billing.mrr.idr", 1.0, sqlmock.AnyArg(), "monitor", nil).
		WillReturnRows(sampleSQLRows().AddRow(1, "billing.mrr.idr", 1.0, time.Now(), "monitor", nil))

	record, err := repo.Insert(context.Background(), SampleInput{MetricKey: "billing.mrr.idr", Value: 1})
	if err != nil {
		t.Fatalf("expected the sample to be stored, got %v", err)
	}
	if record.DurationMS != nil {
		t.Fatalf("expected an unset duration to stay nil, got %v", *record.DurationMS)
	}
}

func TestSampleRepositoryLatestReportsAMissingMetric(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSampleRepository(db)
	mock.ExpectQuery("FROM metric_samples").
		WithArgs("billing.mrr.idr").
		WillReturnRows(sampleSQLRows())

	_, err := repo.Latest(context.Background(), "billing.mrr.idr")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSampleRepositoryListSinceClampsTheLimit(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSampleRepository(db)
	since := testPeriodStart
	mock.ExpectQuery("FROM metric_samples").
		WithArgs("billing.mrr.idr", since, maxPageLimit).
		WillReturnRows(sampleSQLRows())

	if _, err := repo.ListSince(context.Background(), "billing.mrr.idr", since, 10_000); err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
}

// --- evaluations ---

func evaluationSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "goal_id", "sample_id", "observed_value", "target_value",
		"baseline_value", "expected_value", "progress_ratio", "elapsed_ratio",
		"pace_ratio", "on_track", "target_met", "decision", "reason",
		"evaluated_at", "created_at",
	})
}

func addEvaluationRow(rows *sqlmock.Rows, sampleID any, decision string) *sqlmock.Rows {
	return rows.AddRow(
		"22222222-2222-2222-2222-222222222222", "goal-1", sampleID,
		41000000.0, 50000000.0, 32000000.0, 43000000.0,
		0.5, 0.33, 0.9, false, false, decision, "behind pace",
		testPeriodStart, testPeriodStart,
	)
}

func testEvaluation() domain.Evaluation {
	return domain.Evaluation{
		GoalID:        "goal-1",
		ObservedValue: 41000000,
		TargetValue:   50000000,
		BaselineValue: 32000000,
		ExpectedValue: 43000000,
		ProgressRatio: 0.5,
		ElapsedRatio:  0.33,
		PaceRatio:     0.9,
		Decision:      domain.DecisionTrigger,
		Reason:        "behind pace",
		EvaluatedAt:   testPeriodStart,
	}
}

func TestEvaluationRepositoryInsertLinksTheSample(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewEvaluationRepository(db)
	sampleID := int64(9)
	mock.ExpectQuery("INSERT INTO goal_evaluations").
		WithArgs("goal-1", sampleID, 41000000.0, 50000000.0, 32000000.0, 43000000.0,
			0.5, 0.33, 0.9, false, false, "trigger", "behind pace", testPeriodStart).
		WillReturnRows(addEvaluationRow(evaluationSQLRows(), sampleID, "trigger"))

	record, err := repo.Insert(context.Background(), testEvaluation(), &sampleID)
	if err != nil {
		t.Fatalf("expected the evaluation to be stored, got %v", err)
	}
	if record.SampleID == nil || *record.SampleID != 9 {
		t.Fatalf("expected the sample link to round-trip, got %v", record.SampleID)
	}
	if record.Decision != domain.DecisionTrigger {
		t.Fatalf("unexpected decision %q", record.Decision)
	}
}

func TestEvaluationRepositoryInsertStoresASkippedEvaluationWithoutASample(t *testing.T) {
	// A skipped evaluation is exactly the case an operator needs to see, so a
	// missing sample and non-finite ratios must not block the write.
	db, mock := newTestDB(t)
	repo := NewEvaluationRepository(db)
	eval := testEvaluation()
	eval.Decision = domain.DecisionSkippedInvalidSample
	eval.ObservedValue = math.NaN()
	eval.PaceRatio = math.Inf(1)

	mock.ExpectQuery("INSERT INTO goal_evaluations").
		WithArgs("goal-1", nil, 0.0, 50000000.0, 32000000.0, 43000000.0,
			0.5, 0.33, 0.0, false, false, "skipped_invalid_sample", "behind pace", testPeriodStart).
		WillReturnRows(addEvaluationRow(evaluationSQLRows(), nil, "skipped_invalid_sample"))

	record, err := repo.Insert(context.Background(), eval, nil)
	if err != nil {
		t.Fatalf("expected the evaluation to be stored, got %v", err)
	}
	if record.SampleID != nil {
		t.Fatalf("expected no sample link, got %v", *record.SampleID)
	}
}

func TestEvaluationRepositoryListByGoalPaginates(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewEvaluationRepository(db)
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM goal_evaluations WHERE goal_id = \\$1").
		WithArgs("goal-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	mock.ExpectQuery("LIMIT \\$2 OFFSET \\$3").
		WithArgs("goal-1", 10, 0).
		WillReturnRows(addEvaluationRow(evaluationSQLRows(), nil, "noop"))

	records, total, err := repo.ListByGoal(context.Background(), "goal-1", 10, 0)
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 42 || len(records) != 1 {
		t.Fatalf("unexpected result: total=%d records=%d", total, len(records))
	}
}

func TestEvaluationRepositoryLatestPerGoalKeepsTheNewestRowPerGoal(t *testing.T) {
	// Two things in this query are load-bearing and neither would fail loudly if it
	// were dropped: DISTINCT ON (goal_id) is what makes it one row per goal, and the
	// inner ORDER BY is what makes the row it keeps the newest one. Without them a
	// dashboard would quietly show last week's pace, which is worse than showing
	// none.
	db, mock := newTestDB(t)
	repo := NewEvaluationRepository(db)
	mock.ExpectQuery("SELECT count\\(DISTINCT goal_id\\) FROM goal_evaluations").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("(?s)DISTINCT ON \\(goal_id\\).+ORDER BY goal_id, evaluated_at DESC").
		WithArgs(10, 0).
		WillReturnRows(addEvaluationRow(evaluationSQLRows(), nil, "trigger"))

	records, total, err := repo.LatestPerGoal(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 3 {
		t.Fatalf("expected the count to be over distinct goals, got %d", total)
	}
	if len(records) != 1 || records[0].Decision != domain.DecisionTrigger {
		t.Fatalf("unexpected result: %+v", records)
	}
}

func TestEvaluationRepositoryLatestPerGoalOrdersThePageByRecency(t *testing.T) {
	// DISTINCT ON forces the inner query to order by goal_id first, so the ordering
	// a caller actually sees has to be applied outside it. Asserting the outer
	// clause separately is the only way to catch a rewrite that collapses the two
	// and leaves the page sorted by an opaque uuid.
	db, mock := newTestDB(t)
	repo := NewEvaluationRepository(db)
	mock.ExpectQuery("SELECT count\\(DISTINCT goal_id\\) FROM goal_evaluations").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("(?s)\\) latest.+ORDER BY evaluated_at DESC, id DESC.+LIMIT \\$1 OFFSET \\$2").
		WithArgs(defaultPageLimit, 0).
		WillReturnRows(addEvaluationRow(evaluationSQLRows(), nil, "noop"))

	if _, _, err := repo.LatestPerGoal(context.Background(), 0, -5); err != nil {
		t.Fatalf("expected an unbounded request to be normalised, got %v", err)
	}
}
