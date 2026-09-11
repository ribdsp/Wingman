package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// SampleInput is one metric observation to record.
type SampleInput struct {
	MetricKey  string
	Value      float64
	ObservedAt time.Time
	// Source records how the value arrived: a metric source type, or "manual".
	Source string
	// Duration is how long the read took. Zero is stored as NULL.
	Duration time.Duration
}

// SampleRecord is a stored observation.
type SampleRecord struct {
	ID         int64
	MetricKey  string
	Value      float64
	ObservedAt time.Time
	Source     string
	DurationMS *int
}

// SampleRepository stores the metric time series. Keeping every observation is
// what lets a human answer "what did the monitor actually see" months later.
type SampleRepository struct {
	db *sqlx.DB
}

// NewSampleRepository builds a repository over the given pool.
func NewSampleRepository(db *sqlx.DB) *SampleRepository {
	return &SampleRepository{db: db}
}

const sampleColumns = `id, metric_key, value, observed_at, source, duration_ms`

type sampleRow struct {
	ID         int64         `db:"id"`
	MetricKey  string        `db:"metric_key"`
	Value      float64       `db:"value"`
	ObservedAt time.Time     `db:"observed_at"`
	Source     string        `db:"source"`
	DurationMS sql.NullInt64 `db:"duration_ms"`
}

func (r sampleRow) toRecord() SampleRecord {
	var duration *int
	if r.DurationMS.Valid {
		ms := int(r.DurationMS.Int64)
		duration = &ms
	}
	return SampleRecord{
		ID:         r.ID,
		MetricKey:  r.MetricKey,
		Value:      r.Value,
		ObservedAt: r.ObservedAt,
		Source:     r.Source,
		DurationMS: duration,
	}
}

// Insert records an observation.
func (r *SampleRepository) Insert(ctx context.Context, input SampleInput) (SampleRecord, error) {
	const query = `
		INSERT INTO metric_samples (metric_key, value, observed_at, source, duration_ms)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + sampleColumns

	if err := requireFinite("metric "+input.MetricKey, input.Value); err != nil {
		return SampleRecord{}, err
	}

	observedAt := input.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	source := input.Source
	if source == "" {
		source = "monitor"
	}
	var durationMS any
	if input.Duration > 0 {
		durationMS = int(input.Duration.Milliseconds())
	}

	var row sampleRow
	err := r.db.QueryRowxContext(ctx, query,
		input.MetricKey, input.Value, observedAt, source, durationMS,
	).StructScan(&row)
	if err != nil {
		return SampleRecord{}, fmt.Errorf("insert sample for %s: %w", input.MetricKey, classify(err))
	}
	return row.toRecord(), nil
}

// Latest returns the most recent observation of a metric, or ErrNotFound.
func (r *SampleRepository) Latest(ctx context.Context, metricKey string) (SampleRecord, error) {
	const query = `
		SELECT ` + sampleColumns + `
		FROM metric_samples
		WHERE metric_key = $1
		ORDER BY observed_at DESC, id DESC
		LIMIT 1`

	var row sampleRow
	if err := r.db.QueryRowxContext(ctx, query, metricKey).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SampleRecord{}, ErrNotFound
		}
		return SampleRecord{}, fmt.Errorf("latest sample for %s: %w", metricKey, classify(err))
	}
	return row.toRecord(), nil
}

// ListSince returns observations of a metric from newest to oldest.
func (r *SampleRepository) ListSince(ctx context.Context, metricKey string, since time.Time, limit int) ([]SampleRecord, error) {
	const query = `
		SELECT ` + sampleColumns + `
		FROM metric_samples
		WHERE metric_key = $1 AND observed_at >= $2
		ORDER BY observed_at DESC, id DESC
		LIMIT $3`

	limit, _ = normalisePage(limit, 0)
	rows := []sampleRow{}
	if err := r.db.SelectContext(ctx, &rows, query, metricKey, since, limit); err != nil {
		return nil, fmt.Errorf("list samples for %s: %w", metricKey, classify(err))
	}

	records := make([]SampleRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, nil
}
