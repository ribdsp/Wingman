package middleware

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

func TestRateLimitAllowsABurstThenThrottles(t *testing.T) {
	limiter := RateLimit(1, 3, KeyByIP, discardLog())

	for i := 0; i < 3; i++ {
		if rec := serve(t, newRequest(t, "/goals"), RequestID(), limiter); rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	rec := serve(t, newRequest(t, "/goals"), RequestID(), limiter)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the burst is spent, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeRateLimited {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}

func TestRateLimitKeepsCallersApart(t *testing.T) {
	// One noisy caller must not lock everybody else out.
	limiter := RateLimit(1, 1, KeyByIP, discardLog())

	noisy := newRequest(t, "/goals")
	if rec := serve(t, noisy, RequestID(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}
	if rec := serve(t, newRequest(t, "/goals"), RequestID(), limiter); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the same caller throttled, got %d", rec.Code)
	}

	other := newRequest(t, "/goals")
	other.RemoteAddr = "198.51.100.4:12345"
	if rec := serve(t, other, RequestID(), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected a different caller through, got %d", rec.Code)
	}
}

func TestKeyByPrincipalPrefersTheCredentialOverTheAddress(t *testing.T) {
	// Behind a proxy every request shares one address, so the credential is the
	// only key that discriminates.
	limiter := RateLimit(1, 1, KeyByPrincipal, discardLog())
	creds := testCredentials()

	first := newRequest(t, "/goals")
	first.Header.Set(HeaderAPIKey, testSecret)
	if rec := serve(t, first, RequestID(), APIKeyAuth(creds, discardLog()), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected the first request through, got %d", rec.Code)
	}

	again := newRequest(t, "/goals")
	again.Header.Set(HeaderAPIKey, testSecret)
	if rec := serve(t, again, RequestID(), APIKeyAuth(creds, discardLog()), limiter); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the same credential throttled, got %d", rec.Code)
	}

	// Same address, different credential.
	second := newRequest(t, "/goals")
	second.Header.Set(HeaderAPIKey, otherSecret)
	if rec := serve(t, second, RequestID(), APIKeyAuth(creds, discardLog()), limiter); rec.Code != http.StatusOK {
		t.Fatalf("expected a different credential through, got %d", rec.Code)
	}
}

func TestKeyByPrincipalFallsBackToTheAddressWhenUnauthenticated(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Request = newRequest(t, "/goals")

	if got := KeyByPrincipal(c); got == "" || got[:3] != "ip:" {
		t.Fatalf("expected an ip key, got %q", got)
	}

	c.Set(ContextKeyCaller, Caller{Name: "ops", Role: RoleOperator})
	if got := KeyByPrincipal(c); got != "principal:ops" {
		t.Fatalf("expected the principal key, got %q", got)
	}
}

func TestKeyedLimiterRefillsOverTime(t *testing.T) {
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

func TestKeyedLimiterForgetsIdleCallers(t *testing.T) {
	// The map holds one entry per caller. Without a sweep it only ever grows.
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

func TestKeyedLimiterDoesNotSweepOnEveryRequest(t *testing.T) {
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

func TestKeyedLimiterRefusesNonsenseLimits(t *testing.T) {
	// A zero or negative limit would mean "allow nothing", which is never what an
	// operator meant to configure. Config validation rejects it first; this is the
	// backstop.
	for _, c := range []struct{ rps, burst float64 }{{0, 0}, {-1, -5}} {
		limiter := newKeyedLimiter(c.rps, int(c.burst))
		if !limiter.allow("a") {
			t.Fatalf("rps=%v burst=%v: expected at least one request allowed", c.rps, c.burst)
		}
	}
}

func TestKeyByIPIsPrefixedSoKeyspacesCannotCollide(t *testing.T) {
	// Without the prefix, a principal literally named after an address would share
	// a bucket with that address.
	c, _ := gin.CreateTestContext(nil)
	c.Request = newRequest(t, "/goals")

	if got := KeyByIP(c); got != "ip:203.0.113.7" {
		t.Fatalf("unexpected key %q", got)
	}
}
