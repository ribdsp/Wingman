package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/config"
	"github.com/ribdsp/wingman/goal-engine/internal/middleware"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// operatorOnlyRoutes names the calls a human has to make. It is declared here,
// next to the routes it describes, so the list a client reads from /v1/reference
// cannot drift from the guards actually installed below.
//
// Each entry is an operation an agent could otherwise use to hand itself what the
// design says it must be given: clearing its own spend, and rewriting the target
// it is measured against. Releasing the kill switch is the third, and it is absent
// on purpose — that route stays open because engaging the switch must reach the
// same handler, and the direction is separated inside the service.
var operatorOnlyRoutes = []string{
	"POST /v1/approvals/:id/resolve",
	"PATCH /v1/goals/:id",
}

// RouterDeps is everything the router needs beyond the handlers themselves.
type RouterDeps struct {
	Handler *Handler
	// Credentials are the accepted inbound keys, each already carrying the role
	// the operator configured it with.
	Credentials []middleware.Credential
	RateLimit   config.RateLimitConfig
	// TrustedProxies bounds which hops may set X-Forwarded-For. An empty list
	// means the client IP is the socket's peer address, which is the right answer
	// when nothing sits in front of this service.
	TrustedProxies []string
	Logger         zerolog.Logger
}

// NewRouter wires the middleware chain and the routes.
//
// The order of the chain is load-bearing:
//
//	RequestID  — so every later line and every response can be correlated
//	Recover    — outside the handlers, so a panic in one becomes a 500, not a
//	             dropped connection
//	AccessLog  — after Recover, so a panicking request is still logged as one
//	RateLimit  — keyed by IP, ahead of authentication, so an unauthenticated
//	             flood is cheap to refuse
//	APIKeyAuth — everything past here has a known caller
//	RateLimit  — keyed by principal, so one busy agent cannot consume the whole
//	             per-IP budget shared with the operator behind the same NAT
//
// Health and readiness sit outside the chain's authenticated half: a probe that
// needs a credential is a probe that fails during a credential rotation.
func NewRouter(deps RouterDeps) (*gin.Engine, error) {
	if deps.Handler == nil {
		return nil, errNoHandler
	}
	if len(deps.Credentials) == 0 {
		// Serving this API unauthenticated would expose the kill switch and the
		// approval queue to anyone who can reach the port.
		return nil, errNoCredentials
	}

	engine := gin.New()
	// Gin trusts every proxy by default, which lets any client set its own
	// X-Forwarded-For and so choose its own rate-limit bucket.
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

	v1 := engine.Group("/v1",
		middleware.RateLimit(deps.RateLimit.RPS, deps.RateLimit.Burst, middleware.KeyByIP, deps.Logger),
		middleware.APIKeyAuth(deps.Credentials, deps.Logger),
		middleware.RateLimit(deps.RateLimit.RPS, deps.RateLimit.Burst, middleware.KeyByPrincipal, deps.Logger),
	)

	operator := middleware.RequireOperator(deps.Logger)

	v1.GET("/reference", h.Reference)

	goals := v1.Group("/goals")
	{
		goals.POST("", h.CreateGoal)
		goals.GET("", h.ListGoals)
		goals.GET("/:id", h.GetGoal)
		// The recorded pace history. Read-only, and readable by a bot: it is what
		// the monitor already decided, and a dashboard that could show a target but
		// not whether it is being met would be showing the less useful half.
		goals.GET("/:id/evaluations", h.ListGoalEvaluations)
		// Operator only. Also enforced in the service: a route check protects a
		// route, the service check protects the invariant if a second route ever
		// reaches the same operation.
		goals.PATCH("/:id", operator, h.PatchGoal)
	}

	approvals := v1.Group("/approvals")
	{
		approvals.POST("", h.RequestApproval)
		approvals.GET("", h.ListApprovals)
		approvals.GET("/:id", h.GetApproval)
		approvals.POST("/:id/resolve", operator, h.ResolveApproval)
		approvals.POST("/expire", h.ExpireApprovals)
	}

	flags := v1.Group("/flags")
	{
		flags.GET("", h.ListFlags)
		flags.GET("/kill-switch", h.GetKillSwitch)
		// Deliberately not operator-gated at the route: engaging the switch has to
		// be reachable by an agent that notices it is doing damage. Only the
		// release direction is a human's, and that is enforced in the service.
		flags.PUT("/kill-switch", h.SetKillSwitch)
	}

	v1.GET("/audit", h.ListAudit)

	// Where everything stands, in one request: the newest evaluation of every goal
	// that has one. Outside the goals group because it spans them, and separate from
	// the monitor tick because reading the last verdict must never mean running a
	// new one.
	v1.GET("/evaluations/latest", h.LatestEvaluations)

	metricsGroup := v1.Group("/metrics")
	{
		// Read-only by design. A metric definition carries a SQL query, so there is
		// no endpoint that creates one; the registry is loaded from the operator's
		// YAML at startup and never changes while the process lives.
		metricsGroup.GET("", h.ListMetrics)
		metricsGroup.GET("/:key", h.GetMetric)
		// Values, unlike definitions, do arrive over the wire — but only for a metric
		// the operator declared as push. The service refuses a value for anything it
		// can read for itself, so this route cannot be used to overwrite a number that
		// comes from the database.
		metricsGroup.POST("/:key/samples", h.RecordSample)
		metricsGroup.GET("/:key/samples/latest", h.LatestSample)
	}

	v1.POST("/monitor/tick", h.Tick)

	return engine, nil
}

// CredentialsFrom converts configured API keys into middleware credentials,
// carrying each key's role across unchanged.
//
// This lives here rather than in config or middleware so neither package has to
// import the other: config reads the environment, middleware checks requests, and
// the transport layer is what needs both.
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
// Anything unrecognised becomes a bot, the least privileged role. The two enums
// are validated at load time and cannot disagree today, but a mapping that failed
// open would turn a future third role into an operator by accident, and this is
// the one place in the service where that mistake would be silent.
func roleFor(role config.Role) middleware.Role {
	if role == config.RoleOperator {
		return middleware.RoleOperator
	}
	return middleware.RoleBot
}
