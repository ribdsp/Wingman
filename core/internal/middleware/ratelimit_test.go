package middleware

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestRateLimit_allowsABurstThenThrottles(t *testing.T) {
	limiter := RateLimit(1, 3, KeyByIP, discardLog())

	for i := 0; i < 3; i++ {
		if rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), limiter); rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), limiter)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the burst is spent, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeRateLimited {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}

func TestRateLimit_keepsCallersApart(t *testing.T) {
	// One noisy caller must not lock everybody else out.
	limiter := RateLimit(1, 1, KeyByIP, discardLog())

	if rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}
	if rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), limiter); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the same caller throttled, got %d", rec.Code)
	}

	other := newRequest(t, "/v1/chats")
	other.RemoteAddr = "198.51.100.4:12345"
	if rec := serve(t, other, RequestID(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected a different caller through, got %d", rec.Code)
	}
}

func TestRateLimit_appliesBeforeAuthenticationSoASignInFloodIsCheap(t *testing.T) {
	// Every sign-in attempt costs 64 MiB of argon2. The limiter has to be able to
	// refuse one without a credential having been checked, which means keying on the
	// address and running in front of Authenticate.
	limiter := RateLimit(1, 1, KeyByIP, discardLog())

	if rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}

	sessions := newSessions()
	var reached bool
	rec := serveWith(t, newRequest(t, "/v1/chats"), func(c *gin.Context) {
		reached = true
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), limiter, Authenticate(testCredentials(), sessions, discardLog()))

	if reached {
		t.Fatal("the handler ran for a throttled request")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if len(sessions.seen) != 0 {
		t.Fatal("a throttled request still reached the session store")
	}
}

func TestKeyByPrincipal_prefersTheCredentialOverTheAddress(t *testing.T) {
	// Behind a proxy every request shares one address, so the credential is the only
	// key that discriminates.
	limiter := RateLimit(1, 1, KeyByPrincipal, discardLog())
	creds := testCredentials()
	sessions := newSessions()
	authenticate := func() gin.HandlerFunc { return Authenticate(creds, sessions, discardLog()) }

	first := newRequest(t, "/v1/chats")
	first.Header.Set(HeaderAPIKey, operatorSecret)
	if rec := serve(t, first, RequestID(), authenticate(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}

	again := newRequest(t, "/v1/chats")
	again.Header.Set(HeaderAPIKey, operatorSecret)
	if rec := serve(t, again, RequestID(), authenticate(), limiter); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the same credential throttled, got %d", rec.Code)
	}

	// Same address, different credential.
	second := newRequest(t, "/v1/chats")
	second.Header.Set(HeaderAPIKey, botSecret)
	if rec := serve(t, second, RequestID(), authenticate(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected a different credential through, got %d", rec.Code)
	}
}

func TestKeyByPrincipal_keepsTwoPeopleOnOneAddressApart(t *testing.T) {
	// A household, an office, or a phone behind carrier NAT: two accounts share one
	// address routinely, and one of them being busy must not sign the other out.
	limiter := RateLimit(1, 1, KeyByPrincipal, discardLog())
	sessions := newSessions()
	first := sessions.mint(t, Session{UserID: "usr_1", Name: "adi"})
	second := sessions.mint(t, Session{UserID: "usr_2", Name: "rani"})
	authenticate := func() gin.HandlerFunc { return Authenticate(testCredentials(), sessions, discardLog()) }

	request := func(token string) int {
		req := newRequest(t, "/v1/chats")
		req.Header.Set(HeaderAuthorization, "Bearer "+token)
		return serve(t, req, RequestID(), authenticate(), limiter).Code
	}

	if got := request(first); got != http.StatusOK {
		t.Fatalf("expected the first person through, got %d", got)
	}
	if got := request(first); got != http.StatusTooManyRequests {
		t.Fatalf("expected the same person throttled, got %d", got)
	}
	if got := request(second); got != http.StatusOK {
		t.Fatalf("expected the other person on the same address through, got %d", got)
	}
}

func TestKeyByPrincipal_fallsBackToTheAddressWhenUnauthenticated(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Request = newRequest(t, "/v1/chats")

	if got := KeyByPrincipal(c); got != "ip:203.0.113.7" {
		t.Fatalf("expected an ip key, got %q", got)
	}

	c.Set(ContextKeyCaller, Caller{Name: "ops", Role: RoleOperator})
	if got := KeyByPrincipal(c); got != "principal:ops" {
		t.Fatalf("expected the principal key, got %q", got)
	}
}

func TestKeyByIP_isPrefixedSoKeyspacesCannotCollide(t *testing.T) {
	// Without the prefix, a principal literally named after an address would share a
	// bucket with that address.
	c, _ := gin.CreateTestContext(nil)
	c.Request = newRequest(t, "/v1/chats")

	if got := KeyByIP(c); got != "ip:203.0.113.7" {
		t.Fatalf("unexpected key %q", got)
	}
}

func TestKeyedLimiter_refillsOverTime(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	limiter := newKeyedLimiter(1, 1)
	limiter.nowFunc = func() time.Time { return now }

	if !limiter.allow("a") {
		t.Fatal("expected the first request allowed")
	}
	if limiter.allow("a") {
		t.Fatal("expected the second request throttled")
	}

	now = now.Add(time.Second)
	if !limiter.allow("a") {
		t.Fatal("expected a token back after a second")
	}
}

func TestKeyedLimiter_forgetsIdleCallers(t *testing.T) {
	// The map holds one entry per caller. Without a sweep it only ever grows, and here
	// a caller can be any person who ever signed in.
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	limiter := newKeyedLimiter(1, 1)
	limiter.nowFunc = func() time.Time { return now }

	limiter.allow("goes-away")
	if len(limiter.buckets) != 1 {
		t.Fatalf("expected one bucket, got %d", len(limiter.buckets))
	}

	now = now.Add(bucketTTL + time.Minute)
	limiter.allow("still-here")

	if _, ok := limiter.buckets["goes-away"]; ok {
		t.Fatal("expected the idle bucket dropped")
	}
	if _, ok := limiter.buckets["still-here"]; !ok {
		t.Fatal("expected the active bucket kept")
	}
}

func TestKeyedLimiter_doesNotSweepOnEveryRequest(t *testing.T) {
	// Walking the whole map per request would make the limiter the bottleneck it
	// exists to prevent.
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	limiter := newKeyedLimiter(100, 100)
	limiter.nowFunc = func() time.Time { return now }

	limiter.allow("first")
	sweptAfterFirst := limiter.sweptAt

	now = now.Add(time.Second)
	limiter.allow("second")

	if !limiter.sweptAt.Equal(sweptAfterFirst) {
		t.Fatal("expected no second sweep within the gap")
	}
	if len(limiter.buckets) != 2 {
		t.Fatalf("expected both buckets kept, got %d", len(limiter.buckets))
	}
}

func TestKeyedLimiter_refusesNonsenseLimits(t *testing.T) {
	// A zero or negative limit would mean "allow nothing", which is never what an
	// operator meant to configure. Config validation rejects it first; this is the
	// backstop.
	for _, c := range []struct {
		rps   float64
		burst int
	}{{0, 0}, {-1, -5}} {
		limiter := newKeyedLimiter(c.rps, c.burst)
		if !limiter.allow("a") {
			t.Fatalf("rps=%v burst=%v: expected at least one request allowed", c.rps, c.burst)
		}
	}
}
