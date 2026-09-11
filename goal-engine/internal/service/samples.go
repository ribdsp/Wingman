package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

const (
	// maxSampleSkew bounds how far ahead of the engine's clock a reported
	// observation may be. A sample stamped next week would look fresh forever and
	// would make every later one arrive "out of order", so a caller with a wrong
	// clock is told rather than silently trusted.
	maxSampleSkew = 5 * time.Minute

	// maxSampleBackdate bounds how far back a reported observation may be stamped.
	// Backfilling history through this endpoint would rewrite what the engine
	// believes it saw at the time it made a decision.
	maxSampleBackdate = 7 * 24 * time.Hour
)

// SampleRequest is one reported observation of a push metric.
type SampleRequest struct {
	MetricKey string
	Value     float64
	// ObservedAt is when the value was true, not when it was reported. Zero means
	// now.
	ObservedAt time.Time
	// Note is free text for whoever reads the audit entry later.
	Note string
}

// SamplesDeps is everything the samples service needs.
type SamplesDeps struct {
	Samples SampleStore
	Reader  SampleReader
	Metrics MetricLookup
	Audit   AuditSink

	// Clock defaults to time.Now.
	Clock  Clock
	Logger zerolog.Logger
}

// Samples is the write path for metrics nobody can pull.
//
// Most metrics are read by the engine from a database or an HTTP endpoint the
// operator declared. Some numbers only exist where the engine cannot reach —
// a figure from a payment provider's dashboard, a count somebody tallies by hand
// — and those are declared as push metrics and reported here.
//
// The trade-off is worth stating plainly, because it is the operator's to make: a
// push metric is self-reported, so a goal measured on one is exactly as
// trustworthy as the credential feeding it. An agent holding a bot key can report
// the number it is judged by. Two things bound that rather than prevent it — the
// operator decides which metrics are push at all, in a file no API can write, and
// every reported value is in the audit log under the credential that sent it.
// Feed push metrics from their own credential, not the one an agent uses to think.
type Samples struct {
	samples SampleStore
	reader  SampleReader
	metrics MetricLookup
	audit   AuditSink
	clock   Clock
	log     zerolog.Logger
}

// NewSamples validates its wiring and returns a ready service.
func NewSamples(deps SamplesDeps) (*Samples, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Samples != nil, "Samples")
	require(deps.Reader != nil, "Reader")
	require(deps.Metrics != nil, "Metrics")
	require(deps.Audit != nil, "Audit")
	if len(missing) > 0 {
		return nil, fmt.Errorf("samples: missing dependencies: %v", missing)
	}

	s := &Samples{
		samples: deps.Samples,
		reader:  deps.Reader,
		metrics: deps.Metrics,
		audit:   deps.Audit,
		clock:   deps.Clock,
		log:     deps.Logger,
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s, nil
}

// Record stores a reported observation.
//
// The metric has to be one the operator declared, and it has to be declared as
// push. Accepting a value for a metric the operator said comes from the database
// would give the engine two disagreeing sources for the same number, and would let
// whoever holds a credential overwrite a figure that was deliberately put beyond
// their reach.
func (s *Samples) Record(ctx context.Context, req SampleRequest, actor Actor) (repository.SampleRecord, error) {
	if err := actor.validate(); err != nil {
		return repository.SampleRecord{}, err
	}

	key := strings.TrimSpace(req.MetricKey)
	if key == "" {
		return repository.SampleRecord{}, fieldError("metricKey", "is required")
	}
	def, ok := s.metrics.Get(key)
	if !ok {
		return repository.SampleRecord{}, fmt.Errorf("%w: metric %q is not declared", ErrNotFound, key)
	}
	if def.Source != metrics.SourcePush {
		return repository.SampleRecord{}, fieldError("metricKey",
			fmt.Sprintf("is declared as %s, which the engine reads for itself: only push metrics accept reported values", def.Source))
	}
	if math.IsNaN(req.Value) || math.IsInf(req.Value, 0) {
		return repository.SampleRecord{}, fieldError("value", "must be a finite number")
	}

	now := s.clock()
	observedAt := req.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	if observedAt.After(now.Add(maxSampleSkew)) {
		return repository.SampleRecord{}, fieldError("observedAt", "is in the future")
	}
	if observedAt.Before(now.Add(-maxSampleBackdate)) {
		return repository.SampleRecord{}, fieldError("observedAt",
			fmt.Sprintf("is more than %d days old: the engine records what it saw when it decided, and that is not rewritten after the fact",
				int(maxSampleBackdate.Hours()/24)))
	}

	record, err := s.samples.Insert(ctx, repository.SampleInput{
		MetricKey:  def.Key,
		Value:      req.Value,
		ObservedAt: observedAt,
		Source:     string(metrics.SourcePush),
	})
	if err != nil {
		return repository.SampleRecord{}, fmt.Errorf("samples: record %s: %w", def.Key, err)
	}

	// Who reported which number is the whole audit trail for a self-reported
	// metric, so this entry is not optional the way a convenience log line would be.
	s.appendAudit(ctx, repository.AuditEvent{
		ActorType:   actor.Type,
		ActorID:     actor.ID,
		Action:      ActionSampleRecorded,
		SubjectType: SubjectMetric,
		SubjectID:   def.Key,
		Outcome:     "recorded",
		RequestID:   actor.RequestID,
		Detail: detailJSON(map[string]any{
			"value":      req.Value,
			"observedAt": observedAt.Format(time.RFC3339),
			"note":       strings.TrimSpace(req.Note),
		}),
	})
	return record, nil
}

// Latest reads the most recent observation of a metric.
//
// It reads any metric, not only a push one: after adding a goal the first question
// is "what does the engine currently see", and answering it for a SQL-backed
// metric too saves an operator from having to guess.
func (s *Samples) Latest(ctx context.Context, metricKey string) (repository.SampleRecord, error) {
	key := strings.TrimSpace(metricKey)
	if key == "" {
		return repository.SampleRecord{}, fieldError("metricKey", "is required")
	}
	if _, ok := s.metrics.Get(key); !ok {
		return repository.SampleRecord{}, fmt.Errorf("%w: metric %q is not declared", ErrNotFound, key)
	}

	record, err := s.reader.Latest(ctx, key)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.SampleRecord{}, fmt.Errorf("%w: no observation of %q has been recorded", ErrNotFound, key)
		}
		return repository.SampleRecord{}, fmt.Errorf("samples: latest %s: %w", key, err)
	}
	return record, nil
}

// appendAudit records the report. A failed audit write never fails the caller —
// the sample is already stored — but it is logged loudly, because a self-reported
// number with no record of who reported it is the one case where the log matters
// most.
func (s *Samples) appendAudit(ctx context.Context, event repository.AuditEvent) {
	if _, err := s.audit.Append(ctx, event); err != nil {
		s.log.Error().Err(err).
			Str("action", event.Action).
			Str("subjectId", event.SubjectID).
			Msg("audit write failed")
	}
}
