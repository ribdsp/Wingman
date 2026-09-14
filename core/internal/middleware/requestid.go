// Package middleware holds the HTTP concerns that wrap every request:
// attribution, authentication, rate limiting, access logging, and panic recovery.
//
// Each one is a separate file because they fail in different ways and get read at
// different times — the auth rules get read during a security review, the logging
// during an incident.
package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ribdsp/wingman/core/internal/utils"
)

// HeaderRequestID is the header a request id is read from and echoed back on.
const HeaderRequestID = "X-Request-Id"

// maxInboundRequestIDLength bounds an id supplied by the caller. An id ends up in
// the audit log and in every log line for the request, so it is treated as untrusted
// input rather than as a convenience.
const maxInboundRequestIDLength = 64

// RequestID gives every request an id, reusing the caller's when it supplied a usable
// one so a trace survives across services.
//
// This matters more here than in most services: a run started by the goal engine's
// trigger bridge should be traceable from the metric sample that woke it through to
// the tool call it made.
//
// The id is stored on the gin context only. Handlers pass it into the service layer
// explicitly as part of an Actor, rather than smuggling it through context.Context:
// attribution that the compiler asks for is attribution that actually gets recorded.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := sanitiseRequestID(c.GetHeader(HeaderRequestID))
		if id == "" {
			id = uuid.New().String()
		}

		c.Set(utils.ContextKeyRequestID, id)
		c.Header(HeaderRequestID, id)
		c.Next()
	}
}

// sanitiseRequestID returns the caller's id when it is safe to repeat back, and an
// empty string otherwise.
//
// An id is echoed into a response header and written into log lines, so a control
// character in it is a header-splitting or log-forging attempt rather than a trace id.
func sanitiseRequestID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" || len(id) > maxInboundRequestIDLength {
		return ""
	}
	for _, r := range id {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return id
}
