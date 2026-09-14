package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The wire itself: the goal engine's contract, headers, malformed input, and the routes a
// probe reaches without a credential.
//
// The first two tests here are the contract the plan calls out. Core's dispatch response is
// consumed by another service that is already written and must not be touched, so "does the
// engine's client understand this?" is a question with a definite answer — and it is
// answered by replicating that client's parsing rules below rather than by reading them and
// hoping.

// taskEnvelope mirrors the struct of the same name in goal-engine/internal/core/client.go.
//
// Copied rather than imported: the goal engine is a separate Go module, and importing it
// would couple two services that deploy independently — the same reasoning that had the
// response envelope copied instead of shared. Copied *verbatim* so a diff between the two
// files is obvious to a reader who suspects they have drifted.
type taskEnvelope struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	Data   struct {
		ID     string `json:"id"`
		TaskID string `json:"taskId"`
	} `json:"data"`
}

// extractTaskID is the engine client's rule: the first non-empty of four places a task id
// might be. Core answers at data.id, which is the third — the other three are tolerated by
// the client and are not what core sends.
func extractTaskID(env taskEnvelope) string {
	for _, candidate := range []string{env.ID, env.TaskID, env.Data.ID, env.Data.TaskID} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// retryableStatus is the engine client's retry rule, copied for the same reason.
//
// It matters here because the statuses core answers with decide whether a trigger that was
// refused gets retried. A permanent refusal inside this set would make the engine spin
// against a request that can never succeed.
func retryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= http.StatusInternalServerError
}

func parseTaskEnvelope(t *testing.T, rec *httptest.ResponseRecorder) taskEnvelope {
	t.Helper()

	var env taskEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the engine's client could not parse this body: %v; body: %s", err, rec.Body.String())
	}
	return env
}

func TestDispatch_answersWhatTheGoalEnginesClientReads(t *testing.T) {
	// Arrange
	f := newFixture(t)
	const key = "goal-42:2026-09-12T09:00:00Z"
	body := dispatchRequest{
		BotID:     "sales",
		ChannelID: "ops",
		Brief:     "revenue is 18% behind pace with 9 days left in the period",
		Metadata:  map[string]string{"goalId": "goal-42"},
	}

	// Act: the engine sends the key as a header, deliberately not in the query string, so it
	// does not end up in core's access log.
	first := f.doWith(http.MethodPost, "/api/v1/tasks", botSecret, body,
		map[string]string{HeaderIdempotencyKey: key})

	// Assert
	firstEnv := decode(t, first, http.StatusCreated)
	firstID := extractTaskID(parseTaskEnvelope(t, first))
	if firstID == "" {
		t.Fatalf("the engine's client would find no task id in: %s", first.Body.String())
	}

	// The id it finds is the one core meant to send, not something that happened to land in
	// one of the other three fields the client tolerates.
	var task taskView
	dataInto(t, firstEnv, &task)
	if firstID != task.ID {
		t.Errorf("client reads task id %q, core rendered %q", firstID, task.ID)
	}

	// Act again: the same trigger, retried after a timeout the engine saw and core did not.
	second := f.doWith(http.MethodPost, "/api/v1/tasks", botSecret, body,
		map[string]string{HeaderIdempotencyKey: key})

	// Assert: 200 rather than 201, the same task, and no second run.
	decode(t, second, http.StatusOK)
	if secondID := extractTaskID(parseTaskEnvelope(t, second)); secondID != firstID {
		t.Errorf("retry produced task %q, want the original %q", secondID, firstID)
	}
	if filed := f.tasks.count(); filed != 1 {
		t.Errorf("tasks filed = %d, want 1: a retried trigger became a second run", filed)
	}
}

func TestDispatch_readsTheIdempotencyKeyFromEitherPlace(t *testing.T) {
	// Arrange
	f := newFixture(t)
	brief := "check yesterday's numbers"

	cases := map[string]struct {
		body    dispatchRequest
		headers map[string]string
	}{
		"header only": {
			body:    dispatchRequest{Brief: brief},
			headers: map[string]string{HeaderIdempotencyKey: "key-header"},
		},
		"body only": {
			body: dispatchRequest{Brief: brief, IdempotencyKey: "key-body"},
		},
		// A proxy that adds a trailing space to a header must not turn every retry into a
		// 400, so both sides are trimmed before they are compared.
		"both, equal but for whitespace": {
			body:    dispatchRequest{Brief: brief, IdempotencyKey: "key-both"},
			headers: map[string]string{HeaderIdempotencyKey: "  key-both  "},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			rec := f.doWith(http.MethodPost, "/api/v1/tasks", botSecret, tc.body, tc.headers)

			// Assert
			var task taskView
			dataInto(t, decode(t, rec, http.StatusCreated), &task)
			if task.IdempotencyKey == "" {
				t.Errorf("task = %+v, want the key recorded on it", task)
			}
			if strings.TrimSpace(task.IdempotencyKey) != task.IdempotencyKey {
				t.Errorf("stored key %q was not trimmed", task.IdempotencyKey)
			}
		})
	}
}

func TestDispatch_refusesWhenTheTwoKeysDisagreeOrNeitherIsSent(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act + Assert: answered rather than resolved by picking one. Choosing a winner would
	// mean the caller's retry logic and core's deduplication key on different strings, which
	// is exactly the state in which a retry becomes a second run.
	mismatch := f.doWith(http.MethodPost, "/api/v1/tasks", botSecret,
		dispatchRequest{Brief: "work", IdempotencyKey: "from-the-body"},
		map[string]string{HeaderIdempotencyKey: "from-the-header"})
	assertErrorCode(t, mismatch, http.StatusBadRequest, "VALIDATION_ERROR")

	// Absent entirely: a caller that does not send one is told, not quietly given
	// at-least-once semantics.
	missing := f.asBot(http.MethodPost, "/api/v1/tasks", dispatchRequest{Brief: "work"})
	env := decode(t, missing, http.StatusBadRequest)
	if env.Error == nil || env.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("error = %+v, want VALIDATION_ERROR", env.Error)
	}
	if env.Error.Fields["idempotencyKey"] == "" {
		t.Errorf("error.fields = %v, want the field named so a caller can fix it", env.Error.Fields)
	}

	if filed := f.tasks.count(); filed != 0 {
		t.Errorf("tasks filed = %d, want 0", filed)
	}
}

func TestDispatch_permanentRefusalsAreOutsideTheEnginesRetrySet(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")

	cases := map[string]*httptest.ResponseRecorder{
		"no idempotency key": f.asBot(http.MethodPost, "/api/v1/tasks", dispatchRequest{Brief: "work"}),
		"a person dispatching": f.do(http.MethodPost, "/api/v1/tasks", person,
			dispatchRequest{Brief: "work", IdempotencyKey: "k-1"}),
		"no credential": f.unauthenticated(http.MethodPost, "/api/v1/tasks",
			dispatchRequest{Brief: "work", IdempotencyKey: "k-1"}),
	}

	// Act + Assert
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			// None of the three can be made to succeed by sending it again. A status inside
			// the engine's retry set would have it retry on a cooldown for the rest of the
			// period instead of surfacing the misconfiguration.
			if retryableStatus(rec.Code) {
				t.Errorf("status %d is retryable; the engine would spin on a permanent refusal", rec.Code)
			}
		})
	}
}

func TestMalformedJSON_isRefusedWithoutNamingThisServicesInternals(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act: a truncated body, sent verbatim rather than marshalled.
	rec := f.doWith(http.MethodPost, "/api/v1/tasks", botSecret, `{"brief": "work", `,
		map[string]string{HeaderIdempotencyKey: "k-1"})

	// Assert
	assertErrorCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
	// Go's JSON errors name struct fields and types. That describes core's internals rather
	// than the caller's mistake, and an error message is a cheap way to learn the shape of a
	// service you are probing.
	assertBodyOmits(t, rec, map[string]string{
		"Go type name":          "dispatchRequest",
		"decoder's own wording": "unmarshal",
	})
}

func TestUnknownPathAndWrongMethod_bothAnswerWithTheNotFoundCode(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act + Assert: an unknown path is 404.
	unknown := f.asOperator(http.MethodGet, "/v1/does-not-exist", nil)
	env := decode(t, unknown, http.StatusNotFound)
	if env.Error == nil || env.Error.Code != "NOT_FOUND" {
		t.Fatalf("error = %+v, want NOT_FOUND", env.Error)
	}
	if env.Error.Message != "No such endpoint." {
		t.Errorf("message = %q, want %q", env.Error.Message, "No such endpoint.")
	}

	// A known path with the wrong method is 405, and it reuses NOT_FOUND rather than
	// introducing a tenth published code — the nine are a documented contract, and "the
	// method is wrong" is already in the message.
	for _, rec := range []*httptest.ResponseRecorder{
		f.unauthenticated(http.MethodDelete, "/healthz", nil),
		f.asBot(http.MethodDelete, "/v1/tasks", nil),
	} {
		assertErrorCode(t, rec, http.StatusMethodNotAllowed, "NOT_FOUND")
	}
}

func TestCredentials_bothHeadersWorkAndAWrongSchemeDoesNot(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")

	cases := map[string]struct {
		headers    map[string]string
		wantStatus int
	}{
		"bearer": {
			headers:    map[string]string{"Authorization": "Bearer " + operatorSecret},
			wantStatus: http.StatusOK,
		},
		// Lower-cased scheme: a client is not wrong for sending one.
		"bearer, lower case": {
			headers:    map[string]string{"Authorization": "bearer " + operatorSecret},
			wantStatus: http.StatusOK,
		},
		"x-api-key": {
			headers:    map[string]string{"X-API-Key": operatorSecret},
			wantStatus: http.StatusOK,
		},
		// A session token is told apart from a machine key by its shape, not by which header
		// carried it, so either header authenticates a person too.
		"session token in x-api-key": {
			headers:    map[string]string{"X-API-Key": person},
			wantStatus: http.StatusOK,
		},
		// A credential under no scheme at all is a client bug, not an alternative spelling.
		"no scheme": {
			headers:    map[string]string{"Authorization": operatorSecret},
			wantStatus: http.StatusUnauthorized,
		},
		"wrong scheme": {
			headers:    map[string]string{"Authorization": "Basic " + operatorSecret},
			wantStatus: http.StatusUnauthorized,
		},
		// Authorization wins when both are present, so a proxy that adds one cannot be
		// sidestepped by also sending the other.
		"a broken Authorization is not rescued by X-API-Key": {
			headers: map[string]string{
				"Authorization": "Bearer not-a-configured-key",
				"X-API-Key":     operatorSecret,
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			rec := f.doWith(http.MethodGet, "/v1/reference", "", nil, tc.headers)

			// Assert
			if tc.wantStatus == http.StatusOK {
				decode(t, rec, http.StatusOK)
				return
			}
			assertErrorCode(t, rec, tc.wantStatus, "UNAUTHORIZED")
		})
	}
}

func TestAnUnreadableSessionStoreIs503_notAFailedSignIn(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")
	f.sessions.resolve = errBoom

	// Act
	rec := f.do(http.MethodGet, "/v1/me", person, nil)

	// Assert
	//
	// A database that cannot be read is not a wrong password. A 401 here would show the
	// person a sign-in screen for an outage, and would show the operator a spike in failed
	// authentications instead of a broken database.
	assertErrorCode(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	assertBodyOmits(t, rec, map[string]string{"cause of the failure": "boom"})
}

func TestHealthAndReadiness_needNoCredential_andReadinessKeepsItsCauseToItself(t *testing.T) {
	// Arrange
	healthy := newFixture(t)
	broken := newFixture(t, withBrokenReadiness)

	// Act + Assert
	//
	// Both probes are outside the authenticated half of the router: a probe that needs a
	// credential is a probe that fails during a credential rotation.
	decode(t, healthy.unauthenticated(http.MethodGet, "/healthz", nil), http.StatusOK)
	decode(t, healthy.unauthenticated(http.MethodGet, "/readyz", nil), http.StatusOK)

	// Liveness stays 200 even when a dependency is down. A liveness probe that fails with
	// its database gets the container restarted instead of the database fixed.
	decode(t, broken.unauthenticated(http.MethodGet, "/healthz", nil), http.StatusOK)

	notReady := broken.unauthenticated(http.MethodGet, "/readyz", nil)
	assertErrorCode(t, notReady, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	// A readiness body is one of the easier things to end up on a public status page, and a
	// failing pool's error carries the DSN — password included.
	assertBodyOmits(t, notReady, map[string]string{
		"DSN":               brokenReadinessDSN,
		"database password": "hunter2",
		"database host":     "core-db",
	})
}
