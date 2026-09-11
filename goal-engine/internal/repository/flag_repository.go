package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// KillSwitchKey is the flag that halts all autonomous action. It lives in the
// database rather than an environment variable so it can be flipped without a
// redeploy — during an incident, "redeploy to stop the agents" is not a control.
const KillSwitchKey = "kill_switch"

// Flag is an operational switch.
type Flag struct {
	Key       string
	Enabled   bool
	Reason    string
	UpdatedBy string
	UpdatedAt time.Time
}

// FlagRepository reads and writes operational flags.
type FlagRepository struct {
	db *sqlx.DB
}

// NewFlagRepository builds a repository over the given pool.
func NewFlagRepository(db *sqlx.DB) *FlagRepository {
	return &FlagRepository{db: db}
}

const flagColumns = `key, enabled, reason, updated_by, updated_at`

type flagRow struct {
	Key       string    `db:"key"`
	Enabled   bool      `db:"enabled"`
	Reason    string    `db:"reason"`
	UpdatedBy string    `db:"updated_by"`
	UpdatedAt time.Time `db:"updated_at"`
}

func (r flagRow) toFlag() Flag {
	return Flag{
		Key:       r.Key,
		Enabled:   r.Enabled,
		Reason:    r.Reason,
		UpdatedBy: r.UpdatedBy,
		UpdatedAt: r.UpdatedAt,
	}
}

// Get returns one flag, or ErrNotFound.
func (r *FlagRepository) Get(ctx context.Context, key string) (Flag, error) {
	const query = `SELECT ` + flagColumns + ` FROM system_flags WHERE key = $1`

	var row flagRow
	if err := r.db.QueryRowxContext(ctx, query, key).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Flag{}, ErrNotFound
		}
		return Flag{}, fmt.Errorf("get flag %s: %w", key, classify(err))
	}
	return row.toFlag(), nil
}

// Set upserts a flag and returns it as stored.
func (r *FlagRepository) Set(ctx context.Context, key string, enabled bool, reason, updatedBy string) (Flag, error) {
	const query = `
		INSERT INTO system_flags (key, enabled, reason, updated_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE
		SET enabled = excluded.enabled,
			reason = excluded.reason,
			updated_by = excluded.updated_by
		RETURNING ` + flagColumns

	if updatedBy == "" {
		updatedBy = "system"
	}

	var row flagRow
	if err := r.db.QueryRowxContext(ctx, query, key, enabled, reason, updatedBy).StructScan(&row); err != nil {
		return Flag{}, fmt.Errorf("set flag %s: %w", key, classify(err))
	}
	return row.toFlag(), nil
}

// List returns every flag, alphabetically.
func (r *FlagRepository) List(ctx context.Context) ([]Flag, error) {
	const query = `SELECT ` + flagColumns + ` FROM system_flags ORDER BY key ASC`

	rows := []flagRow{}
	if err := r.db.SelectContext(ctx, &rows, query); err != nil {
		return nil, fmt.Errorf("list flags: %w", classify(err))
	}

	flags := make([]Flag, 0, len(rows))
	for _, row := range rows {
		flags = append(flags, row.toFlag())
	}
	return flags, nil
}

// KillSwitchEngaged reports whether autonomous action is currently halted.
//
// A read failure is returned as an error rather than defaulting to "not
// engaged". Callers are expected to treat an unreadable kill switch as a reason
// to do nothing this tick: skipping a check costs nothing, while assuming the
// switch is off because the database blinked would let agents act during exactly
// the kind of incident the switch exists for. A missing row is the same case,
// since a deleted row must not silently re-enable autonomy.
func (r *FlagRepository) KillSwitchEngaged(ctx context.Context) (bool, error) {
	flag, err := r.Get(ctx, KillSwitchKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, fmt.Errorf("kill switch flag %q is missing: refusing to assume it is off", KillSwitchKey)
		}
		return false, err
	}
	return flag.Enabled, nil
}
