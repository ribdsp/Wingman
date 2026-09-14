package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// LinkCode is one outstanding invitation for a chat account to attach itself to a
// Wingman account.
//
// It carries no code — not even the hash. The caller already had the code to mint or
// redeem it, and a value that travels back up into a handler is a value that ends up
// in a response body or a log line. This is the shortest-lived credential in the
// system, because it is the only one that travels through a third party's servers.
type LinkCode struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
}

// Live reports whether the code would still be redeemable at the given time.
func (c LinkCode) Live(now time.Time) bool {
	return c.ConsumedAt == nil && c.ExpiresAt.After(now)
}

// LinkCodeRepository stores link codes. Every method takes the *stored* form — the
// SHA-256 from auth.HashToken — for the same reason SessionRepository does: a
// plaintext credential must never reach a query.
type LinkCodeRepository struct {
	db *sqlx.DB
}

// NewLinkCodeRepository builds a repository over the given pool.
func NewLinkCodeRepository(db *sqlx.DB) *LinkCodeRepository {
	return &LinkCodeRepository{db: db}
}

const linkCodeColumns = "id, user_id, created_at, expires_at, consumed_at"

// NewLinkCode is what Mint needs.
type NewLinkCode struct {
	UserID    string
	CodeHash  string
	ExpiresAt time.Time
}

// Mint records a new code and discards whatever the same account had outstanding.
//
// One live code per person, and the delete is in the same statement as the insert so
// there is no window in which somebody has none. A data-modifying CTE runs whether or
// not the outer query reads from it, which is why the DELETE below has no reference in
// the INSERT and still happens.
//
// Superseded rows are deleted rather than marked consumed. The two are different
// facts: this table is what answers "was my code used?", and filing a replaced code
// under consumed_at would answer yes about a code nobody ever redeemed. The history of
// minting lives in the audit log, which is the place that keeps things.
func (r *LinkCodeRepository) Mint(ctx context.Context, input NewLinkCode) (LinkCode, error) {
	const query = `
		WITH superseded AS (
			DELETE FROM channel_link_codes
			WHERE user_id = $1 AND consumed_at IS NULL
		)
		INSERT INTO channel_link_codes (user_id, code_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING ` + linkCodeColumns

	if input.CodeHash == "" {
		return LinkCode{}, errors.New("link code: code hash is required")
	}
	if input.ExpiresAt.IsZero() {
		// A code with no expiry is a permanent account credential sitting in
		// somebody's chat history, where they cannot delete it from both sides.
		return LinkCode{}, errors.New("link code: expiry is required")
	}

	var row linkCodeRow
	err := r.db.QueryRowxContext(ctx, query, input.UserID, input.CodeHash, input.ExpiresAt).StructScan(&row)
	if err != nil {
		return LinkCode{}, fmt.Errorf("mint link code for user %s: %w", input.UserID, classify(err))
	}
	return row.toLinkCode(), nil
}

// Consume claims a code and returns the account it belongs to, or ErrNotFound.
//
// Single use is enforced by this one statement: the row is only updated if it has not
// been consumed and has not expired, so two senders racing on the same code produce
// one winner and one ErrNotFound. A read followed by a write would produce two winners,
// and the second one would be somebody who read the code over the first one's shoulder.
//
// Unknown, expired and already used all answer ErrNotFound. A sender who can tell
// those apart can tell that a code was once real, which is half of guessing one.
func (r *LinkCodeRepository) Consume(ctx context.Context, codeHash string, at time.Time) (string, error) {
	const query = `
		UPDATE channel_link_codes SET consumed_at = $2
		WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING user_id`

	if codeHash == "" {
		return "", ErrNotFound
	}
	if at.IsZero() {
		// The zero time is before every expiry, so it would turn every real code into
		// a wrong one. Refused rather than answered, because "wrong code" is what the
		// person would be told.
		return "", errors.New("link code: consuming needs a time")
	}

	var userID string
	if err := r.db.QueryRowxContext(ctx, query, codeHash, at).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		// Not folded into ErrNotFound: an outage would otherwise tell a person their
		// code is wrong, and send them to mint another one that also cannot be read.
		return "", fmt.Errorf("consume link code: %w", classify(err))
	}
	return userID, nil
}

// DeleteSpent removes codes that expired or were used before the cutoff.
//
// Both are deleted, unlike sessions where a revocation is kept: a spent link code
// records nothing a person needs to see later, and the audit log already holds who
// linked what and when. There is no index for this — the table holds at most one live
// row per account, so the sweep is a scan over almost nothing.
func (r *LinkCodeRepository) DeleteSpent(ctx context.Context, before time.Time) (int, error) {
	const query = `
		DELETE FROM channel_link_codes
		WHERE expires_at < $1 OR consumed_at < $1`

	result, err := r.db.ExecContext(ctx, query, before)
	if err != nil {
		return 0, fmt.Errorf("delete spent link codes: %w", classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted link codes: %w", err)
	}
	return int(affected), nil
}

type linkCodeRow struct {
	ID         string       `db:"id"`
	UserID     string       `db:"user_id"`
	CreatedAt  time.Time    `db:"created_at"`
	ExpiresAt  time.Time    `db:"expires_at"`
	ConsumedAt sql.NullTime `db:"consumed_at"`
}

func (r linkCodeRow) toLinkCode() LinkCode {
	return LinkCode{
		ID:         r.ID,
		UserID:     r.UserID,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		ConsumedAt: timePtr(r.ConsumedAt),
	}
}
