package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/config"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
)

// testNow is the fixed clock every fixture runs on. Mid-period so a goal created
// in a test is neither unstarted nor overdue.
var testNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// Credentials the tests authenticate with. They are long enough to pass the
// config validator, which is what a real deployment's keys go through.
const (
	operatorSecret = "operator-secret-key-0123456789ab"
	botSecret      = "bot-secret-key-0123456789abcdef"
)

func init() { gin.SetMode(gin.TestMode) }

// fixture is a running router over in-memory storage.
type fixture struct {
	router  *gin.Engine
	handler *Handler

	goals     *memGoals
	approvals *memApprovals
	spend     *memSpend
	flags     *memFlags
	audit     *memAudit
	samples   *memSamples
	sampler   *memSampler
	tasks     *memTasks
	policies  *memPolicies
	evals     *memEvaluations
	registry  *metrics.Registry
}

// newFixture builds the whole stack: real middleware, real services, fake
// storage. The point is that a test exercises the same path a request takes in
// production, so a rule enforced in the wrong layer shows up as a failure.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{
		goals:     newMemGoals(),
		approvals: newMemApprovals(),
		spend:     &memSpend{},
		flags:     newMemFlags(),
		audit:     &memAudit{},
		samples:   newMemSamples(),
		sampler:   &memSampler{value: 50},
		tasks:     &memTasks{},
		policies: &memPolicies{byAction: map[string]domain.ApprovalPolicy{
			"ads.spend": {
				ActionType:       "ads.spend",
				Currency:         "IDR",
				AutoApproveBelow: 100_000,
				DailyCap:         500_000,
				HardCap:          5_000_000,
				Enabled:          true,
			},
		}},
		evals:    newMemEvaluations(),
		registry: testRegistry(t),
	}

	clock := func() time.Time { return testNow }
	log := zerolog.Nop()

	goals, err := service.NewGoals(service.GoalsDeps{
		Goals: f.goals, Metrics: f.registry, Audit: f.audit, Clock: clock, Logger: log,
	})
	if err != nil {
		t.Fatalf("goals: %v", err)
	}
	approvals, err := service.NewApprovals(service.ApprovalsDeps{
		Approvals: f.approvals, Spend: f.spend, Policies: f.policies, Flags: f.flags,
		Audit: f.audit, Clock: clock, Logger: log,
	})
	if err != nil {
		t.Fatalf("approvals: %v", err)
	}
	flags, err := service.NewFlags(service.FlagsDeps{
		Flags: f.flags, Audit: f.audit, Clock: clock, Logger: log,
	})
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	auditLog, err := service.NewAuditLog(service.AuditLogDeps{Reader: f.audit, Clock: clock})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	evaluations, err := service.NewEvaluations(service.EvaluationsDeps{
		Reader: f.evals, Goals: f.goals,
	})
	if err != nil {
		t.Fatalf("evaluations: %v", err)
	}
	monitor, err := service.NewMonitor(service.MonitorDeps{
		Goals: &monitorGoals{}, Samples: f.samples, LatestSample: f.samples,
		Evaluations: f.evals,
		Dispatches:  memDispatches{}, Flags: f.flags, Audit: f.audit, Metrics: f.registry,
		Sampler: f.sampler, Tasks: f.tasks, DefaultBotID: "bot-default",
		DefaultChannelID: "channel-default", Clock: clock, Logger: log,
	})
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	samples, err := service.NewSamples(service.SamplesDeps{
		Samples: f.samples, Reader: f.samples, Metrics: f.registry, Audit: f.audit,
		Clock: clock, Logger: log,
	})
	if err != nil {
		t.Fatalf("samples: %v", err)
	}

	h, err := New(Deps{
		Goals: goals, Approvals: approvals, Flags: flags, Audit: auditLog,
		Evaluations: evaluations, Monitor: monitor, Samples: samples,
		Metrics: f.registry, Logger: log,
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	f.handler = h

	router, err := NewRouter(RouterDeps{
		Handler: h,
		Credentials: CredentialsFrom([]config.APIKey{
			{Name: "ops", Secret: operatorSecret, Role: config.RoleOperator},
			{Name: "bot-growth", Secret: botSecret, Role: config.RoleBot},
		}),
		// Generous enough that no test trips the limiter by accident; the limiter's
		// own behaviour is covered in internal/middleware.
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		Logger:    log,
	})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	f.router = router
	return f
}

// testRegistry writes a metrics file and loads it, so the tests run against the
// same loader production does rather than a hand-built registry.
//
// One metric is http on purpose: its URL and auth header must never appear in an
// API response, and a push-only registry could not catch that.
func testRegistry(t *testing.T) *metrics.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.yaml")
	body := `metrics:
  - key: acme.mrr
    description: Monthly recurring revenue
    unit: IDR
    source: push
  - key: acme.signups
    description: New signups today
    unit: count
    source: http
    url: https://internal.example.invalid/admin/metrics
    jsonPath: data.signups
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write metrics config: %v", err)
	}
	registry, err := metrics.Load(path)
	if err != nil {
		t.Fatalf("load metrics config: %v", err)
	}
	return registry
}

// do issues an authenticated request as the given secret.
func (f *fixture) do(t *testing.T, method, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// asOperator and asBot are the two callers every authorization test needs.
func (f *fixture) asOperator(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, operatorSecret, body)
}

func (f *fixture) asBot(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, botSecret, body)
}

// unauthenticated is the third caller worth naming: the one with no credential.
func (f *fixture) unauthenticated(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, "", body)
}

// envelope is the decoded response body.
type envelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
	Meta struct {
		RequestID  string `json:"requestId"`
		Timestamp  string `json:"timestamp"`
		Pagination *struct {
			Page       int `json:"page"`
			Limit      int `json:"limit"`
			TotalItems int `json:"totalItems"`
			TotalPages int `json:"totalPages"`
		} `json:"pagination"`
	} `json:"meta"`
}

// decode reads the envelope, asserting the status on the way through.
func decode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) envelope {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("expected status %d, got %d: %s", wantStatus, rec.Code, rec.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("expected a JSON envelope, got %q: %v", rec.Body.String(), err)
	}
	if env.Meta.RequestID == "" {
		t.Fatal("expected a request id on every response")
	}
	return env
}

// dataInto decodes the envelope's data field into target.
func dataInto(t *testing.T, env envelope, target any) {
	t.Helper()
	if len(env.Data) == 0 {
		t.Fatalf("expected data in the response, got %+v", env)
	}
	if err := json.Unmarshal(env.Data, target); err != nil {
		t.Fatalf("expected data to decode: %v (%s)", err, env.Data)
	}
}

// errorCode returns the API error code from a failure response.
func errorCode(t *testing.T, env envelope) string {
	t.Helper()
	if env.Error == nil {
		t.Fatalf("expected an error in the response, got %+v", env)
	}
	return env.Error.Code
}

// validGoalBody is a goal the service accepts, used wherever the request itself
// is not what a test is about.
func validGoalBody() string {
	return `{
		"product": "acme",
		"title": "Reach 100M MRR",
		"sourceText": "we need 100M MRR by the end of the quarter",
		"metricKey": "acme.mrr",
		"comparator": "gte",
		"targetValue": 100000000,
		"periodStart": "2026-09-01T00:00:00Z",
		"periodEnd": "2026-12-01T00:00:00Z",
		"botId": "bot-growth",
		"channelId": "channel-growth"
	}`
}

// seedGoal creates a goal through the API and returns its id.
func (f *fixture) seedGoal(t *testing.T) string {
	t.Helper()
	env := decode(t, f.asOperator(t, http.MethodPost, "/v1/goals", validGoalBody()), http.StatusCreated)
	var view goalView
	dataInto(t, env, &view)
	return view.ID
}

// seedPendingApproval files a spend large enough to need a human and returns its
// id.
func (f *fixture) seedPendingApproval(t *testing.T) string {
	t.Helper()
	body := `{"actionType":"ads.spend","amount":400000,"currency":"IDR"}`
	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals", body), http.StatusCreated)
	var view approvalView
	dataInto(t, env, &view)
	if view.Outcome != string(domain.ApprovalPending) {
		t.Fatalf("expected a pending approval to work with, got %q", view.Outcome)
	}
	return view.ID
}
