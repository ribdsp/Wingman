package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// RequireOperator rejects a request whose credential is not a human's.
//
// It guards the routes where an agent acting for itself would defeat the point
// of the service: clearing its own spend, rewriting the target it is measured
// against, releasing the switch that stops it. Authentication says a caller is
// known; this says a caller is allowed.
//
// The same rules are enforced again in the service layer, on purpose. A route
// check protects a route; the service-layer check protects the invariant, and
// survives somebody adding a second way to reach the same operation.
func RequireOperator(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		caller, ok := CallerOf(c)
		if !ok {
			// Reaching here means RequireOperator was mounted without APIKeyAuth in
			// front of it. Answering 403 rather than 401 would hide the wiring bug.
			log.Error().
				Str("requestId", utils.RequestID(c)).
				Str("path", c.Request.URL.Path).
				Msg("RequireOperator ran on an unauthenticated route")
			utils.Error(c, http.StatusUnauthorized, utils.ErrCodeUnauthorized, "A valid API key is required.")
			c.Abort()
			return
		}

		if !caller.IsOperator() {
			log.Warn().
				Str("requestId", utils.RequestID(c)).
				Str("principal", caller.Name).
				Str("role", string(caller.Role)).
				Str("method", c.Request.Method).
				Str("path", c.Request.URL.Path).
				Msg("refused an agent an operator-only action")

			utils.Error(c, http.StatusForbidden, utils.ErrCodeForbidden,
				"This action is reserved for a human operator.")
			c.Abort()
			return
		}

		c.Next()
	}
}
