package config

import (
	"strings"
	"testing"
	"time"
)

// setValidEnv provides the minimum environment a healthy boot needs.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://wingman@localhost:5432/wingman_goals")
	t.Setenv("WINGMAN_CORE_BASE_URL", "https://core.wingman.test")
	t.Setenv("WINGMAN_CORE_API_KEY", "core-key-0123456789abcdefghijkl")
	t.Setenv("GOAL_ENGINE_API_KEYS", "engine-key-0123456789abcdefghijkl")
}

func TestLoadAppliesDefaults(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("expected config to load, got: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Fatalf("expected default port %d, got %d", defaultPort, cfg.Port)
	}
	if cfg.Monitor.Interval != defaultMonitorInterval {
		t.Fatalf("expected default interval %s, got %s", defaultMonitorInterval, cfg.Monitor.Interval)
	}
	if !cfg.Monitor.Enabled {
		t.Fatal("expected the monitor to be enabled by default")
	}
	if cfg.MetricsConfigPath != defaultMetricsConfigPath {
		t.Fatalf("unexpected metrics path %q", cfg.MetricsConfigPath)
	}
	if cfg.Location == nil {
		t.Fatal("expected a resolved timezone")
	}
	if len(cfg.APIKeys) != 1 {
		t.Fatalf("expected one api key, got %d", len(cfg.APIKeys))
	}
	if !strings.HasPrefix(cfg.APIKeys[0].Name, "key-") {
		t.Fatalf("expected an unnamed key to be fingerprinted, got %q", cfg.APIKeys[0].Name)
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("APP_ENV", "production")
	t.Setenv("MONITOR_INTERVAL", "15m")
	t.Setenv("MONITOR_ENABLED", "false")
	t.Setenv("MONITOR_MAX_GOALS_PER_TICK", "50")
	t.Setenv("TIMEZONE", "UTC")
	t.Setenv("GOAL_ENGINE_API_KEYS", "engine-key-0123456789abcdefghijkl, second-key-0123456789abcdefghijkl")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Port != 9090 {
		t.Fatalf("expected port 9090, got %d", cfg.Port)
	}
	if !cfg.IsProduction() {
		t.Fatal("expected production env")
	}
	if cfg.Monitor.Interval != 15*time.Minute {
		t.Fatalf("expected 15m interval, got %s", cfg.Monitor.Interval)
	}
	if cfg.Monitor.Enabled {
		t.Fatal("expected the monitor to be disabled")
	}
	if cfg.Monitor.MaxGoalsPerTick != 50 {
		t.Fatalf("expected 50 goals per tick, got %d", cfg.Monitor.MaxGoalsPerTick)
	}
	if cfg.Location.String() != "UTC" {
		t.Fatalf("expected UTC, got %s", cfg.Location)
	}
	// Counted, not printed: a failure message must not carry the keys.
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("expected two api keys, got %d", len(cfg.APIKeys))
	}
}

func TestLoadNamesAPIKeysForTheAuditLog(t *testing.T) {
	// "ops changed the target" is the entry that is worth keeping. That name has
	// to come from configuration, because nothing else in the request knows it.
	setValidEnv(t)
	t.Setenv("GOAL_ENGINE_API_KEYS", "ops:engine-key-0123456789abcdefghijkl, ci:second-key-0123456789abcdefghijkl")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("expected two api keys, got %d", len(cfg.APIKeys))
	}
	if cfg.APIKeys[0].Name != "ops" || cfg.APIKeys[1].Name != "ci" {
		t.Fatalf("unexpected names: %q, %q", cfg.APIKeys[0].Name, cfg.APIKeys[1].Name)
	}
	if cfg.APIKeys[0].Secret != "engine-key-0123456789abcdefghijkl" {
		t.Fatal("expected the secret to be everything after the first colon")
	}
}

func TestLoadRefusesAmbiguousAPIKeys(t *testing.T) {
	// Two principals behind one name, or one name behind two keys, makes every
	// entry in the audit log ambiguous. Better to fail at startup.
	cases := []struct {
		name  string
		value string
		wants string
	}{
		{
			name:  "same name twice",
			value: "ops:engine-key-0123456789abcdefghijkl, ops:second-key-0123456789abcdefghijkl",
			wants: "reuses the name",
		},
		{
			name:  "same key twice",
			value: "ops:engine-key-0123456789abcdefghijkl, ci:engine-key-0123456789abcdefghijkl",
			wants: "repeats the key",
		},
		{
			name:  "same key twice unnamed",
			value: "engine-key-0123456789abcdefghijkl, engine-key-0123456789abcdefghijkl",
			wants: "repeats the key",
		},
		{
			name:  "name with a space",
			value: "ops dwi:engine-key-0123456789abcdefghijkl",
			wants: "whitespace",
		},
		{
			name:  "name too long",
			value: strings.Repeat("n", maxPrincipalNameLength+1) + ":engine-key-0123456789abcdefghijkl",
			wants: "longer than",
		},
		{
			name:  "named but short key",
			value: "ops:short",
			wants: "shorter than",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("GOAL_ENGINE_API_KEYS", c.value)

			_, err := Load()
			if err == nil {
				t.Fatal("expected configuration to be rejected")
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("expected %q in %q", c.wants, err)
			}
			if strings.Contains(err.Error(), "engine-key-0123456789abcdefghijkl") {
				t.Fatal("the error message leaked a key")
			}
		})
	}
}

func TestLoadTrimsTrailingSlashFromCoreURL(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WINGMAN_CORE_BASE_URL", "https://core.wingman.test/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Core.BaseURL != "https://core.wingman.test" {
		t.Fatalf("expected the trailing slash to be trimmed, got %q", cfg.Core.BaseURL)
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	cases := map[string]struct {
		override map[string]string
		wants    string
	}{
		"no database url": {
			override: map[string]string{"DATABASE_URL": ""},
			wants:    "DATABASE_URL",
		},
		"no core url": {
			override: map[string]string{"WINGMAN_CORE_BASE_URL": ""},
			wants:    "WINGMAN_CORE_BASE_URL",
		},
		"no core key": {
			override: map[string]string{"WINGMAN_CORE_API_KEY": ""},
			wants:    "WINGMAN_CORE_API_KEY",
		},
		"no inbound keys": {
			override: map[string]string{"GOAL_ENGINE_API_KEYS": ""},
			wants:    "GOAL_ENGINE_API_KEYS",
		},
		"short inbound key": {
			override: map[string]string{"GOAL_ENGINE_API_KEYS": "short"},
			wants:    "shorter than",
		},
		"non http core url": {
			override: map[string]string{"WINGMAN_CORE_BASE_URL": "ftp://core.wingman.test"},
			wants:    "http",
		},
		"bad port": {
			override: map[string]string{"PORT": "not-a-number"},
			wants:    "PORT",
		},
		"port out of range": {
			override: map[string]string{"PORT": "70000"},
			wants:    "PORT",
		},
		"bad duration": {
			override: map[string]string{"MONITOR_INTERVAL": "every hour"},
			wants:    "MONITOR_INTERVAL",
		},
		"interval too short": {
			override: map[string]string{"MONITOR_INTERVAL": "10s"},
			wants:    "at least 1m",
		},
		"bad boolean": {
			override: map[string]string{"MONITOR_ENABLED": "maybe"},
			wants:    "MONITOR_ENABLED",
		},
		"zero goals per tick": {
			override: map[string]string{"MONITOR_MAX_GOALS_PER_TICK": "0"},
			wants:    "MONITOR_MAX_GOALS_PER_TICK",
		},
		"unknown timezone": {
			override: map[string]string{"TIMEZONE": "Mars/Olympus_Mons"},
			wants:    "TIMEZONE",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			for k, v := range tc.override {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("expected %s to fail validation", name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("expected error to mention %q, got: %v", tc.wants, err)
			}
		})
	}
}

func TestLoadAllowsMissingCoreKeyInDryRun(t *testing.T) {
	// Dry run is how an operator watches what the goal layer wants to do before
	// letting it touch the core, so it must not require core credentials.
	setValidEnv(t)
	t.Setenv("WINGMAN_CORE_API_KEY", "")
	t.Setenv("TRIGGER_DRY_RUN", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected dry run to load without a core key, got: %v", err)
	}
	if !cfg.Core.DryRun {
		t.Fatal("expected DryRun to be set")
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("GOAL_ENGINE_API_KEYS", "")
	t.Setenv("PORT", "abc")

	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"DATABASE_URL", "GOAL_ENGINE_API_KEYS", "PORT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %q, got: %v", want, err)
		}
	}
}

func TestLoadAppliesRateLimitDefaults(t *testing.T) {
	// An operator who never thinks about rate limiting still gets one.
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RateLimit.RPS != defaultRateLimitRPS {
		t.Fatalf("expected default rps %v, got %v", defaultRateLimitRPS, cfg.RateLimit.RPS)
	}
	if cfg.RateLimit.Burst != defaultRateLimitBurst {
		t.Fatalf("expected default burst %d, got %d", defaultRateLimitBurst, cfg.RateLimit.Burst)
	}
}

func TestLoadParsesAFractionalRateLimit(t *testing.T) {
	// Some endpoints are worth less than one request a second.
	setValidEnv(t)
	t.Setenv("RATE_LIMIT_RPS", "0.5")
	t.Setenv("RATE_LIMIT_BURST", "2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RateLimit.RPS != 0.5 {
		t.Fatalf("expected 0.5 rps, got %v", cfg.RateLimit.RPS)
	}
	if cfg.RateLimit.Burst != 2 {
		t.Fatalf("expected burst 2, got %d", cfg.RateLimit.Burst)
	}
}

func TestLoadRejectsARateLimitThatWouldLetNothingThrough(t *testing.T) {
	// A limiter configured to allow nothing is a self-inflicted outage, and one
	// configured with a nonsense number is a typo nobody would notice at runtime.
	cases := []struct {
		name  string
		rps   string
		burst string
		wants string
	}{
		{"zero rps", "0", "", "RATE_LIMIT_RPS"},
		{"negative rps", "-1", "", "RATE_LIMIT_RPS"},
		{"not a number", "fast", "", "RATE_LIMIT_RPS"},
		{"not a number infinity", "Inf", "", "finite"},
		{"not a number nan", "NaN", "", "finite"},
		{"zero burst", "", "0", "RATE_LIMIT_BURST"},
		{"negative burst", "", "-3", "RATE_LIMIT_BURST"},
		{"burst is not an integer", "", "3.5", "RATE_LIMIT_BURST"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("RATE_LIMIT_RPS", c.rps)
			t.Setenv("RATE_LIMIT_BURST", c.burst)

			_, err := Load()
			if err == nil {
				t.Fatal("expected the configuration rejected")
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("expected the error to mention %q, got: %v", c.wants, err)
			}
		})
	}
}

func TestLoadAppliesGovernanceAndDeploymentDefaults(t *testing.T) {
	// These four decide how long an agent's spend request stays actionable, how
	// stale a reported metric may be, whether the schema is migrated on boot, and
	// which proxy hop may name the client. All four have a working default, and all
	// four have to be visible to the operator who wants a different one.
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ApprovalTTL != defaultApprovalTTL {
		t.Fatalf("expected the default approval ttl %s, got %s", defaultApprovalTTL, cfg.ApprovalTTL)
	}
	if cfg.Monitor.MaxSampleAge != defaultMaxSampleAge {
		t.Fatalf("expected the default sample age %s, got %s", defaultMaxSampleAge, cfg.Monitor.MaxSampleAge)
	}
	if cfg.MigrationsDir != defaultMigrationsDir {
		t.Fatalf("expected the default migrations dir %q, got %q", defaultMigrationsDir, cfg.MigrationsDir)
	}
	if !cfg.AutoMigrate {
		t.Fatal("expected migrations to be applied on boot by default: self-hosting is the deployment model")
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("expected no trusted proxies by default, got %v", cfg.TrustedProxies)
	}
	if cfg.Core.TaskPath != "" || cfg.Core.DefaultBotID != "" || cfg.Core.DefaultChannelID != "" {
		t.Fatalf("expected the core defaults to be unset, got %+v", cfg.Core)
	}
}

func TestLoadParsesGovernanceAndDeploymentOverrides(t *testing.T) {
	setValidEnv(t)
	t.Setenv("APPROVAL_TTL", "4h")
	t.Setenv("METRIC_MAX_SAMPLE_AGE", "90m")
	t.Setenv("MIGRATIONS_DIR", "/srv/wingman/migrations")
	t.Setenv("AUTO_MIGRATE", "false")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.7, 10.0.0.8")
	t.Setenv("WINGMAN_CORE_TASK_PATH", "api/v2/tasks")
	t.Setenv("WINGMAN_DEFAULT_BOT_ID", "bot-ops")
	t.Setenv("WINGMAN_DEFAULT_CHANNEL_ID", "channel-ops")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ApprovalTTL != 4*time.Hour {
		t.Fatalf("expected a 4h approval ttl, got %s", cfg.ApprovalTTL)
	}
	if cfg.Monitor.MaxSampleAge != 90*time.Minute {
		t.Fatalf("expected a 90m sample age, got %s", cfg.Monitor.MaxSampleAge)
	}
	if cfg.MigrationsDir != "/srv/wingman/migrations" {
		t.Fatalf("unexpected migrations dir %q", cfg.MigrationsDir)
	}
	if cfg.AutoMigrate {
		t.Fatal("expected migrations to be left to the operator")
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0] != "10.0.0.7" || cfg.TrustedProxies[1] != "10.0.0.8" {
		t.Fatalf("expected both proxies, trimmed, got %v", cfg.TrustedProxies)
	}
	if cfg.Core.TaskPath != "api/v2/tasks" {
		t.Fatalf("unexpected task path %q", cfg.Core.TaskPath)
	}
	if cfg.Core.DefaultBotID != "bot-ops" || cfg.Core.DefaultChannelID != "channel-ops" {
		t.Fatalf("unexpected core defaults %+v", cfg.Core)
	}
}

func TestLoadRejectsTimeoutsShortEnoughToBreakTheirOwnPurpose(t *testing.T) {
	// An approval that expires before a human can read it, or a staleness limit
	// shorter than the gap between two feeds, would each turn a guardrail into an
	// outage.
	cases := []struct {
		name  string
		key   string
		value string
		wants string
	}{
		{"approval ttl too short", "APPROVAL_TTL", "30s", "APPROVAL_TTL"},
		{"approval ttl not a duration", "APPROVAL_TTL", "soon", "APPROVAL_TTL"},
		{"sample age too short", "METRIC_MAX_SAMPLE_AGE", "10s", "METRIC_MAX_SAMPLE_AGE"},
		{"sample age not a duration", "METRIC_MAX_SAMPLE_AGE", "a while", "METRIC_MAX_SAMPLE_AGE"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(c.key, c.value)

			_, err := Load()
			if err == nil {
				t.Fatal("expected the configuration rejected")
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("expected the error to mention %q, got: %v", c.wants, err)
			}
		})
	}
}
