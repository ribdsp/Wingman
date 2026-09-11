// Package config loads and validates the goal-engine's environment.
//
// Loading is fail-fast and reports every problem at once: a service that wakes
// autonomous agents should refuse to start half-configured rather than discover
// a missing credential mid-decision.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	defaultPort               = 8080
	defaultAppEnv             = "development"
	defaultLogLevel           = "info"
	defaultTimezone           = "Asia/Jakarta"
	defaultMonitorInterval    = time.Hour
	defaultCoreTimeout        = 30 * time.Second
	defaultSampleTimeout      = 15 * time.Second
	defaultShutdownGrace      = 20 * time.Second
	defaultMetricsConfigPath  = "config/metrics.yaml"
	defaultPoliciesConfigPath = "config/policies.yaml"
	defaultMigrationsDir      = "migrations"
	// defaultApprovalTTL and defaultMaxSampleAge mirror the service-layer defaults
	// of the same names. They are repeated here because every other knob an
	// operator can turn is visible in this file, and a governance timeout that
	// could only be changed in code would not be.
	defaultApprovalTTL  = 24 * time.Hour
	defaultMaxSampleAge = 26 * time.Hour
	// Rate limits are set well above what an operator or a well-behaved bot needs;
	// they exist to bound a retry loop, not to shape traffic.
	defaultRateLimitRPS   = 10.0
	defaultRateLimitBurst = 30
	// minAPIKeyLength rejects keys short enough to brute-force.
	minAPIKeyLength = 24
	// maxPrincipalNameLength bounds the name an API key is recorded under.
	maxPrincipalNameLength = 64
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

	Core    CoreConfig
	Monitor MonitorConfig

	// RateLimit bounds how fast one caller may hit the API.
	RateLimit RateLimitConfig

	// APIKeys are the accepted inbound credentials for this service.
	APIKeys []APIKey

	// TrustedProxies bounds which hops may set X-Forwarded-For. Empty means the
	// client IP is the socket's peer address, which is correct when nothing sits in
	// front of this service — and wrong, in a way that lets a caller choose its own
	// rate-limit bucket, when something does.
	TrustedProxies []string

	MetricsConfigPath  string
	PoliciesConfigPath string

	// MigrationsDir holds the SQL migrations applied at startup when AutoMigrate is
	// on. Self-hosting is the deployment model, so migrating on boot is the default;
	// an operator running a managed database can turn it off and apply them out of
	// band.
	MigrationsDir string
	AutoMigrate   bool

	// ApprovalTTL is how long a spend request waits for a human before it expires.
	// It is a governance setting, not a performance one: it decides how long an
	// agent's request stays actionable while nobody is looking.
	ApprovalTTL time.Duration

	ShutdownGrace time.Duration
}

// APIKey is one accepted inbound credential together with the principal name
// every action taken with it is recorded under.
//
// The audit log this service keeps exists to answer "who moved that target".
// A log that can only say "some valid key" does not answer it, so a key carries
// a name from the moment it is configured.
type APIKey struct {
	// Name is what appears in the audit log.
	Name string
	// Secret is the credential itself. It is never logged, and never rendered in
	// an error message.
	Secret string
	// Role is what the credential is allowed to be. It comes from which
	// environment variable the key was listed in, never from the request.
	Role Role
}

// Role separates a human operator from an autonomous agent.
//
// This distinction is the load-bearing part of the approval gate: "over the
// threshold, ask a human" only means something if the service can tell a human
// from the bot that wants the money. A role is therefore a property of the
// credential, fixed at startup by the operator who wrote the env file, and never
// something a caller can assert about itself.
type Role string

const (
	// RoleOperator is a human. Only an operator may clear a spend, release the
	// kill switch, or rewrite a goal.
	RoleOperator Role = "operator"
	// RoleBot is an agent in Wingman core. A bot may state goals, read, and ask
	// permission — it may not grant itself any of the above.
	RoleBot Role = "bot"
)

// CoreConfig points at the Wingman core (the Rakazo fork) that the trigger
// bridge tasks.
type CoreConfig struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
	// TaskPath overrides the path a task is created at. It exists because the core
	// is a fork: an operator who renamed the route should not have to fork this
	// service too.
	TaskPath string
	// DefaultBotID and DefaultChannelID are used when a goal names no bot of its
	// own, so a goal created without one can still be acted on.
	DefaultBotID     string
	DefaultChannelID string
	// DryRun logs the task that would be created and skips the HTTP call. It
	// exists so an operator can watch what the goal layer wants to do for a few
	// days before letting it act.
	DryRun bool
}

// RateLimitConfig bounds request rate per caller. It is a runaway-client guard
// rather than a security boundary: the limits are generous enough that ordinary
// operator use never sees them.
type RateLimitConfig struct {
	// RPS is the sustained requests per second allowed per caller.
	RPS float64
	// Burst is how many requests may arrive at once before throttling starts.
	Burst int
}

// MonitorConfig controls the scheduled metric monitor.
type MonitorConfig struct {
	Enabled  bool
	Interval time.Duration
	// SampleTimeout bounds one metric query.
	SampleTimeout time.Duration
	// MaxGoalsPerTick bounds how much work one tick may do.
	MaxGoalsPerTick int
	// MaxSampleAge is how stale the last reported value of a push metric may be
	// before goals measured on it stop being evaluated. A dead feed leaves its last
	// number in place, and a goal judged on that number looks exactly as on-track as
	// it did when the feed stopped.
	MaxSampleAge time.Duration
}

// IsProduction reports whether the service is running with production defaults.
func (c Config) IsProduction() bool {
	return strings.EqualFold(c.AppEnv, "production")
}

// Load reads .env when present, then the process environment, and validates the
// result. The returned error aggregates every problem found.
func Load() (Config, error) {
	// A missing .env is normal in container deployments; only surface real
	// parse failures.
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
		AppEnv:             stringEnv("APP_ENV", defaultAppEnv),
		LogLevel:           stringEnv("LOG_LEVEL", defaultLogLevel),
		DatabaseURL:        strings.TrimSpace(os.Getenv("DATABASE_URL")),
		MetricsConfigPath:  stringEnv("METRICS_CONFIG_PATH", defaultMetricsConfigPath),
		PoliciesConfigPath: stringEnv("POLICIES_CONFIG_PATH", defaultPoliciesConfigPath),
		MigrationsDir:      stringEnv("MIGRATIONS_DIR", defaultMigrationsDir),
		TrustedProxies:     splitAndTrim(os.Getenv("TRUSTED_PROXIES")),
	}
	cfg.AutoMigrate = boolEnv("AUTO_MIGRATE", true, fail)

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

	cfg.Core = CoreConfig{
		BaseURL:          strings.TrimRight(strings.TrimSpace(os.Getenv("WINGMAN_CORE_BASE_URL")), "/"),
		APIKey:           strings.TrimSpace(os.Getenv("WINGMAN_CORE_API_KEY")),
		Timeout:          durationEnv("WINGMAN_CORE_TIMEOUT", defaultCoreTimeout, fail),
		TaskPath:         strings.TrimSpace(os.Getenv("WINGMAN_CORE_TASK_PATH")),
		DefaultBotID:     strings.TrimSpace(os.Getenv("WINGMAN_DEFAULT_BOT_ID")),
		DefaultChannelID: strings.TrimSpace(os.Getenv("WINGMAN_DEFAULT_CHANNEL_ID")),
		DryRun:           boolEnv("TRIGGER_DRY_RUN", false, fail),
	}
	if cfg.Core.BaseURL == "" {
		fail("WINGMAN_CORE_BASE_URL is required")
	} else if !strings.HasPrefix(cfg.Core.BaseURL, "http://") && !strings.HasPrefix(cfg.Core.BaseURL, "https://") {
		fail("WINGMAN_CORE_BASE_URL must start with http:// or https://")
	}
	if cfg.Core.APIKey == "" && !cfg.Core.DryRun {
		fail("WINGMAN_CORE_API_KEY is required unless TRIGGER_DRY_RUN=true")
	}

	cfg.Monitor = MonitorConfig{
		Enabled:         boolEnv("MONITOR_ENABLED", true, fail),
		Interval:        durationEnv("MONITOR_INTERVAL", defaultMonitorInterval, fail),
		SampleTimeout:   durationEnv("METRIC_SAMPLE_TIMEOUT", defaultSampleTimeout, fail),
		MaxGoalsPerTick: intEnv("MONITOR_MAX_GOALS_PER_TICK", 200, fail),
		MaxSampleAge:    durationEnv("METRIC_MAX_SAMPLE_AGE", defaultMaxSampleAge, fail),
	}
	if cfg.Monitor.Interval < time.Minute {
		fail("MONITOR_INTERVAL must be at least 1m, got %s", cfg.Monitor.Interval)
	}
	if cfg.Monitor.SampleTimeout <= 0 {
		fail("METRIC_SAMPLE_TIMEOUT must be positive")
	}
	if cfg.Monitor.MaxGoalsPerTick < 1 {
		fail("MONITOR_MAX_GOALS_PER_TICK must be at least 1")
	}
	if cfg.Monitor.MaxSampleAge < time.Minute {
		// Anything shorter would stop evaluating a goal between one feed and the next,
		// which reads as a broken engine rather than a stale metric.
		fail("METRIC_MAX_SAMPLE_AGE must be at least 1m, got %s", cfg.Monitor.MaxSampleAge)
	}

	cfg.ApprovalTTL = durationEnv("APPROVAL_TTL", defaultApprovalTTL, fail)
	if cfg.ApprovalTTL < time.Minute {
		// A request that expires faster than a human can read it is a denial with
		// extra steps.
		fail("APPROVAL_TTL must be at least 1m, got %s", cfg.ApprovalTTL)
	}

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

	cfg.ShutdownGrace = durationEnv("SHUTDOWN_GRACE", defaultShutdownGrace, fail)

	// Both key lists share one uniqueness check: a secret listed as both an
	// operator and a bot would leave the privilege of a request depending on which
	// list happened to be searched first.
	seenName, seenSecret := map[string]string{}, map[string]string{}
	operators := parseAPIKeys("GOAL_ENGINE_API_KEYS", os.Getenv("GOAL_ENGINE_API_KEYS"), RoleOperator, seenName, seenSecret, fail)
	bots := parseAPIKeys("GOAL_ENGINE_BOT_KEYS", os.Getenv("GOAL_ENGINE_BOT_KEYS"), RoleBot, seenName, seenSecret, fail)
	if len(operators) == 0 {
		fail("GOAL_ENGINE_API_KEYS is required: at least one operator API key must be configured")
	}
	// Bot keys are optional: a deployment can run goal-first for a while, with an
	// operator stating goals and the monitor watching them, before any agent is
	// given a credential of its own.
	cfg.APIKeys = append(operators, bots...)
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func stringEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return fallback
	}
	return v
}

func floatEnv(key string, fallback float64, fail func(string, ...any)) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fail("%s must be a number, got %q", key, raw)
		return fallback
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		fail("%s must be a finite number, got %q", key, raw)
		return fallback
	}
	return v
}

func boolEnv(key string, fallback bool, fail func(string, ...any)) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		fail("%s must be a boolean, got %q", key, raw)
		return fallback
	}
	return v
}

func durationEnv(key string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s must be a Go duration such as 1h or 30s, got %q", key, raw)
		return fallback
	}
	return v
}

// parseAPIKeys reads one comma-separated key list, where each entry is either
// "name:secret" or a bare secret.
//
// Everything before the first colon is the name; everything after it is the
// secret, so a secret must not contain a colon. An unnamed key is labelled by a
// short hash of itself, which still ties an audit entry to exactly one
// credential without recording the credential.
//
// seenName and seenSecret are shared across every list so that an operator key
// and a bot key can never be the same secret or answer to the same name. That
// matters more than ordinary uniqueness: the two lists carry different
// privileges, and a credential in both would make the privilege ambiguous.
func parseAPIKeys(envName, raw string, role Role, seenName, seenSecret map[string]string, fail func(string, ...any)) []APIKey {
	entries := splitAndTrim(raw)
	keys := make([]APIKey, 0, len(entries))

	for i, entry := range entries {
		where := fmt.Sprintf("%s[%d]", envName, i)
		name, secret := "", entry
		if idx := strings.Index(entry, ":"); idx >= 0 {
			name = strings.TrimSpace(entry[:idx])
			secret = strings.TrimSpace(entry[idx+1:])
		}

		// The secret is never echoed back, only ever described.
		if len(secret) < minAPIKeyLength {
			fail("%s has a key shorter than %d characters", where, minAPIKeyLength)
			continue
		}
		if name == "" {
			name = fingerprintName(secret)
		} else if len(name) > maxPrincipalNameLength {
			fail("%s has a name longer than %d characters", where, maxPrincipalNameLength)
			continue
		} else if strings.ContainsAny(name, " \t\n") {
			fail("%s has a name containing whitespace: %q", where, name)
			continue
		}

		// Two principals sharing a name, or one name behind two keys, makes every
		// entry the audit log holds ambiguous. The key is checked first: a repeated
		// key also collides on its fingerprint name, and "you listed the same key
		// twice" is the more useful thing to be told.
		if prev, dup := seenSecret[secret]; dup {
			fail("%s repeats the key from %s", where, prev)
			continue
		}
		if prev, dup := seenName[name]; dup {
			fail("%s reuses the name from %s: %q", where, prev, name)
			continue
		}

		seenName[name] = where
		seenSecret[secret] = where
		keys = append(keys, APIKey{Name: name, Secret: secret, Role: role})
	}
	return keys
}

// fingerprintName labels an unnamed key with eight hex characters of its
// SHA-256. That identifies which key acted without helping anyone reconstruct
// it.
func fingerprintName(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "key-" + hex.EncodeToString(sum[:4])
}

func splitAndTrim(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
