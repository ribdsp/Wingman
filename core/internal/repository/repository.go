// Package repository is the only place in core that talks SQL.
//
// Repositories return domain types, not rows: the agent loop never sees a database
// concern, and a caller never sees a driver error. Every query is parameterised.
//
// One rule is specific to this service and runs through every file here. Core holds
// several people's conversations in one database, so **every query that touches a
// user's content carries that user's id in its WHERE clause**, and the id is an
// explicit parameter rather than something read from a context. A method that returns
// a row without having been told whose row it is would be one refactor away from
// returning somebody else's.
package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Sentinel errors callers can match on.
var (
	// ErrNotFound means no row matched — including the case where a row exists but
	// belongs to another account, which is the same answer on purpose.
	ErrNotFound = errors.New("repository: record not found")
	// ErrConflict means a unique constraint rejected the write. For a task carrying
	// an idempotency key this is the expected, healthy outcome of a retry.
	ErrConflict = errors.New("repository: conflicting record")
)

// PostgreSQL error codes used for mapping driver errors onto sentinels.
const (
	pgUniqueViolation      = "23505"
	pgCheckViolation       = "23514"
	pgForeignKeyViolation  = "23503"
	pgNotNullViolation     = "23502"
	pgNumericValueOutRange = "22003"
	// pgInvalidTextRepr covers both a malformed uuid and a value that is not a member
	// of one of the enums in migration 000001.
	pgInvalidTextRepr = "22P02"
)

// classify maps a driver error onto a sentinel where one applies, and otherwise
// returns the original error.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return err
	}
	switch string(pqErr.Code) {
	case pgUniqueViolation:
		return ErrConflict
	case pgInvalidTextRepr:
		// A malformed uuid can never match a row, so "not found" is both true and
		// less informative to somebody probing for ids than a syntax error.
		return ErrNotFound
	default:
		return err
	}
}

// IsConstraintViolation reports whether the error is a data-integrity rejection
// rather than an infrastructure failure. Handlers use it to answer 400 instead of
// 500: a CHECK constraint firing means the caller sent something the schema refuses,
// which is the caller's problem to fix.
func IsConstraintViolation(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	switch string(pqErr.Code) {
	case pgCheckViolation, pgForeignKeyViolation, pgNotNullViolation,
		pgNumericValueOutRange, pgInvalidTextRepr:
		return true
	default:
		return false
	}
}

// Page bounds. A caller asking for everything still gets a bounded result: an
// unbounded list query over a chat history is how a UI takes down a database.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// normalisePage clamps caller-supplied paging into a safe range.
func normalisePage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// requireOneRow turns "no row changed" into ErrNotFound.
//
// Every update in this package is scoped by owner as well as by id, so no rows
// affected covers two cases at once — no such row, and not yours. They deliberately
// answer the same, because distinguishing them tells a caller that an id they do not
// own exists.
func requireOneRow(result sql.Result, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect affected rows for %s: %w", id, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// nullIfEmpty stores an empty string as NULL, keeping "never set" distinct from
// "explicitly blank" in nullable text columns.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// timeOrNull stores the zero time as NULL. A zero timestamptz would read as the year
// 1, and a finished_at of 0001-01-01 is worse than an honest NULL.
func timeOrNull(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

// timePtr copies a nullable timestamp out of a row. The copy matters: returning
// &row.Field would hand callers a pointer into a struct the next scan reuses.
func timePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}

// marshalMetadata renders a task's metadata for a jsonb column. A nil map becomes an
// empty object rather than SQL NULL, so a reader never has to handle both.
func marshalMetadata(metadata map[string]string) (string, error) {
	if len(metadata) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	return string(encoded), nil
}

// unmarshalMetadata reads a jsonb column back.
//
// A stored document that will not decode is an error rather than an empty map: the
// metadata is how a task is correlated with the goal that caused it, and losing that
// silently makes an unattended run untraceable.
func unmarshalMetadata(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" || raw == "{}" {
		return map[string]string{}, nil
	}
	metadata := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	return metadata, nil
}
