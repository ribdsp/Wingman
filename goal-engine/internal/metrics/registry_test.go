package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a metrics config into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `
datasources:
  - name: billing
    driver: postgres
    dsnEnv: BILLING_READONLY_DSN
    maxOpenConns: 2

metrics:
  - key: billing.mrr.idr
    description: Monthly recurring revenue
    unit: IDR
    source: sql
    datasource: billing
    query: SELECT coalesce(sum(amount), 0)::double precision FROM subscriptions WHERE status = 'active'

  - key: support.csat
    source: http
    url: https://example.test/api/csat
    jsonPath: data.score

  - key: manual.pipeline.value
    source: push
`

func TestLoadReadsValidConfig(t *testing.T) {
	// Arrange
	t.Setenv("BILLING_READONLY_DSN", "postgres://reader@localhost/billing")
	path := writeConfig(t, validConfig)

	// Act
	registry, err := Load(path)

	// Assert
	if err != nil {
		t.Fatalf("expected config to load, got: %v", err)
	}
	if registry.Len() != 3 {
		t.Fatalf("expected 3 metrics, got %d", registry.Len())
	}
	def, ok := registry.Get("billing.mrr.idr")
	if !ok {
		t.Fatal("expected billing.mrr.idr to be registered")
	}
	if def.Source != SourceSQL || def.Datasource != "billing" {
		t.Fatalf("unexpected definition: %+v", def)
	}
	sources := registry.Datasources()
	if len(sources) != 1 || sources[0].DSN() != "postgres://reader@localhost/billing" {
		t.Fatalf("expected resolved dsn, got %+v", sources)
	}
	if sources[0].MaxOpenConns != 2 {
		t.Fatalf("expected maxOpenConns 2, got %d", sources[0].MaxOpenConns)
	}
}

func TestLoadDefaultsHTTPMethodToGET(t *testing.T) {
	t.Setenv("BILLING_READONLY_DSN", "postgres://reader@localhost/billing")

	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	def, _ := registry.Get("support.csat")
	if def.Method != "GET" {
		t.Fatalf("expected default method GET, got %q", def.Method)
	}
}

func TestLoadKeysAreSorted(t *testing.T) {
	t.Setenv("BILLING_READONLY_DSN", "postgres://reader@localhost/billing")

	registry, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := registry.Keys()
	want := []string{"billing.mrr.idr", "manual.pipeline.value", "support.csat"}
	if len(got) != len(want) {
		t.Fatalf("expected %d keys, got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected keys %v, got %v", want, got)
		}
	}
}

func TestLoadFailsWhenDSNEnvIsMissing(t *testing.T) {
	// Deliberately do not set BILLING_READONLY_DSN.
	_, err := Load(writeConfig(t, validConfig))

	if err == nil {
		t.Fatal("expected an error when the dsn env var is unset")
	}
	if !strings.Contains(err.Error(), "BILLING_READONLY_DSN") {
		t.Fatalf("expected the error to name the missing variable, got: %v", err)
	}
}

func TestLoadRejectsDangerousQuery(t *testing.T) {
	t.Setenv("BILLING_READONLY_DSN", "postgres://reader@localhost/billing")
	body := `
datasources:
  - name: billing
    dsnEnv: BILLING_READONLY_DSN
metrics:
  - key: billing.evil
    source: sql
    datasource: billing
    query: SELECT 1; DROP TABLE subscriptions
`

	_, err := Load(writeConfig(t, body))

	if err == nil {
		t.Fatal("expected a stacked statement to be rejected at load time")
	}
	if !strings.Contains(err.Error(), "billing.evil") {
		t.Fatalf("expected the error to name the metric, got: %v", err)
	}
}

func TestLoadRejectsInvalidDefinitions(t *testing.T) {
	t.Setenv("BILLING_READONLY_DSN", "postgres://reader@localhost/billing")

	cases := map[string]string{
		"unknown datasource": `
datasources:
  - name: billing
    dsnEnv: BILLING_READONLY_DSN
metrics:
  - key: a.b
    source: sql
    datasource: nope
    query: SELECT 1
`,
		"duplicate key": `
datasources:
  - name: billing
    dsnEnv: BILLING_READONLY_DSN
metrics:
  - key: a.b
    source: sql
    datasource: billing
    query: SELECT 1
  - key: a.b
    source: push
`,
		"missing source": `
metrics:
  - key: a.b
`,
		"unknown source": `
metrics:
  - key: a.b
    source: carrier-pigeon
`,
		"push with query": `
metrics:
  - key: a.b
    source: push
    query: SELECT 1
`,
		"http without json path": `
metrics:
  - key: a.b
    source: http
    url: https://example.test/x
`,
		"http with bad scheme": `
metrics:
  - key: a.b
    source: http
    url: file:///etc/passwd
    jsonPath: value
`,
		"http with unsupported method": `
metrics:
  - key: a.b
    source: http
    url: https://example.test/x
    method: DELETE
    jsonPath: value
`,
		"uppercase key": `
metrics:
  - key: Billing.MRR
    source: push
`,
		"blank key": `
metrics:
  - source: push
`,
		"unknown yaml field": `
metrics:
  - key: a.b
    source: push
    sorce: typo
`,
		"auth header without env": `
metrics:
  - key: a.b
    source: http
    url: https://example.test/x
    jsonPath: value
    authHeader: X-Token
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestLoadResolvesHTTPAuthValueFromEnv(t *testing.T) {
	t.Setenv("CSAT_TOKEN", "Bearer secret-token")
	body := `
metrics:
  - key: support.csat
    source: http
    url: https://example.test/api/csat
    jsonPath: data.score
    authHeader: X-Api-Key
    authValueEnv: CSAT_TOKEN
`

	registry, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	def, _ := registry.Get("support.csat")
	if def.AuthValue() != "Bearer secret-token" {
		t.Fatalf("expected the token to be resolved from the environment, got %q", def.AuthValue())
	}
}

func TestLoadDefaultsAuthHeaderToAuthorization(t *testing.T) {
	t.Setenv("CSAT_TOKEN", "Bearer secret-token")
	body := `
metrics:
  - key: support.csat
    source: http
    url: https://example.test/api/csat
    jsonPath: data.score
    authValueEnv: CSAT_TOKEN
`

	registry, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	def, _ := registry.Get("support.csat")
	if def.AuthHeader != "Authorization" {
		t.Fatalf("expected default auth header, got %q", def.AuthHeader)
	}
}

func TestLoadFailsOnMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	// An operator fixing config should see the whole list, not the first line.
	body := `
metrics:
  - key: BAD KEY
    source: sql
    datasource: missing
    query: DELETE FROM t
  - key: also.bad
    source: nonsense
`

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected errors")
	}
	message := err.Error()
	for _, want := range []string{"BAD KEY", "also.bad"} {
		if !strings.Contains(message, want) {
			t.Fatalf("expected error to mention %q, got: %v", want, message)
		}
	}
}
