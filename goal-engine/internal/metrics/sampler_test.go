package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func httpDef(url, jsonPath string) Definition {
	return Definition{
		Key:      "support.csat",
		Source:   SourceHTTP,
		URL:      url,
		Method:   http.MethodGet,
		JSONPath: jsonPath,
	}
}

func TestHTTPSamplerExtractsNestedValue(t *testing.T) {
	// Arrange
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"score":4.25}}`))
	}))
	defer server.Close()
	sampler := NewHTTPSampler(server.Client(), time.Second)

	// Act
	sample, err := sampler.Sample(context.Background(), httpDef(server.URL, "data.score"))

	// Assert
	if err != nil {
		t.Fatalf("expected a sample, got error: %v", err)
	}
	if sample.Value != 4.25 {
		t.Fatalf("expected 4.25, got %v", sample.Value)
	}
	if sample.MetricKey != "support.csat" || sample.Source != SourceHTTP {
		t.Fatalf("unexpected sample metadata: %+v", sample)
	}
	if sample.ObservedAt.IsZero() {
		t.Fatal("expected ObservedAt to be set")
	}
}

func TestHTTPSamplerWalksArrayIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"value":1},{"value":99}]}`))
	}))
	defer server.Close()

	sample, err := NewHTTPSampler(server.Client(), time.Second).
		Sample(context.Background(), httpDef(server.URL, "items.1.value"))
	if err != nil {
		t.Fatalf("expected a sample, got error: %v", err)
	}
	if sample.Value != 99 {
		t.Fatalf("expected 99, got %v", sample.Value)
	}
}

func TestHTTPSamplerAcceptsNumericString(t *testing.T) {
	// Plenty of dashboards return money as a string to avoid float issues.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mrr":"12500000.50"}`))
	}))
	defer server.Close()

	sample, err := NewHTTPSampler(server.Client(), time.Second).
		Sample(context.Background(), httpDef(server.URL, "mrr"))
	if err != nil {
		t.Fatalf("expected a sample, got error: %v", err)
	}
	if sample.Value != 12500000.50 {
		t.Fatalf("expected 12500000.50, got %v", sample.Value)
	}
}

func TestHTTPSamplerSendsConfiguredHeaders(t *testing.T) {
	var gotAuth, gotCustom string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Api-Key")
		gotCustom = r.Header.Get("X-Tenant")
		_, _ = w.Write([]byte(`{"v":1}`))
	}))
	defer server.Close()

	def := httpDef(server.URL, "v")
	def.AuthHeader = "X-Api-Key"
	def.authValue = "secret-token"
	def.Headers = map[string]string{"X-Tenant": "wingman"}

	if _, err := NewHTTPSampler(server.Client(), time.Second).Sample(context.Background(), def); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if gotAuth != "secret-token" {
		t.Fatalf("expected auth header to be sent, got %q", gotAuth)
	}
	if gotCustom != "wingman" {
		t.Fatalf("expected custom header to be sent, got %q", gotCustom)
	}
}

func TestHTTPSamplerRejectsBadResponses(t *testing.T) {
	cases := map[string]struct {
		status   int
		body     string
		jsonPath string
	}{
		"server error":     {status: 500, body: `{"v":1}`, jsonPath: "v"},
		"not json":         {status: 200, body: `<html>nope</html>`, jsonPath: "v"},
		"missing key":      {status: 200, body: `{"other":1}`, jsonPath: "v"},
		"null value":       {status: 200, body: `{"v":null}`, jsonPath: "v"},
		"non numeric":      {status: 200, body: `{"v":"high"}`, jsonPath: "v"},
		"object not value": {status: 200, body: `{"v":{"a":1}}`, jsonPath: "v"},
		"index on object":  {status: 200, body: `{"v":{"a":1}}`, jsonPath: "v.0"},
		"index overflow":   {status: 200, body: `{"v":[1]}`, jsonPath: "v.5"},
		"descend scalar":   {status: 200, body: `{"v":1}`, jsonPath: "v.deeper"},
		"empty segment":    {status: 200, body: `{"v":1}`, jsonPath: "v..x"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := NewHTTPSampler(server.Client(), time.Second).
				Sample(context.Background(), httpDef(server.URL, tc.jsonPath))
			if err == nil {
				t.Fatalf("expected %s to fail", name)
			}
			if !strings.Contains(err.Error(), "support.csat") {
				t.Fatalf("expected the error to name the metric, got: %v", err)
			}
		})
	}
}

func TestHTTPSamplerRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", maxResponseBytes+16) + `"}`))
	}))
	defer server.Close()

	if _, err := NewHTTPSampler(server.Client(), 5*time.Second).
		Sample(context.Background(), httpDef(server.URL, "pad")); err == nil {
		t.Fatal("expected an oversized response to be rejected")
	}
}

func TestHTTPSamplerHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := NewHTTPSampler(server.Client(), time.Minute).
		Sample(ctx, httpDef(server.URL, "v")); err == nil {
		t.Fatal("expected the request to fail once the context expired")
	}
}

func TestHTTPSamplerRefusesNonHTTPMetric(t *testing.T) {
	def := Definition{Key: "billing.mrr", Source: SourceSQL}

	_, err := NewHTTPSampler(nil, time.Second).Sample(context.Background(), def)
	if err == nil {
		t.Fatal("expected a sql metric to be refused by the http sampler")
	}
}

func TestMuxSamplerRoutesBySource(t *testing.T) {
	sqlCalls, httpCalls := 0, 0
	mux := NewMuxSampler(
		samplerFunc(func(ctx context.Context, def Definition) (Sample, error) {
			sqlCalls++
			return Sample{MetricKey: def.Key}, nil
		}),
		samplerFunc(func(ctx context.Context, def Definition) (Sample, error) {
			httpCalls++
			return Sample{MetricKey: def.Key}, nil
		}),
	)

	if _, err := mux.Sample(context.Background(), Definition{Key: "a", Source: SourceSQL}); err != nil {
		t.Fatalf("sql route: %v", err)
	}
	if _, err := mux.Sample(context.Background(), Definition{Key: "b", Source: SourceHTTP}); err != nil {
		t.Fatalf("http route: %v", err)
	}
	if _, err := mux.Sample(context.Background(), Definition{Key: "c", Source: SourcePush}); err == nil {
		t.Fatal("expected a push metric to be refused")
	}
	if _, err := mux.Sample(context.Background(), Definition{Key: "d", Source: "weird"}); err == nil {
		t.Fatal("expected an unknown source to be refused")
	}
	if sqlCalls != 1 || httpCalls != 1 {
		t.Fatalf("expected one call per route, got sql=%d http=%d", sqlCalls, httpCalls)
	}
}

func TestMuxSamplerReportsMissingSampler(t *testing.T) {
	mux := NewMuxSampler(nil, nil)

	if _, err := mux.Sample(context.Background(), Definition{Key: "a", Source: SourceSQL}); err == nil {
		t.Fatal("expected an error when no sql sampler is configured")
	}
	if _, err := mux.Sample(context.Background(), Definition{Key: "b", Source: SourceHTTP}); err == nil {
		t.Fatal("expected an error when no http sampler is configured")
	}
}

// samplerFunc adapts a function to the Sampler interface.
type samplerFunc func(ctx context.Context, def Definition) (Sample, error)

func (f samplerFunc) Sample(ctx context.Context, def Definition) (Sample, error) {
	return f(ctx, def)
}
