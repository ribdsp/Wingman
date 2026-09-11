package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// ApprovalResolution mirrors the approval_resolution enum: what a human did with
// a pending request.
type ApprovalResolution string

const (
	// ResolutionApproved means a human cleared the action.
	ResolutionApproved ApprovalResolution = "approved"
	// ResolutionRejected means a human refused it.
	ResolutionRejected ApprovalResolution = "rejected"
	// ResolutionExpired means nobody answered in time. Silence is not consent.
	ResolutionExpired ApprovalResolution = "expired"
)

// ApprovalInput is a decided approval request, ready to be recorded. The outcome
// is computed by domain.DecideApproval before it ever reaches storage.
type ApprovalInput struct {
	ActionType string
	Amount     float64
	Currency   string
	// RequestedBy is the bot that asked. Recorded, never trusted for policy.
	RequestedBy    string
	GoalID         *string
	IdempotencyKey string
	Outcome        domain.ApprovalOutcome
	PolicyReason   string
	Payload        string
	ExpiresAt      *time.Time
}

// ApprovalRecord is a stored approval request.
type ApprovalRecord struct {
	ID             string
	ActionType     string
	Amount         float64
	Currency       string
	RequestedBy    string
	GoalID         *string
	IdempotencyKey string
	Outcome        domain.ApprovalOutcome
	PolicyReason   string
	Resolution     *ApprovalResolution
	ResolvedBy     *string
	ResolvedAt     *time.Time
	ResolutionNote string
	Payload        string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
}

// IsOpen reports whether the request is still waiting on a human.
func (r ApprovalRecord) IsOpen() bool {
	return r.Outcome == domain.ApprovalPending && r.Resolution == nil
}

// ApprovalFilter narrows an approval listing.
type ApprovalFilter struct {
	ActionType string
	Outcome    domain.ApprovalOutcome
	// OpenOnly restricts the result to requests still awaiting a human.
	OpenOnly bool
	Limit    int
	Offset   int
}

// ApprovalRepository stores approval requests.
type ApprovalRepository struct {
	db *sqlx.DB
}

// NewApprovalRepository builds a repository over the given pool.
func NewApprovalRepository(db *sqlx.DB) *ApprovalRepository {
	return &ApprovalRepository{db: db}
}

// amount is read as double precision. The ledger keeps numeric for exactness;
// float64 is only used at the point a threshold is compared, which is exact for
// any amount below 2^53.
const approvalColumns = `id, action_type, amount::double precision AS amount, currency,
	requested_by, goal_id, idempotency_key, outcome, policy_reason, resolution,
	resolved_by, resolved_at, resolution_note, payload::text AS payload,
	created_at, expires_at`

type approvalRow struct {
	ID             string         `db:"id"`
	ActionType     string         `db:"action_type"`
	Amount         float64        `db:"amount"`
	Currency       string         `db:"currency"`
	RequestedBy    string         `db:"requested_by"`
	GoalID         sql.NullString `db:"goal_id"`
	IdempotencyKey string         `db:"idempotency_key"`
	Outcome        string         `db:"outcome"`
	PolicyReason   string         `db:"policy_reason"`
	Resolution     sql.NullString `db:"resolution"`
	ResolvedBy     sql.NullString `db:"resolved_by"`
	ResolvedAt     sql.NullTime   `db:"resolved_at"`
	ResolutionNote string         `db:"resolution_note"`
	Payload        string         `db:"payload"`
	CreatedAt      time.Time      `db:"created_at"`
	ExpiresAt      sql.NullTime   `db:"expires_at"`
}

func (r approvalRow) toRecord() ApprovalRecord {
	record := ApprovalRecord{
		ID:             r.ID,
		ActionType:     r.ActionType,
		Amount:         r.Amount,
		Currency:       r.Currency,
		RequestedBy:    r.RequestedBy,
		IdempotencyKey: r.IdempotencyKey,
		Outcome:        domain.ApprovalOutcome(r.Outcome),
		PolicyReason:   r.PolicyReason,
		ResolutionNote: r.ResolutionNote,
		Payload:        r.Payload,
		CreatedAt:      r.CreatedAt,
	}
	if r.GoalID.Valid {
		id := r.GoalID.String
		record.GoalID = &id
	}
	if r.Resolution.Valid {
		resolution := ApprovalResolution(r.Resolution.String)
		record.Resolution = &resolution
	}
	if r.ResolvedBy.Valid {
		by := r.ResolvedBy.String
		record.ResolvedBy = &by
	}
	if r.ResolvedAt.Valid {
		at := r.ResolvedAt.Time
		record.ResolvedAt = &at
	}
	if r.ExpiresAt.Valid {
		at := r.ExpiresAt.Time
		record.ExpiresAt = &at
	}
	return record
}

// Create records a decided request. A repeat idempotency key returns
// ErrConflict, so a retrying agent cannot raise the same spend twice.
func (r *ApprovalRepository) Create(ctx context.Context, input ApprovalInput) (ApprovalRecord, error) {
	const query = `
		INSERT INTO approval_requests (
			action_type, amount, currency, requested_by, goal_id, idempotency_key,
			outcome, policy_reason, payload, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
		RETURNING ` + approvalColumns

	if input.IdempotencyKey == "" {
		return ApprovalRecord{}, errors.New("approval: idempotency key is required")
	}
	if err := requireFinite("approval amount", input.Amount); err != nil {
		return ApprovalRecord{}, err
	}

	var goalID any
	if input.GoalID != nil && *input.GoalID != "" {
		goalID = *input.GoalID
	}
	var expiresAt any
	if input.ExpiresAt != nil {
		expiresAt = *input.ExpiresAt
	}

	var row approvalRow
	err := r.db.QueryRowxContext(ctx, query,
		input.ActionType, input.Amount, strings.ToUpper(input.Currency), input.RequestedBy,
		goalID, input.IdempotencyKey, string(input.Outcome), input.PolicyReason,
		jsonOrEmpty(input.Payload), expiresAt,
	).StructScan(&row)
	if err != nil {
		return ApprovalRecord{}, fmt.Errorf("create approval for %s: %w", input.ActionType, classify(err))
	}
	return row.toRecord(), nil
}

// GetByID returns one approval request, or ErrNotFound.
func (r *ApprovalRepository) GetByID(ctx context.Context, id string) (ApprovalRecord, error) {
	const query = `SELECT ` + approvalColumns + ` FROM approval_requests WHERE id = $1`

	var row approvalRow
	if err := r.db.QueryRowxContext(ctx, query, id).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApprovalRecord{}, ErrNotFound
		}
		return ApprovalRecord{}, fmt.Errorf("get approval %s: %w", id, classify(err))
	}
	return row.toRecord(), nil
}

// GetByIdempotencyKey returns the request recorded under a key, or ErrNotFound.
func (r *ApprovalRepository) GetByIdempotencyKey(ctx context.Context, key string) (ApprovalRecord, error) {
	const query = `SELECT ` + approvalColumns + ` FROM approval_requests WHERE idempotency_key = $1`

	var row approvalRow
	if err := r.db.QueryRowxContext(ctx, query, key).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ApprovalRecord{}, ErrNotFound
		}
		return ApprovalRecord{}, fmt.Errorf("get approval by key: %w", classify(err))
	}
	return row.toRecord(), nil
}

// List returns a page of approval requests plus the total number of matches.
func (r *ApprovalRepository) List(ctx context.Context, filter ApprovalFilter) ([]ApprovalRecord, int, error) {
	where := []string{"1 = 1"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if filter.ActionType != "" {
		add("action_type = $%d", filter.ActionType)
	}
	if filter.Outcome != "" {
		add("outcome = $%d", string(filter.Outcome))
	}
	if filter.OpenOnly {
		where = append(where, "outcome = 'pending' AND resolution IS NULL")
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM approval_requests WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count approvals: %w", classify(err))
	}

	limit, offset := normalisePage(filter.Limit, filter.Offset)
	args = append(args, limit, offset)
	query := fmt.Sprintf(
		`SELECT %s FROM approval_requests WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		approvalColumns, clause, len(args)-1, len(args),
	)

	rows := []approvalRow{}
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, 0, fmt.Errorf("list approvals: %w", classify(err))
	}

	records := make([]ApprovalRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, total, nil
}

// Resolve records a human decision on a pending request.
//
// The WHERE clause is the concurrency control: two operators clicking approve
// and reject at the same time cannot both win, and the loser gets ErrConflict
// rather than silently overwriting the first decision.
func (r *ApprovalRepository) Resolve(ctx context.Context, id string, resolution ApprovalResolution, resolvedBy, note string) (ApprovalRecord, error) {
	const query = `
		UPDATE approval_requests
		SET resolution = $2, resolved_by = $3, resolved_at = now(), resolution_note = $4
		WHERE id = $1 AND outcome = 'pending' AND resolution IS NULL
		RETURNING ` + approvalColumns

	if resolvedBy == "" {
		return ApprovalRecord{}, errors.New("approval: a resolution must record who made it")
	}

	var row approvalRow
	err := r.db.QueryRowxContext(ctx, query, id, string(resolution), resolvedBy, note).StructScan(&row)
	if err == nil {
		return row.toRecord(), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ApprovalRecord{}, fmt.Errorf("resolve approval %s: %w", id, classify(err))
	}

	// No row matched: either it does not exist, or it was already decided.
	// Distinguishing the two is worth an extra read so the caller can answer 404
	// or 409 rather than a blanket error.
	if _, getErr := r.GetByID(ctx, id); getErr != nil {
		return ApprovalRecord{}, getErr
	}
	return ApprovalRecord{}, ErrConflict
}

// ExpireOverdue marks pending requests past their deadline as expired. Timing
// out to "expired" rather than leaving them open keeps a stale card from being
// approved weeks later.
func (r *ApprovalRepository) ExpireOverdue(ctx context.Context, now time.Time) (int, error) {
	const query = `
		UPDATE approval_requests
		SET resolution = 'expired', resolved_by = 'system', resolved_at = $1,
			resolution_note = 'expired without a human decision'
		WHERE outcome = 'pending'
			AND resolution IS NULL
			AND expires_at IS NOT NULL
			AND expires_at <= $1`

	result, err := r.db.ExecContext(ctx, query, now)
	if err != nil {
		return 0, fmt.Errorf("expire overdue approvals: %w", classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired approvals: %w", err)
	}
	return int(affected), nil
}
