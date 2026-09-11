package metrics

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const sampleQuery = "SELECT coalesce(sum(amount), 0)::double precision FROM invoices"

func sqlDef() Definition {
	return Definition{
		Key:        "billing.mrr.idr",
		Source:     SourceSQL,
		Datasource: "billing",
		Query:      sampleQuery,
	}
}

// newMockPool returns a sampler wired to a mock database.
func newMockPool(t *testing.T) (*SQLSampler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet database expectations: %v", err)
		}
		_ = db.Close()
	})
	return NewSQLSampler(map[string]Querier{"billing": db}, 15*time.Second), mock
}

func TestSQLSamplerReadsScalarInReadOnlyTransaction(t *testing.T) {
	// Arrange
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(sampleQuery).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(12500000.0))
	mock.ExpectRollback()

	// Act
	sample, err := sampler.Sample(context.Background(), sqlDef())

	// Assert
	if err != nil {
		t.Fatalf("expected a sample, got error: %v", err)
	}
	if sample.Value != 12500000.0 {
		t.Fatalf("expected 12500000, got %v", sample.Value)
	}
	if sample.MetricKey != "billing.mrr.idr" || sample.Source != SourceSQL {
		t.Fatalf("unexpected sample metadata: %+v", sample)
	}
	if sample.Note != "datasource=billing" {
		t.Fatalf("expected provenance in the note, got %q", sample.Note)
	}
	if sample.ObservedAt.IsZero() {
		t.Fatal("expected ObservedAt to be set")
	}
}

func TestSQLSamplerAcceptsNumericTextFromPostgres(t *testing.T) {
	// lib/pq hands numeric columns back as bytes; database/sql converts them.
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(sampleQuery).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte("4210.75")))
	mock.ExpectRollback()

	sample, err := sampler.Sample(context.Background(), sqlDef())
	if err != nil {
		t.Fatalf("expected a sample, got error: %v", err)
	}
	if sample.Value != 4210.75 {
		t.Fatalf("expected 4210.75, got %v", sample.Value)
	}
}

func TestSQLSamplerRejectsNullResult(t *testing.T) {
	// A bare sum() over no rows returns NULL. Treating that as zero would let a
	// broken query look like a collapsed metric and wake an agent.
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(sampleQuery).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(nil))
	mock.ExpectRollback()

	_, err := sampler.Sample(context.Background(), sqlDef())
	if err == nil {
		t.Fatal("expected NULL to be rejected")
	}
	if !strings.Contains(err.Error(), "coalesce") {
		t.Fatalf("expected the error to suggest coalesce(), got: %v", err)
	}
}

func TestSQLSamplerRejectsEmptyResult(t *testing.T) {
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(sampleQuery).WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectRollback()

	_, err := sampler.Sample(context.Background(), sqlDef())
	if err == nil {
		t.Fatal("expected an empty result set to be rejected")
	}
	if !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("expected the error to mention no rows, got: %v", err)
	}
}

func TestSQLSamplerRejectsNonFiniteResult(t *testing.T) {
	// PostgreSQL double precision can legitimately hold NaN. An unguarded NaN
	// compares false against every threshold, which reads as "off track".
	for _, raw := range []string{"NaN", "Infinity", "-Infinity"} {
		t.Run(raw, func(t *testing.T) {
			sampler, mock := newMockPool(t)
			mock.ExpectBegin()
			mock.ExpectExec("SET LOCAL statement_timeout = 15000").WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(sampleQuery).
				WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(raw)))
			mock.ExpectRollback()

			if _, err := sampler.Sample(context.Background(), sqlDef()); err == nil {
				t.Fatalf("expected %s to be rejected", raw)
			}
		})
	}
}

func TestSQLSamplerPropagatesQueryFailure(t *testing.T) {
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(sampleQuery).WillReturnError(errors.New("relation \"invoices\" does not exist"))
	mock.ExpectRollback()

	_, err := sampler.Sample(context.Background(), sqlDef())
	if err == nil {
		t.Fatal("expected the query failure to surface")
	}
	if !strings.Contains(err.Error(), "billing.mrr.idr") {
		t.Fatalf("expected the error to name the metric, got: %v", err)
	}
}

func TestSQLSamplerPropagatesBeginFailure(t *testing.T) {
	sampler, mock := newMockPool(t)
	mock.ExpectBegin().WillReturnError(errors.New("too many connections"))

	if _, err := sampler.Sample(context.Background(), sqlDef()); err == nil {
		t.Fatal("expected the begin failure to surface")
	}
}

func TestSQLSamplerPropagatesStatementTimeoutFailure(t *testing.T) {
	sampler, mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout = 15000").
		WillReturnError(errors.New("permission denied"))
	mock.ExpectRollback()

	if _, err := sampler.Sample(context.Background(), sqlDef()); err == nil {
		t.Fatal("expected the timeout setup failure to surface")
	}
}

func TestSQLSamplerRejectsUnknownDatasource(t *testing.T) {
	sampler := NewSQLSampler(map[string]Querier{}, time.Second)

	_, err := sampler.Sample(context.Background(), sqlDef())
	if err == nil {
		t.Fatal("expected an unknown datasource to be rejected")
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Fatalf("expected the error to name the datasource, got: %v", err)
	}
}

func TestSQLSamplerRejectsNonSQLMetric(t *testing.T) {
	sampler := NewSQLSampler(map[string]Querier{}, time.Second)

	_, err := sampler.Sample(context.Background(), Definition{Key: "x", Source: SourcePush})
	if !errors.Is(err, ErrNotPullable) {
		t.Fatalf("expected ErrNotPullable, got: %v", err)
	}
}

func TestSQLSamplerRevalidatesQueryBeforeExecuting(t *testing.T) {
	// The registry validates at load time. This is the last gate before the
	// query reaches a database, and it must hold even if a definition was built
	// in code rather than loaded from config.
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer func() { _ = db.Close() }()

	sampler := NewSQLSampler(map[string]Querier{"billing": db}, time.Second)
	def := sqlDef()
	def.Query = "SELECT 1; DROP TABLE invoices"

	if _, err := sampler.Sample(context.Background(), def); err == nil {
		t.Fatal("expected the stacked statement to be refused before execution")
	}
}

func TestNewSQLSamplerFallsBackToADefaultTimeout(t *testing.T) {
	sampler := NewSQLSampler(nil, 0)

	if sampler.statementTimeout <= 0 {
		t.Fatalf("expected a positive default timeout, got %s", sampler.statementTimeout)
	}
}

// Compile-time assertion that the standard pool satisfies the narrow interface
// the sampler depends on.
var _ Querier = (*sql.DB)(nil)
