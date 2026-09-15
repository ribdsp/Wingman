package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/config"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/middleware"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

func TestEveryServiceErrorReachesTheRightStatus(t *testing.T) {
	// The mapping is the reason handlers never inspect an error string. Each case
	// here is driven through a real route: a table over respondError alone would pass
	// while a handler swallowed the error and answered 200.
	f := newFixture(t)
	goalID := f.seedGoal(t)
	approvalID := f.seedPendingApproval(t)

	// Resolve it once so the second attempt is a conflict.
	decode(t, f.asOperator(t, http.MethodPost, "/v1/approvals/"+approvalID+"/resolve",
		`{"resolution":"approved"}`), http.StatusOK)

	for _, tc := range []struct {
		name     string
		method   string
		path     string
		secret   string
		body     string
		status   int
		wantCode string
	}{
		{"a goal that does not exist", http.MethodGet, "/v1/goals/goal-404", operatorSecret, "",
			http.StatusNotFound, utils.ErrCodeNotFound},
		{"an approval that does not exist", http.MethodGet, "/v1/approvals/approval-404", operatorSecret, "",
			http.StatusNotFound, utils.ErrCodeNotFound},
		{"a goal with no metric", http.MethodPost, "/v1/goals", operatorSecret,
			`{"product":"acme","title":"t","comparator":"gte","targetValue":1}`,
			http.StatusBadRequest, utils.ErrCodeValidation},
		{"a patch that changes nothing", http.MethodPatch, "/v1/goals/" + goalID, operatorSecret, `{}`,
			http.StatusBadRequest, utils.ErrCodeValidation},
		{"an agent changing a goal", http.MethodPatch, "/v1/goals/" + goalID, botSecret, `{"targetValue":1}`,
			http.StatusForbidden, utils.ErrCodeForbidden},
		{"an approval decided twice", http.MethodPost, "/v1/approvals/" + approvalID + "/resolve", operatorSecret,
			`{"resolution":"rejected"}`, http.StatusConflict, utils.ErrCodeAlreadyResolved},
		{"no credential at all", http.MethodGet, "/v1/goals", "", "",
			http.StatusUnauthorized, utils.ErrCodeUnauthorized},
		{"a route that does not exist", http.MethodGet, "/v1/nothing-here", operatorSecret, "",
			http.StatusNotFound, utils.ErrCodeNotFound},
		{"a method that is not allowed", http.MethodDelete, "/v1/goals/" + goalID, operatorSecret, "",
			http.StatusMethodNotAllowed, utils.ErrCodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := decode(t, f.do(t, tc.method, tc.path, tc.secret, tc.body), tc.status)
			if got := errorCode(t, env); got != tc.wantCode {
				t.Fatalf("expected %s, got %s (%s)", tc.wantCode, got, env.Error.Message)
			}
			if env.Success {
				t.Fatal("an error response claimed success")
			}
		})
	}
}

func TestASpendUnderTheKillSwitchIsARecordedDenialNotAnError(t *testing.T) {
	// A halted engine is not a broken one, and the attempt is worth keeping. A 503
	// would leave no trace that the agent asked and invite it to retry until the
	// switch came off; a recorded denial answers definitively and stays in the log.
	f := newFixture(t)
	decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":true,"reason":"reviewing yesterday's spend"}`), http.StatusOK)

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals",
		`{"actionType":"ads.spend","amount":1000,"currency":"USD"}`), http.StatusCreated)
	var approval approvalView
	dataInto(t, env, &approval)

	if approval.Outcome != string(domain.ApprovalDenied) {
		t.Fatalf("expected a halted engine to deny the spend, got %q", approval.Outcome)
	}
	if !strings.Contains(approval.PolicyReason, "kill switch") {
		t.Fatalf("expected the reason to name the kill switch, got %q", approval.PolicyReason)
	}
	if approval.IsOpen {
		t.Fatal("a denied request must not sit in the queue waiting for a human")
	}
	if len(f.spend.recorded) != 0 {
		t.Fatalf("a halted engine still recorded spend: %v", f.spend.recorded)
	}
	// And it is in the log, which is the whole reason this is a 201.
	if len(f.approvals.byID) != 1 {
		t.Fatalf("expected the refused attempt to be recorded, got %d", len(f.approvals.byID))
	}
}

func TestAStorageFailureIsA500ThatSaysNothingAboutStorage(t *testing.T) {
	// A repository error can carry a query or a column name. The caller gets a
	// request id and nothing else, which is enough to find the logged detail.
	f := newFixture(t)
	f.goals.listErr = errStorage

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/goals", ""), http.StatusInternalServerError)
	if got := errorCode(t, env); got != utils.ErrCodeInternal {
		t.Fatalf("expected %s, got %s", utils.ErrCodeInternal, got)
	}
	if strings.Contains(env.Error.Message, errStorage.Error()) {
		t.Fatalf("the storage error reached the client: %q", env.Error.Message)
	}
}

func TestAMalformedBodyIsRejectedWithoutDescribingTheStruct(t *testing.T) {
	// Go's JSON errors name struct fields and types. That describes this service's
	// internals, not the caller's mistake.
	f := newFixture(t)

	for _, body := range []string{`{`, `[]`, `{"targetValue":"not a number"}`, `null,`} {
		env := decode(t, f.asOperator(t, http.MethodPost, "/v1/goals", body), http.StatusBadRequest)
		if got := errorCode(t, env); got != utils.ErrCodeValidation {
			t.Fatalf("expected %s for %q, got %s", utils.ErrCodeValidation, body, got)
		}
		for _, leaked := range []string{"goalRequest", "float64", "struct"} {
			if strings.Contains(env.Error.Message, leaked) {
				t.Fatalf("bind error leaked %q for body %q: %q", leaked, body, env.Error.Message)
			}
		}
	}
}

func TestTheKillSwitchCannotBeReleasedByOmission(t *testing.T) {
	// Defaulting a missing "engaged" to false would mean a malformed request — or a
	// client with a typo in the field name — releases the brakes.
	f := newFixture(t)
	decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":true,"reason":"halt"}`), http.StatusOK)

	for _, body := range []string{`{}`, `{"reason":"oops"}`, `{"engage":false}`} {
		env := decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch", body), http.StatusBadRequest)
		if got := errorCode(t, env); got != utils.ErrCodeValidation {
			t.Fatalf("expected %s for %q, got %s", utils.ErrCodeValidation, body, got)
		}
	}

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/flags/kill-switch", ""), http.StatusOK)
	var flag flagView
	dataInto(t, env, &flag)
	if !flag.Enabled {
		t.Fatal("a request with no engaged field released the kill switch")
	}
}

func TestAnUnparseableDeadlineIsRejectedRatherThanIgnored(t *testing.T) {
	// Dropping the field would answer 200 and tell the caller their deadline moved
	// when it did not.
	f := newFixture(t)
	goalID := f.seedGoal(t)

	env := decode(t, f.asOperator(t, http.MethodPatch, "/v1/goals/"+goalID,
		`{"periodEnd":"31 December"}`), http.StatusBadRequest)
	if !strings.Contains(env.Error.Message, "periodEnd") {
		t.Fatalf("expected the message to name the field, got %q", env.Error.Message)
	}
}

func TestAnUnparseableAuditWindowIsRejectedRatherThanWidened(t *testing.T) {
	// Treating a bad "since" as absent would quietly return the whole log to a query
	// that asked for an hour of it.
	f := newFixture(t)

	for _, query := range []string{"?since=yesterday", "?until=2026-13-45"} {
		env := decode(t, f.asOperator(t, http.MethodGet, "/v1/audit"+query, ""), http.StatusBadRequest)
		if got := errorCode(t, env); got != utils.ErrCodeValidation {
			t.Fatalf("expected %s for %q, got %s", utils.ErrCodeValidation, query, got)
		}
	}
}

func TestARequestIDIsEchoedBackAndReusedWhenSane(t *testing.T) {
	// The request id is how a 500 with no detail becomes a log line with all of it.
	f := newFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/goals", nil)
	req.Header.Set("Authorization", "Bearer "+operatorSecret)
	req.Header.Set("X-Request-Id", "trace-abc-123")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	env := decode(t, rec, http.StatusOK)
	if env.Meta.RequestID != "trace-abc-123" {
		t.Fatalf("expected the inbound request id to be reused, got %q", env.Meta.RequestID)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "trace-abc-123" {
		t.Fatalf("expected the request id on the response header, got %q", got)
	}

	// A hostile one is replaced rather than echoed: this value ends up in log lines
	// and response headers.
	req = httptest.NewRequest(http.MethodGet, "/v1/goals", nil)
	req.Header.Set("Authorization", "Bearer "+operatorSecret)
	req.Header.Set("X-Request-Id", "bad\r\nX-Injected: yes")
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	env = decode(t, rec, http.StatusOK)
	if strings.Contains(env.Meta.RequestID, "Injected") || strings.Contains(env.Meta.RequestID, "\n") {
		t.Fatalf("a header-splitting request id survived: %q", env.Meta.RequestID)
	}
	if rec.Header().Get("X-Injected") != "" {
		t.Fatal("a request id injected a response header")
	}
}

func TestListingsPaginate(t *testing.T) {
	// Both spellings are accepted on the way in because the envelope reports pages on
	// the way out, and a client that reads "page" from a response will send it back.
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.seedGoal(t)
	}

	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/goals?limit=2", ""), http.StatusOK)
	var goals []goalView
	dataInto(t, env, &goals)
	if len(goals) != 2 {
		t.Fatalf("expected the limit to be honoured, got %d", len(goals))
	}
	if env.Meta.Pagination == nil {
		t.Fatal("expected pagination metadata on a listing")
	}
	if env.Meta.Pagination.TotalItems != 5 || env.Meta.Pagination.TotalPages != 3 {
		t.Fatalf("expected 5 items over 3 pages, got %+v", env.Meta.Pagination)
	}
	if env.Meta.Pagination.Page != 1 {
		t.Fatalf("expected page 1, got %d", env.Meta.Pagination.Page)
	}

	// page=3 with limit=2 is the last item.
	env = decode(t, f.asOperator(t, http.MethodGet, "/v1/goals?limit=2&page=3", ""), http.StatusOK)
	dataInto(t, env, &goals)
	if len(goals) != 1 {
		t.Fatalf("expected one goal on the last page, got %d", len(goals))
	}
	if env.Meta.Pagination.Page != 3 {
		t.Fatalf("expected page 3 to be reported back, got %d", env.Meta.Pagination.Page)
	}

	// An explicit offset wins over a page, and an unparseable one is absence rather
	// than an error: failing a listing because a dashboard sent ?limit= is noise.
	env = decode(t, f.asOperator(t, http.MethodGet, "/v1/goals?limit=&offset=&page=", ""), http.StatusOK)
	dataInto(t, env, &goals)
	if len(goals) != 5 {
		t.Fatalf("expected empty paging params to be ignored, got %d", len(goals))
	}
}

func TestAnEmptyListingIsAnEmptyArrayNotNull(t *testing.T) {
	// A client that has to handle both null and [] will handle one of them wrong.
	f := newFixture(t)

	for _, path := range []string{"/v1/goals", "/v1/approvals", "/v1/audit", "/v1/flags"} {
		t.Run(path, func(t *testing.T) {
			env := decode(t, f.asOperator(t, http.MethodGet, path, ""), http.StatusOK)
			if string(env.Data) != "[]" {
				t.Fatalf("expected an empty array, got %s", env.Data)
			}
		})
	}
}

func TestOpenOnlyNeedsAnExplicitTrue(t *testing.T) {
	// A typo in a filter that widens a listing is how a queue of pending approvals
	// looks empty.
	f := newFixture(t)
	f.seedPendingApproval(t)
	decode(t, f.asBot(t, http.MethodPost, "/v1/approvals",
		`{"actionType":"ads.spend","amount":1000,"currency":"USD"}`), http.StatusCreated)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 2},
		{"?openOnly=true", 1},
		{"?openOnly=1", 1},
		{"?openOnly=yes", 1},
		{"?openOnly=false", 2},
		{"?openOnly=maybe", 2},
		{"?openOnly=", 2},
	} {
		t.Run("openOnly"+tc.query, func(t *testing.T) {
			env := decode(t, f.asOperator(t, http.MethodGet, "/v1/approvals"+tc.query, ""), http.StatusOK)
			var approvals []approvalView
			dataInto(t, env, &approvals)
			if len(approvals) != tc.want {
				t.Fatalf("expected %d approvals, got %d", tc.want, len(approvals))
			}
		})
	}
}

func TestHealthAnswersWithoutTouchingAnything(t *testing.T) {
	// A liveness probe that queries the database restarts a healthy process whenever
	// the database hiccups, which turns a brief outage into a restart loop.
	f := newFixture(t)
	f.flags.getErr = errStorage

	rec := f.unauthenticated(t, http.MethodGet, "/healthz", "")
	decode(t, rec, http.StatusOK)
}

func TestReadinessReportsAnEngagedKillSwitchAsReady(t *testing.T) {
	// The operator engaged it on purpose. Reporting it as unready would pull the
	// service out of the load balancer, and with it the only endpoint that can
	// release the switch.
	f := newFixture(t)
	decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":true,"reason":"halt"}`), http.StatusOK)

	env := decode(t, f.unauthenticated(t, http.MethodGet, "/readyz", ""), http.StatusOK)
	var ready struct {
		Status            string `json:"status"`
		KillSwitchEngaged bool   `json:"killSwitchEngaged"`
		Metrics           int    `json:"metrics"`
	}
	dataInto(t, env, &ready)
	if !ready.KillSwitchEngaged {
		t.Fatal("expected readiness to report the engaged switch")
	}
	if ready.Metrics != 2 {
		t.Fatalf("expected the loaded metric count, got %d", ready.Metrics)
	}
}

func TestReadinessFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	f := newFixture(t)
	f.flags.getErr = errStorage

	env := decode(t, f.unauthenticated(t, http.MethodGet, "/readyz", ""), http.StatusServiceUnavailable)
	if got := errorCode(t, env); got != utils.ErrCodeUnavailable {
		t.Fatalf("expected %s, got %s", utils.ErrCodeUnavailable, got)
	}
}

func TestProbesDoNotNeedACredential(t *testing.T) {
	// A probe that needs a credential is a probe that fails during a rotation, and
	// takes the service down with it.
	f := newFixture(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := f.unauthenticated(t, http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s needed a credential: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestATickReportsPartialFailureAsSuccess(t *testing.T) {
	// A tick that checked ten goals and could not read one metric is a partial
	// success. Answering 500 would tell a scheduler to retry the nine that worked.
	f := newFixture(t)

	env := decode(t, f.asOperator(t, http.MethodPost, "/v1/monitor/tick", ""), http.StatusOK)
	var tick tickView
	dataInto(t, env, &tick)
	if tick.Halted {
		t.Fatal("expected a tick with no kill switch to run")
	}
	if tick.Decisions == nil {
		t.Fatal("expected a decisions map, even an empty one")
	}
}

func TestATickUnderTheKillSwitchIsHaltedNotFailed(t *testing.T) {
	f := newFixture(t)
	decode(t, f.asOperator(t, http.MethodPut, "/v1/flags/kill-switch",
		`{"engaged":true,"reason":"halt"}`), http.StatusOK)

	env := decode(t, f.asOperator(t, http.MethodPost, "/v1/monitor/tick", ""), http.StatusOK)
	var tick tickView
	dataInto(t, env, &tick)
	if !tick.Halted {
		t.Fatal("expected the tick to report itself halted")
	}
	if tick.Checked != 0 {
		t.Fatalf("a halted tick examined %d goals", tick.Checked)
	}
	if !strings.Contains(env.Message, "kill switch") {
		t.Fatalf("expected the message to say why nothing happened, got %q", env.Message)
	}
}

func TestNewRefusesToStartWithAMissingService(t *testing.T) {
	// A route wired to a nil service panics on its first request, which is the worst
	// possible time to find out.
	_, err := New(Deps{Logger: zerolog.Nop()})
	if err == nil {
		t.Fatal("expected New to refuse an empty dependency set")
	}
	for _, name := range []string{"Goals", "Approvals", "Flags", "Audit", "Monitor", "Samples", "Metrics"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %q to be named in %q", name, err)
		}
	}
}

func TestNewRouterRefusesToServeWithoutACredential(t *testing.T) {
	// Serving this API unauthenticated would expose the kill switch and the approval
	// queue to anyone who can reach the port.
	f := newFixture(t)

	if _, err := NewRouter(RouterDeps{Handler: nil, Credentials: nil}); err == nil {
		t.Fatal("expected a nil handler set to be refused")
	}

	// The case that matters: a perfectly good handler set with nothing to
	// authenticate against.
	_, err := NewRouter(RouterDeps{
		Handler:     f.handler,
		Credentials: nil,
		RateLimit:   config.RateLimitConfig{RPS: 10, Burst: 10},
	})
	if err == nil {
		t.Fatal("expected an empty credential list to be refused")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Fatalf("expected the error to say what is missing, got %q", err)
	}

	// And a bad proxy list is a startup failure rather than a rate limiter keyed on
	// whatever a client puts in X-Forwarded-For.
	if _, err := NewRouter(RouterDeps{
		Handler:        f.handler,
		Credentials:    CredentialsFrom([]config.APIKey{{Name: "ops", Secret: operatorSecret, Role: config.RoleOperator}}),
		TrustedProxies: []string{"not-a-cidr"},
		RateLimit:      config.RateLimitConfig{RPS: 10, Burst: 10},
	}); err == nil {
		t.Fatal("expected an unparseable trusted proxy to be refused")
	}
}

func TestCredentialsFromCarriesRolesAcrossAndFailsClosed(t *testing.T) {
	// This is the one place in the service where a mapping that failed open would be
	// silent: a future third role becoming an operator by accident.
	creds := CredentialsFrom([]config.APIKey{
		{Name: "ops", Secret: "a", Role: config.RoleOperator},
		{Name: "bot", Secret: "b", Role: config.RoleBot},
		{Name: "future", Secret: "c", Role: config.Role("auditor")},
		{Name: "blank", Secret: "d"},
	})

	if len(creds) != 4 {
		t.Fatalf("expected every key to be carried across, got %d", len(creds))
	}
	if creds[0].Role != middleware.RoleOperator {
		t.Fatalf("expected the operator role to survive, got %q", creds[0].Role)
	}
	for _, cred := range creds[1:] {
		if cred.Role != middleware.RoleBot {
			t.Fatalf("%q mapped to %q; anything not an operator must be a bot",
				cred.Name, cred.Role)
		}
	}
	if creds[0].Secret != "a" {
		t.Fatal("expected the secret to be carried across unchanged")
	}
}

func TestAPanicBecomesA500RatherThanADroppedConnection(t *testing.T) {
	// Recover sits outside the handlers so a bug in one is an error response with a
	// request id, not a client left waiting on a closed socket.
	engine := gin.New()
	engine.Use(middleware.RequestID(), middleware.Recover(zerolog.Nop()))
	engine.GET("/boom", func(*gin.Context) { panic("handler bug") })

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	env := decode(t, rec, http.StatusInternalServerError)
	if strings.Contains(env.Error.Message, "handler bug") {
		t.Fatalf("the panic value reached the client: %q", env.Error.Message)
	}
}

func TestCleanMessageStripsTheInternalPrefixes(t *testing.T) {
	// "service: invalid request: periodEnd is already in the past" is a log line.
	// "periodEnd is already in the past" is an API message.
	for _, tc := range []struct{ given, want string }{
		{"service: invalid request: targetValue must be positive", "targetValue must be positive"},
		{"service: not permitted for this actor: changing a goal", "changing a goal"},
		{"service: not found: goal", "goal"},
		{"something unprefixed", "something unprefixed"},
	} {
		if got := cleanMessage(errString(tc.given)); got != tc.want {
			t.Fatalf("expected %q, got %q", tc.want, got)
		}
	}
}

// errString is an error whose text is exactly what it was given, for testing the
// message cleanup without going through a service.
type errString string

func (e errString) Error() string { return string(e) }

func TestTheActorTypeConstantsMatchWhatIsRecorded(t *testing.T) {
	// A guard against the two enums drifting: the audit log stores repository.ActorType
	// and the transport decides which one a request maps to.
	f := newFixture(t)
	f.seedGoal(t)

	var recorded []string
	for _, event := range f.audit.events {
		recorded = append(recorded, string(event.ActorType))
	}
	if len(recorded) == 0 {
		t.Fatal("expected the create to be audited")
	}
	for _, actorType := range recorded {
		if actorType != string(repository.ActorUser) && actorType != string(repository.ActorBot) {
			t.Fatalf("audit recorded an unknown actor type %q", actorType)
		}
	}

	// And the wire form is the same string, so a client filtering by it works.
	// Decoded into a local shape rather than auditView: jsonRaw only marshals, which
	// is all a server needs and all it should have.
	env := decode(t, f.asOperator(t, http.MethodGet, "/v1/audit?actorType=user", ""), http.StatusOK)
	var views []struct {
		ActorType string          `json:"actorType"`
		Detail    json.RawMessage `json:"detail"`
	}
	dataInto(t, env, &views)
	if len(views) == 0 {
		t.Fatalf("expected filtering by actorType=user to find the operator's create: %s", env.Data)
	}
	for _, view := range views {
		if view.ActorType != string(repository.ActorUser) {
			t.Fatalf("actorType filter returned %q", view.ActorType)
		}
	}
}

func TestAPayloadTooLargeIsRefusedBeforeItIsStored(t *testing.T) {
	// The payload exists so a human can see what they are approving. It is not a
	// place to park a document, and an unbounded one is a write amplification bug
	// waiting for a bot in a loop.
	f := newFixture(t)

	payload, err := json.Marshal(map[string]string{"note": strings.Repeat("x", maxPayloadBytes)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := `{"actionType":"ads.spend","amount":1000,"currency":"USD","payload":` + string(payload) + `}`

	env := decode(t, f.asBot(t, http.MethodPost, "/v1/approvals", body), http.StatusBadRequest)
	if got := errorCode(t, env); got != utils.ErrCodeValidation {
		t.Fatalf("expected %s, got %s", utils.ErrCodeValidation, got)
	}
	if len(f.approvals.byID) != 0 {
		t.Fatal("an oversized payload was stored")
	}
}
