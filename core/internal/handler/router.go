package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/middleware"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// operatorOnlyRoutes names the calls that need whoever runs the instance. It is declared
// next to the routes it describes so the list a client reads from /v1/reference cannot
// drift from the guards actually installed below.
//
// All three are account administration. There is deliberately nothing about runs here: an
// operator can read any run and cancel any run, but that is decided in the service from the
// actor, not gated at the route, because the same handler has to serve a person reading
// their own.
var operatorOnlyRoutes = []string{
	"POST /v1/accounts",
	"GET /v1/accounts",
	"PUT /v1/accounts/:id/active",
}

// RouterDeps is everything the router needs beyond the handlers themselves.
type RouterDeps struct {
	Handler *Handler
	// Credentials are the accepted inbound machine keys, each already carrying the role
	// the operator configured it with. A user's session is not in here — it is resolved
	// per request against Sessions.
	Credentials []middleware.Credential
	// Sessions resolves a session token to the person holding it. Without it no signed-in
	// human could authenticate, so it is required rather than optional.
	Sessions  middleware.SessionStore
	RateLimit config.RateLimitConfig
	// TrustedProxies bounds which hops may set X-Forwarded-For. An empty list means the
	// client IP is the socket's peer address, which is the right answer when nothing sits
	// in front of this service.
	TrustedProxies []string
	Logger         zerolog.Logger
}

// NewRouter wires the middleware chain and the routes.
//
// The order of the chain is load-bearing, and is the same order the goal engine uses:
//
//	RequestID    — so every later line and every response can be correlated
//	Recover      — outside the handlers, so a panic in one becomes a 500, not a dropped
//	               connection
//	AccessLog    — after Recover, so a panicking request is still logged as one
//	RateLimit    — keyed by IP, ahead of authentication, so an unauthenticated flood is
//	               cheap to refuse
//	Authenticate — everything past here has a known caller
//	RateLimit    — keyed by principal, so one busy agent cannot consume the whole per-IP
//	               budget it shares with a person behind the same NAT
//
// Health and readiness sit outside the authenticated half: a probe that needs a credential
// is a probe that fails during a credential rotation.
//
// Registration and sign-in also sit outside it, and they are the two routes where the
// per-IP limiter is the only thing standing between the door and a password guesser. That
// is why they are in a group with it rather than on the bare engine.
func NewRouter(deps RouterDeps) (*gin.Engine, error) {
	if deps.Handler == nil {
		return nil, errNoHandler
	}
	if deps.Sessions == nil {
		return nil, errNoSessions
	}
	if len(deps.Credentials) == 0 {
		return nil, errNoCredentials
	}

	engine := gin.New()
	// Gin trusts every proxy by default, which lets any client set its own
	// X-Forwarded-For and so choose its own rate-limit bucket — and, on the two
	// unauthenticated routes below, its own password-guessing budget.
	if err := engine.SetTrustedProxies(deps.TrustedProxies); err != nil {
		return nil, err
	}
	engine.HandleMethodNotAllowed = true

	engine.Use(
		middleware.RequestID(),
		middleware.Recover(deps.Logger),
		middleware.AccessLog(deps.Logger),
	)

	engine.NoRoute(func(c *gin.Context) {
		utils.Error(c, http.StatusNotFound, utils.ErrCodeNotFound, "No such endpoint.")
	})
	engine.NoMethod(func(c *gin.Context) {
		utils.Error(c, http.StatusMethodNotAllowed, utils.ErrCodeNotFound,
			"That method is not allowed on this endpoint.")
	})

	h := deps.Handler
	engine.GET("/healthz", h.Health)
	engine.GET("/readyz", h.Ready)

	byIP := middleware.RateLimit(deps.RateLimit.RPS, deps.RateLimit.Burst, middleware.KeyByIP, deps.Logger)
	// Built once and shared by both authenticated groups below. Each RateLimit call owns
	// its own bucket store, so constructing a second one for the goal engine's path would
	// hand a caller two separate budgets and double the rate the operator configured.
	authenticate := middleware.Authenticate(deps.Credentials, deps.Sessions, deps.Logger)
	byPrincipal := middleware.RateLimit(deps.RateLimit.RPS, deps.RateLimit.Burst, middleware.KeyByPrincipal, deps.Logger)

	// The unauthenticated door. Register refuses itself when CORE_OPEN_REGISTRATION is
	// false — the check is in the service, because there is no credential here to hang a
	// middleware on.
	open := engine.Group("/v1/auth", byIP)
	{
		open.POST("/register", h.Register)
		open.POST("/signin", h.SignIn)
	}

	v1 := engine.Group("/v1", byIP, authenticate, byPrincipal)

	var (
		operator = middleware.RequireOperator(deps.Logger)
		user     = middleware.RequireUser(deps.Logger)
	)

	v1.GET("/reference", h.Reference)

	// Signing out is authenticated because it ends the session the caller presented, and
	// a route that ended a session named in the body would be a way to sign somebody else
	// out. A machine key here ends nothing and still answers 200.
	v1.POST("/auth/signout", h.SignOut)

	me := v1.Group("/me", user)
	{
		me.GET("", h.Me)
		me.PUT("/password", h.ChangePassword)
		me.GET("/ledger", h.Ledger)
		me.GET("/sessions", h.ListSessions)
		me.DELETE("/sessions", h.RevokeAllSessions)
		me.DELETE("/sessions/:id", h.RevokeSession)
	}

	// Operator only, on the route and again in the service. A route check protects a
	// route; the service check protects the invariant if a second route ever reaches the
	// same operation.
	accounts := v1.Group("/accounts", operator)
	{
		accounts.POST("", h.CreateAccount)
		accounts.GET("", h.ListAccounts)
		accounts.PUT("/:id/active", h.SetAccountActive)
	}

	chats := v1.Group("/chats", user)
	{
		chats.POST("", h.CreateChat)
		chats.GET("", h.ListChats)
		chats.GET("/:id", h.GetChat)
		chats.PUT("/:id/title", h.RenameChat)
		// Archive rather than delete, and there is no delete: a chat's messages are the
		// visible half of an append-only run transcript.
		chats.POST("/:id/archive", h.ArchiveChat)
		chats.GET("/:id/messages", h.ChatMessages)
	}

	// A person saying something to their agent. RequireUser because a machine key has no
	// chat to say it in — unattended work arrives through the dispatch route below.
	v1.POST("/messages", user, h.SendMessage)

	// Connecting a chat account, and only ever half of it. This is the half a signed-in
	// person performs: ask for a code, see what is connected, disconnect one. The other
	// half — attaching an external id to an account — happens on the inbound path when a
	// code arrives *over* the channel, because that message is the proof. RequireUser
	// because a machine key owns no chat account and must not be able to connect one.
	channels := v1.Group("/channels", user)
	{
		channels.POST("/link-codes", h.MintLinkCode)
		channels.GET("", h.ListChannels)
		// No update. A connection is made by proving it and ended by revoking it; editing
		// one would mean moving somebody else's chat account onto this account.
		channels.DELETE("/:id", h.Unlink)
	}

	tasks := v1.Group("/tasks")
	{
		// No route guard: the service scopes reads by actor, giving a person their own
		// tasks and the goal engine the unattended account's. Dispatch refuses a person
		// itself, because starting work nobody is watching is an operator's decision.
		tasks.POST("", h.DispatchTask)
		tasks.GET("", h.ListTasks)
		tasks.GET("/:id", h.GetTask)
		tasks.GET("/:id/runs", h.TaskRuns)
	}

	runs := v1.Group("/runs")
	{
		runs.GET("/:id", h.GetRun)
		runs.GET("/:id/steps", h.RunSteps)
		runs.GET("/:id/cost", h.RunCost)
		runs.POST("/:id/cancel", h.CancelRun)
	}

	// The goal engine telling the operator something happened — a goal behind pace, a spend
	// waiting for a human. No route guard, on the same reasoning as the tasks group: the
	// service refuses a signed-in person itself, so who may make this instance send a
	// message is decided in one place.
	//
	// There is no GET here and no history. A notification is a message that was sent, not a
	// record: the goal engine's audit log is what says a trigger fired and an approval is
	// pending, and a second list of the same events with weaker rules around it would be a
	// second answer to the same question.
	v1.POST("/notifications", h.Notify)

	// The path the goal engine is hardcoded to call — DefaultTaskPath in
	// goal-engine/internal/core/client.go. It reaches the same handler as POST /v1/tasks
	// rather than a copy of it: two dispatch paths would be two places for the
	// idempotency rule to be got wrong, and that rule is what stops a retried trigger
	// becoming a second run.
	engine.POST("/api/v1/tasks", byIP, authenticate, byPrincipal, h.DispatchTask)

	return engine, nil
}

// CredentialsFrom converts configured API keys into middleware credentials, carrying each
// key's role across unchanged.
//
// This lives here rather than in config or middleware so neither package has to import the
// other: config reads the environment, middleware checks requests, and the transport layer
// is what needs both.
func CredentialsFrom(keys []config.APIKey) []middleware.Credential {
	creds := make([]middleware.Credential, 0, len(keys))
	for _, key := range keys {
		creds = append(creds, middleware.Credential{
			Name:   key.Name,
			Secret: key.Secret,
			Role:   roleFor(key.Role),
		})
	}
	return creds
}

// roleFor maps a configured role onto the transport's role.
//
// Anything unrecognised becomes a bot, the least privileged machine role. The two enums are
// validated at load time and cannot disagree today, but a mapping that failed open would
// turn a future third role into an operator by accident, and this is the one place in the
// service where that mistake would be silent.
//
// Note what is not here: config has no user role to map. A human is authenticated by a
// session, and no environment variable can produce one.
func roleFor(role config.Role) middleware.Role {
	if role == config.RoleOperator {
		return middleware.RoleOperator
	}
	return middleware.RoleBot
}
