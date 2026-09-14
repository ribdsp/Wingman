package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"golang.org/x/time/rate"

	"github.com/ribdsp/wingman/core/internal/utils"
)

// bucketTTL is how long an idle bucket is kept. Without it, the map grows once per
// distinct caller and never shrinks.
const bucketTTL = 10 * time.Minute

// KeyFunc decides what a request is limited against.
type KeyFunc func(*gin.Context) string

// KeyByIP limits per source address. Applied ahead of authentication, this is what
// keeps an unauthenticated flood from reaching the rest of the stack — including the
// sign-in route, where every attempt costs 64 MiB of argon2.
func KeyByIP(c *gin.Context) string { return "ip:" + c.ClientIP() }

// KeyByPrincipal limits per credential. Applied behind authentication, this is what
// stops one misbehaving caller — a channel bridge retrying the same brief a thousand
// times — from consuming the whole service.
func KeyByPrincipal(c *gin.Context) string {
	if name := Principal(c); name != "" {
		return "principal:" + name
	}
	return "ip:" + c.ClientIP()
}

// RateLimit throttles requests sharing a key to rps per second with room for a burst.
//
// This is a safety net against a runaway caller, not a security control: on a
// self-hosted deployment behind a proxy every request may share one address, so the
// per-principal limiter is the one that discriminates. Neither of them is the thing that
// bounds spending — that is the token budget, because a single request can cost more
// than a thousand.
func RateLimit(rps float64, burst int, key KeyFunc, log zerolog.Logger) gin.HandlerFunc {
	limiter := newKeyedLimiter(rps, burst)

	return func(c *gin.Context) {
		if limiter.allow(key(c)) {
			c.Next()
			return
		}

		log.Warn().
			Str("requestId", utils.RequestID(c)).
			Str("principal", Principal(c)).
			Str("ip", c.ClientIP()).
			Str("path", c.Request.URL.Path).
			Msg("rate limited a request")

		utils.Error(c, http.StatusTooManyRequests, utils.ErrCodeRateLimited,
			"Too many requests. Slow down and retry.")
		c.Abort()
	}
}

// keyedLimiter is one token bucket per key, with idle buckets swept on use so there is
// no background goroutine to shut down.
type keyedLimiter struct {
	rps   rate.Limit
	burst int

	mu       sync.Mutex
	buckets  map[string]*bucket
	sweptAt  time.Time
	nowFunc  func() time.Time
	sweepGap time.Duration
}

type bucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func newKeyedLimiter(rps float64, burst int) *keyedLimiter {
	if rps <= 0 {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &keyedLimiter{
		rps:      rate.Limit(rps),
		burst:    burst,
		buckets:  map[string]*bucket{},
		nowFunc:  time.Now,
		sweepGap: bucketTTL,
	}
}

func (k *keyedLimiter) allow(key string) bool {
	now := k.nowFunc()

	k.mu.Lock()
	defer k.mu.Unlock()

	k.sweep(now)

	b, ok := k.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(k.rps, k.burst)}
		k.buckets[key] = b
	}
	b.seen = now

	// AllowN is given the time explicitly rather than letting the limiter read the
	// clock itself, so the whole thing is deterministic under test.
	return b.limiter.AllowN(now, 1)
}

// sweep drops buckets nobody has used for a while. It runs at most once per sweepGap so
// a busy service is not walking the whole map on every request.
func (k *keyedLimiter) sweep(now time.Time) {
	if now.Sub(k.sweptAt) < k.sweepGap {
		return
	}
	k.sweptAt = now

	for key, b := range k.buckets {
		if now.Sub(b.seen) > bucketTTL {
			delete(k.buckets, key)
		}
	}
}
