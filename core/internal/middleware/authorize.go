package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

// RequireOperator rejects a request whose credential is not an operator's.
//
// It guards the routes where a bot or a user acting for itself would defeat the point:
// changing a tool grant, cancelling somebody else's run, reading the whole audit log.
// Authentication says a caller is known; this says a caller is allowed.
//
// The same rules are enforced again in the service layer, on purpose. A route check
// protects a route; the service-layer check protects the invariant, and survives
// somebody adding a second way to reach the same operation.
func RequireOperator(log zerolog.Logger) gin.HandlerFunc {
	return requireRole(log, "This action is reserved for an operator.",
		func(caller Caller) bool { return caller.IsOperator() },
	)
}

// RequireUser rejects a request that is not a signed-in human's.
//
// It guards a person's own data — their chats, their messages, their runs. An operator
// key is refused here as firmly as a bot key, and that is the boundary working rather
// than an oversight: those routes answer "show me *my* conversations", and a machine
// credential has no answer to that question. Whoever runs the instance reaches the same
// rows through the database, which leaves a trace that a route would not.
func RequireUser(log zerolog.Logger) gin.HandlerFunc {
	return requireRole(log, "This action needs a signed-in account.",
		func(caller Caller) bool { return caller.IsUser() && caller.UserID != "" },
	)
}

// requireRole is the shared body of the two guards above: the same 401-versus-403
// distinction and the same log line, with only the predicate and the sentence differing.
func requireRole(log zerolog.Logger, message string, allowed func(Caller) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		caller, ok := CallerOf(c)
		if !ok {
			// Reaching here means the guard was mounted without Authenticate in front
			// of it. Answering 403 rather than 401 would hide the wiring bug.
			log.Error().
				Str("requestId", utils.RequestID(c)).
				Str("path", c.Request.URL.Path).
				Msg("an authorisation guard ran on an unauthenticated route")
			utils.Error(c, http.StatusUnauthorized, utils.ErrCodeUnauthorized,
				"A valid API key or session is required.")
			c.Abort()
			return
		}

		if !allowed(caller) {
			log.Warn().
				Str("requestId", utils.RequestID(c)).
				Str("principal", caller.Name).
				Str("role", string(caller.Role)).
				Str("method", c.Request.Method).
				Str("path", c.Request.URL.Path).
				Msg("refused a caller an action its role does not carry")

			utils.Error(c, http.StatusForbidden, utils.ErrCodeForbidden, message)
			c.Abort()
			return
		}

		c.Next()
	}
}

// OwnerOf returns the user id a request's own data is scoped to.
//
// An operator or a bot has none, and the second return value says so rather than
// handing back an empty string that a WHERE clause would happily accept. Callers that
// need one should be behind RequireUser; this exists for the handlers that serve both
// kinds and have to branch.
func OwnerOf(c *gin.Context) (string, bool) {
	caller, ok := CallerOf(c)
	if !ok || !caller.IsUser() {
		return "", false
	}
	id := strings.TrimSpace(caller.UserID)
	return id, id != ""
}
