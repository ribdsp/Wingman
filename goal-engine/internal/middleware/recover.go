package middleware

import (
	"errors"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// Recover turns a panic into a 500 with the standard envelope, so one bad
// request cannot take the monitor down with it.
//
// The response never carries the panic value. A panic message routinely contains
// a query fragment, a struct dump, or a connection string.
func Recover(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			// A client that hung up mid-response is not a bug in this service, and a
			// stack trace for it is noise. There is nothing left to write a body to
			// either.
			if isDisconnect(recovered) {
				log.Warn().
					Str("requestId", utils.RequestID(c)).
					Str("path", c.Request.URL.Path).
					Msg("client disconnected mid-response")
				c.Abort()
				return
			}

			log.Error().
				Str("requestId", utils.RequestID(c)).
				Str("principal", Principal(c)).
				Str("method", c.Request.Method).
				Str("path", c.Request.URL.Path).
				Interface("panic", recovered).
				Str("stack", string(debug.Stack())).
				Msg("recovered from a panic")

			utils.Error(c, http.StatusInternalServerError, utils.ErrCodeInternal,
				"Something went wrong. The request id in this response will find it in the logs.")
			c.Abort()
		}()

		c.Next()
	}
}

// isDisconnect reports whether the panic came from writing to a connection the
// client had already closed.
func isDisconnect(recovered any) bool {
	err, ok := recovered.(error)
	if !ok {
		return false
	}

	var netErr *net.OpError
	if !errors.As(err, &netErr) {
		return false
	}

	var sysErr *os.SyscallError
	if !errors.As(netErr.Err, &sysErr) {
		return false
	}
	msg := strings.ToLower(sysErr.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}
