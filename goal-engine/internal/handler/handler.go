// Package handler is the HTTP boundary of the goal engine.
//
// Handlers here do three things and nothing else: turn a request into the
// arguments a service expects, call it, and render what comes back. Every rule
// about what is allowed lives in internal/service or internal/domain, because a
// rule enforced in a handler only holds for the one route that happens to call
// it — and this service exists to bound what an autonomous agent may do.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/middleware"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// Router wiring failures. They are values rather than inline strings so a caller
// can tell a misconfiguration from a runtime fault.
var (
	errNoHandler     = errors.New("handler: router needs a handler set")
	errNoCredentials = errors.New("handler: router needs at least one credential: " +
		"serving this API unauthenticated would expose the kill switch and the approval queue")
)

// jsonRaw is a JSON document already stored as text.
//
// The audit log's detail column holds JSON, and re-encoding it into a string
// field would hand the client a quoted blob to decode a second time. Emitting it
// verbatim keeps the response one document.
type jsonRaw string

// MarshalJSON writes the stored document as-is when it is valid JSON.
func (r jsonRaw) MarshalJSON() ([]byte, error) {
	trimmed := strings.TrimSpace(string(r))
	if trimmed == "" {
		return []byte("null"), nil
	}
	if !json.Valid([]byte(trimmed)) {
		// Stored detail is always written as JSON, but a row from an older schema
		// or written by hand must not be able to corrupt the whole response body.
		// Quoting it keeps the response parseable and the anomaly visible.
		return json.Marshal(trimmed)
	}
	return []byte(trimmed), nil
}

// Deps is everything the HTTP layer needs. Each service is optional in the type
// system but checked at construction: a route wired to a nil service would panic
// on the first request instead of failing at startup.
type Deps struct {
	Goals     *service.Goals
	Approvals *service.Approvals
	Flags     *service.Flags
	Audit     *service.AuditLog
	Monitor   *service.Monitor
	Samples   *service.Samples
	Metrics   *metrics.Registry

	Logger zerolog.Logger
}

// Handler holds the services the routes call.
type Handler struct {
	goals     *service.Goals
	approvals *service.Approvals
	flags     *service.Flags
	audit     *service.AuditLog
	monitor   *service.Monitor
	samples   *service.Samples
	metrics   *metrics.Registry
	log       zerolog.Logger
}

// New validates its wiring and returns a ready handler set.
func New(deps Deps) (*Handler, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Goals != nil, "Goals")
	require(deps.Approvals != nil, "Approvals")
	require(deps.Flags != nil, "Flags")
	require(deps.Audit != nil, "Audit")
	require(deps.Monitor != nil, "Monitor")
	require(deps.Samples != nil, "Samples")
	require(deps.Metrics != nil, "Metrics")
	if len(missing) > 0 {
		return nil, errors.New("handler: missing dependencies: " + strings.Join(missing, ", "))
	}

	return &Handler{
		goals:     deps.Goals,
		approvals: deps.Approvals,
		flags:     deps.Flags,
		audit:     deps.Audit,
		monitor:   deps.Monitor,
		samples:   deps.Samples,
		metrics:   deps.Metrics,
		log:       deps.Logger,
	}, nil
}

// actorFrom builds the service-layer actor from the authenticated caller.
//
// The mapping is one-way on purpose: the role comes from the credential the
// authentication middleware matched, so there is no path by which a request body
// or header can promote its own actor. An unauthenticated request — which should
// be impossible on any route that calls this — maps to a bot, the least
// privileged actor, so a wiring mistake fails closed.
func actorFrom(c *gin.Context) service.Actor {
	actor := service.Actor{
		Type:      repository.ActorBot,
		ID:        middleware.Principal(c),
		RequestID: utils.RequestID(c),
	}
	if caller, ok := middleware.CallerOf(c); ok && caller.IsOperator() {
		actor.Type = repository.ActorUser
	}
	return actor
}

// respondError renders a service error as the API's envelope.
//
// The mapping is the whole reason handlers never inspect an error string: a
// service returns a sentinel, this decides the status. Anything unrecognised is
// a 500 whose detail is logged and not returned — an error from a repository can
// carry a query or a column name, and neither belongs in a response body.
func (h *Handler) respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrValidation):
		if fields := service.ValidationFields(err); len(fields) > 0 {
			utils.ValidationError(c, "The request could not be accepted as given.", fields)
			return
		}
		utils.Error(c, http.StatusBadRequest, utils.ErrCodeValidation, cleanMessage(err))

	case errors.Is(err, service.ErrNotFound):
		utils.Error(c, http.StatusNotFound, utils.ErrCodeNotFound, "Not found.")

	case errors.Is(err, service.ErrForbidden):
		// The message is safe to return in full: it names the operation and the
		// actor type, which is exactly what the caller needs to understand that
		// asking again will not help.
		utils.Error(c, http.StatusForbidden, utils.ErrCodeForbidden, cleanMessage(err))

	case errors.Is(err, service.ErrAlreadyResolved):
		utils.Error(c, http.StatusConflict, utils.ErrCodeAlreadyResolved,
			"This approval has already been decided.")

	default:
		h.log.Error().Err(err).
			Str("requestId", utils.RequestID(c)).
			Str("path", c.FullPath()).
			Msg("request failed")
		utils.Error(c, http.StatusInternalServerError, utils.ErrCodeInternal,
			"Something went wrong. The request id is in this response.")
	}
}

// cleanMessage strips the internal sentinel prefixes from an error before it is
// shown to a caller. "service: invalid request: periodEnd is already in the
// past" is a log line; "periodEnd is already in the past" is an API message.
func cleanMessage(err error) string {
	msg := err.Error()
	for _, prefix := range []string{
		"service: invalid request: ",
		"service: not permitted for this actor: ",
		"service: not found: ",
	} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

// badRequest answers a request that never reached a service, e.g. one whose JSON
// did not parse.
func badRequest(c *gin.Context, message string) {
	utils.Error(c, http.StatusBadRequest, utils.ErrCodeValidation, message)
}

// bindJSON decodes a request body, answering the caller itself on failure.
//
// The bind error is not returned verbatim: Go's JSON errors name struct fields
// and types, which describes this service's internals rather than the caller's
// mistake.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		badRequest(c, "The request body is not valid JSON for this endpoint.")
		return false
	}
	return true
}

// page reads limit and offset from the query string.
//
// Anything unparseable is treated as absent rather than rejected: the service
// clamps the values anyway, and failing a listing because a dashboard sent
// ?limit= is noise, not safety.
func page(c *gin.Context) (limit, offset int) {
	limit = intQuery(c, "limit", 0)
	offset = intQuery(c, "offset", 0)
	if p := intQuery(c, "page", 0); p > 1 && offset == 0 {
		// Pagination metadata is rendered as pages, so accept pages on the way in
		// too. A caller that sends both wins with the explicit offset.
		effective := limit
		if effective <= 0 {
			effective = defaultRenderedLimit
		}
		offset = (p - 1) * effective
	}
	return limit, offset
}

// defaultRenderedLimit mirrors the service's own default page size. It is only
// used to turn a page number into an offset and to render pagination metadata,
// never to bound a query — the service clamps that itself.
const defaultRenderedLimit = 50

// intQuery reads an integer query parameter, falling back to a default.
func intQuery(c *gin.Context, name string, fallback int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// timeQuery reads an RFC 3339 timestamp query parameter. An unparseable value
// reports false so the caller can reject it rather than silently widen a window.
func timeQuery(c *gin.Context, name string) (time.Time, bool, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, errors.New(name + " must be an RFC 3339 timestamp")
	}
	return parsed, true, nil
}

// paged renders a list response, converting the offset back into a page number
// so the envelope's pagination block reads the way the rest of the portfolio's
// APIs do.
func paged(c *gin.Context, message string, data any, limit, offset, total int) {
	if limit <= 0 {
		limit = defaultRenderedLimit
	}
	pageNumber := offset/limit + 1
	utils.SuccessWithPagination(c, http.StatusOK, message, data, pageNumber, limit, total)
}
