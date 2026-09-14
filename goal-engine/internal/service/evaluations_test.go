package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// fakeEvaluationReader records the paging arguments it was handed, because the
// clamp is the only rule this service has and an unclamped limit reaches the
// database.
type fakeEvaluationReader struct {
	records []repository.EvaluationRecord
	total   int
	err     error

	goalID string
	limit  int
	offset int
	calls  int
}

func (f *fakeEvaluationReader) ListByGoal(_ context.Context, goalID string, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	f.calls++
	f.goalID, f.limit, f.offset = goalID, limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.records, f.total, nil
}

func (f *fakeEvaluationReader) LatestPerGoal(_ context.Context, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	f.calls++
	f.limit, f.offset = limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.records, f.total, nil
}

// fakeGoalLookup answers whether a goal exists.
type fakeGoalLookup struct {
	byID map[string]repository.GoalRecord
	err  error
}

func (f *fakeGoalLookup) GetByID(_ context.Context, id string) (repository.GoalRecord, error) {
	if f.err != nil {
		return repository.GoalRecord{}, f.err
	}
	record, ok := f.byID[id]
	if !ok {
		return repository.GoalRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func evaluationRecord(goalID string, decision domain.Decision) repository.EvaluationRecord {
	return repository.EvaluationRecord{
		ID: "eval-" + goalID,
		Evaluation: domain.Evaluation{
			GoalID:      goalID,
			PaceRatio:   0.82,
			Decision:    decision,
			Reason:      "behind pace",
			EvaluatedAt: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
		},
	}
}

func newTestEvaluations(t *testing.T, reader EvaluationReader, goals GoalLookup) *Evaluations {
	t.Helper()
	evaluations, err := NewEvaluations(EvaluationsDeps{Reader: reader, Goals: goals})
	if err != nil {
		t.Fatalf("expected a ready reader, got %v", err)
	}
	return evaluations
}

func TestNewEvaluations_missingDependencies_refusesToBuild(t *testing.T) {
	// Arrange / Act
	_, err := NewEvaluations(EvaluationsDeps{})

	// Assert
	if err == nil {
		t.Fatal("expected a service with no storage to refuse to build")
	}
}

func TestEvaluations_listByGoal_returnsTheStoredHistory(t *testing.T) {
	// Arrange
	reader := &fakeEvaluationReader{
		records: []repository.EvaluationRecord{evaluationRecord("goal-1", domain.DecisionTrigger)},
		total:   9,
	}
	goals := &fakeGoalLookup{byID: map[string]repository.GoalRecord{"goal-1": {}}}
	evaluations := newTestEvaluations(t, reader, goals)

	// Act
	records, total, err := evaluations.ListByGoal(context.Background(), "  goal-1  ", 10, 20)

	// Assert
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 9 || len(records) != 1 {
		t.Fatalf("unexpected result: total=%d records=%d", total, len(records))
	}
	if reader.goalID != "goal-1" {
		t.Fatalf("expected the id to be trimmed before the query, got %q", reader.goalID)
	}
	if reader.limit != 10 || reader.offset != 20 {
		t.Fatalf("expected the page to pass through, got limit=%d offset=%d", reader.limit, reader.offset)
	}
}

func TestEvaluations_listByGoal_unknownGoal_isNotFoundRatherThanAnEmptyPage(t *testing.T) {
	// A mistyped id answering 200 with no rows reads as a goal the monitor has never
	// managed to evaluate, which is a real and alarming state. They must not look
	// the same.
	// Arrange
	reader := &fakeEvaluationReader{}
	evaluations := newTestEvaluations(t, reader, &fakeGoalLookup{})

	// Act
	_, _, err := evaluations.ListByGoal(context.Background(), "goal-404", 0, 0)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if reader.calls != 0 {
		t.Fatalf("expected no history query for a goal that does not exist, got %d", reader.calls)
	}
}

func TestEvaluations_listByGoal_blankID_isAValidationError(t *testing.T) {
	// Arrange
	evaluations := newTestEvaluations(t, &fakeEvaluationReader{}, &fakeGoalLookup{})

	// Act
	_, _, err := evaluations.ListByGoal(context.Background(), "   ", 0, 0)

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestEvaluations_listByGoal_clampsAnUnboundedLimit(t *testing.T) {
	// Arrange
	reader := &fakeEvaluationReader{}
	goals := &fakeGoalLookup{byID: map[string]repository.GoalRecord{"goal-1": {}}}
	evaluations := newTestEvaluations(t, reader, goals)

	// Act
	if _, _, err := evaluations.ListByGoal(context.Background(), "goal-1", 100_000, -3); err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}

	// Assert
	if reader.limit != maxPageLimit || reader.offset != 0 {
		t.Fatalf("expected the page to be clamped, got limit=%d offset=%d", reader.limit, reader.offset)
	}
}

func TestEvaluations_listByGoal_lookupFailure_isNotFlattenedIntoNotFound(t *testing.T) {
	// An unreachable database must not answer "no such goal": that is the same lie
	// as reporting a dead metric feed as on track.
	// Arrange
	goals := &fakeGoalLookup{err: errors.New("connection reset")}
	evaluations := newTestEvaluations(t, &fakeEvaluationReader{}, goals)

	// Act
	_, _, err := evaluations.ListByGoal(context.Background(), "goal-1", 0, 0)

	// Assert
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrValidation) {
		t.Fatalf("expected an internal failure, got %v", err)
	}
}

func TestEvaluations_latest_returnsTheClampedPage(t *testing.T) {
	// Arrange
	reader := &fakeEvaluationReader{
		records: []repository.EvaluationRecord{
			evaluationRecord("goal-1", domain.DecisionTrigger),
			evaluationRecord("goal-2", domain.DecisionNoop),
		},
		total: 2,
	}
	evaluations := newTestEvaluations(t, reader, &fakeGoalLookup{})

	// Act
	records, total, err := evaluations.Latest(context.Background(), 0, 0)

	// Assert
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 2 || len(records) != 2 {
		t.Fatalf("unexpected result: total=%d records=%d", total, len(records))
	}
	if reader.limit != defaultPageLimit {
		t.Fatalf("expected the default page limit, got %d", reader.limit)
	}
}

func TestEvaluations_latest_readerFailure_isWrapped(t *testing.T) {
	// Arrange
	reader := &fakeEvaluationReader{err: errors.New("connection reset")}
	evaluations := newTestEvaluations(t, reader, &fakeGoalLookup{})

	// Act
	_, _, err := evaluations.Latest(context.Background(), 0, 0)

	// Assert
	if err == nil {
		t.Fatal("expected the failure to reach the caller")
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrValidation) {
		t.Fatalf("expected an internal failure rather than a client error, got %v", err)
	}
}
