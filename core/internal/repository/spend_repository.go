package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// Spend is one recorded token charge.
type Spend struct {
	UserID     string
	RunID      string
	Provider   string
	Model      string
	TokensIn   int64
	TokensOut  int64
	OccurredAt time.Time
}

// Total is what the charge came to.
func (s Spend) Total() int64 { return s.TokensIn + s.TokensOut }

// UserDailyTotal is one account's spend for a day.
type UserDailyTotal struct {
	UserID string
	Tokens int64
}

// SpendRepository is the per-user token ledger.
//
// It answers one question the run loop asks before every iteration — how much has
// this account spent today — and one the metrics push asks once a minute: how much has
// everybody spent today. Both are reads of the same rows.
//
// The day boundary is computed here rather than stored, because "today" depends on the
// deployment's timezone, and a boundary baked into a row would be wrong the first time
// an operator moved. The location comes from configuration and is fixed at startup.
type SpendRepository struct {
	db *sqlx.DB
	// loc is the timezone the daily cap resets in. A cap that reset at UTC midnight
	// for an operator seven hours ahead of UTC would reset in the middle of their
	// working morning.
	loc *time.Location
}

// NewSpendRepository builds a ledger that reckons its days in loc.
//
// A nil location is taken as UTC rather than being an error: a ledger that refuses to
// construct would stop the service starting, and a daily boundary in the wrong zone is
// a reporting inconvenience, not a spending one — the per-run cap is unaffected.
func NewSpendRepository(db *sqlx.DB, loc *time.Location) *SpendRepository {
	if loc == nil {
		loc = time.UTC
	}
	return &SpendRepository{db: db, loc: loc}
}

// Record files a token charge against an account.
//
// It is called after the model call returns, not before it is made, so the ledger
// records what was actually spent. The consequence is that a single call can carry a
// run past its allowance — which is why the ladder passes RemainingTokens to the
// provider as a ceiling rather than relying on this to stop it.
func (r *SpendRepository) Record(ctx context.Context, spend Spend) error {
	const query = `
		INSERT INTO token_spend (user_id, run_id, provider, model, tokens_in, tokens_out, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	if spend.TokensIn < 0 || spend.TokensOut < 0 {
		// A negative row would refund an allowance nobody granted.
		return fmt.Errorf("spend for user %s is negative: in=%d out=%d", spend.UserID, spend.TokensIn, spend.TokensOut)
	}

	at := spend.OccurredAt
	if at.IsZero() {
		return fmt.Errorf("spend for user %s has no timestamp", spend.UserID)
	}

	_, err := r.db.ExecContext(ctx, query,
		spend.UserID, nullIfEmpty(spend.RunID), spend.Provider, spend.Model,
		spend.TokensIn, spend.TokensOut, at)
	if err != nil {
		return fmt.Errorf("record spend for user %s: %w", spend.UserID, classify(err))
	}
	return nil
}

// Ledger reads what an account has spent on the day containing now.
//
// The returned Ledger is what the continuation ladder is given. It is marked readable
// only when the query succeeded: a failure returns an unreadable ledger *and* the
// error, so a caller that ignores one still gets the fail-closed answer from the other.
// Reporting a broken read as zero spend would be indistinguishable from an untouched
// budget, which is the one confusion the ladder exists to prevent.
func (r *SpendRepository) Ledger(ctx context.Context, userID string, now time.Time) (domain.Ledger, error) {
	const query = `
		SELECT coalesce(sum(tokens_total), 0)
		FROM token_spend
		WHERE user_id = $1 AND occurred_at >= $2 AND occurred_at < $3`

	start, end := r.DayBounds(now)
	var total int64
	if err := r.db.QueryRowContext(ctx, query, userID, start, end).Scan(&total); err != nil {
		return domain.Ledger{Readable: false}, fmt.Errorf("read today's spend for user %s: %w", userID, classify(err))
	}
	return domain.Ledger{Readable: true, TokensToday: total}, nil
}

// TotalForDay is the whole instance's spend on the day containing now.
//
// This is the number pushed to the goal engine as the ops.tokens_spent sample. It is a
// running total rather than an increment on purpose: re-sending one after a failed push
// cannot inflate it, so at-least-once delivery needs no bookkeeping on this side.
func (r *SpendRepository) TotalForDay(ctx context.Context, now time.Time) (int64, error) {
	const query = `
		SELECT coalesce(sum(tokens_total), 0)
		FROM token_spend
		WHERE occurred_at >= $1 AND occurred_at < $2`

	start, end := r.DayBounds(now)
	var total int64
	if err := r.db.QueryRowContext(ctx, query, start, end).Scan(&total); err != nil {
		return 0, fmt.Errorf("read today's total spend: %w", classify(err))
	}
	return total, nil
}

// TotalsByUserForDay breaks the day's spend down by account, largest first.
//
// An operator watching one number climb cannot tell a busy team from one runaway loop.
// This is what answers that, and it is a read of the same rows rather than a second
// counter that could disagree with the first.
func (r *SpendRepository) TotalsByUserForDay(ctx context.Context, now time.Time, limit int) ([]UserDailyTotal, error) {
	const query = `
		SELECT user_id, coalesce(sum(tokens_total), 0) AS tokens
		FROM token_spend
		WHERE occurred_at >= $1 AND occurred_at < $2
		GROUP BY user_id
		ORDER BY tokens DESC, user_id ASC
		LIMIT $3`

	start, end := r.DayBounds(now)
	limit, _ = normalisePage(limit, 0)

	rows := []struct {
		UserID string `db:"user_id"`
		Tokens int64  `db:"tokens"`
	}{}
	if err := r.db.SelectContext(ctx, &rows, query, start, end, limit); err != nil {
		return nil, fmt.Errorf("read today's spend by user: %w", classify(err))
	}

	totals := make([]UserDailyTotal, 0, len(rows))
	for _, row := range rows {
		totals = append(totals, UserDailyTotal{UserID: row.UserID, Tokens: row.Tokens})
	}
	return totals, nil
}

// ForRun lists what one run cost, oldest first — the same order as its transcript, so
// the two read side by side. This is what makes an expensive run answerable to "on what?".
func (r *SpendRepository) ForRun(ctx context.Context, runID string) ([]Spend, error) {
	const query = `
		SELECT user_id, run_id, provider, model, tokens_in, tokens_out, occurred_at
		FROM token_spend
		WHERE run_id = $1
		ORDER BY occurred_at ASC, id ASC`

	rows := []spendRow{}
	if err := r.db.SelectContext(ctx, &rows, query, runID); err != nil {
		return nil, fmt.Errorf("read spend for run %s: %w", runID, classify(err))
	}

	spends := make([]Spend, 0, len(rows))
	for _, row := range rows {
		spends = append(spends, row.toSpend())
	}
	return spends, nil
}

// DayBounds returns the half-open interval [start, end) covering the local day that
// contains now.
//
// Half-open rather than inclusive at both ends because a charge landing exactly on
// midnight has to belong to exactly one day. It is exported because the metrics push
// reports which window a total covers, and a second implementation of "when does the
// day start" would eventually disagree with this one.
func (r *SpendRepository) DayBounds(now time.Time) (time.Time, time.Time) {
	local := now.In(r.loc)
	// Built from the calendar date rather than by truncating a duration: Truncate
	// works in UTC, so it lands on the wrong instant in every zone that is not UTC,
	// and on a zone with a half-hour offset it lands on no midnight at all.
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, r.loc)
	// AddDate rather than Add(24h): across a daylight-saving change a local day is
	// 23 or 25 hours long, and a fixed 24 would either clip an hour of spend or
	// count an hour twice.
	return start, start.AddDate(0, 0, 1)
}

type spendRow struct {
	UserID     string    `db:"user_id"`
	RunID      *string   `db:"run_id"`
	Provider   string    `db:"provider"`
	Model      string    `db:"model"`
	TokensIn   int64     `db:"tokens_in"`
	TokensOut  int64     `db:"tokens_out"`
	OccurredAt time.Time `db:"occurred_at"`
}

func (r spendRow) toSpend() Spend {
	spend := Spend{
		UserID:     r.UserID,
		Provider:   r.Provider,
		Model:      r.Model,
		TokensIn:   r.TokensIn,
		TokensOut:  r.TokensOut,
		OccurredAt: r.OccurredAt,
	}
	if r.RunID != nil {
		spend.RunID = *r.RunID
	}
	return spend
}
