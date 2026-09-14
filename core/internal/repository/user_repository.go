package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// User is an account. There is no role field, and that absence is the design: an
// account is a person, not an administrator of the box core runs on. Privilege comes
// from which environment variable listed a key, so no row a user can affect is able
// to raise one.
type User struct {
	ID          string
	Email       string
	DisplayName string
	IsActive    bool
	CreatedAt   time.Time
}

// UserRepository stores accounts.
type UserRepository struct {
	db *sqlx.DB
}

// NewUserRepository builds a repository over the given pool.
func NewUserRepository(db *sqlx.DB) *UserRepository {
	return &UserRepository{db: db}
}

// userColumns deliberately omits password_hash. A hash that is never selected cannot
// be logged by a struct dump or returned by a handler that renders whatever it was
// given; the one method that needs it asks for it by name.
const userColumns = `id, email, display_name, is_active, created_at`

type userRow struct {
	ID          string    `db:"id"`
	Email       string    `db:"email"`
	DisplayName string    `db:"display_name"`
	IsActive    bool      `db:"is_active"`
	CreatedAt   time.Time `db:"created_at"`
}

// toUser is a conversion rather than a field-by-field literal on purpose: the two types
// have to stay identical, and a conversion makes the compiler say so the moment one gains
// a field the other lacks. The db tags are ignored by the conversion.
func (r userRow) toUser() User {
	return User(r)
}

// Create records a new account.
//
// The email is lower-cased here as well as being checked by the schema. Two accounts
// differing only in capitalisation would be two token budgets for one person, and the
// person would not be able to tell which one they had signed in to.
func (r *UserRepository) Create(ctx context.Context, email, displayName, passwordHash string) (User, error) {
	const query = `
		INSERT INTO users (email, display_name, password_hash)
		VALUES ($1, $2, $3)
		RETURNING ` + userColumns

	email = normaliseEmail(email)
	if email == "" {
		return User{}, errors.New("user: email is required")
	}
	// The schema refuses anything that is not an argon2id PHC string, but failing
	// here names the caller instead of the constraint.
	if !strings.HasPrefix(passwordHash, "$argon2id$") {
		return User{}, errors.New("user: password hash is not an argon2id hash")
	}

	var row userRow
	err := r.db.QueryRowxContext(ctx, query, email, strings.TrimSpace(displayName), passwordHash).StructScan(&row)
	if err != nil {
		// The email is not repeated into the error on a conflict: this error travels
		// to a sign-up response, and echoing it back confirms which addresses have
		// accounts on this instance.
		return User{}, fmt.Errorf("create user: %w", classify(err))
	}
	return row.toUser(), nil
}

// GetByID returns one account.
func (r *UserRepository) GetByID(ctx context.Context, id string) (User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE id = $1`

	var row userRow
	if err := r.db.QueryRowxContext(ctx, query, id).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user %s: %w", id, classify(err))
	}
	return row.toUser(), nil
}

// GetByEmail returns one account by the address it was created with, or ErrNotFound.
//
// It exists for the operator's own wiring rather than for a request: cmd resolves
// CORE_UNATTENDED_OWNER to a user id at boot, so an unattended dispatch has an owner
// before the first one arrives. No request path reaches it, because an address is
// something a stranger can guess and an id is not.
//
// The address is normalised the same way Create normalises it, so an operator writing
// Ops@Example.com in the environment finds the account they made.
func (r *UserRepository) GetByEmail(ctx context.Context, email string) (User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE email = $1`

	var row userRow
	if err := r.db.QueryRowxContext(ctx, query, normaliseEmail(email)).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		// The address is not quoted: this error reaches a startup log, and an account
		// address is somebody's personal data.
		return User{}, fmt.Errorf("get user by email: %w", classify(err))
	}
	return row.toUser(), nil
}

// Credentials is what sign-in needs: the stored hash, and whether the account may be
// signed in to at all.
type Credentials struct {
	UserID       string
	PasswordHash string
	IsActive     bool
}

// GetCredentialsByEmail returns the stored hash for an email address, or ErrNotFound.
//
// It returns the deactivated case as well rather than filtering it out, so the caller
// can verify the password before answering. Skipping the verification for an unknown
// or disabled account is what turns a login form into a way to enumerate accounts by
// response time.
func (r *UserRepository) GetCredentialsByEmail(ctx context.Context, email string) (Credentials, error) {
	const query = `SELECT id, password_hash, is_active FROM users WHERE email = $1`

	var row struct {
		ID           string `db:"id"`
		PasswordHash string `db:"password_hash"`
		IsActive     bool   `db:"is_active"`
	}
	if err := r.db.QueryRowxContext(ctx, query, normaliseEmail(email)).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, fmt.Errorf("get credentials: %w", classify(err))
	}
	return Credentials{UserID: row.ID, PasswordHash: row.PasswordHash, IsActive: row.IsActive}, nil
}

// UpdatePassword replaces an account's hash.
func (r *UserRepository) UpdatePassword(ctx context.Context, userID, passwordHash string) error {
	const query = `UPDATE users SET password_hash = $2 WHERE id = $1`

	if !strings.HasPrefix(passwordHash, "$argon2id$") {
		return errors.New("user: password hash is not an argon2id hash")
	}

	result, err := r.db.ExecContext(ctx, query, userID, passwordHash)
	if err != nil {
		return fmt.Errorf("update password for user %s: %w", userID, classify(err))
	}
	return requireOneRow(result, userID)
}

// SetActive enables or disables an account.
//
// Deactivation rather than deletion, because a run, its transcript and its spend are
// the record of money that was actually spent and they reference this row.
func (r *UserRepository) SetActive(ctx context.Context, userID string, active bool) error {
	const query = `UPDATE users SET is_active = $2 WHERE id = $1`

	result, err := r.db.ExecContext(ctx, query, userID, active)
	if err != nil {
		return fmt.Errorf("set user %s active=%t: %w", userID, active, classify(err))
	}
	return requireOneRow(result, userID)
}

// Count returns how many accounts exist.
//
// It answers one question: whether this is a fresh instance. `core createuser` uses
// it to say "there is already an account here" instead of quietly making a second
// one, which on a box reachable from the internet is the difference between a setup
// step and a back door.
func (r *UserRepository) Count(ctx context.Context) (int, error) {
	const query = `SELECT count(*) FROM users`

	var total int
	if err := r.db.QueryRowContext(ctx, query).Scan(&total); err != nil {
		return 0, fmt.Errorf("count users: %w", classify(err))
	}
	return total, nil
}

// List returns accounts, oldest first. Operator-only at the service layer: the list
// of who has an account on this instance is not a user's business.
func (r *UserRepository) List(ctx context.Context, limit, offset int) ([]User, int, error) {
	const countQuery = `SELECT count(*) FROM users`
	const listQuery = `
		SELECT ` + userColumns + `
		FROM users
		ORDER BY created_at ASC, id ASC
		LIMIT $1 OFFSET $2`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []userRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list users: %w", classify(err))
	}

	users := make([]User, 0, len(rows))
	for _, row := range rows {
		users = append(users, row.toUser())
	}
	return users, total, nil
}

// normaliseEmail lower-cases and trims an address so lookup and insertion agree. The
// schema's CHECK enforces the same rule, which is what catches a query path that
// forgets to come through here.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
