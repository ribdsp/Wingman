package middleware

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// Headers an inbound credential may arrive on.
const (
	HeaderAuthorization = "Authorization"
	HeaderAPIKey        = "X-API-Key"
	bearerPrefix        = "bearer "
)

// ContextKeyCaller is where Authenticate stores the authenticated Caller.
const ContextKeyCaller = "caller"

// Role separates the three kinds of principal core serves. It mirrors config.Role,
// declared here so this package needs no config import.
type Role string

const (
	// RoleOperator is whoever runs this instance.
	RoleOperator Role = "operator"
	// RoleBot is a service — in practice the goal engine's trigger bridge.
	RoleBot Role = "bot"
	// RoleUser is a human with an account. It is reachable only through the session
	// path below, and no environment key can produce it.
	RoleUser Role = "user"
)

// Credential is one accepted inbound machine key, the principal name recorded against
// everything done with it, and what that key is allowed to be.
//
// It is declared here rather than taken from the config package so this middleware can
// be tested without an environment, and so a reader of the auth rules does not have to
// go looking somewhere else for the shape of a key.
type Credential struct {
	Name   string
	Secret string
	Role   Role
}

// Caller is the authenticated identity as the rest of the request sees it.
//
// It carries no secret on purpose: it is stored on the gin context, where every handler
// and every middleware downstream can read it, and a credential put somewhere that
// broadly readable eventually gets logged by accident.
type Caller struct {
	// Name is what the audit log records.
	Name string
	// Role decides what the caller may do, and comes from where the credential lives.
	Role Role
	// UserID is set for RoleUser and empty for everything else. Every query against a
	// user's own data scopes on it, so an empty one must never reach a repository —
	// see Authenticate, which refuses to build a user Caller without it.
	UserID string
}

// IsOperator reports whether the request came from an operator's key.
func (c Caller) IsOperator() bool { return c.Role == RoleOperator }

// IsUser reports whether the request came from a signed-in human.
func (c Caller) IsUser() bool { return c.Role == RoleUser }

// Session is what a live session token resolves to.
type Session struct {
	UserID string
	// Name is the handle the audit log records for this person.
	Name string
}

// ErrNoSession means the token is well formed but not live — unknown, expired or
// revoked. Which of the three is deliberately not distinguished: telling a caller
// their token was "expired" rather than "unknown" confirms it was once real.
var ErrNoSession = errors.New("no live session for that token")

// SessionStore resolves a session token to the person holding it.
//
// It takes the *stored* form. The middleware hashes the presented token before this is
// called, so a plaintext session token never crosses into the repository layer and
// cannot end up in a query log.
type SessionStore interface {
	ResolveSession(ctx context.Context, storedToken string, now time.Time) (Session, error)
}

// Authenticate rejects any request that does not carry either a configured machine key
// or a live session token, and records which one it was.
//
// The two credential kinds are told apart by shape, not by trying both: a session
// token is prefixed and fixed-length (auth.ParseSessionToken), and everything else is
// compared against the configured keys. That is what keeps the roles unforgeable in
// both directions — an environment key cannot become a user, because it is not shaped
// like a session token, and a session token cannot become an operator, because it is
// never compared against the key list.
func Authenticate(creds []Credential, sessions SessionStore, log zerolog.Logger) gin.HandlerFunc {
	// These three are wiring bugs, and a wiring bug in authentication should stop the
	// process rather than serve requests. Config validation already requires an
	// operator key; this guards against a future caller assembling the list wrong.
	if len(creds) == 0 {
		panic("middleware: Authenticate requires at least one credential")
	}
	if sessions == nil {
		panic("middleware: Authenticate requires a session store")
	}
	for _, cred := range creds {
		// A key with no role would be neither trusted nor rejected — it would simply
		// fail every operator check with a confusing 403. RoleUser is refused outright:
		// an environment variable that could mint a human is the one confusion this
		// whole design exists to prevent.
		if cred.Role != RoleOperator && cred.Role != RoleBot {
			panic("middleware: credential " + cred.Name + " has no machine role")
		}
		// A configured key shaped like a session token would be routed down the session
		// path and could never match, so it would look revoked rather than misconfigured.
		if _, err := auth.ParseSessionToken(cred.Secret); err == nil {
			panic("middleware: credential " + cred.Name + " is shaped like a session token")
		}
	}

	return func(c *gin.Context) {
		presented := presentedToken(c)
		if presented == "" {
			unauthorised(c, log, "no credential presented")
			return
		}

		if stored, err := auth.ParseSessionToken(presented); err == nil {
			authenticateSession(c, sessions, stored, log)
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

// authenticateSession resolves a well-formed session token, or answers for why it
// could not.
func authenticateSession(c *gin.Context, sessions SessionStore, storedToken string, log zerolog.Logger) {
	session, err := sessions.ResolveSession(c.Request.Context(), storedToken, time.Now())
	switch {
	case errors.Is(err, ErrNoSession):
		unauthorised(c, log, "session not live")
		return
	case err != nil:
		// A database that cannot be read is not a wrong password. Answering 401 here
		// would show the user a sign-in screen for an outage, and would show the
		// operator a spike in failed authentications instead of a broken database.
		log.Error().
			Err(err).
			Str("requestId", utils.RequestID(c)).
			Str("path", c.Request.URL.Path).
			Msg("could not read the session store")
		utils.Error(c, http.StatusServiceUnavailable, utils.ErrCodeUnavailable,
			"Sign-in is temporarily unavailable. Try again shortly.")
		c.Abort()
		return
	case session.UserID == "":
		// A session with no user behind it would produce a Caller whose UserID is
		// empty, and every "scope this query to the caller" clause would then match
		// nobody — or, one refactor later, everybody.
		log.Error().
			Str("requestId", utils.RequestID(c)).
			Str("path", c.Request.URL.Path).
			Msg("the session store returned a session with no user")
		utils.Error(c, http.StatusInternalServerError, utils.ErrCodeInternal,
			"Something went wrong. The request id in this response will find it in the logs.")
		c.Abort()
		return
	}

	name := session.Name
	if name == "" {
		// The audit log needs something that identifies the actor. An id is worse to
		// read than a handle and better than a blank.
		name = "user:" + session.UserID
	}
	c.Set(ContextKeyCaller, Caller{Name: name, Role: RoleUser, UserID: session.UserID})
	c.Next()
}

// CallerOf returns the authenticated caller, and false on an unauthenticated route.
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

// Principal returns the authenticated name for the request, or an empty string on an
// unauthenticated route. It is what the access log and the per-caller rate limiter key
// on.
func Principal(c *gin.Context) string {
	caller, _ := CallerOf(c)
	return caller.Name
}

// PresentedToken returns the raw credential the request carried, or an empty string when
// it carried none.
//
// It exists for one route: signing out, which ends the session the caller presented and
// so needs the token itself rather than the identity it resolved to. That is also why the
// authenticated Caller does not carry it — a credential stored on the gin context, where
// every middleware and handler downstream can read it, eventually gets logged by
// accident. A caller of this must hand the value straight to the service and never log,
// echo or store it.
func PresentedToken(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return presentedToken(c)
}

// presentedToken reads the credential from either supported header. Authorization wins
// when both are present, so a proxy that adds one cannot be sidestepped by also
// sending the other.
func presentedToken(c *gin.Context) string {
	header := strings.TrimSpace(c.GetHeader(HeaderAuthorization))
	if header != "" {
		if len(header) > len(bearerPrefix) && strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
			return strings.TrimSpace(header[len(bearerPrefix):])
		}
		// A credential sent under the wrong scheme is a client bug, not an alternative
		// spelling. Falling through to X-API-Key here would let a malformed
		// Authorization header be silently ignored.
		return ""
	}
	return strings.TrimSpace(c.GetHeader(HeaderAPIKey))
}

// matchCredential compares the presented token against every configured key in
// constant time.
//
// It deliberately does not stop at the first match: with an early return, the time
// taken would reveal a matching key's position in the list, and with a plain string
// comparison it would reveal how many leading bytes were right.
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

// unauthorised answers with the same body whatever went wrong. The log line carries
// the detail; the response does not help anyone probe for valid credentials, and in
// particular does not say whether the thing presented was the wrong *kind*.
func unauthorised(c *gin.Context, log zerolog.Logger, reason string) {
	log.Warn().
		Str("requestId", utils.RequestID(c)).
		Str("ip", c.ClientIP()).
		Str("method", c.Request.Method).
		Str("path", c.Request.URL.Path).
		Str("reason", reason).
		Msg("rejected an unauthenticated request")

	utils.Error(c, http.StatusUnauthorized, utils.ErrCodeUnauthorized,
		"A valid API key or session is required.")
	c.Abort()
}
