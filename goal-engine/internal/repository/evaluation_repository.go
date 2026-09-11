package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// EvaluationRecord is a stored evaluation. Every goal check is written, not just
// the ones that acted: the audit trail has to show the decisions not to act too.
type EvaluationRecord struct {
	domain.Evaluation
	ID        string
	SampleID  *int64
	CreatedAt time.Time
}

// EvaluationRepository stores goal evaluations.
type EvaluationRepository struct {
	db *sqlx.DB
}

// NewEvaluationRepository builds a repository over the given pool.
func NewEvaluationRepository(db *sqlx.DB) *EvaluationRepository {
	return &EvaluationRepository{db: db}
}

const evaluationColumns = `id, goal_id, sample_id, observed_value, target_value,
	baseline_value, expected_value, progress_ratio, elapsed_ratio, pace_ratio,
	on_track, target_met, decision, reason, evaluated_at, created_at`

type evaluationRow struct {
	ID            string        `db:"id"`
	GoalID        string        `db:"goal_id"`
	SampleID      sql.NullInt64 `db:"sample_id"`
	ObservedValue float64       `db:"observed_value"`
	TargetValue   float64       `db:"target_value"`
	BaselineValue float64       `db:"baseline_value"`
	ExpectedValue float64       `db:"expected_value"`
	ProgressRatio float64       `db:"progress_ratio"`
	ElapsedRatio  float64       `db:"elapsed_ratio"`
	PaceRatio     float64       `db:"pace_ratio"`
	OnTrack       bool          `db:"on_track"`
	TargetMet     bool          `db:"target_met"`
	Decision      string        `db:"decision"`
	Reason        string        `db:"reason"`
	EvaluatedAt   time.Time     `db:"evaluated_at"`
	CreatedAt     time.Time     `db:"created_at"`
}

func (r evaluationRow) toRecord() EvaluationRecord {
	var sampleID *int64
	if r.SampleID.Valid {
		id := r.SampleID.Int64
		sampleID = &id
	}
	return EvaluationRecord{
		Evaluation: domain.Evaluation{
			GoalID:        r.GoalID,
			ObservedValue: r.ObservedValue,
			TargetValue:   r.TargetValue,
			BaselineValue: r.BaselineValue,
			ExpectedValue: r.ExpectedValue,
			ProgressRatio: r.ProgressRatio,
			ElapsedRatio:  r.ElapsedRatio,
			PaceRatio:     r.PaceRatio,
			OnTrack:       r.OnTrack,
			TargetMet:     r.TargetMet,
			Decision:      domain.Decision(r.Decision),
			Reason:        r.Reason,
			EvaluatedAt:   r.EvaluatedAt,
		},
		ID:        r.ID,
		SampleID:  sampleID,
		CreatedAt: r.CreatedAt,
	}
}

// Insert stores an evaluation, optionally linked to the sample it read.
//
// Non-finite ratios are coerced to zero rather than rejected: an evaluation that
// skipped a broken sample still has to be recorded, and refusing the write would
// erase the very event an operator needs to see.
func (r *EvaluationRepository) Insert(ctx context.Context, eval domain.Evaluation, sampleID *int64) (EvaluationRecord, error) {
	const query = `
		INSERT INTO goal_evaluations (
			goal_id, sample_id, observed_value, target_value, baseline_value,
			expected_value, progress_ratio, elapsed_ratio, pace_ratio,
			on_track, target_met, decision, reason, evaluated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING ` + evaluationColumns

	var sample any
	if sampleID != nil {
		sample = *sampleID
	}
	evaluatedAt := eval.EvaluatedAt
	if evaluatedAt.IsZero() {
		evaluatedAt = time.Now()
	}

	var row evaluationRow
	err := r.db.QueryRowxContext(ctx, query,
		eval.GoalID, sample,
		finiteOrZero(eval.ObservedValue), finiteOrZero(eval.TargetValue),
		finiteOrZero(eval.BaselineValue), finiteOrZero(eval.ExpectedValue),
		finiteOrZero(eval.ProgressRatio), finiteOrZero(eval.ElapsedRatio),
		finiteOrZero(eval.PaceRatio),
		eval.OnTrack, eval.TargetMet, string(eval.Decision), eval.Reason, evaluatedAt,
	).StructScan(&row)
	if err != nil {
		return EvaluationRecord{}, fmt.Errorf("insert evaluation for goal %s: %w", eval.GoalID, classify(err))
	}
	return row.toRecord(), nil
}

// ListByGoal returns a goal's evaluations, newest first.
func (r *EvaluationRepository) ListByGoal(ctx context.Context, goalID string, limit, offset int) ([]EvaluationRecord, int, error) {
	const countQuery = `SELECT count(*) FROM goal_evaluations WHERE goal_id = $1`
	const listQuery = `
		SELECT ` + evaluationColumns + `
		FROM goal_evaluations
		WHERE goal_id = $1
		ORDER BY evaluated_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery, goalID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count evaluations for goal %s: %w", goalID, classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []evaluationRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, goalID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list evaluations for goal %s: %w", goalID, classify(err))
	}

	records := make([]EvaluationRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, total, nil
}
