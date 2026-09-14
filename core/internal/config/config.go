// Package config loads and validates Wingman core's environment.
//
// Loading is fail-fast and reports every problem at once: a service that runs
// autonomous agents against somebody's API key and somebody else's sandbox should
// refuse to start half-configured rather than discover a missing credential four
// tool calls into a run.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	// defaultPort is 8081, not 8080: the goal engine has 8080, and the two services
	// are expected to run side by side on one host.
	defaultPort   = 8081
	defaultAppEnv = "development"
	// defaultLogLevel is info. Debug is never the default because the debug path
	// logs request bodies, and a request body here can contain a user's brief.
	defaultLogLevel = "info"
	// defaultTimezone is UTC because this service is self-hosted anywhere and an unset
	// TIMEZONE must not silently render every timestamp — and place every daily token
	// cap boundary — in whichever zone the author happened to work in. An operator who
	// wants local time sets the variable.
	defaultTimezone      = "UTC"
	defaultShutdownGrace = 30 * time.Second
	defaultMigrationsDir = "migrations"

	defaultToolsConfigPath = "config/tools.yaml"
	defaultMCPConfigPath   = "config/mcp.yaml"
	defaultHTTPConfigPath  = "config/http-tools.yaml"

	// defaultSessionTTL is a week. Long enough that a self-hosted user is not
	// signing in daily, short enough that a stolen token is not a permanent one.
	defaultSessionTTL = 168 * time.Hour
	minSessionTTL     = 15 * time.Minute
	maxSessionTTL     = 90 * 24 * time.Hour

	// Rate limits are set well above what a person or a well-behaved bot needs;
	// they exist to bound a retry loop, not to shape traffic.
	defaultRateLimitRPS   = 10.0
	defaultRateLimitBurst = 30

	defaultGoalEngineTimeout = 15 * time.Second
	// defaultSpendMetric is the push metric core reports its token spend to, so
	// cost becomes something a goal can be written against.
	defaultSpendMetric = "ops.tokens_spent"

	defaultRunWorkers  = 2
	maxRunWorkers      = 64
	defaultQueuePoll   = 5 * time.Second
	minQueuePoll       = time.Second
	defaultSandboxRoot = "workspaces"
	// defaultSandboxImage is pinned by digest-less tag here only because the
	// operator is expected to replace it; the .env.example says so.
	defaultSandboxImage = "wingman/sandbox:latest"
	// defaultSandboxMemory bounds a container on a small VPS. A runaway process
	// inside the sandbox should kill its own container, not the host.
	defaultSandboxMemory = "512m"

	// A link code is a bearer credential: whoever sends it from a chat account attaches
	// that account to the person who minted it. Fifteen minutes is long enough to pick
	// up another device and open a chat app, short enough that one left in a scrollback
	// is already dead. internal/service/channels.go holds the same three bounds and
	// clamps to them — this refuses instead, because a value an operator wrote and a
	// value the service used should not silently differ.
	defaultLinkCodeTTL = 15 * time.Minute
	minLinkCodeTTL     = time.Minute
	maxLinkCodeTTL     = time.Hour
)

// Config is the fully validated runtime configuration.
type Config struct {
	AppEnv   string
	Port     int
	LogLevel string
	// Location is resolved from TIMEZONE and used for every timestamp the API
	// renders.
	Location *time.Location

	DatabaseURL string

	// MigrationsDir holds the SQL applied at startup when AutoMigrate is on.
	// Self-hosting is the deployment model, so migrating on boot is the default.
	MigrationsDir string
	AutoMigrate   bool

	Auth       AuthConfig
	Providers  ProvidersConfig
	Sandbox    SandboxConfig
	Tools      ToolsConfig
	Runner     RunnerConfig
	GoalEngine GoalEngineConfig
	Channels   ChannelsConfig
	RateLimit  RateLimitConfig

	// Limits bounds every run this instance starts. It is already defaulted and
	// validated by the time Load returns.
	Limits domain.RunLimits

	// APIKeys are the accepted inbound machine credentials — operators and bots.
	// Human users are not here; they live in the database.
	APIKeys []APIKey

	// UnattendedOwner is the email address of the account that work nobody asked for —
	// a goal-engine trigger — is filed against and billed to.
	//
	// An address rather than an account id, because an operator writes this into a
	// compose file by hand; cmd resolves it to an id once at boot. Required as soon as
	// a bot key exists: a dispatch with no owner has no budget to spend against and no
	// ledger to appear in, and the alternative — an unowned run — is a run whose cost
	// nobody sees.
	UnattendedOwner string

	// TrustedProxies bounds which hops may set X-Forwarded-For. Empty means the
	// client IP is the socket's peer address, which is right when nothing sits in
	// front of this service — and wrong, in a way that lets a caller choose its own
	// rate-limit bucket, when something does.
	TrustedProxies []string

	ShutdownGrace time.Duration
}

// AuthConfig covers the human half of authentication.
type AuthConfig struct {
	// OpenRegistration allows anyone who can reach the service to create an
	// account. It defaults to false, and that default is the safety property: a
	// self-hosted box found on the internet with open sign-up is a box running
	// strangers' code in your sandbox on your API key. The first account is made
	// with `core createuser`.
	OpenRegistration bool
	// SessionTTL is how long a session token stays valid.
	SessionTTL time.Duration
}

// ProvidersConfig holds the model providers this instance can use.
type ProvidersConfig struct {
	AnthropicAPIKey string
	OpenAIAPIKey    string
	// Default is which provider a run uses when it names none.
	Default string
	// DefaultModel is the model id for that provider. It is not defaulted in code:
	// model names change on the vendors' schedule, and a stale constant baked into
	// a release is worse than a required setting.
	DefaultModel string
}

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// SandboxConfig chooses and bounds where tool calls execute.
type SandboxConfig struct {
	// Backend is "local" or "docker". Remote backends (E2B, Daytona, a separate
	// VPS) are later work behind the same port.
	Backend string
	// WorkspaceRoot is the parent directory of the per-user workspaces. Workspaces
	// are never shared between users.
	WorkspaceRoot string
	// Image is the container image the docker backend runs.
	Image string
	// Network is the docker network mode, defaulting to "none". A sandbox with
	// network access is a different threat model from one without, so reaching the
	// internet from inside a tool call is something an operator turns on.
	Network string
	// Memory is the container memory limit, in docker's syntax.
	Memory string
}

const (
	SandboxLocal  = "local"
	SandboxDocker = "docker"
)

// ToolsConfig points at the operator-owned tool declarations.
//
// All three files are operator-owned and there is deliberately no API that writes
// them, for the same reason the goal engine has no API that creates a metric: a list
// of what an autonomous agent may do is not a list an autonomous agent may edit.
type ToolsConfig struct {
	// GrantsPath declares each tool's class and whether it is enabled.
	GrantsPath string
	// MCPPath declares the MCP servers to connect to.
	MCPPath string
	// HTTPPath declares the HTTP APIs whose endpoints are offered as tools.
	HTTPPath string
}

// RunnerConfig bounds the background workers that execute queued runs.
type RunnerConfig struct {
	// Workers is how many runs may execute at once across the whole instance.
	Workers int
	// QueuePoll is how often a worker looks for queued work.
	QueuePoll time.Duration
}

// GoalEngineConfig points back at the goal engine, for the kill switch and for
// reporting spend.
//
// An empty BaseURL means no goal engine is configured, and then neither feature is
// active — core runs standalone. When a base URL *is* set the kill switch is
// fail-closed: unreadable means engaged, and engaged means halt. Those two rules
// have to be stated together, because "absent" and "unreachable" must not be
// confused. Nothing configured is a deliberate choice; configured and unanswerable
// is a fault.
type GoalEngineConfig struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
	// ReportSpend pushes each run's token usage to the goal engine as a metric
	// sample.
	ReportSpend bool
	// SpendMetric is the metric name those samples are filed under.
	SpendMetric string
}

// Configured reports whether a goal engine is wired up at all.
func (g GoalEngineConfig) Configured() bool { return g.BaseURL != "" }

// ChannelsConfig is which chat platforms this instance connects to, and the rules that
// bound a message arriving on one.
//
// Every credential here opens an *outbound* connection — Telegram long polling, Slack's
// Socket Mode socket, Discord's gateway — so none of them implies a public address, an
// inbound port, or a route that has to verify somebody else's request signature. An
// absent token is a platform this instance is simply not on, which is the default: core
// is useful reached over HTTP alone, by a client and by the goal engine.
type ChannelsConfig struct {
	TelegramToken string
	// Slack issues two credentials and needs both: the bot token talks to the Web API,
	// the app token opens the socket.
	SlackBotToken string
	SlackAppToken string
	DiscordToken  string

	// LinkCodeTTL is how long a minted link code stays redeemable.
	LinkCodeTTL time.Duration
	// MinInterval is the shortest gap between two messages accepted from one sender on
	// one platform. It is a budget guard rather than a politeness rule: every accepted
	// message is a run, and a run spends the linked person's tokens.
	MinInterval time.Duration
	// AllowGroups lets a message from a shared room reach the agent. Off by default,
	// because in a group the linked person's budget is spendable by anybody who can type
	// there, and the run would be filed against them.
	AllowGroups bool
}

// Configured reports whether any platform has credentials. False is the ordinary case.
func (c ChannelsConfig) Configured() bool {
	return c.TelegramToken != "" || c.SlackConfigured() || c.DiscordToken != ""
}

// SlackConfigured reports whether both of Slack's tokens are present. One alone connects
// to nothing, which is why Load refuses rather than starting half a Slack app.
func (c ChannelsConfig) SlackConfigured() bool {
	return c.SlackBotToken != "" && c.SlackAppToken != ""
}

// RateLimitConfig bounds request rate per caller. It is a runaway-client guard
// rather than a security boundary.
type RateLimitConfig struct {
	RPS   float64
	Burst int
}

// IsProduction reports whether the service is running with production defaults.
func (c Config) IsProduction() bool {
	return strings.EqualFold(c.AppEnv, "production")
}

// Load reads .env when present, then the process environment, and validates the
// result. The returned error aggregates every problem found.
func Load() (Config, error) {
	// A missing .env is normal in a container; only surface real parse failures.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return Config{}, fmt.Errorf("parse .env: %w", err)
		}
	}

	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	cfg := Config{
		AppEnv:         stringEnv("APP_ENV", defaultAppEnv),
		LogLevel:       stringEnv("LOG_LEVEL", defaultLogLevel),
		DatabaseURL:    strings.TrimSpace(os.Getenv("DATABASE_URL")),
		MigrationsDir:  stringEnv("MIGRATIONS_DIR", defaultMigrationsDir),
		TrustedProxies: splitAndTrim(os.Getenv("TRUSTED_PROXIES")),
	}
	cfg.AutoMigrate = boolEnv("AUTO_MIGRATE", true, fail)
	cfg.ShutdownGrace = durationEnv("SHUTDOWN_GRACE", defaultShutdownGrace, fail)

	cfg.Port = intEnv("PORT", defaultPort, fail)
	if cfg.Port < 1 || cfg.Port > 65535 {
		fail("PORT must be between 1 and 65535, got %d", cfg.Port)
	}

	loc, err := time.LoadLocation(stringEnv("TIMEZONE", defaultTimezone))
	if err != nil {
		fail("TIMEZONE is not a known IANA zone: %v", err)
		loc = time.UTC
	}
	cfg.Location = loc

	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}

	cfg.Auth = AuthConfig{
		OpenRegistration: boolEnv("CORE_OPEN_REGISTRATION", false, fail),
		SessionTTL:       durationEnv("SESSION_TTL", defaultSessionTTL, fail),
	}
	switch {
	case cfg.Auth.SessionTTL < minSessionTTL:
		fail("SESSION_TTL must be at least %s, got %s", minSessionTTL, cfg.Auth.SessionTTL)
	case cfg.Auth.SessionTTL > maxSessionTTL:
		fail("SESSION_TTL must be at most %s, got %s", maxSessionTTL, cfg.Auth.SessionTTL)
	}

	cfg.Providers = loadProviders(fail)
	cfg.Sandbox = loadSandbox(fail)

	cfg.Tools = ToolsConfig{
		GrantsPath: stringEnv("TOOLS_CONFIG_PATH", defaultToolsConfigPath),
		MCPPath:    stringEnv("MCP_CONFIG_PATH", defaultMCPConfigPath),
		HTTPPath:   stringEnv("HTTP_TOOLS_CONFIG_PATH", defaultHTTPConfigPath),
	}

	cfg.Runner = RunnerConfig{
		Workers:   intEnv("RUN_WORKERS", defaultRunWorkers, fail),
		QueuePoll: durationEnv("RUN_QUEUE_POLL", defaultQueuePoll, fail),
	}
	switch {
	case cfg.Runner.Workers < 1:
		fail("RUN_WORKERS must be at least 1, got %d", cfg.Runner.Workers)
	case cfg.Runner.Workers > maxRunWorkers:
		// Each worker is a concurrent model conversation with a container behind it.
		fail("RUN_WORKERS must be at most %d, got %d", maxRunWorkers, cfg.Runner.Workers)
	}
	if cfg.Runner.QueuePoll < minQueuePoll {
		fail("RUN_QUEUE_POLL must be at least %s, got %s", minQueuePoll, cfg.Runner.QueuePoll)
	}

	cfg.GoalEngine = loadGoalEngine(fail)
	cfg.Channels = loadChannels(fail)
	cfg.Limits = loadLimits(fail)

	cfg.RateLimit = RateLimitConfig{
		RPS:   floatEnv("RATE_LIMIT_RPS", defaultRateLimitRPS, fail),
		Burst: intEnv("RATE_LIMIT_BURST", defaultRateLimitBurst, fail),
	}
	if cfg.RateLimit.RPS <= 0 {
		fail("RATE_LIMIT_RPS must be positive, got %v", cfg.RateLimit.RPS)
	}
	if cfg.RateLimit.Burst < 1 {
		fail("RATE_LIMIT_BURST must be at least 1, got %d", cfg.RateLimit.Burst)
	}

	// Both key lists share one uniqueness check, so a secret listed as both an
	// operator and a bot cannot leave a request's privilege depending on search
	// order.
	seenName, seenSecret := map[string]string{}, map[string]string{}
	operators := parseAPIKeys("CORE_API_KEYS", os.Getenv("CORE_API_KEYS"), RoleOperator, seenName, seenSecret, fail)
	bots := parseAPIKeys("CORE_BOT_KEYS", os.Getenv("CORE_BOT_KEYS"), RoleBot, seenName, seenSecret, fail)
	if len(operators) == 0 {
		fail("CORE_API_KEYS is required: at least one operator API key must be configured")
	}
	// Bot keys are optional: core is useful with human users alone, before a goal
	// engine is pointed at it.
	cfg.APIKeys = append(operators, bots...)

	// Left as the operator wrote it apart from case and spacing; whether an account
	// exists for it is Accounts.ResolveOwner's question, asked once at boot.
	cfg.UnattendedOwner = strings.ToLower(strings.TrimSpace(os.Getenv("CORE_UNATTENDED_OWNER")))
	if len(bots) > 0 && cfg.UnattendedOwner == "" {
		fail("CORE_UNATTENDED_OWNER is required when CORE_BOT_KEYS is set: " +
			"unattended work is filed against a named account, and there is no default")
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func loadProviders(fail func(string, ...any)) ProvidersConfig {
	p := ProvidersConfig{
		AnthropicAPIKey: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		OpenAIAPIKey:    strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		Default:         strings.ToLower(stringEnv("DEFAULT_PROVIDER", ProviderAnthropic)),
		DefaultModel:    strings.TrimSpace(os.Getenv("DEFAULT_MODEL")),
	}

	switch p.Default {
	case ProviderAnthropic:
		if p.AnthropicAPIKey == "" {
			fail("ANTHROPIC_API_KEY is required when DEFAULT_PROVIDER=%s", ProviderAnthropic)
		}
	case ProviderOpenAI:
		if p.OpenAIAPIKey == "" {
			fail("OPENAI_API_KEY is required when DEFAULT_PROVIDER=%s", ProviderOpenAI)
		}
	default:
		fail("DEFAULT_PROVIDER must be %q or %q, got %q", ProviderAnthropic, ProviderOpenAI, p.Default)
	}

	// No fallback model name is compiled in. Vendors retire model ids on their own
	// schedule, and a stale constant in a release fails at the first model call with
	// a message about the vendor rather than about this config.
	if p.DefaultModel == "" {
		fail("DEFAULT_MODEL is required")
	}
	return p
}

func loadSandbox(fail func(string, ...any)) SandboxConfig {
	s := SandboxConfig{
		Backend:       strings.ToLower(stringEnv("SANDBOX_BACKEND", SandboxLocal)),
		WorkspaceRoot: stringEnv("SANDBOX_WORKSPACE_ROOT", defaultSandboxRoot),
		Image:         stringEnv("SANDBOX_IMAGE", defaultSandboxImage),
		Network:       stringEnv("SANDBOX_NETWORK", "none"),
		Memory:        stringEnv("SANDBOX_MEMORY", defaultSandboxMemory),
	}
	if s.Backend != SandboxLocal && s.Backend != SandboxDocker {
		fail("SANDBOX_BACKEND must be %q or %q, got %q", SandboxLocal, SandboxDocker, s.Backend)
	}
	// WorkspaceRoot needs no emptiness check: stringEnv treats a blank variable as
	// unset and hands back defaultSandboxRoot, so there is no path to an empty one.
	return s
}

func loadGoalEngine(fail func(string, ...any)) GoalEngineConfig {
	g := GoalEngineConfig{
		BaseURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("GOAL_ENGINE_BASE_URL")), "/"),
		APIKey:      strings.TrimSpace(os.Getenv("GOAL_ENGINE_API_KEY")),
		Timeout:     durationEnv("GOAL_ENGINE_TIMEOUT", defaultGoalEngineTimeout, fail),
		SpendMetric: stringEnv("GOAL_ENGINE_SPEND_METRIC", defaultSpendMetric),
	}
	g.ReportSpend = boolEnv("GOAL_ENGINE_REPORT_SPEND", g.Configured(), fail)

	if !g.Configured() {
		// Nothing configured, nothing to check. Both features stay off, and the
		// startup log says so plainly rather than leaving an operator to infer it.
		if g.ReportSpend {
			fail("GOAL_ENGINE_REPORT_SPEND=true needs GOAL_ENGINE_BASE_URL")
		}
		return g
	}

	if !strings.HasPrefix(g.BaseURL, "http://") && !strings.HasPrefix(g.BaseURL, "https://") {
		fail("GOAL_ENGINE_BASE_URL must start with http:// or https://")
	}
	// A configured goal engine that core cannot authenticate to is the worst of the
	// three states: the kill switch reads as unreadable, which means engaged, which
	// means every run halts. Refusing to boot says why once instead of leaving that
	// to be diagnosed from halted runs.
	if g.APIKey == "" {
		fail("GOAL_ENGINE_API_KEY is required when GOAL_ENGINE_BASE_URL is set")
	}
	if g.Timeout <= 0 {
		fail("GOAL_ENGINE_TIMEOUT must be positive")
	}
	return g
}

// loadChannels reads the chat-platform credentials and the two rules bounding an inbound
// message. No token is echoed in any problem it reports.
func loadChannels(fail func(string, ...any)) ChannelsConfig {
	c := ChannelsConfig{
		TelegramToken: strings.TrimSpace(os.Getenv("CHANNEL_TELEGRAM_TOKEN")),
		SlackBotToken: strings.TrimSpace(os.Getenv("CHANNEL_SLACK_BOT_TOKEN")),
		SlackAppToken: strings.TrimSpace(os.Getenv("CHANNEL_SLACK_APP_TOKEN")),
		DiscordToken:  strings.TrimSpace(os.Getenv("CHANNEL_DISCORD_TOKEN")),
		LinkCodeTTL:   durationEnv("CHANNEL_LINK_CODE_TTL", defaultLinkCodeTTL, fail),
		MinInterval:   durationEnv("CHANNEL_MIN_INTERVAL", domain.MinInboundInterval, fail),
		AllowGroups:   boolEnv("CHANNEL_ALLOW_GROUPS", false, fail),
	}

	// Half a Slack app is the one combination that is a mistake rather than a choice.
	// Left to boot it fails when the socket opens, with a 401 an operator has to go and
	// look up; named here it costs a line.
	switch {
	case c.SlackBotToken != "" && c.SlackAppToken == "":
		fail("CHANNEL_SLACK_APP_TOKEN is required when CHANNEL_SLACK_BOT_TOKEN is set: Socket Mode needs both")
	case c.SlackAppToken != "" && c.SlackBotToken == "":
		fail("CHANNEL_SLACK_BOT_TOKEN is required when CHANNEL_SLACK_APP_TOKEN is set: Socket Mode needs both")
	case c.SlackBotToken != "" && c.SlackBotToken == c.SlackAppToken:
		// The two are issued separately and are not interchangeable, so one value in both
		// places is a paste error rather than a working app.
		fail("CHANNEL_SLACK_BOT_TOKEN and CHANNEL_SLACK_APP_TOKEN must be different credentials")
	}

	switch {
	case c.LinkCodeTTL < minLinkCodeTTL:
		fail("CHANNEL_LINK_CODE_TTL must be at least %s, got %s", minLinkCodeTTL, c.LinkCodeTTL)
	case c.LinkCodeTTL > maxLinkCodeTTL:
		fail("CHANNEL_LINK_CODE_TTL must be at most %s, got %s", maxLinkCodeTTL, c.LinkCodeTTL)
	}

	// Zero would read as "no throttle", and there is no way to ask for that. An
	// unthrottled sender is one run per message against somebody else's token budget.
	if c.MinInterval < domain.MinInboundInterval {
		fail("CHANNEL_MIN_INTERVAL must be at least %s, got %s", domain.MinInboundInterval, c.MinInterval)
	}
	return c
}

func loadLimits(fail func(string, ...any)) domain.RunLimits {
	limits := domain.RunLimits{
		MaxIterations:       intEnv("RUN_MAX_ITERATIONS", 0, fail),
		MaxToolCalls:        intEnv("RUN_MAX_TOOL_CALLS", 0, fail),
		MaxTokensPerRun:     int64Env("RUN_MAX_TOKENS", 0, fail),
		MaxTokensPerUserDay: int64Env("USER_DAILY_TOKEN_CAP", 0, fail),
		StepTimeout:         durationEnv("RUN_STEP_TIMEOUT", 0, fail),
		SandboxTimeout:      durationEnv("SANDBOX_TIMEOUT", 0, fail),
	}

	// Validate before defaulting: a zero here means "not set", and WithDefaults is
	// what turns it into a working number. Doing it the other way round would
	// silently accept a value the domain rejects.
	if err := limits.Validate(); err != nil {
		var errs domain.ValidationErrors
		if errors.As(err, &errs) {
			for field, message := range errs.Fields() {
				fail("%s %s", envNameForLimit(field), message)
			}
		} else {
			fail("run limits are invalid: %v", err)
		}
	}
	return limits.WithDefaults()
}

// envNameForLimit translates a domain field name back to the variable an operator
// actually set, so the error names something they can edit.
func envNameForLimit(field string) string {
	switch field {
	case "maxIterations":
		return "RUN_MAX_ITERATIONS"
	case "maxToolCalls":
		return "RUN_MAX_TOOL_CALLS"
	case "maxTokensPerRun":
		return "RUN_MAX_TOKENS"
	case "maxTokensPerUserDay":
		return "USER_DAILY_TOKEN_CAP"
	case "stepTimeout":
		return "RUN_STEP_TIMEOUT"
	case "sandboxTimeout":
		return "SANDBOX_TIMEOUT"
	default:
		return field
	}
}
