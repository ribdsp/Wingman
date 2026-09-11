package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

const pushMetric = "ops.tickets_closed"

// samplesFixture wires the samples service over the same fakes the monitor uses,
// so a value reported in one test can be read back the way a tick would read it.
type samplesFixture struct {
	samples *Samples
	store   *fakeSamples
	metrics *fakeMetrics
	audit   *fakeAudit
}

func newSamplesFixture(t *testing.T) *samplesFixture {
	t.Helper()
	f := &samplesFixture{
		store:   &fakeSamples{},
		metrics: newFakeMetrics(testMetric),
		audit:   &fakeAudit{},
	}
	f.metrics.declare(pushMetric, metrics.SourcePush)

	svc, err := NewSamples(SamplesDeps{
		Samples: f.store,
		Reader:  f.store,
		Metrics: f.metrics,
		Audit:   f.audit,
		Clock:   fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a samples service, got %v", err)
	}
	f.samples = svc
	return f
}

func TestNewSamplesNamesEveryMissingDependency(t *testing.T) {
	_, err := NewSamples(SamplesDeps{})
	if err == nil {
		t.Fatal("expected an incomplete samples service to be refused")
	}
	for _, name := range []string{"Samples", "Reader", "Metrics", "Audit"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %s to be named in %v", name, err)
		}
	}
}

func TestAReportedValueIsStoredAsAPushObservation(t *testing.T) {
	// Arrange
	f := newSamplesFixture(t)

	// Act
	record, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric,
		Value:     42,
	}, testActor())

	// Assert
	if err != nil {
		t.Fatalf("expected the value to be accepted, got %v", err)
	}
	if record.Value != 42 || record.MetricKey != pushMetric {
		t.Fatalf("expected the reported value back, got %+v", record)
	}
	if len(f.store.inserted) != 1 {
		t.Fatalf("expected one stored observation, got %d", len(f.store.inserted))
	}
	stored := f.store.inserted[0]
	if stored.Source != string(metrics.SourcePush) {
		// How a number arrived is what tells a human later whether to trust it.
		t.Fatalf("expected the observation to be marked as pushed, got %q", stored.Source)
	}
	if !stored.ObservedAt.Equal(testNow) {
		t.Fatalf("expected an absent timestamp to default to now, got %v", stored.ObservedAt)
	}
}

func TestAReportedValueIsAuditedUnderTheCredentialThatSentIt(t *testing.T) {
	// A push metric is self-reported, so the only accountability is the record of
	// who reported what.
	f := newSamplesFixture(t)
	actor := Actor{Type: repository.ActorBot, ID: "bot-growth", RequestID: "req-7"}

	if _, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey:  pushMetric,
		Value:      7,
		ObservedAt: testNow.Add(-time.Hour),
		Note:       "  counted by hand  ",
	}, actor); err != nil {
		t.Fatalf("expected the value to be accepted, got %v", err)
	}

	event := f.audit.last(t, ActionSampleRecorded)
	if event.Action != ActionSampleRecorded || event.SubjectType != SubjectMetric {
		t.Fatalf("expected a recorded-sample entry, got %+v", event)
	}
	if event.SubjectID != pushMetric {
		t.Fatalf("expected the metric as the subject, got %q", event.SubjectID)
	}
	if event.ActorType != repository.ActorBot || event.ActorID != "bot-growth" {
		t.Fatalf("expected the reporting credential on the entry, got %s/%s", event.ActorType, event.ActorID)
	}
	if event.RequestID != "req-7" {
		t.Fatalf("expected the request id to be carried, got %q", event.RequestID)
	}
	for _, want := range []string{`"value":7`, `"note":"counted by hand"`} {
		if !strings.Contains(event.Detail, want) {
			t.Fatalf("expected %s in the detail, got %s", want, event.Detail)
		}
	}
}

func TestAValueForAMetricTheEngineReadsItselfIsRefused(t *testing.T) {
	// Accepting this would let whoever holds a credential overwrite a number the
	// operator deliberately put beyond their reach, and leave the engine with two
	// disagreeing sources for one metric.
	f := newSamplesFixture(t)

	_, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: testMetric,
		Value:     999,
	}, testActor())

	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if fields := ValidationFields(err); fields["metricKey"] == "" {
		t.Fatalf("expected the metric to be named as the problem, got %v", fields)
	}
	if len(f.store.inserted) != 0 {
		t.Fatal("expected nothing to be stored")
	}
}

func TestAValueForAnUndeclaredMetricIsRefused(t *testing.T) {
	f := newSamplesFixture(t)

	_, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: "invented.by.the.caller",
		Value:     1,
	}, testActor())

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestAReportedValueMustBeANumber(t *testing.T) {
	f := newSamplesFixture(t)

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := f.samples.Record(context.Background(), SampleRequest{
			MetricKey: pushMetric,
			Value:     value,
		}, testActor())
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("expected %v to be refused, got %v", value, err)
		}
	}
}

func TestAReportedTimestampIsBoundedInBothDirections(t *testing.T) {
	f := newSamplesFixture(t)

	cases := []struct {
		name       string
		observedAt time.Time
		wantOK     bool
	}{
		{"a little clock skew is tolerated", testNow.Add(2 * time.Minute), true},
		{"a value from the future is refused", testNow.Add(time.Hour), false},
		{"yesterday is fine", testNow.Add(-24 * time.Hour), true},
		{"backfilling last month is refused", testNow.Add(-30 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.samples.Record(context.Background(), SampleRequest{
				MetricKey:  pushMetric,
				Value:      1,
				ObservedAt: tc.observedAt,
			}, testActor())
			if tc.wantOK && err != nil {
				t.Fatalf("expected %v to be accepted, got %v", tc.observedAt, err)
			}
			if !tc.wantOK {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("expected %v to be refused, got %v", tc.observedAt, err)
				}
				if fields := ValidationFields(err); fields["observedAt"] == "" {
					t.Fatalf("expected observedAt to be named, got %v", fields)
				}
			}
		})
	}
}

func TestAValueWithoutAnActorIsRefused(t *testing.T) {
	// An unattributed value on a self-reported metric is worse than no value.
	f := newSamplesFixture(t)

	_, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric,
		Value:     1,
	}, Actor{})

	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected an actorless report to be refused, got %v", err)
	}
}

func TestAStorageFailureIsReportedRatherThanAudited(t *testing.T) {
	f := newSamplesFixture(t)
	f.store.err = errors.New("disk on fire")

	_, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric,
		Value:     1,
	}, testActor())

	if err == nil {
		t.Fatal("expected the failure to reach the caller")
	}
	if len(f.audit.events) != 0 {
		t.Fatal("expected nothing to be audited for a value that was never stored")
	}
}

func TestAFailedAuditWriteDoesNotLoseTheValue(t *testing.T) {
	// The observation is already stored; failing the request would invite a retry
	// that stores it twice.
	f := newSamplesFixture(t)
	f.audit.err = errors.New("audit table gone")

	if _, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric,
		Value:     1,
	}, testActor()); err != nil {
		t.Fatalf("expected the value to be kept, got %v", err)
	}
	if len(f.store.inserted) != 1 {
		t.Fatalf("expected the observation to be stored, got %d", len(f.store.inserted))
	}
}

func TestTheLatestValueCanBeReadBack(t *testing.T) {
	// A pusher needs to be able to confirm the number landed, and an operator adding
	// a goal needs to know what the engine currently sees.
	f := newSamplesFixture(t)
	if _, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric, Value: 3,
	}, testActor()); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := f.samples.Record(context.Background(), SampleRequest{
		MetricKey: pushMetric, Value: 9,
	}, testActor()); err != nil {
		t.Fatalf("record: %v", err)
	}

	record, err := f.samples.Latest(context.Background(), pushMetric)
	if err != nil {
		t.Fatalf("expected the last value, got %v", err)
	}
	if record.Value != 9 {
		t.Fatalf("expected the most recent value, got %v", record.Value)
	}
}

func TestTheLatestValueOfAMetricNobodyHasObservedIsNotFound(t *testing.T) {
	f := newSamplesFixture(t)

	_, err := f.samples.Latest(context.Background(), pushMetric)

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestTheLatestValueOfAnUndeclaredMetricIsNotFound(t *testing.T) {
	f := newSamplesFixture(t)

	if _, err := f.samples.Latest(context.Background(), "  "); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a blank key to be refused, got %v", err)
	}
	if _, err := f.samples.Latest(context.Background(), "invented"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected an undeclared metric to be not found, got %v", err)
	}
}

func TestAReadFailureIsNotMistakenForAnAbsentValue(t *testing.T) {
	// "Nothing reported" and "storage is broken" lead to different actions, so they
	// must not collapse into the same answer.
	f := newSamplesFixture(t)
	f.store.latestErr = errors.New("connection reset")

	_, err := f.samples.Latest(context.Background(), pushMetric)

	if errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a storage failure, not a not-found, got %v", err)
	}
	if err == nil {
		t.Fatal("expected the failure to reach the caller")
	}
}
