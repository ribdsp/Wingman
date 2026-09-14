package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// Evaluations is the read side of the goal evaluation history.
//
// Like AuditLog it has no write method. Evaluations are written by the monitor as
// part of reaching them, and an endpoint that could add one could add a verdict
// that no recorded observation supports.
//
// It exists so that no client ever recomputes pace. The evaluator's arithmetic is
// the definition of "behind" in this system — it is what decides whether an agent
// is woken — and a dashboard dividing progress by elapsed time for itself would
// eventually disagree with it. That disagreement would be invisible: two plausible
// numbers, no error, and an operator trusting the wrong one.
type Evaluations struct {
	reader EvaluationReader
	goals  GoalLookup
}

// EvaluationsDeps is everything the evaluation reader needs.
type EvaluationsDeps struct {
	Reader EvaluationReader
	Goals  GoalLookup
}

// NewEvaluations validates its wiring and returns a ready reader.
func NewEvaluations(deps EvaluationsDeps) (*Evaluations, error) {
	missing := []string{}
	if deps.Reader == nil {
		missing = append(missing, "Reader")
	}
	if deps.Goals == nil {
		missing = append(missing, "Goals")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("evaluations: missing dependencies: %v", missing)
	}
	return &Evaluations{reader: deps.Reader, goals: deps.Goals}, nil
}

// ListByGoal reads one goal's evaluation history, newest first.
//
// The goal is looked up first so that "no history yet" and "no such goal" are
// different answers. An empty page for a mistyped id would read as a goal the
// monitor has never once managed to evaluate, which is a real state and an alarming
// one; the two must not look the same.
func (e *Evaluations) ListByGoal(ctx context.Context, goalID string, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" {
		return nil, 0, fieldError("id", "is required")
	}
	if _, err := e.goals.GetByID(ctx, goalID); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, 0, ErrNotFound
		}
		// An unreachable database must not answer "no such goal".
		return nil, 0, fmt.Errorf("evaluations: goal lookup: %w", err)
	}

	limit, offset = clampPage(limit, offset)
	records, total, err := e.reader.ListByGoal(ctx, goalID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("evaluations: list by goal: %w", err)
	}
	return records, total, nil
}

// Latest reads where everything stands: the most recent evaluation of every goal
// that has one, newest first.
//
// One call rather than one per goal. This is what a dashboard opens with, and a
// screen that fires a request per row is a screen nobody leaves open — which
// matters, because the whole point of the monitor is that somebody notices.
func (e *Evaluations) Latest(ctx context.Context, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	limit, offset = clampPage(limit, offset)
	records, total, err := e.reader.LatestPerGoal(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("evaluations: latest: %w", err)
	}
	return records, total, nil
}
