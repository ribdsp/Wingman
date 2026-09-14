// Package handler is core's HTTP boundary.
//
// Handlers here do three things and nothing else: turn a request into the arguments a
// service expects, call it, and render what comes back. Every rule about what is
// allowed lives in internal/service or internal/domain, because a rule enforced in a
// handler only holds for the one route that happens to call it — and this service runs
// an autonomous agent on somebody's machine with somebody's API key.
//
// Two consequences worth stating, because they are what makes the layer boring:
//
//   - No handler reads a repository, and none writes SQL.
//   - No handler decides who a caller is. The role comes from the credential the
//     authentication middleware matched, and actorFrom only translates it.
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/middleware"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// Router wiring failures. They are values rather than inline strings so a caller can
// tell a misconfiguration from a runtime fault.
var (
	errNoHandler     = errors.New("handler: router needs a handler set")
	errNoSessions    = errors.New("handler: router needs a session store: without one no signed-in person could be authenticated")
	errNoCredentials = errors.New("handler: router needs at least one credential: " +
		"serving this API unauthenticated would hand anyone who can reach the port an agent with a sandbox and an API key")
)

// Deps is everything the HTTP layer needs. Each service is optional in the type system
// but checked at construction: a route wired to a nil service would panic on the first
// request instead of failing at startup.
type Deps struct {
	Accounts *service.Accounts
	Sessions *service.Sessions
	Chats    *service.Chats
	Tasks    *service.Tasks
	Runs     *service.Runs
	Channels *service.Channels
	// Notifications is how the goal engine tells the operator something happened. Required
	// like the rest: an instance with notifications switched off still has the route, and
	// the service answers it with nobody to tell rather than with a panic.
	Notifications *service.Notifications

	// ChannelsConnected is which platforms this instance actually holds a connection to,
	// for /v1/reference. It is a plain list of names rather than the hub itself, so this
	// package stays unaware that internal/channel exists — and it is worth publishing
	// because a link code minted for a platform nothing is connected to can never be
	// redeemed, and the client showing the button is what should know that.
	ChannelsConnected []string

	// Ready reports whether core's dependencies are reachable. It is a function rather
	// than a service because readiness is a property of the process, and cmd is what
	// knows how to ask.
	Ready func(c *gin.Context) error

	// Clock is only ever used to answer "would this still work now?" about something a
	// service already returned — whether a session is live, say. Nothing is decided with
	// it. It is injectable so those renderings can be asserted, and defaults to
	// time.Now.
	Clock service.Clock

	Logger zerolog.Logger
}

// Handler holds the services the routes call.
type Handler struct {
	accounts      *service.Accounts
	sessions      *service.Sessions
	chats         *service.Chats
	tasks         *service.Tasks
	runs          *service.Runs
	channels      *service.Channels
	notifications *service.Notifications
	connected     []string
	ready         func(c *gin.Context) error
	now           service.Clock
	log           zerolog.Logger
}

// New validates its wiring and returns a ready handler set.
func New(deps Deps) (*Handler, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Accounts != nil, "Accounts")
	require(deps.Sessions != nil, "Sessions")
	require(deps.Chats != nil, "Chats")
	require(deps.Tasks != nil, "Tasks")
	require(deps.Runs != nil, "Runs")
	require(deps.Channels != nil, "Channels")
	require(deps.Notifications != nil, "Notifications")
	require(deps.Ready != nil, "Ready")
	if len(missing) > 0 {
		return nil, errors.New("handler: missing dependencies: " + strings.Join(missing, ", "))
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}

	return &Handler{
		accounts:      deps.Accounts,
		sessions:      deps.Sessions,
		chats:         deps.Chats,
		tasks:         deps.Tasks,
		runs:          deps.Runs,
		channels:      deps.Channels,
		notifications: deps.Notifications,
		connected:     deps.ChannelsConnected,
		ready:         deps.Ready,
		now:           deps.Clock,
		log:           deps.Logger,
	}, nil
}

// actorFrom builds the service-layer actor from the authenticated caller.
//
// The mapping is one-way on purpose: the role comes from the credential the
// authentication middleware matched, so no request body, header or path parameter can
// promote its own actor. In particular the user id is the session's, never the
// request's — an actor that could name its own account could read anybody's chats.
//
// An unrecognised role becomes a bot, the least privileged of the three. An
// unauthenticated request produces an actor with no type at all, which every service
// refuses: on the two routes that take no credential nothing calls this, so reaching it
// there is a wiring bug and must not resolve to a role.
func actorFrom(c *gin.Context) service.Actor {
	actor := service.Actor{RequestID: utils.RequestID(c)}

	caller, ok := middleware.CallerOf(c)
	if !ok {
		return actor
	}
	actor.ID = caller.Name

	switch caller.Role {
	case middleware.RoleOperator:
		actor.Type = service.ActorOperator
	case middleware.RoleUser:
		actor.Type = service.ActorUser
		actor.UserID = caller.UserID
	default:
		actor.Type = service.ActorBot
	}
	return actor
}

// respondError renders a service error as the API's envelope.
//
// The mapping is the whole reason handlers never inspect an error string: a service
// returns one of five sentinels, this decides the status. Anything unrecognised is a 500
// whose detail is logged and not returned — an error from a repository can carry a
// query, a column name or a DSN, and none of those belongs in a response body.
func (h *Handler) respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrValidation):
		// A field-by-field answer when the service produced one, so a form can mark the
		// inputs rather than showing one sentence above all of them.
		var fields domain.ValidationErrors
		if errors.As(err, &fields) && len(fields) > 0 {
			utils.ValidationError(c, "The request could not be accepted as given.", fields.Fields())
			return
		}
		utils.Error(c, http.StatusBadRequest, utils.ErrCodeValidation, cleanMessage(err))

	case errors.Is(err, service.ErrUnauthenticated):
		// The same sentence for every kind of credential failure. The service already
		// refuses to distinguish them; rendering them differently here would undo that.
		utils.Error(c, http.StatusUnauthorized, utils.ErrCodeUnauthorized,
			"Those credentials are not valid.")

	case errors.Is(err, service.ErrForbidden):
		// Safe to return in full: it names the operation and the caller's own role,
		// which is exactly what somebody needs to understand that asking again will not
		// help. It never names another account.
		utils.Error(c, http.StatusForbidden, utils.ErrCodeForbidden, cleanMessage(err))

	case errors.Is(err, service.ErrNotFound):
		// Deliberately not the service's message. "That chat belongs to somebody else"
		// and "there is no such chat" are the same answer here, because an id that can
		// be confirmed can be enumerated.
		utils.Error(c, http.StatusNotFound, utils.ErrCodeNotFound, "Not found.")

	case errors.Is(err, service.ErrConflict):
		utils.Error(c, http.StatusConflict, utils.ErrCodeAlreadyResolved, cleanMessage(err))

	default:
		h.log.Error().Err(err).
			Str("requestId", utils.RequestID(c)).
			Str("path", c.FullPath()).
			Msg("request failed")
		utils.Error(c, http.StatusInternalServerError, utils.ErrCodeInternal,
			"Something went wrong. The request id in this response will find it in the logs.")
	}
}

// cleanMessage strips the internal sentinel prefixes from an error before it is shown to
// a caller. "request is not valid: a task id is required" is a log line; "a task id is
// required" is an API message.
func cleanMessage(err error) string {
	msg := err.Error()
	for _, prefix := range []string{
		service.ErrValidation.Error() + ": ",
		service.ErrForbidden.Error() + ": ",
		service.ErrConflict.Error() + ": ",
		service.ErrNotFound.Error() + ": ",
		service.ErrUnauthenticated.Error() + ": ",
	} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

// badRequest answers a request that never reached a service, e.g. one whose JSON did not
// parse.
func badRequest(c *gin.Context, message string) {
	utils.Error(c, http.StatusBadRequest, utils.ErrCodeValidation, message)
}

// bindJSON decodes a request body, answering the caller itself on failure.
//
// The bind error is not returned verbatim: Go's JSON errors name struct fields and
// types, which describes this service's internals rather than the caller's mistake.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		badRequest(c, "The request body is not valid JSON for this endpoint.")
		return false
	}
	return true
}

// defaultRenderedLimit mirrors the services' own default page size. It is only used to
// turn a page number into an offset and to render pagination metadata, never to bound a
// query — the service clamps that itself.
const defaultRenderedLimit = 50

// page reads limit and offset from the query string.
//
// Anything unparseable is treated as absent rather than rejected: the service clamps the
// values anyway, and failing a listing because a dashboard sent ?limit= is noise, not
// safety.
func page(c *gin.Context) (limit, offset int) {
	limit = intQuery(c, "limit", 0)
	offset = intQuery(c, "offset", 0)
	if p := intQuery(c, "page", 0); p > 1 && offset == 0 {
		// Pagination metadata is rendered as pages, so accept pages on the way in too. A
		// caller that sends both wins with the explicit offset.
		effective := limit
		if effective <= 0 {
			effective = defaultRenderedLimit
		}
		offset = (p - 1) * effective
	}
	return limit, offset
}

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

// boolQuery reads a flag from the query string. Absent and unparseable both mean false,
// which is the restrictive reading for every flag core takes this way.
func boolQuery(c *gin.Context, name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(c.Query(name)))
	return err == nil && value
}

// paged renders a list response, converting the offset back into a page number so the
// envelope's pagination block reads the way the rest of the portfolio's APIs do.
func paged(c *gin.Context, message string, data any, limit, offset, total int) {
	if limit <= 0 {
		limit = defaultRenderedLimit
	}
	pageNumber := offset/limit + 1
	utils.SuccessWithPagination(c, http.StatusOK, message, data, pageNumber, limit, total)
}
