package repository

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

// newMock returns a pool backed by go-sqlmock and registers the expectation check.
//
// The mock matches queries as regular expressions, which is what lets a test assert the
// shape of a query rather than only its effect. That matters most for the rule this
// package exists to keep: several tests below match on the WHERE clause naming a user
// id, so dropping the scoping fails a test instead of silently widening a read.
func newMock(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("a query the test expected was never made: %v", err)
		}
		_ = db.Close()
	})
	return sqlx.NewDb(db, "postgres"), mock
}

// pgError builds a driver error carrying a PostgreSQL code, the way lib/pq reports one.
//
// The parameter is a pqerror.Code rather than a string because pq.ErrorCode, the alias
// that used to spell this, is deprecated in lib/pq v1.12 and marked //go:fix inline. The
// pg* constants are untyped, so every call site is unchanged.
func pgError(code pqerror.Code) error {
	return &pq.Error{Code: code, Message: "test"}
}

// fixedNow is an arbitrary instant. No repository reads a clock — every timestamp is a
// parameter — so the value only has to be stable enough to assert on.
var fixedNow = time.Date(2026, 9, 11, 14, 30, 0, 0, time.UTC)

func TestClassify_uniqueViolationIsAConflict(t *testing.T) {
	// Arrange
	err := pgError(pgUniqueViolation)

	// Act
	got := classify(err)

	// Assert
	// A retry under the same idempotency key lands here, and the caller has to be able
	// to tell it apart from an outage: one answers 200 with the original task, the
	// other answers 503.
	if !errors.Is(got, ErrConflict) {
		t.Errorf("classify(23505) = %v; want ErrConflict", got)
	}
}

func TestClassify_malformedIdentifierIsNotFound(t *testing.T) {
	// Arrange
	err := pgError(pgInvalidTextRepr)

	// Act
	got := classify(err)

	// Assert
	// A uuid that is not a uuid can never match a row, and answering "not found" tells
	// somebody probing for ids less than a syntax error does.
	if !errors.Is(got, ErrNotFound) {
		t.Errorf("classify(22P02) = %v; want ErrNotFound", got)
	}
}

func TestClassify_leavesAnythingElseAlone(t *testing.T) {
	// Arrange
	checkViolation := pgError(pgCheckViolation)
	plain := errors.New("connection reset")

	// Act & Assert
	// A CHECK constraint firing is the caller's problem, but it is not one of the two
	// sentinels, and folding it into either would lose which constraint refused.
	if got := classify(checkViolation); !errors.Is(got, checkViolation) {
		t.Errorf("classify(23514) = %v; want the original error", got)
	}
	if got := classify(plain); !errors.Is(got, plain) {
		t.Errorf("classify(non-driver error) = %v; want the original error", got)
	}
	if classify(nil) != nil {
		t.Error("classify(nil) returned an error")
	}
}

func TestIsConstraintViolation_separatesCallerFaultsFromOutages(t *testing.T) {
	// Arrange
	callerFaults := []pqerror.Code{
		pgCheckViolation, pgForeignKeyViolation, pgNotNullViolation,
		pgNumericValueOutRange, pgInvalidTextRepr,
	}

	// Act & Assert
	for _, code := range callerFaults {
		if !IsConstraintViolation(pgError(code)) {
			t.Errorf("code %s was not treated as a caller fault; a handler would answer 500 for a bad request", code)
		}
	}
	// A unique violation is a conflict, not a malformed request: it has its own
	// sentinel and its own status code.
	if IsConstraintViolation(pgError(pgUniqueViolation)) {
		t.Error("a unique violation was treated as a malformed request")
	}
	if IsConstraintViolation(errors.New("connection reset")) {
		t.Error("an infrastructure failure was treated as a caller fault; the caller would be told to fix their request")
	}
}

func TestNormalisePage_boundsEveryCallerSuppliedRange(t *testing.T) {
	// Arrange
	cases := []struct {
		name                  string
		limit, offset         int
		wantLimit, wantOffset int
	}{
		{"unset limit takes the default", 0, 0, defaultPageLimit, 0},
		{"negative limit takes the default", -5, 0, defaultPageLimit, 0},
		{"oversized limit is capped", 10_000, 0, maxPageLimit, 0},
		{"negative offset starts at the beginning", 10, -3, 10, 0},
		{"a sane page is left alone", 25, 50, 25, 50},
		{"the cap itself is allowed", maxPageLimit, 0, maxPageLimit, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			limit, offset := normalisePage(tc.limit, tc.offset)

			// Assert
			// An unbounded list over a chat history is how a UI takes down a database,
			// so there is no input that yields an unlimited page.
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Errorf("normalisePage(%d, %d) = (%d, %d); want (%d, %d)",
					tc.limit, tc.offset, limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestRequireOneRow_noRowsIsNotFound(t *testing.T) {
	// Arrange
	nothingChanged := sqlmock.NewResult(0, 0)
	oneChanged := sqlmock.NewResult(0, 1)

	// Act & Assert
	// Every update in this package is scoped by owner as well as by id, so "no rows"
	// covers no such row and not yours at once. They answer the same on purpose.
	if err := requireOneRow(nothingChanged, "id-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("requireOneRow(0 rows) = %v; want ErrNotFound", err)
	}
	if err := requireOneRow(oneChanged, "id-1"); err != nil {
		t.Errorf("requireOneRow(1 row) = %v; want nil", err)
	}
}

func TestRequireOneRow_unreadableResultIsAnError(t *testing.T) {
	// Arrange
	broken := sqlmock.NewErrorResult(errors.New("driver lost the count"))

	// Act
	err := requireOneRow(broken, "id-1")

	// Assert
	// A driver that cannot say how many rows changed has not told us the update
	// happened, and reporting success on that would be reporting a guess.
	if err == nil {
		t.Fatal("an unreadable row count was reported as a successful update")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("an unreadable row count was reported as a missing row")
	}
}

func TestNullIfEmpty_keepsNeverSetDistinctFromBlank(t *testing.T) {
	// Act & Assert
	if got := nullIfEmpty(""); got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v; want nil", got)
	}
	if got := nullIfEmpty("run_01"); got != "run_01" {
		t.Errorf("nullIfEmpty(%q) = %v; want the value", "run_01", got)
	}
}

func TestTimeOrNull_zeroTimeBecomesNull(t *testing.T) {
	// Act & Assert
	// A zero timestamptz reads as the year 1, and a finished_at of 0001-01-01 is worse
	// than an honest NULL: it makes a run look finished before it started.
	if got := timeOrNull(time.Time{}); got != nil {
		t.Errorf("timeOrNull(zero) = %v; want nil", got)
	}
	if got := timeOrNull(fixedNow); got != fixedNow {
		t.Errorf("timeOrNull(%s) = %v; want the value", fixedNow, got)
	}
}

func TestTimePtr_copiesRatherThanAliasingTheRow(t *testing.T) {
	// Arrange
	row := sql.NullTime{Time: fixedNow, Valid: true}

	// Act
	got := timePtr(row)
	// Simulate the next scan reusing the same struct.
	row.Time = fixedNow.Add(72 * time.Hour)

	// Assert
	if got == nil {
		t.Fatal("a valid timestamp came back nil")
	}
	// Returning &row.Field would hand every caller a pointer into a struct the next
	// row overwrites, so a list of sessions would end up all showing the last one's
	// revocation time.
	if !got.Equal(fixedNow) {
		t.Errorf("the returned time changed with the row: got %s, want %s", got, fixedNow)
	}
	if timePtr(sql.NullTime{}) != nil {
		t.Error("a NULL timestamp did not come back nil")
	}
}

func TestMarshalMetadata_nilMapBecomesAnEmptyObject(t *testing.T) {
	// Act
	empty, err := marshalMetadata(nil)
	if err != nil {
		t.Fatalf("marshalMetadata(nil) failed: %v", err)
	}
	filled, err := marshalMetadata(map[string]string{"goalId": "goal_7"})
	if err != nil {
		t.Fatalf("marshalMetadata failed: %v", err)
	}

	// Assert
	// SQL NULL would make every reader handle two shapes of "no metadata".
	if empty != "{}" {
		t.Errorf("marshalMetadata(nil) = %q; want an empty object", empty)
	}
	if filled != `{"goalId":"goal_7"}` {
		t.Errorf("marshalMetadata = %q; want the encoded map", filled)
	}
}

func TestUnmarshalMetadata_undecodableDocumentIsAnError(t *testing.T) {
	// Act
	blank, err := unmarshalMetadata("   ")
	if err != nil {
		t.Fatalf("unmarshalMetadata(blank) failed: %v", err)
	}
	filled, err := unmarshalMetadata(`{"goalId":"goal_7"}`)
	if err != nil {
		t.Fatalf("unmarshalMetadata failed: %v", err)
	}
	_, broken := unmarshalMetadata(`{"goalId":`)

	// Assert
	if len(blank) != 0 {
		t.Errorf("unmarshalMetadata(blank) = %v; want an empty map", blank)
	}
	if filled["goalId"] != "goal_7" {
		t.Errorf("unmarshalMetadata = %v; want goalId decoded", filled)
	}
	// The metadata is how an unattended task is correlated with the goal that caused
	// it. Losing that silently makes the run untraceable.
	if broken == nil {
		t.Error("an undecodable metadata document was returned as an empty map")
	}
}
