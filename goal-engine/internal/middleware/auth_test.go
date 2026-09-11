package middleware

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

const (
	testSecret  = "engine-key-0123456789abcdefghijkl"
	otherSecret = "second-key-0123456789abcdefghijkl"
)

func testCredentials() []Credential {
	return []Credential{
		{Name: "ops", Secret: testSecret, Role: RoleOperator},
		{Name: "ci", Secret: otherSecret, Role: RoleBot},
	}
}

func TestAPIKeyAuthAcceptsABearerTokenAndNamesThePrincipal(t *testing.T) {
	// The name is the point: it is what the audit log records against whatever the
	// request goes on to do.
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAuthorization, "Bearer "+testSecret)

	rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	data, ok := decode(t, rec).Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data: %v", decode(t, rec).Data)
	}
	if data["principal"] != "ops" {
		t.Fatalf("expected ops, got %v", data["principal"])
	}
}

func TestAPIKeyAuthAcceptsTheSchemeInAnyCase(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req := newRequest(t, "/goals")
		req.Header.Set(HeaderAuthorization, scheme+" "+testSecret)

		rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))
		if rec.Code != http.StatusOK {
			t.Fatalf("scheme %q: expected 200, got %d", scheme, rec.Code)
		}
	}
}

func TestAPIKeyAuthAcceptsTheAPIKeyHeader(t *testing.T) {
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, otherSecret)

	rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	data := decode(t, rec).Data.(map[string]any)
	if data["principal"] != "ci" {
		t.Fatalf("expected ci, got %v", data["principal"])
	}
}

func TestAPIKeyAuthDoesNotLetASecondHeaderRescueABadFirstOne(t *testing.T) {
	// A proxy that sets Authorization must not be sidesteppable by also sending
	// X-API-Key. A malformed Authorization header is a failure, not an invitation
	// to look elsewhere.
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAuthorization, "Basic "+testSecret)
	req.Header.Set(HeaderAPIKey, testSecret)

	rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
}

func TestAPIKeyAuthRejectsTheRequestsItShould(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"nothing presented", nil},
		{"an unknown key", map[string]string{HeaderAuthorization: "Bearer wrong-key-0123456789abcdefghijkl"}},
		{"the wrong scheme", map[string]string{HeaderAuthorization: "Basic " + testSecret}},
		{"a bare token", map[string]string{HeaderAuthorization: testSecret}},
		{"an empty bearer", map[string]string{HeaderAuthorization: "Bearer "}},
		{"an empty api key", map[string]string{HeaderAPIKey: ""}},
		{"a key with trailing junk", map[string]string{HeaderAPIKey: testSecret + "x"}},
		{"a truncated key", map[string]string{HeaderAPIKey: testSecret[:len(testSecret)-1]}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := newRequest(t, "/goals")
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}

			rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

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

func TestAPIKeyAuthTellsTheCallerNothingUsefulAboutWhy(t *testing.T) {
	// One message for every failure. Distinguishing "no such key" from "wrong key"
	// hands an attacker a working oracle.
	missing := serve(t, newRequest(t, "/goals"), RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	wrongReq := newRequest(t, "/goals")
	wrongReq.Header.Set(HeaderAPIKey, "wrong-key-0123456789abcdefghijkl")
	wrong := serve(t, wrongReq, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if got, want := decode(t, wrong).Error.Message, decode(t, missing).Error.Message; got != want {
		t.Fatalf("the failure messages differ: %q vs %q", got, want)
	}
}

func TestAPIKeyAuthNeverEchoesACredential(t *testing.T) {
	// A 401 body that quotes what was sent ends up in a browser history, a proxy
	// log, or a screenshot.
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, testSecret+"-almost")

	rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if strings.Contains(rec.Body.String(), testSecret) {
		t.Fatalf("the response leaked a credential: %s", rec.Body)
	}
}

func TestAPIKeyAuthRefusesToBeBuiltWithNoCredentials(t *testing.T) {
	// A wiring mistake that accepts nothing should fail at startup, not turn into
	// a day of debugging 401s.
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	APIKeyAuth(nil, discardLog())
}

func TestPrincipalIsEmptyOnAnUnauthenticatedRequest(t *testing.T) {
	if got := Principal(nil); got != "" {
		t.Fatalf("expected an empty principal for a nil context, got %q", got)
	}
}

func TestMatchCredentialFindsTheRightKeyWhereverItSits(t *testing.T) {
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

func TestAPIKeyAuthCarriesTheRoleTheKeyWasConfiguredWith(t *testing.T) {
	// The role decides whether the caller may clear a spend, so it has to come from
	// the operator's env file and nowhere else.
	cases := []struct {
		secret string
		name   string
		role   Role
	}{
		{testSecret, "ops", RoleOperator},
		{otherSecret, "ci", RoleBot},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := newRequest(t, "/goals")
			req.Header.Set(HeaderAPIKey, c.secret)

			var seen Caller
			var found bool
			serveWith(t, req, func(ctx *gin.Context) {
				seen, found = CallerOf(ctx)
				utils.Success(ctx, http.StatusOK, "ok", nil)
			}, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

			if !found {
				t.Fatal("expected a caller on the context")
			}
			if seen.Name != c.name || seen.Role != c.role {
				t.Fatalf("expected %s/%s, got %s/%s", c.name, c.role, seen.Name, seen.Role)
			}
			if seen.IsOperator() != (c.role == RoleOperator) {
				t.Fatalf("IsOperator disagrees with role %s", c.role)
			}
		})
	}
}

func TestAPIKeyAuthIgnoresARoleTheCallerClaims(t *testing.T) {
	// A bot that sends X-Role: operator is still a bot.
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, otherSecret)
	req.Header.Set("X-Role", "operator")
	req.Header.Set("X-Principal", "ops")

	var seen Caller
	serveWith(t, req, func(ctx *gin.Context) {
		seen, _ = CallerOf(ctx)
		utils.Success(ctx, http.StatusOK, "ok", nil)
	}, RequestID(), APIKeyAuth(testCredentials(), discardLog()))

	if seen.Role != RoleBot || seen.Name != "ci" {
		t.Fatalf("the request talked its way into %s/%s", seen.Name, seen.Role)
	}
}

func TestAPIKeyAuthRefusesToBeBuiltWithARolelessKey(t *testing.T) {
	// A key with no role fails every operator check with a confusing 403. It is a
	// wiring mistake, so it fails at startup.
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	APIKeyAuth([]Credential{{Name: "nameless", Secret: testSecret}}, discardLog())
}

func TestCallerOfReportsNobodyOnAnUnauthenticatedRequest(t *testing.T) {
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
