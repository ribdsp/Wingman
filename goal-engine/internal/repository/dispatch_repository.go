package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// DispatchStatus mirrors the dispatch_status enum.
type DispatchStatus string

const (
	// DispatchPending has been recorded but not yet accepted by Wingman core.
	DispatchPending DispatchStatus = "pending"
	// DispatchSent was accepted; an agent task exists.
	DispatchSent DispatchStatus = "sent"
	// DispatchFailed was rejected or errored. It never woke an agent.
	DispatchFailed DispatchStatus = "failed"
	// DispatchAbandoned was given up on after repeated failures.
	DispatchAbandoned DispatchStatus = "abandoned"
)

// DispatchInput is one attempt to wake an agent.
type DispatchInput struct {
	GoalID       string
	EvaluationID string
	BotID        string
	ChannelID    string
	// IdempotencyKey is derived from the goal and its period so a retry can
	// never create a second agent task for the same shortfall.
	IdempotencyKey string
	// Brief is the instruction the agent receives, in the operator's own terms.
	Brief string
	// RequestPayload is the JSON body sent to Wingman core, stored verbatim.
	RequestPayload string
}

// DispatchRecord is a stored dispatch.
type DispatchRecord struct {
	ID             string
	GoalID         string
	EvaluationID   string
	BotID          string
	ChannelID      string
	IdempotencyKey string
	Brief          string
	RequestPayload string
	Status         DispatchStatus
	Attempts       int
	ResponseStatus *int
	ExternalTaskID string
	LastError      string
	CreatedAt      time.Time
	SentAt         *time.Time
}

// DispatchHistory is what the decision core needs to know about past triggers
// for one goal in one period.
type DispatchHistory struct {
	LastTriggeredAt *time.Time
	CountThisPeriod int
}

// DispatchRepository stores trigger dispatches.
type DispatchRepository struct {
	db *sqlx.DB
}

// NewDispatchRepository builds a repository over the given pool.
func NewDispatchRepository(db *sqlx.DB) *DispatchRepository {
	return &DispatchRepository{db: db}
}

const dispatchColumns = `id, goal_id, evaluation_id, bot_id, channel_id,
	idempotency_key, brief, request_payload::text AS request_payload, status,
	attempts, response_status, external_task_id, last_error, created_at, sent_at`

type dispatchRow struct {
	ID             string         `db:"id"`
	GoalID         string         `db:"goal_id"`
	EvaluationID   string         `db:"evaluation_id"`
	BotID          string         `db:"bot_id"`
	ChannelID      string         `db:"channel_id"`
	IdempotencyKey string         `db:"idempotency_key"`
	Brief          string         `db:"brief"`
	RequestPayload string         `db:"request_payload"`
	Status         string         `db:"status"`
	Attempts       int            `db:"attempts"`
	ResponseStatus sql.NullInt64  `db:"response_status"`
	ExternalTaskID sql.NullString `db:"external_task_id"`
	LastError      sql.NullString `db:"last_error"`
	CreatedAt      time.Time      `db:"created_at"`
	SentAt         sql.NullTime   `db:"sent_at"`
}

func (r dispatchRow) toRecord() DispatchRecord {
	var responseStatus *int
	if r.ResponseStatus.Valid {
		status := int(r.ResponseStatus.Int64)
		responseStatus = &status
	}
	var sentAt *time.Time
	if r.SentAt.Valid {
		at := r.SentAt.Time
		sentAt = &at
	}
	return DispatchRecord{
		ID:             r.ID,
		GoalID:         r.GoalID,
		EvaluationID:   r.EvaluationID,
		BotID:          r.BotID,
		ChannelID:      r.ChannelID,
		IdempotencyKey: r.IdempotencyKey,
		Brief:          r.Brief,
		RequestPayload: r.RequestPayload,
		Status:         DispatchStatus(r.Status),
		Attempts:       r.Attempts,
		ResponseStatus: responseStatus,
		ExternalTaskID: r.ExternalTaskID.String,
		LastError:      r.LastError.String,
		CreatedAt:      r.CreatedAt,
		SentAt:         sentAt,
	}
}

// Create claims the right to dispatch. A duplicate idempotency key returns
// ErrConflict, which is the healthy outcome of a retry rather than a failure:
// the row already exists, so no second agent task will be created.
func (r *DispatchRepository) Create(ctx context.Context, input DispatchInput) (DispatchRecord, error) {
	const query = `
		INSERT INTO trigger_dispatches (
			goal_id, evaluation_id, bot_id, channel_id, idempotency_key, brief,
			request_payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING ` + dispatchColumns

	if input.IdempotencyKey == "" {
		return DispatchRecord{}, errors.New("dispatch: idempotency key is required")
	}

	var row dispatchRow
	err := r.db.QueryRowxContext(ctx, query,
		input.GoalID, input.EvaluationID, input.BotID, input.ChannelID,
		input.IdempotencyKey, input.Brief, jsonOrEmpty(input.RequestPayload),
	).StructScan(&row)
	if err != nil {
		return DispatchRecord{}, fmt.Errorf("create dispatch for goal %s: %w", input.GoalID, classify(err))
	}
	return row.toRecord(), nil
}

// GetByIdempotencyKey returns an existing dispatch, or ErrNotFound.
func (r *DispatchRepository) GetByIdempotencyKey(ctx context.Context, key string) (DispatchRecord, error) {
	const query = `SELECT ` + dispatchColumns + ` FROM trigger_dispatches WHERE idempotency_key = $1`

	var row dispatchRow
	if err := r.db.QueryRowxContext(ctx, query, key).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DispatchRecord{}, ErrNotFound
		}
		return DispatchRecord{}, fmt.Errorf("get dispatch by key: %w", classify(err))
	}
	return row.toRecord(), nil
}

// MarkSent records that Wingman core accepted the task.
func (r *DispatchRepository) MarkSent(ctx context.Context, id string, responseStatus int, externalTaskID string) error {
	const query = `
		UPDATE trigger_dispatches
		SET status = 'sent',
			attempts = attempts + 1,
			response_status = $2,
			external_task_id = $3,
			last_error = NULL,
			sent_at = now()
		WHERE id = $1`

	result, err := r.db.ExecContext(ctx, query, id, responseStatus, nullIfEmpty(externalTaskID))
	if err != nil {
		return fmt.Errorf("mark dispatch %s sent: %w", id, classify(err))
	}
	return requireOneRow(result, id)
}

// MarkFailed records a failed attempt. The attempt counter is what a retry
// worker uses to decide when to abandon.
func (r *DispatchRepository) MarkFailed(ctx context.Context, id string, responseStatus *int, message string) error {
	const query = `
		UPDATE trigger_dispatches
		SET status = 'failed',
			attempts = attempts + 1,
			response_status = $2,
			last_error = $3
		WHERE id = $1`

	var status any
	if responseStatus != nil {
		status = *responseStatus
	}
	result, err := r.db.ExecContext(ctx, query, id, status, nullIfEmpty(message))
	if err != nil {
		return fmt.Errorf("mark dispatch %s failed: %w", id, classify(err))
	}
	return requireOneRow(result, id)
}

// MarkAbandoned stops retrying a dispatch.
func (r *DispatchRepository) MarkAbandoned(ctx context.Context, id, reason string) error {
	const query = `UPDATE trigger_dispatches SET status = 'abandoned', last_error = $2 WHERE id = $1`

	result, err := r.db.ExecContext(ctx, query, id, nullIfEmpty(reason))
	if err != nil {
		return fmt.Errorf("mark dispatch %s abandoned: %w", id, classify(err))
	}
	return requireOneRow(result, id)
}

// History reports the trigger activity the cooldown and per-period budget are
// checked against.
//
// Only pending and sent dispatches count. A failed dispatch never reached an
// agent, so charging it against the budget would silently spend a goal's whole
// allowance on an outage.
func (r *DispatchRepository) History(ctx context.Context, goalID string, periodStart time.Time) (DispatchHistory, error) {
	const query = `
		SELECT max(created_at) AS last_at, count(*) AS total
		FROM trigger_dispatches
		WHERE goal_id = $1
			AND created_at >= $2
			AND status IN ('pending', 'sent')`

	var row struct {
		LastAt sql.NullTime `db:"last_at"`
		Total  int          `db:"total"`
	}
	if err := r.db.QueryRowxContext(ctx, query, goalID, periodStart).StructScan(&row); err != nil {
		return DispatchHistory{}, fmt.Errorf("dispatch history for goal %s: %w", goalID, classify(err))
	}

	history := DispatchHistory{CountThisPeriod: row.Total}
	if row.LastAt.Valid {
		at := row.LastAt.Time
		history.LastTriggeredAt = &at
	}
	return history, nil
}

// ListByGoal returns a goal's dispatches, newest first.
func (r *DispatchRepository) ListByGoal(ctx context.Context, goalID string, limit, offset int) ([]DispatchRecord, int, error) {
	const countQuery = `SELECT count(*) FROM trigger_dispatches WHERE goal_id = $1`
	const listQuery = `
		SELECT ` + dispatchColumns + `
		FROM trigger_dispatches
		WHERE goal_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery, goalID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count dispatches for goal %s: %w", goalID, classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []dispatchRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, goalID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list dispatches for goal %s: %w", goalID, classify(err))
	}

	records := make([]DispatchRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, total, nil
}

// ListRetryable returns failed dispatches a retry worker may attempt again.
func (r *DispatchRepository) ListRetryable(ctx context.Context, maxAttempts, limit int) ([]DispatchRecord, error) {
	const query = `
		SELECT ` + dispatchColumns + `
		FROM trigger_dispatches
		WHERE status IN ('pending', 'failed') AND attempts < $1
		ORDER BY created_at ASC
		LIMIT $2`

	limit, _ = normalisePage(limit, 0)
	rows := []dispatchRow{}
	if err := r.db.SelectContext(ctx, &rows, query, maxAttempts, limit); err != nil {
		return nil, fmt.Errorf("list retryable dispatches: %w", classify(err))
	}

	records := make([]DispatchRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, nil
}
