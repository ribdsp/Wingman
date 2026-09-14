package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

// quietPaths are logged at debug rather than info. A health check every few seconds
// would otherwise bury the requests somebody actually wants to read.
var quietPaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
}

// AccessLog writes one structured line per request.
//
// Route holds the matched pattern and path holds what was actually asked for: the
// pattern is what you group by, the path is what you need when one specific request
// went wrong.
//
// The role is logged alongside the name because the two answer different questions.
// The name says which credential acted; the role says whether it should have been able
// to, and a bot appearing where only users belong is the line worth finding.
func AccessLog(log zerolog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		status := c.Writer.Status()
		event := log.Info()
		switch {
		case status >= 500:
			event = log.Error()
		case status >= 400:
			event = log.Warn()
		case quietPaths[c.Request.URL.Path]:
			event = log.Debug()
		}

		caller, _ := CallerOf(c)
		event.
			Str("requestId", utils.RequestID(c)).
			Str("principal", caller.Name).
			Str("role", string(caller.Role)).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Str("route", c.FullPath()).
			Int("status", status).
			Dur("latency", time.Since(started)).
			Int("bytes", c.Writer.Size()).
			Str("ip", c.ClientIP()).
			Msg("request")
	}
}
