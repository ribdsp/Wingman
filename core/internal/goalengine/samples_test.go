package goalengine

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Spend reporting. This is the one call in the package that permits nothing: it tells
// the goal engine what a run cost so an operator can write a goal against cost.

func TestReportTokens_filesTheRunsCostAsASampleOnTheOperatorsMetric(t *testing.T) {
	// Arrange
	fake, client := newEngine(t, http.StatusCreated, sampleRecordedBody)
	observed := time.Date(2026, 9, 12, 10, 30, 0, 0, time.UTC)

	// Act
	err := client.ReportTokens(context.Background(), TokenSample{
		Tokens:     1234,
		ObservedAt: observed,
		Note:       "run run_1 (anthropic/claude-opus-5)",
	})

	// Assert
	if err != nil {
		t.Fatalf("ReportTokens() = %v; want the sample filed", err)
	}
	got := fake.only(t)
	if want := "/v1/metrics/ops.tokens_spent/samples"; got.method != http.MethodPost || got.path != want {
		t.Errorf("request = %s %s; want POST %s", got.method, got.path, want)
	}

	body := got.decode(t)
	if body["value"] != float64(1234) {
		t.Errorf("value = %v; want 1234", body["value"])
	}
	// RFC 3339, because the engine rejects anything else rather than quietly
	// substituting now — a caller who meant to backdate a value learns that they did
	// not.
	if body["observedAt"] != observed.Format(time.RFC3339) {
		t.Errorf("observedAt = %v; want %q", body["observedAt"], observed.Format(time.RFC3339))
	}
	// The note is what a person scanning the sample list reads. It names the run and
	// the model, and nothing that could be a credential.
	if note, _ := body["note"].(string); !strings.Contains(note, "run_1") {
		t.Errorf("note = %v; want the run named", body["note"])
	}
}

func TestReportTokens_noTimestamp_letsTheEngineRecordWhenItArrived(t *testing.T) {
	// Arrange
	// A zero time is not a timestamp. Sending one would file the sample in 1 CE and
	// leave the metric looking stale for two thousand years.
	fake, client := newEngine(t, http.StatusCreated, sampleRecordedBody)

	// Act
	if err := client.ReportTokens(context.Background(), TokenSample{Tokens: 10}); err != nil {
		t.Fatalf("ReportTokens() = %v; want the sample filed", err)
	}

	// Assert
	body := fake.only(t).decode(t)
	if _, ok := body["observedAt"]; ok {
		t.Errorf("observedAt = %v; want it absent so the engine timestamps it", body["observedAt"])
	}
}

func TestReportTokens_aRunThatCostNothing_isStillReported(t *testing.T) {
	// Arrange
	// Zero is a real reading, and a sample saying so keeps the feed fresh: the engine
	// stops evaluating a metric whose latest sample is older than
	// METRIC_MAX_SAMPLE_AGE rather than reading a dead feed's last number as on-track.
	// A client that skipped zeros would be the thing that killed the feed.
	fake, client := newEngine(t, http.StatusCreated, sampleRecordedBody)

	// Act
	if err := client.ReportTokens(context.Background(), TokenSample{Tokens: 0}); err != nil {
		t.Fatalf("ReportTokens() = %v; want the sample filed", err)
	}

	// Assert
	body := fake.only(t).decode(t)
	if body["value"] != float64(0) {
		t.Errorf("value = %v; want an explicit zero, which is why the field is a pointer", body["value"])
	}
}

func TestReportTokens_nothingWorthSending_isRefusedBeforeTheRequest(t *testing.T) {
	tests := []struct {
		name   string
		metric string
		sample TokenSample
		want   string
	}{
		{
			// Guessing a metric name would file samples the operator never declared,
			// and the engine would reject them — leaving it unclear which end was
			// misconfigured.
			name:   "no metric configured",
			metric: "",
			sample: TokenSample{Tokens: 10},
			want:   "no spend metric",
		},
		{
			// Negative spend is not a reading, it is a bug upstream. Filing it would
			// make a cost metric go backwards and a goal look suddenly healthy.
			name:   "negative tokens",
			metric: "ops.tokens_spent",
			sample: TokenSample{Tokens: -1},
			want:   "negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			fake, client := newEngine(t, http.StatusCreated, sampleRecordedBody, WithSpendMetric(test.metric))

			// Act
			err := client.ReportTokens(context.Background(), test.sample)

			// Assert
			if err == nil {
				t.Fatal("ReportTokens() = nil; want a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to mention %q", err, test.want)
			}
			if len(fake.requests) != 0 {
				t.Errorf("the engine received %d requests; want none", len(fake.requests))
			}
		})
	}
}

func TestReportTokens_aMetricNameNeedingEscaping_doesNotReachAPathNobodyDeclared(t *testing.T) {
	// Arrange
	// The key comes from configuration, so it is not this package's to trust. A slash
	// in it would otherwise post to a different endpoint entirely.
	fake, client := newEngine(t, http.StatusCreated, sampleRecordedBody, WithSpendMetric("ops/tokens spent"))

	// Act
	if err := client.ReportTokens(context.Background(), TokenSample{Tokens: 5}); err != nil {
		t.Fatalf("ReportTokens() = %v; want the request sent as escaped", err)
	}

	// Assert
	if want := "/v1/metrics/ops%2Ftokens%20spent/samples"; fake.only(t).path != want {
		t.Errorf("path = %q; want %q", fake.only(t).path, want)
	}
}

func TestReportTokens_theEngineRefused_saysWhichCallFailedAndWhetherToTryAgain(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
	}{
		{
			name:          "the engine failed",
			status:        http.StatusServiceUnavailable,
			body:          `{"success":false,"error":{"code":"internal_error"}}`,
			wantRetryable: true,
		},
		{
			// A metric that is not in the engine's config/metrics.yaml, or one that
			// is not a push metric. Retrying will not add it.
			name:          "the metric is not declared",
			status:        http.StatusNotFound,
			body:          `{"success":false,"error":{"code":"not_found","message":"unknown metric"}}`,
			wantRetryable: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			_, client := newEngine(t, test.status, test.body)

			// Act
			err := client.ReportTokens(context.Background(), TokenSample{Tokens: 10})

			// Assert
			if err == nil {
				t.Fatal("ReportTokens() = nil; want the failure reported")
			}
			// Reported, never fatal to a run: this call is bookkeeping, and a run that
			// stopped because its cost could not be filed would be a safety feature
			// costing work it had already done.
			if !strings.Contains(err.Error(), "token spend") {
				t.Errorf("error = %v; want it to say which call failed", err)
			}
			if got := IsRetryable(err); got != test.wantRetryable {
				t.Errorf("IsRetryable() = %v; want %v", got, test.wantRetryable)
			}
		})
	}
}
