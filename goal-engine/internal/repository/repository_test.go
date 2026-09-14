package repository

import (
	"errors"
	"math"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
)

// newTestDB returns an sqlx pool backed by a mock driver. Expectations are
// verified on cleanup, so a query the code did not make fails the test.
func newTestDB(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet database expectations: %v", err)
		}
		_ = db.Close()
	})
	return sqlx.NewDb(db, "postgres"), mock
}

func TestClassifyMapsDriverErrorsOntoSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"nil stays nil", nil, nil},
		{"unique violation is a conflict", &pq.Error{Code: pgUniqueViolation}, ErrConflict},
		{"malformed uuid cannot match a row", &pq.Error{Code: pgInvalidTextRepr}, ErrNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); !errors.Is(got, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestClassifyPassesThroughUnrelatedErrors(t *testing.T) {
	original := errors.New("connection reset")

	if got := classify(original); !errors.Is(got, original) {
		t.Fatalf("expected the original error back, got %v", got)
	}
	// A check violation is a caller problem, not a missing row: it must not be
	// flattened into ErrNotFound, or a bad request would answer 404.
	checkErr := &pq.Error{Code: pgCheckViolation}
	if got := classify(checkErr); errors.Is(got, ErrNotFound) || errors.Is(got, ErrConflict) {
		t.Fatalf("expected a check violation to pass through, got %v", got)
	}
}

func TestIsConstraintViolationSeparatesCallerErrorsFromOutages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"check violation", &pq.Error{Code: pgCheckViolation}, true},
		{"foreign key violation", &pq.Error{Code: pgForeignKeyViolation}, true},
		{"not null violation", &pq.Error{Code: pgNotNullViolation}, true},
		{"numeric out of range", &pq.Error{Code: pgNumericValueOutRange}, true},
		{"unique violation is a conflict, not a bad request", &pq.Error{Code: pgUniqueViolation}, false},
		{"plain error", errors.New("timeout"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConstraintViolation(tc.err); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestNormalisePageClampsCallerInput(t *testing.T) {
	cases := []struct {
		name                  string
		limit, offset         int
		wantLimit, wantOffset int
	}{
		{"defaults", 0, 0, defaultPageLimit, 0},
		{"negative limit falls back", -5, 0, defaultPageLimit, 0},
		{"oversized limit is capped", 100000, 0, maxPageLimit, 0},
		{"negative offset floors at zero", 10, -3, 10, 0},
		{"valid values pass through", 25, 50, 25, 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset := normalisePage(tc.limit, tc.offset)
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("expected (%d, %d), got (%d, %d)", tc.wantLimit, tc.wantOffset, limit, offset)
			}
		})
	}
}

func TestJSONOrEmptyDefaultsToAnObject(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t"} {
		if got := jsonOrEmpty(raw); got != "{}" {
			t.Fatalf("expected %q to become {}, got %q", raw, got)
		}
	}
	if got := jsonOrEmpty(`{"goalId":"abc"}`); got != `{"goalId":"abc"}` {
		t.Fatalf("expected the payload to pass through, got %q", got)
	}
}

func TestNullIfEmptyKeepsUnsetDistinctFromBlank(t *testing.T) {
	if got := nullIfEmpty(""); got != nil {
		t.Fatalf("expected nil for an empty string, got %v", got)
	}
	if got := nullIfEmpty("task-1"); got != "task-1" {
		t.Fatalf("expected the value back, got %v", got)
	}
}

func TestRequireFiniteRejectsBrokenNumbers(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := requireFinite("metric x", value); err == nil {
			t.Fatalf("expected %v to be rejected", value)
		}
	}
	if err := requireFinite("metric x", 0); err != nil {
		t.Fatalf("expected zero to be accepted, got %v", err)
	}
}

func TestRequireFiniteNamesTheOffendingValue(t *testing.T) {
	err := requireFinite("metric billing.mrr.idr", math.NaN())

	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "billing.mrr.idr") {
		t.Fatalf("expected the label in the error, got %v", err)
	}
}

func TestFiniteOrZeroFlattensBrokenAuditNumbers(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := finiteOrZero(value); got != 0 {
			t.Fatalf("expected %v to flatten to 0, got %v", value, got)
		}
	}
	if got := finiteOrZero(1.5); got != 1.5 {
		t.Fatalf("expected 1.5 to pass through, got %v", got)
	}
}

func TestRequireOneRowReportsAMissingRow(t *testing.T) {
	if err := requireOneRow(sqlmock.NewResult(0, 0), "goal-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := requireOneRow(sqlmock.NewResult(0, 1), "goal-1"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestRequireOneRowSurfacesADriverFailure(t *testing.T) {
	result := sqlmock.NewErrorResult(errors.New("driver lost the count"))

	err := requireOneRow(result, "goal-1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the driver failure to surface, got %v", err)
	}
	if !contains(err.Error(), "goal-1") {
		t.Fatalf("expected the id in the error, got %v", err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
