package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// Sample is one observation of a metric.
type Sample struct {
	MetricKey  string
	Value      float64
	ObservedAt time.Time
	Source     SourceType
	// Note carries human-readable provenance, e.g. which datasource answered.
	Note string
}

// Sampler reads the current value of a metric.
type Sampler interface {
	Sample(ctx context.Context, def Definition) (Sample, error)
}

// ErrNotPullable is returned when a push metric is handed to a puller.
var ErrNotPullable = errors.New("metric is push-based and cannot be pulled")

// Querier is the subset of database access the SQL sampler needs. Keeping it
// narrow lets tests substitute a fake without a database.
type Querier interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// SQLSampler reads scalar metrics from configured databases.
type SQLSampler struct {
	pools map[string]Querier
	// statementTimeout bounds one query inside the database, independent of the
	// caller's context, so a stuck query cannot hold a connection open.
	statementTimeout time.Duration
	clock            func() time.Time
}

// NewSQLSampler builds a sampler over the given per-datasource pools.
func NewSQLSampler(pools map[string]Querier, statementTimeout time.Duration) *SQLSampler {
	if statementTimeout <= 0 {
		statementTimeout = 15 * time.Second
	}
	return &SQLSampler{
		pools:            pools,
		statementTimeout: statementTimeout,
		clock:            time.Now,
	}
}

// Sample runs the metric's query in a read-only transaction and returns the
// single scalar it produced.
func (s *SQLSampler) Sample(ctx context.Context, def Definition) (Sample, error) {
	if def.Source != SourceSQL {
		return Sample{}, fmt.Errorf("metric %s: %w", def.Key, ErrNotPullable)
	}
	pool, ok := s.pools[def.Datasource]
	if !ok {
		return Sample{}, fmt.Errorf("metric %s: datasource %q has no connection pool", def.Key, def.Datasource)
	}
	// Re-check the query even though the registry validated it at load: this is
	// the last point before it reaches the database.
	if err := ValidateQuery(def.Query); err != nil {
		return Sample{}, fmt.Errorf("metric %s: %w", def.Key, err)
	}

	// ReadOnly is the real guarantee here; the string guard is only a first pass.
	tx, err := pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Sample{}, fmt.Errorf("metric %s: begin read-only transaction: %w", def.Key, err)
	}
	defer func() {
		// Nothing was written, so a rollback is always the correct exit.
		_ = tx.Rollback()
	}()

	// statement_timeout cannot be parameterised; the value is an integer derived
	// from configuration, never from user input.
	timeoutMS := int64(s.statementTimeout / time.Millisecond)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeoutMS)); err != nil {
		return Sample{}, fmt.Errorf("metric %s: set statement timeout: %w", def.Key, err)
	}

	var value sql.NullFloat64
	if err := tx.QueryRowContext(ctx, def.Query).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Sample{}, fmt.Errorf("metric %s: query returned no rows, it must always return exactly one", def.Key)
		}
		return Sample{}, fmt.Errorf("metric %s: query failed: %w", def.Key, err)
	}
	if !value.Valid {
		return Sample{}, fmt.Errorf("metric %s: query returned NULL, wrap the aggregate in coalesce()", def.Key)
	}
	if math.IsNaN(value.Float64) || math.IsInf(value.Float64, 0) {
		return Sample{}, fmt.Errorf("metric %s: query returned a non-finite value (%v)", def.Key, value.Float64)
	}

	return Sample{
		MetricKey:  def.Key,
		Value:      value.Float64,
		ObservedAt: s.clock().UTC(),
		Source:     SourceSQL,
		Note:       "datasource=" + def.Datasource,
	}, nil
}
