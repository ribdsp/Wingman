package middleware

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/utils"
)

const (
	operatorSecret = "core-operator-key-0123456789abc"
	botSecret      = "core-bot-key-0123456789abcdefgh"
)

func testCredentials() []Credential {
	return []Credential{
		{Name: "ops", Secret: operatorSecret, Role: RoleOperator},
		{Name: "bridge", Secret: botSecret, Role: RoleBot},
	}
}

// fakeSessions is a SessionStore backed by a map. It records every token it was handed
// so a test can prove the plaintext never crossed the boundary, and how many times it
// was consulted at all — which is how the shape-based routing is checked.
type fakeSessions struct {
	live map[string]Session
	err  error
	seen []string
}

func newSessions() *fakeSessions { return &fakeSessions{live: map[string]Session{}} }

func (f *fakeSessions) ResolveSession(_ context.Context, storedToken string, _ time.Time) (Session, error) {
	f.seen = append(f.seen, storedToken)
	if f.err != nil {
		return Session{}, f.err
	}
	session, ok := f.live[storedToken]
	if !ok {
		return Session{}, ErrNoSession
	}
	return session, nil
}

// mint registers a live session and returns the token its holder presents.
func (f *fakeSessions) mint(t *testing.T, session Session) string {
	t.Helper()
	plaintext, stored, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("could not mint a session token: %v", err)
	}
	f.live[stored] = session
	return plaintext
}

// deadToken is a well-formed session token nobody has a session for: revoked, expired
// or invented. The three are indistinguishable from here on purpose.
func deadToken(t *testing.T) string {
	t.Helper()
	plaintext, _, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("could not mint a session token: %v", err)
	}
	return plaintext
}

func TestAuthenticate_bearerKeyNamesThePrincipalAndItsRole(t *testing.T) {
	// The name is the point: it is what the audit log records against whatever the
	// request goes on to do. The role is what decides whether it may.
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+operatorSecret)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	data := dataOf(t, rec)
	if data["principal"] != "ops" || data["role"] != string(RoleOperator) {
		t.Fatalf("expected ops/operator, got %v/%v", data["principal"], data["role"])
	}
}

func TestAuthenticate_acceptsTheSchemeInAnyCase(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req := newRequest(t, "/v1/chats")
		req.Header.Set(HeaderAuthorization, scheme+" "+operatorSecret)

		rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))
		if rec.Code != http.StatusOK {
			t.Fatalf("scheme %q: expected 200, got %d", scheme, rec.Code)
		}
	}
}

func TestAuthenticate_acceptsTheAPIKeyHeader(t *testing.T) {
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, botSecret)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if data := dataOf(t, rec); data["principal"] != "bridge" || data["role"] != string(RoleBot) {
		t.Fatalf("expected bridge/bot, got %v/%v", data["principal"], data["role"])
	}
}

func TestAuthenticate_doesNotLetASecondHeaderRescueABadFirstOne(t *testing.T) {
	// A proxy that sets Authorization must not be sidesteppable by also sending
	// X-API-Key. A malformed Authorization header is a failure, not an invitation to
	// look elsewhere.
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Basic "+operatorSecret)
	req.Header.Set(HeaderAPIKey, operatorSecret)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
}

func TestAuthenticate_rejectsTheRequestsItShould(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"nothing presented", nil},
		{"an unknown key", map[string]string{HeaderAuthorization: "Bearer wrong-key-0123456789abcdefghijkl"}},
		{"the wrong scheme", map[string]string{HeaderAuthorization: "Basic " + operatorSecret}},
		{"a bare token", map[string]string{HeaderAuthorization: operatorSecret}},
		{"an empty bearer", map[string]string{HeaderAuthorization: "Bearer "}},
		{"an empty api key", map[string]string{HeaderAPIKey: ""}},
		{"a key with trailing junk", map[string]string{HeaderAPIKey: operatorSecret + "x"}},
		{"a truncated key", map[string]string{HeaderAPIKey: operatorSecret[:len(operatorSecret)-1]}},
		{"a session token that looks the part", map[string]string{HeaderAPIKey: "wgm_not-a-real-token"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := newRequest(t, "/v1/chats")
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}

			rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
			}
			body := decode(t, rec)
			if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeUnauthorized {
				t.Fatalf("unexpected envelope: %+v", body)
			}
		})
	}
}

func TestAuthenticate_tellsTheCallerNothingUsefulAboutWhy(t *testing.T) {
	// One message for every failure. Distinguishing "no such key" from "wrong key" —
	// or, worse here, "that is not a key" from "that session expired" — hands an
	// attacker a working oracle.
	sessions := newSessions()
	build := func() gin.HandlerFunc { return Authenticate(testCredentials(), sessions, discardLog()) }

	missing := serve(t, newRequest(t, "/v1/chats"), RequestID(), build())

	wrongKeyReq := newRequest(t, "/v1/chats")
	wrongKeyReq.Header.Set(HeaderAPIKey, "wrong-key-0123456789abcdefghijkl")
	wrongKey := serve(t, wrongKeyReq, RequestID(), build())

	deadSessionReq := newRequest(t, "/v1/chats")
	deadSessionReq.Header.Set(HeaderAPIKey, deadToken(t))
	deadSession := serve(t, deadSessionReq, RequestID(), build())

	want := decode(t, missing).Error.Message
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a wrong key":    wrongKey,
		"a dead session": deadSession,
	} {
		if got := decode(t, rec).Error.Message; got != want {
			t.Fatalf("%s answers %q where a missing credential answers %q", name, got, want)
		}
	}
}

func TestAuthenticate_neverEchoesACredential(t *testing.T) {
	// A 401 body that quotes what was sent ends up in a browser history, a proxy log,
	// or a screenshot.
	almost := operatorSecret + "-almost"
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, almost)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if strings.Contains(rec.Body.String(), operatorSecret) {
		t.Fatalf("the response leaked a credential: %s", rec.Body)
	}

	// And the same for a session token, which is just as live a secret.
	token := deadToken(t)
	sessionReq := newRequest(t, "/v1/chats")
	sessionReq.Header.Set(HeaderAuthorization, "Bearer "+token)

	sessionRec := serve(t, sessionReq, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if strings.Contains(sessionRec.Body.String(), token) {
		t.Fatalf("the response leaked a session token: %s", sessionRec.Body)
	}
}

func TestAuthenticate_aLiveSessionBecomesTheUserBehindIt(t *testing.T) {
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_7", Name: "rani"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	data := dataOf(t, rec)
	if data["principal"] != "rani" || data["role"] != string(RoleUser) || data["userId"] != "usr_7" {
		t.Fatalf("expected rani/user/usr_7, got %v/%v/%v", data["principal"], data["role"], data["userId"])
	}
}

func TestAuthenticate_aSessionWithNoNameStillIdentifiesTheActor(t *testing.T) {
	// The audit log needs something to record. An id is worse to read than a handle
	// and better than a blank.
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_9"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

	if got := dataOf(t, rec)["principal"]; got != "user:usr_9" {
		t.Fatalf("expected user:usr_9, got %v", got)
	}
}

func TestAuthenticate_hashesTheTokenBeforeTheStoreSeesIt(t *testing.T) {
	// A plaintext session token reaching the repository layer would end up in a slow
	// query log, and a slow query log is not where live credentials belong.
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_1", Name: "adi"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)
	serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

	if len(sessions.seen) != 1 {
		t.Fatalf("expected the store consulted once, got %d times", len(sessions.seen))
	}
	presented := sessions.seen[0]
	if strings.Contains(presented, token) || strings.Contains(presented, strings.TrimPrefix(token, "wgm_")) {
		t.Fatalf("the store was handed the plaintext token: %q", presented)
	}
	if _, err := hex.DecodeString(presented); err != nil || len(presented) != 64 {
		t.Fatalf("expected a sha-256 digest, got %q", presented)
	}
}

func TestAuthenticate_aDeadSessionIsRejectedNotExcused(t *testing.T) {
	sessions := newSessions()

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+deadToken(t))

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
	if body := decode(t, rec); body.Error == nil || body.Error.Code != utils.ErrCodeUnauthorized {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}

func TestAuthenticate_anUnreadableSessionStoreIsAnOutageNotAWrongPassword(t *testing.T) {
	// Answering 401 here would show the person a sign-in screen for a database
	// outage, and would show the operator a spike in failed authentications instead
	// of a broken database.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	sessions := newSessions()
	sessions.err = errors.New("dial tcp 10.0.0.4:5432: connect: connection refused")

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+deadToken(t))

	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, log))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeUnavailable {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	// The address of the database is not the caller's business.
	if strings.Contains(rec.Body.String(), "10.0.0.4") || strings.Contains(rec.Body.String(), "dial tcp") {
		t.Fatalf("the response leaked the driver error: %s", rec.Body)
	}
	if got := entryFrom(t, &logged)["level"]; got != "error" {
		t.Fatalf("expected the outage logged at error, got %v", got)
	}
}

func TestAuthenticate_aSessionWithNoUserIsARefusalNotACaller(t *testing.T) {
	// A Caller whose UserID is empty would make every "scope this query to the
	// caller" clause match nobody — or, one refactor later, everybody.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	sessions := newSessions()
	token := sessions.mint(t, Session{Name: "nobody"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	var reached bool
	rec := serveWith(t, req, func(c *gin.Context) {
		reached = true
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), Authenticate(testCredentials(), sessions, log))

	if reached {
		t.Fatal("the handler ran for a session with no user behind it")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body)
	}
	if body := decode(t, rec); body.Error == nil || body.Error.Code != utils.ErrCodeInternal {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	if got := entryFrom(t, &logged)["level"]; got != "error" {
		t.Fatalf("expected the broken session logged at error, got %v", got)
	}
}

func TestAuthenticate_anEnvironmentKeyCanNeverBecomeAUser(t *testing.T) {
	// This is the invariant the whole two-path design exists for. A key in the
	// environment is a machine, whatever it presents itself as, and the session store
	// is not even asked about it.
	sessions := newSessions()

	for _, secret := range []string{operatorSecret, botSecret} {
		req := newRequest(t, "/v1/chats")
		req.Header.Set(HeaderAPIKey, secret)

		rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

		data := dataOf(t, rec)
		if data["role"] == string(RoleUser) {
			t.Fatalf("an environment key authenticated as a user: %v", data)
		}
		if data["userId"] != "" {
			t.Fatalf("an environment key carried a user id: %v", data["userId"])
		}
	}
	if len(sessions.seen) != 0 {
		t.Fatalf("the session store was consulted for a machine key: %v", sessions.seen)
	}
}

func TestAuthenticate_aSessionTokenIsNeverComparedAgainstTheKeyList(t *testing.T) {
	// The other direction of the same invariant: a token shaped like a session goes
	// down the session path and stays there, so no amount of guessing at it can land
	// on an operator key.
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_3", Name: "sari"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)
	rec := serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()))

	if got := dataOf(t, rec)["role"]; got != string(RoleUser) {
		t.Fatalf("expected a user, got %v", got)
	}
	if len(sessions.seen) != 1 {
		t.Fatalf("expected the session path taken, store consulted %d times", len(sessions.seen))
	}
}

func TestAuthenticate_ignoresARoleTheCallerClaims(t *testing.T) {
	// A bot that sends X-Role: operator is still a bot.
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, botSecret)
	req.Header.Set("X-Role", "operator")
	req.Header.Set("X-Principal", "ops")
	req.Header.Set("X-User-Id", "usr_1")

	var seen Caller
	serveWith(t, req, func(c *gin.Context) {
		seen, _ = CallerOf(c)
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()))

	if seen.Role != RoleBot || seen.Name != "bridge" || seen.UserID != "" {
		t.Fatalf("the request talked its way into %s/%s/%s", seen.Name, seen.Role, seen.UserID)
	}
	if seen.IsOperator() || seen.IsUser() {
		t.Fatal("a bot reported itself as an operator or a user")
	}
}

func TestAuthenticate_refusesAWiringMistakeAtStartup(t *testing.T) {
	// Each of these serves requests wrongly rather than not at all, which is the
	// worse failure. They stop the process instead.
	sessions := newSessions()
	confusable := deadToken(t)

	cases := []struct {
		name     string
		creds    []Credential
		sessions SessionStore
	}{
		{"no credentials at all", nil, sessions},
		{"no session store", testCredentials(), nil},
		{"a key with no role", []Credential{{Name: "roleless", Secret: operatorSecret}}, sessions},
		{"a key claiming to be a user", []Credential{{Name: "impostor", Secret: operatorSecret, Role: RoleUser}}, sessions},
		{"a key shaped like a session token", []Credential{{Name: "confusable", Secret: confusable, Role: RoleOperator}}, sessions},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			Authenticate(c.creds, c.sessions, discardLog())
		})
	}
}

func TestAuthenticate_buildsWithAWellFormedConfiguration(t *testing.T) {
	// The counterpart to the table above: the ordinary case must not panic.
	Authenticate(testCredentials(), newSessions(), discardLog())
}

func TestMatchCredential_findsTheRightKeyWhereverItSits(t *testing.T) {
	creds := []Credential{
		{Name: "first", Secret: "aaaa-key-0123456789abcdefghijkl", Role: RoleOperator},
		{Name: "middle", Secret: "bbbb-key-0123456789abcdefghijkl", Role: RoleBot},
		{Name: "last", Secret: "cccc-key-0123456789abcdefghijkl", Role: RoleOperator},
	}

	for _, want := range creds {
		got, ok := matchCredential(creds, want.Secret)
		if !ok || got.Name != want.Name {
			t.Fatalf("expected %q, got %q (found=%v)", want.Name, got.Name, ok)
		}
	}
	if _, ok := matchCredential(creds, "dddd-key-0123456789abcdefghijkl"); ok {
		t.Fatal("expected no match")
	}
	if _, ok := matchCredential(creds, ""); ok {
		t.Fatal("expected an empty token not to match")
	}
	if _, ok := matchCredential(nil, "anything"); ok {
		t.Fatal("expected no match against no credentials")
	}
}

func TestCallerOf_reportsNobodyOnAnUnauthenticatedRequest(t *testing.T) {
	if _, ok := CallerOf(nil); ok {
		t.Fatal("expected no caller for a nil context")
	}

	c, _ := gin.CreateTestContext(nil)
	if _, ok := CallerOf(c); ok {
		t.Fatal("expected no caller before auth runs")
	}

	// Something other than a Caller under the key must not be read as one.
	c.Set(ContextKeyCaller, "ops")
	if _, ok := CallerOf(c); ok {
		t.Fatal("expected a non-Caller value to be refused")
	}
}

func TestPrincipal_isEmptyOnAnUnauthenticatedRequest(t *testing.T) {
	if got := Principal(nil); got != "" {
		t.Fatalf("expected an empty principal for a nil context, got %q", got)
	}
}
