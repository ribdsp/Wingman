// Package repository is the only place that talks SQL.
//
// Repositories return domain types, not rows: the decision core never sees a
// database concern, and callers never see a driver error. Every query is
// parameterised.
package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/lib/pq"
)

// Sentinel errors callers can match on.
var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("repository: record not found")
	// ErrConflict means a unique constraint rejected the write. For dispatches
	// this is the expected, healthy outcome of a retry.
	ErrConflict = errors.New("repository: conflicting record")
)

// PostgreSQL error codes used for mapping driver errors onto sentinels.
const (
	pgUniqueViolation      = "23505"
	pgCheckViolation       = "23514"
	pgInvalidTextRepr      = "22P02"
	pgForeignKeyViolation  = "23503"
	pgNotNullViolation     = "23502"
	pgNumericValueOutRange = "22003"
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
		// A malformed uuid can never match a row.
		return ErrNotFound
	default:
		return err
	}
}

// IsConstraintViolation reports whether the error is a data-integrity rejection
// rather than an infrastructure failure. Handlers use it to answer 400 instead
// of 500: a CHECK constraint firing means the caller sent something the schema
// refuses, which is the caller's problem to fix.
func IsConstraintViolation(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	switch string(pqErr.Code) {
	case pgCheckViolation, pgForeignKeyViolation, pgNotNullViolation, pgNumericValueOutRange, pgInvalidTextRepr:
		return true
	default:
		return false
	}
}

// Page bounds. A caller asking for everything still gets a bounded result: an
// unbounded list query is how a monitoring endpoint takes down a database.
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

// requireOneRow turns "no rows changed" into ErrNotFound.
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

// jsonOrEmpty defaults a JSON column to an empty object so the jsonb cast never
// fails on an unset field.
func jsonOrEmpty(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

// nullIfEmpty stores an empty string as NULL, keeping "never set" distinct from
// "explicitly blank" in nullable text columns.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// requireFinite rejects NaN and infinities before they reach the database. The
// schema also refuses them, but failing here names the value and the caller.
func requireFinite(label string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s: %v is not a finite number", label, value)
	}
	return nil
}

// finiteOrZero replaces a non-finite value with zero. It is for audit columns
// only, where losing the record would be worse than storing a flattened number;
// never use it for a value a decision will be made from.
func finiteOrZero(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}
