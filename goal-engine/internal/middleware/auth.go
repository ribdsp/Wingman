package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// Headers an inbound credential may arrive on.
const (
	HeaderAuthorization = "Authorization"
	HeaderAPIKey        = "X-API-Key"
	bearerPrefix        = "bearer "
)

// ContextKeyCaller is where APIKeyAuth stores the authenticated Caller.
const ContextKeyCaller = "caller"

// Role separates a human operator from an autonomous agent. It mirrors
// config.Role, declared here so this package needs no config import.
type Role string

const (
	// RoleOperator is a human.
	RoleOperator Role = "operator"
	// RoleBot is an agent in Wingman core.
	RoleBot Role = "bot"
)

// Credential is one accepted inbound key, the principal name recorded against
// everything done with it, and what that key is allowed to be.
//
// It is declared here rather than taken from the config package so this
// middleware can be tested without an environment, and so a reader of the auth
// rules does not have to go looking somewhere else for the shape of a key.
type Credential struct {
	Name   string
	Secret string
	Role   Role
}

// Caller is the authenticated identity as the rest of the request sees it.
//
// It carries no secret on purpose: it is stored on the gin context, where every
// handler and every middleware downstream can read it, and a credential put
// somewhere that broadly readable eventually gets logged by accident.
type Caller struct {
	Name string
	Role Role
}

// IsOperator reports whether the request came from a human's credential. It is
// the check that stands between an agent and its own approval.
func (c Caller) IsOperator() bool { return c.Role == RoleOperator }

// APIKeyAuth rejects any request that does not carry a configured key, and
// records which key it was.
//
// The name is the whole point: this service can move a revenue target and
// approve spending, so every write it accepts has to be attributable to one
// credential — and the role that credential was configured with decides what it
// is allowed to do, independent of anything the request claims.
func APIKeyAuth(creds []Credential, log zerolog.Logger) gin.HandlerFunc {
	// A build with no credentials would accept nothing, which is a
	// misconfiguration worth failing loudly on rather than serving 401s all day.
	// Config validation already requires at least one, so this is a guard against
	// a future caller wiring it up wrong.
	if len(creds) == 0 {
		panic("middleware: APIKeyAuth requires at least one credential")
	}
	// A key with no role would be neither trusted nor rejected — it would simply
	// fail every operator check with a confusing 403. Catch it at wiring time.
	for _, cred := range creds {
		if cred.Role != RoleOperator && cred.Role != RoleBot {
			panic("middleware: credential " + cred.Name + " has no role")
		}
	}

	return func(c *gin.Context) {
		presented := presentedToken(c)
		if presented == "" {
			unauthorised(c, log, "no credential presented")
			return
		}

		cred, ok := matchCredential(creds, presented)
		if !ok {
			unauthorised(c, log, "credential not recognised")
			return
		}

		c.Set(ContextKeyCaller, Caller{Name: cred.Name, Role: cred.Role})
		c.Next()
	}
}

// CallerOf returns the authenticated caller, and false on an unauthenticated
// route.
func CallerOf(c *gin.Context) (Caller, bool) {
	if c == nil {
		return Caller{}, false
	}
	caller, ok := c.Get(ContextKeyCaller)
	if !ok {
		return Caller{}, false
	}
	typed, ok := caller.(Caller)
	return typed, ok
}

// Principal returns the authenticated name for the request, or an empty string
// on an unauthenticated route. It is what the access log and the per-caller rate
// limiter key on.
func Principal(c *gin.Context) string {
	caller, _ := CallerOf(c)
	return caller.Name
}

// presentedToken reads the credential from either supported header. Authorization// wins when both are present, so a proxy that adds one cannot be sidestepped by
// also sending the other.
func presentedToken(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader(HeaderAuthorization))
	if header != "" {
		if len(header) > len(bearerPrefix) && strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
			return strings.TrimSpace(header[len(bearerPrefix):])
		}
		// A credential sent under the wrong scheme is a client bug, not an
		// alternative spelling. Falling through to X-API-Key here would let a
		// malformed Authorization header be silently ignored.
		return ""
	}
	return strings.TrimSpace(c.GetHeader(HeaderAPIKey))
}

// matchCredential compares the presented token against every configured key in
// constant time.
//
// It deliberately does not stop at the first match: with an early return, the
// time taken would reveal a matching key's position in the list, and with a
// plain string comparison it would reveal how many leading bytes were right.
func matchCredential(creds []Credential, presented string) (Credential, bool) {
	token := []byte(presented)

	var (
		matched Credential
		found   bool
	)
	for _, cred := range creds {
		if subtle.ConstantTimeCompare([]byte(cred.Secret), token) == 1 {
			matched, found = cred, true
		}
	}
	return matched, found
}

// unauthorised answers with the same body whatever went wrong. The log line
// carries the detail; the response does not help anyone probe for valid keys.
func unauthorised(c *gin.Context, log zerolog.Logger, reason string) {
	log.Warn().
		Str("requestId", utils.RequestID(c)).
		Str("ip", c.ClientIP()).
		Str("method", c.Request.Method).
		Str("path", c.Request.URL.Path).
		Str("reason", reason).
		Msg("rejected an unauthenticated request")

	utils.Error(c, http.StatusUnauthorized, utils.ErrCodeUnauthorized, "A valid API key is required.")
	c.Abort()
}
