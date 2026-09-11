package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// quietPaths are logged at debug rather than info. A health check every few
// seconds would otherwise bury the requests somebody actually wants to read.
var quietPaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
}

// AccessLog writes one structured line per request.
//
// Route holds the matched pattern and path holds what was actually asked for:
// the pattern is what you group by, the path is what you need when one specific
// request went wrong.
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

		event.
			Str("requestId", utils.RequestID(c)).
			Str("principal", Principal(c)).
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
