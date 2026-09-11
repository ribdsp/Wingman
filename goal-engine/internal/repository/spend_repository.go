package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// spendLockClass namespaces this service's advisory locks so they cannot collide
// with locks taken by anything else sharing the database.
const spendLockClass = 4711

// SpendInput is one committed spend to record.
type SpendInput struct {
	ActionType string
	Amount     float64
	Currency   string
	ApprovalID *string
	BotID      string
	OccurredAt time.Time
	Note       string
}

// SpendRecord is a stored ledger entry.
type SpendRecord struct {
	ID         int64
	ActionType string
	Amount     float64
	Currency   string
	ApprovalID *string
	BotID      string
	OccurredAt time.Time
	Note       string
}

// queryer is the subset of sqlx both a pool and a transaction satisfy, which is
// what lets the same query methods run inside or outside a transaction.
type queryer interface {
	QueryRowxContext(ctx context.Context, query string, args ...any) *sqlx.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

// SpendRepository stores committed spend, which is what daily caps are enforced
// against.
type SpendRepository struct {
	db *sqlx.DB
	// q is where queries actually run: the pool normally, a transaction inside
	// WithinActionLock.
	q queryer
}

// NewSpendRepository builds a repository over the given pool.
func NewSpendRepository(db *sqlx.DB) *SpendRepository {
	return &SpendRepository{db: db, q: db}
}

const spendColumns = `id, action_type, amount::double precision AS amount, currency,
	approval_id, bot_id, occurred_at, note`

type spendRow struct {
	ID         int64          `db:"id"`
	ActionType string         `db:"action_type"`
	Amount     float64        `db:"amount"`
	Currency   string         `db:"currency"`
	ApprovalID sql.NullString `db:"approval_id"`
	BotID      string         `db:"bot_id"`
	OccurredAt time.Time      `db:"occurred_at"`
	Note       string         `db:"note"`
}

func (r spendRow) toRecord() SpendRecord {
	record := SpendRecord{
		ID:         r.ID,
		ActionType: r.ActionType,
		Amount:     r.Amount,
		Currency:   r.Currency,
		BotID:      r.BotID,
		OccurredAt: r.OccurredAt,
		Note:       r.Note,
	}
	if r.ApprovalID.Valid {
		id := r.ApprovalID.String
		record.ApprovalID = &id
	}
	return record
}

// SpendLedger is the read-and-write surface of the ledger. It is declared here
// rather than at the call site because WithinActionLock hands one to its
// callback: naming it lets a caller be tested without a database, which for the
// code that enforces spending caps is worth the small inversion.
type SpendLedger interface {
	SpentSince(ctx context.Context, actionType, currency string, since time.Time) (float64, error)
	Record(ctx context.Context, input SpendInput) (SpendRecord, error)
}

// WithinActionLock runs fn holding a transaction-scoped advisory lock for one
// action type.
//
// Without it the daily cap is advisory only: two concurrent requests both read
// the same "spent today", both find room under the cap, and both commit. The
// lock serialises read-decide-record per action type, which is the only way the
// cap means anything. The ledger handed to fn runs inside that transaction.
func (r *SpendRepository) WithinActionLock(ctx context.Context, actionType string, fn func(locked SpendLedger) error) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spend transaction for %s: %w", actionType, classify(err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	const lockQuery = `SELECT pg_advisory_xact_lock($1, hashtext($2))`
	if _, err := tx.ExecContext(ctx, lockQuery, spendLockClass, actionType); err != nil {
		return fmt.Errorf("lock action type %s: %w", actionType, classify(err))
	}

	if err := fn(&SpendRepository{db: r.db, q: tx}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit spend transaction for %s: %w", actionType, classify(err))
	}
	committed = true
	return nil
}

// Record appends a committed spend.
func (r *SpendRepository) Record(ctx context.Context, input SpendInput) (SpendRecord, error) {
	const query = `
		INSERT INTO spend_ledger (action_type, amount, currency, approval_id, bot_id, occurred_at, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING ` + spendColumns

	if err := requireFinite("spend amount", input.Amount); err != nil {
		return SpendRecord{}, err
	}

	var approvalID any
	if input.ApprovalID != nil && *input.ApprovalID != "" {
		approvalID = *input.ApprovalID
	}
	occurredAt := input.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	var row spendRow
	err := r.q.QueryRowxContext(ctx, query,
		input.ActionType, input.Amount, strings.ToUpper(input.Currency), approvalID,
		input.BotID, occurredAt, input.Note,
	).StructScan(&row)
	if err != nil {
		return SpendRecord{}, fmt.Errorf("record spend for %s: %w", input.ActionType, classify(err))
	}
	return row.toRecord(), nil
}

// SpentSince totals committed spend for an action type since a point in time.
// An empty currency sums every currency, which is only meaningful when the
// action type is known to use one.
func (r *SpendRepository) SpentSince(ctx context.Context, actionType, currency string, since time.Time) (float64, error) {
	const query = `
		SELECT coalesce(sum(amount), 0)::double precision
		FROM spend_ledger
		WHERE action_type = $1
			AND occurred_at >= $2
			AND ($3 = '' OR currency = $3)`

	var total float64
	row := r.q.QueryRowxContext(ctx, query, actionType, since, strings.ToUpper(currency))
	if err := row.Scan(&total); err != nil {
		return 0, fmt.Errorf("sum spend for %s: %w", actionType, classify(err))
	}
	return total, nil
}

// ListSince returns ledger entries for an action type, newest first.
func (r *SpendRepository) ListSince(ctx context.Context, actionType string, since time.Time, limit int) ([]SpendRecord, error) {
	const query = `
		SELECT ` + spendColumns + `
		FROM spend_ledger
		WHERE ($1 = '' OR action_type = $1) AND occurred_at >= $2
		ORDER BY occurred_at DESC, id DESC
		LIMIT $3`

	limit, _ = normalisePage(limit, 0)
	rows := []spendRow{}
	if err := r.q.SelectContext(ctx, &rows, query, actionType, since, limit); err != nil {
		return nil, fmt.Errorf("list spend: %w", classify(err))
	}

	records := make([]SpendRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, nil
}
