package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jmoiron/sqlx"
)

// LiveSession is a session that authentication accepted, and the person behind it.
//
// It carries no token — not even the hash. The caller already had the token to look it
// up, and putting it back in a value that travels up into a request context is how a
// credential ends up in a log line.
type LiveSession struct {
	ID          string
	UserID      string
	DisplayName string
	Email       string
	ExpiresAt   time.Time
}

// Session is one row of the sessions table, for a person reviewing their devices.
type Session struct {
	ID         string
	UserID     string
	UserAgent  string
	CreatedIP  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastSeenAt *time.Time
}

// Live reports whether the session would authenticate at the given time.
func (s Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// SessionRepository stores sessions. Every method takes the *stored* form of a token —
// the SHA-256 from auth.HashToken — because a plaintext session token must never reach
// a query, where it would be one slow-query log away from being a set of live logins.
type SessionRepository struct {
	db *sqlx.DB
}

// NewSessionRepository builds a repository over the given pool.
func NewSessionRepository(db *sqlx.DB) *SessionRepository {
	return &SessionRepository{db: db}
}

// NewSession is what Create needs.
type NewSession struct {
	UserID    string
	TokenHash string
	UserAgent string
	// CreatedIP is recorded to help a person recognise a session that is not theirs.
	// An unparseable address is stored as NULL rather than rejected: an odd
	// X-Forwarded-For must not be able to stop somebody signing in.
	CreatedIP string
	ExpiresAt time.Time
}

// Create records a new session.
func (r *SessionRepository) Create(ctx context.Context, input NewSession) (Session, error) {
	const query = `
		INSERT INTO sessions (user_id, token_hash, user_agent, created_ip, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, user_id, user_agent, host(created_ip) AS created_ip,
			created_at, expires_at, revoked_at, last_seen_at`

	if input.TokenHash == "" {
		return Session{}, errors.New("session: token hash is required")
	}
	if input.ExpiresAt.IsZero() {
		// A session with no expiry is a permanent credential handed out by a form.
		return Session{}, errors.New("session: expiry is required")
	}

	var row sessionRow
	err := r.db.QueryRowxContext(ctx, query,
		input.UserID, input.TokenHash, input.UserAgent,
		inetOrNull(input.CreatedIP), input.ExpiresAt,
	).StructScan(&row)
	if err != nil {
		return Session{}, fmt.Errorf("create session for user %s: %w", input.UserID, classify(err))
	}
	return row.toSession(), nil
}

// Resolve looks up a live session by its stored token and returns the person behind
// it, or ErrNotFound.
//
// Unknown, expired and revoked all answer ErrNotFound. The caller cannot tell which,
// and that is intentional: telling somebody their token was "expired" rather than
// "unknown" confirms it was once real. A deactivated account answers the same way —
// is_active is checked here so that disabling an account ends its live sessions
// immediately, rather than at the next sign-in.
func (r *SessionRepository) Resolve(ctx context.Context, tokenHash string, now time.Time) (LiveSession, error) {
	const query = `
		SELECT s.id, s.user_id, u.display_name, u.email, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
			AND s.revoked_at IS NULL
			AND s.expires_at > $2
			AND u.is_active`

	if tokenHash == "" {
		return LiveSession{}, ErrNotFound
	}

	var row struct {
		ID          string    `db:"id"`
		UserID      string    `db:"user_id"`
		DisplayName string    `db:"display_name"`
		Email       string    `db:"email"`
		ExpiresAt   time.Time `db:"expires_at"`
	}
	if err := r.db.QueryRowxContext(ctx, query, tokenHash, now).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LiveSession{}, ErrNotFound
		}
		// Everything else is an infrastructure failure, and the caller has to be able
		// to tell it apart from a wrong token: one shows a sign-in screen, the other
		// answers 503.
		return LiveSession{}, fmt.Errorf("resolve session: %w", classify(err))
	}
	return LiveSession{
		ID:          row.ID,
		UserID:      row.UserID,
		DisplayName: row.DisplayName,
		Email:       row.Email,
		ExpiresAt:   row.ExpiresAt,
	}, nil
}

// TouchLastSeen records that a session was used.
//
// It is a separate call rather than part of Resolve, because Resolve runs on every
// authenticated request and a write on every request is a write the service can decide
// to skip. The service touches at most once every few minutes; "last seen" is for a
// person recognising their own devices, not an access log.
func (r *SessionRepository) TouchLastSeen(ctx context.Context, tokenHash string, at time.Time) error {
	const query = `UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`

	if _, err := r.db.ExecContext(ctx, query, tokenHash, at); err != nil {
		return fmt.Errorf("touch session: %w", classify(err))
	}
	// No requireOneRow: a session that was revoked between authenticating and this
	// update is a race with a correct outcome, not a failure to report.
	return nil
}

// RevokeByToken ends one session — a sign-out.
func (r *SessionRepository) RevokeByToken(ctx context.Context, tokenHash string, at time.Time) error {
	const query = `
		UPDATE sessions SET revoked_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, tokenHash, at)
	if err != nil {
		return fmt.Errorf("revoke session: %w", classify(err))
	}
	return requireOneRow(result, "session")
}

// RevokeByID ends one of a user's sessions from their device list.
//
// The user id is in the WHERE clause, not checked afterwards in Go: an id somebody
// guessed must not be revocable, and a query that cannot revoke another account's
// session is a stronger guarantee than a comparison a later refactor can drop.
func (r *SessionRepository) RevokeByID(ctx context.Context, userID, sessionID string, at time.Time) error {
	const query = `
		UPDATE sessions SET revoked_at = $3
		WHERE id = $2 AND user_id = $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, userID, sessionID, at)
	if err != nil {
		return fmt.Errorf("revoke session %s: %w", sessionID, classify(err))
	}
	return requireOneRow(result, sessionID)
}

// RevokeAllForUser ends every session an account has. This is what a password change
// runs, and what an operator runs when somebody reports a lost laptop.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID string, at time.Time) (int, error) {
	const query = `
		UPDATE sessions SET revoked_at = $2
		WHERE user_id = $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, userID, at)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions for user %s: %w", userID, classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count revoked sessions for user %s: %w", userID, err)
	}
	return int(affected), nil
}

// ListForUser returns a person's sessions, newest first, so they can see their own
// devices and end one.
func (r *SessionRepository) ListForUser(ctx context.Context, userID string, limit, offset int) ([]Session, error) {
	const query = `
		SELECT id, user_id, user_agent, host(created_ip) AS created_ip,
			created_at, expires_at, revoked_at, last_seen_at
		FROM sessions
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	limit, offset = normalisePage(limit, offset)
	rows := []sessionRow{}
	if err := r.db.SelectContext(ctx, &rows, query, userID, limit, offset); err != nil {
		return nil, fmt.Errorf("list sessions for user %s: %w", userID, classify(err))
	}

	sessions := make([]Session, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, row.toSession())
	}
	return sessions, nil
}

// DeleteExpired removes sessions that expired before the cutoff.
//
// Expired rows are deleted while revoked rows are kept: a revocation is a fact
// somebody may need to explain later, and an expiry is bookkeeping. The cutoff is a
// parameter so the sweep can keep recent history without keeping all of it.
func (r *SessionRepository) DeleteExpired(ctx context.Context, before time.Time) (int, error) {
	const query = `DELETE FROM sessions WHERE expires_at < $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted sessions: %w", err)
	}
	return int(affected), nil
}

type sessionRow struct {
	ID         string         `db:"id"`
	UserID     string         `db:"user_id"`
	UserAgent  string         `db:"user_agent"`
	CreatedIP  sql.NullString `db:"created_ip"`
	CreatedAt  time.Time      `db:"created_at"`
	ExpiresAt  time.Time      `db:"expires_at"`
	RevokedAt  sql.NullTime   `db:"revoked_at"`
	LastSeenAt sql.NullTime   `db:"last_seen_at"`
}

func (r sessionRow) toSession() Session {
	return Session{
		ID:         r.ID,
		UserID:     r.UserID,
		UserAgent:  r.UserAgent,
		CreatedIP:  r.CreatedIP.String,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		RevokedAt:  timePtr(r.RevokedAt),
		LastSeenAt: timePtr(r.LastSeenAt),
	}
}

// inetOrNull keeps an unparseable address out of an inet column. The address is
// recorded for a human to recognise, so a missing one costs nothing; a failed insert
// would cost somebody their sign-in.
func inetOrNull(address string) any {
	if net.ParseIP(address) == nil {
		return nil
	}
	return address
}
