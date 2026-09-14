package config

import (
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/domain"
)

const (
	operatorKey = "core-operator-key-0123456789abc"
	botKey      = "core-bot-key-0123456789abcdefgh"
)

// setValidEnv provides the minimum environment a healthy boot needs, and clears
// every optional variable so a default assertion tests the code rather than the
// shell the suite happens to run in.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://wingman@localhost:5433/wingman_core")
	t.Setenv("CORE_API_KEYS", operatorKey)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-0123456789")
	t.Setenv("DEFAULT_MODEL", "claude-opus-5")

	for _, key := range []string{
		"APP_ENV", "PORT", "LOG_LEVEL", "TIMEZONE", "SHUTDOWN_GRACE",
		"MIGRATIONS_DIR", "AUTO_MIGRATE", "TRUSTED_PROXIES",
		"CORE_BOT_KEYS", "CORE_OPEN_REGISTRATION", "SESSION_TTL",
		"CORE_UNATTENDED_OWNER",
		"OPENAI_API_KEY", "DEFAULT_PROVIDER",
		"SANDBOX_BACKEND", "SANDBOX_WORKSPACE_ROOT", "SANDBOX_IMAGE",
		"SANDBOX_NETWORK", "SANDBOX_MEMORY",
		"TOOLS_CONFIG_PATH", "MCP_CONFIG_PATH", "HTTP_TOOLS_CONFIG_PATH",
		"RUN_WORKERS", "RUN_QUEUE_POLL",
		"GOAL_ENGINE_BASE_URL", "GOAL_ENGINE_API_KEY", "GOAL_ENGINE_TIMEOUT",
		"GOAL_ENGINE_REPORT_SPEND", "GOAL_ENGINE_SPEND_METRIC",
		"CHANNEL_TELEGRAM_TOKEN", "CHANNEL_SLACK_BOT_TOKEN", "CHANNEL_SLACK_APP_TOKEN",
		"CHANNEL_DISCORD_TOKEN", "CHANNEL_LINK_CODE_TTL", "CHANNEL_MIN_INTERVAL",
		"CHANNEL_ALLOW_GROUPS",
		"RUN_MAX_ITERATIONS", "RUN_MAX_TOOL_CALLS", "RUN_MAX_TOKENS",
		"USER_DAILY_TOKEN_CAP", "RUN_STEP_TIMEOUT", "SANDBOX_TIMEOUT",
		"RATE_LIMIT_RPS", "RATE_LIMIT_BURST",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_appliesDefaults(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("expected config to load, got: %v", err)
	}
	// 8081, not 8080: the goal engine has 8080 and the two run side by side.
	if cfg.Port != defaultPort {
		t.Errorf("port = %d; want %d", cfg.Port, defaultPort)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("log level = %q; want %q — debug logs request bodies, and a body here holds a user's brief", cfg.LogLevel, defaultLogLevel)
	}
	if cfg.Location == nil {
		t.Fatal("expected a resolved timezone")
	}
	if cfg.MigrationsDir != defaultMigrationsDir || !cfg.AutoMigrate {
		t.Errorf("migrations = %q, auto = %v; want the schema applied on boot: self-hosting is the deployment model", cfg.MigrationsDir, cfg.AutoMigrate)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("trusted proxies = %v; want none, so the client IP is the socket's peer", cfg.TrustedProxies)
	}
	if cfg.Tools.GrantsPath != defaultToolsConfigPath || cfg.Tools.MCPPath != defaultMCPConfigPath ||
		cfg.Tools.HTTPPath != defaultHTTPConfigPath {
		t.Errorf("tool config paths = %+v; want the defaults", cfg.Tools)
	}
	if cfg.RateLimit.RPS != defaultRateLimitRPS || cfg.RateLimit.Burst != defaultRateLimitBurst {
		t.Errorf("rate limit = %+v; want an operator who never thinks about it to still get one", cfg.RateLimit)
	}
	if cfg.ShutdownGrace != defaultShutdownGrace {
		t.Errorf("shutdown grace = %s; want %s", cfg.ShutdownGrace, defaultShutdownGrace)
	}
}

// Registration closed by default is the safety property, not a preference: a
// self-hosted box found on the internet with open sign-up is a box running
// strangers' code in your sandbox on your API key.
func TestLoad_registrationIsClosedUnlessAnOperatorOpensIt(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Auth.OpenRegistration {
		t.Error("registration was open by default")
	}
	if cfg.Auth.SessionTTL != defaultSessionTTL {
		t.Errorf("session ttl = %s; want %s", cfg.Auth.SessionTTL, defaultSessionTTL)
	}

	// Act again, with it turned on deliberately.
	t.Setenv("CORE_OPEN_REGISTRATION", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Auth.OpenRegistration {
		t.Error("an operator who asked for open registration did not get it")
	}
}

func TestLoad_rejectsASessionTTLOutsideItsBounds(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"far too short":   "1m",
		"far too long":    "365d",
		"not a duration":  "a week",
		"negative":        "-1h",
		"only just short": (minSessionTTL - time.Second).String(),
	}

	// Act & Assert
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("SESSION_TTL", value)

			if _, err := Load(); err == nil {
				t.Fatalf("SESSION_TTL=%q was accepted", value)
			} else if !strings.Contains(err.Error(), "SESSION_TTL") {
				t.Errorf("error = %v; want it to name SESSION_TTL", err)
			}
		})
	}
}

// The sandbox defaults to no network. A sandbox that can reach the internet is a
// different threat model from one that cannot, so it is something an operator turns
// on rather than something they inherit.
func TestLoad_sandboxDefaultsToLocalWithNoNetwork(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sandbox.Backend != SandboxLocal {
		t.Errorf("backend = %q; want %q", cfg.Sandbox.Backend, SandboxLocal)
	}
	if cfg.Sandbox.Network != "none" {
		t.Errorf("network = %q; want %q", cfg.Sandbox.Network, "none")
	}
	if cfg.Sandbox.WorkspaceRoot != defaultSandboxRoot {
		t.Errorf("workspace root = %q; want %q", cfg.Sandbox.WorkspaceRoot, defaultSandboxRoot)
	}
	if cfg.Sandbox.Memory != defaultSandboxMemory {
		t.Errorf("memory limit = %q; want a bound so a runaway process kills its container, not the host", cfg.Sandbox.Memory)
	}

	// Act again, with the variable present but blank — an operator who commented a
	// value out. Blank means unset, not "no workspace root".
	t.Setenv("SANDBOX_WORKSPACE_ROOT", "   ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sandbox.WorkspaceRoot != defaultSandboxRoot {
		t.Errorf("workspace root = %q; want the default back", cfg.Sandbox.WorkspaceRoot)
	}
}

func TestLoad_rejectsAnUnknownSandboxBackend(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("SANDBOX_BACKEND", "e2b")

	// Act
	_, err := Load()

	// Assert
	// Naming a backend that does not exist yet must fail at boot, not at the first
	// tool call inside somebody's run.
	if err == nil {
		t.Fatal("an unimplemented backend was accepted")
	}
	if !strings.Contains(err.Error(), "SANDBOX_BACKEND") {
		t.Errorf("error = %v; want it to name SANDBOX_BACKEND", err)
	}
}

func TestLoad_acceptsTheDockerBackend(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("SANDBOX_BACKEND", "DOCKER")
	t.Setenv("SANDBOX_IMAGE", "wingman/sandbox:pinned")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Case-folded, because an operator typing a backend name is not writing code.
	if cfg.Sandbox.Backend != SandboxDocker {
		t.Errorf("backend = %q; want %q", cfg.Sandbox.Backend, SandboxDocker)
	}
	if cfg.Sandbox.Image != "wingman/sandbox:pinned" {
		t.Errorf("image = %q; want the operator's own", cfg.Sandbox.Image)
	}
}

func TestLoad_requiresTheKeyForWhicheverProviderIsDefault(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		provider  string
		anthropic string
		openai    string
		wants     string
	}{
		"anthropic default without its key": {"anthropic", "", "sk-openai-test", "ANTHROPIC_API_KEY"},
		"openai default without its key":    {"openai", "sk-ant-test", "", "OPENAI_API_KEY"},
		"a provider that does not exist":    {"gemini", "sk-ant-test", "", "DEFAULT_PROVIDER"},
	}

	// Act & Assert
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("DEFAULT_PROVIDER", tc.provider)
			t.Setenv("ANTHROPIC_API_KEY", tc.anthropic)
			t.Setenv("OPENAI_API_KEY", tc.openai)

			_, err := Load()
			if err == nil {
				t.Fatal("expected the configuration rejected")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v; want it to mention %q", err, tc.wants)
			}
		})
	}
}

// No model name is compiled in as a fallback. Vendors retire model ids on their own
// schedule, and a stale constant in a release fails at the first model call with a
// message about the vendor rather than about this config.
func TestLoad_requiresADefaultModelWithNoCompiledInFallback(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("DEFAULT_MODEL", "")

	// Act
	_, err := Load()

	// Assert
	if err == nil {
		t.Fatal("a boot with no model configured was accepted")
	}
	if !strings.Contains(err.Error(), "DEFAULT_MODEL") {
		t.Errorf("error = %v; want it to name DEFAULT_MODEL", err)
	}
}

func TestLoad_acceptsBothProvidersAtOnce(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("DEFAULT_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("DEFAULT_MODEL", "gpt-5")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Both keys configured is the expected state: the default picks which one a run
	// uses when it names none, it does not switch the other one off.
	if cfg.Providers.Default != ProviderOpenAI {
		t.Errorf("default provider = %q; want %q", cfg.Providers.Default, ProviderOpenAI)
	}
	if cfg.Providers.AnthropicAPIKey == "" || cfg.Providers.OpenAIAPIKey == "" {
		t.Error("one of the two provider keys was dropped")
	}
}

// An operator key is required; a bot key is not. Core is useful with human users
// alone, before a goal engine is ever pointed at it.
func TestLoad_requiresAnOperatorKeyButNotABotKey(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Role != RoleOperator {
		t.Fatalf("expected one operator key, got %+v roles", rolesOf(cfg.APIKeys))
	}
	if !strings.HasPrefix(cfg.APIKeys[0].Name, "key-") {
		t.Errorf("unnamed key was labelled %q; want a fingerprint so the audit log can say which key acted", cfg.APIKeys[0].Name)
	}

	// Act again, with no operator key at all.
	t.Setenv("CORE_API_KEYS", "")
	if _, err := Load(); err == nil {
		t.Error("a boot with no operator key was accepted")
	} else if !strings.Contains(err.Error(), "CORE_API_KEYS") {
		t.Errorf("error = %v; want it to name CORE_API_KEYS", err)
	}
}

// A bot key is a credential that files work nobody asked for. Somebody has to own that
// work: it is billed to an account's daily cap and shows up in that account's ledger.
// There is deliberately no default owner — an implicit one would put a stranger's
// unattended spending on whichever account happened to be first.
func TestLoad_requiresAnUnattendedOwnerAsSoonAsABotCanDispatch(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("CORE_BOT_KEYS", "goal-engine:"+botKey)

	// Act
	_, err := Load()

	// Assert
	if err == nil {
		t.Fatal("a bot key with no unattended owner was accepted")
	}
	if !strings.Contains(err.Error(), "CORE_UNATTENDED_OWNER") {
		t.Errorf("error = %v; want it to name CORE_UNATTENDED_OWNER", err)
	}

	// Act again, with the owner set — and in the case and spacing an operator might
	// actually type it, because the address is looked up against stored, lower-cased
	// email and a mismatch here would fail the boot for a reason nobody could see.
	t.Setenv("CORE_UNATTENDED_OWNER", "  Unattended@Wingman.Test  ")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.UnattendedOwner != "unattended@wingman.test" {
		t.Errorf("unattended owner = %q, want it trimmed and lower-cased", cfg.UnattendedOwner)
	}
}

// With no bot key there is nothing that can dispatch unattended, so the owner is not
// required. Core is useful with human users alone.
func TestLoad_needsNoUnattendedOwnerWithoutABotKey(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.UnattendedOwner != "" {
		t.Errorf("unattended owner = %q, want empty", cfg.UnattendedOwner)
	}
}

// Privilege is a property of which variable listed the key, fixed before the first
// request arrives. There is no request field that raises a role, which is what makes
// there be nothing to forge.
func TestLoad_fixesRoleByWhichVariableListedTheKey(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("CORE_API_KEYS", "ops:"+operatorKey)
	t.Setenv("CORE_BOT_KEYS", "goal-engine:"+botKey)
	// A bot key is what makes an unattended owner required; see
	// TestLoad_requiresAnUnattendedOwnerAsSoonAsABotCanDispatch.
	t.Setenv("CORE_UNATTENDED_OWNER", "unattended@wingman.test")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("expected two keys, got %d", len(cfg.APIKeys))
	}
	byName := map[string]Role{}
	for _, k := range cfg.APIKeys {
		byName[k.Name] = k.Role
	}
	if byName["ops"] != RoleOperator {
		t.Errorf("ops has role %q; want %q", byName["ops"], RoleOperator)
	}
	if byName["goal-engine"] != RoleBot {
		t.Errorf("goal-engine has role %q; want %q", byName["goal-engine"], RoleBot)
	}
	// RoleUser arrives only from a session token in the database. An environment key
	// must never be able to become one.
	for _, k := range cfg.APIKeys {
		if k.Role == RoleUser {
			t.Errorf("key %q was loaded as a user; environment keys are never users", k.Name)
		}
	}
}

func TestLoad_refusesAmbiguousKeys(t *testing.T) {
	// Arrange
	// Two principals behind one name, or one name behind two keys, makes every entry
	// in the audit log ambiguous. A secret in both lists is worse still: the
	// request's privilege would depend on which list happened to be searched first.
	cases := map[string]struct {
		operators string
		bots      string
		wants     string
	}{
		"same name twice in one list": {"ops:" + operatorKey + ", ops:" + botKey, "", "reuses the name"},
		"same key twice in one list":  {"ops:" + operatorKey + ", ci:" + operatorKey, "", "repeats the key"},
		"same key in both lists":      {"ops:" + operatorKey, "bot:" + operatorKey, "repeats the key"},
		"same name in both lists":     {"shared:" + operatorKey, "shared:" + botKey, "reuses the name"},
		"name with a space":           {"ops dwi:" + operatorKey, "", "whitespace"},
		"name too long":               {strings.Repeat("n", maxPrincipalNameLength+1) + ":" + operatorKey, "", "longer than"},
		"key too short":               {"ops:short", "", "shorter than"},
		"bot key too short":           {"ops:" + operatorKey, "bot:short", "shorter than"},
	}

	// Act & Assert
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("CORE_API_KEYS", tc.operators)
			t.Setenv("CORE_BOT_KEYS", tc.bots)

			_, err := Load()
			if err == nil {
				t.Fatal("expected the configuration rejected")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v; want it to mention %q", err, tc.wants)
			}
			// The message describes the key, never echoes it, or the boot log
			// becomes a place credentials live.
			if strings.Contains(err.Error(), operatorKey) || strings.Contains(err.Error(), botKey) {
				t.Error("the error message leaked a key")
			}
		})
	}
}

// An absent goal engine and an unreachable one are different states, and confusing
// them is expensive in both directions. Nothing configured means both features are
// off by choice; configured and unanswerable is a fault.
func TestLoad_withNoGoalEngineLeavesBothFeaturesOff(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GoalEngine.Configured() {
		t.Error("a goal engine was reported as configured with no base URL set")
	}
	if cfg.GoalEngine.ReportSpend {
		t.Error("spend reporting was on with nowhere to report it to")
	}
}

func TestLoad_withAGoalEngineTurnsSpendReportingOnByDefault(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("GOAL_ENGINE_BASE_URL", "https://goals.wingman.test/")
	t.Setenv("GOAL_ENGINE_API_KEY", "engine-bot-key-0123456789abcdef")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.GoalEngine.Configured() {
		t.Fatal("expected the goal engine to be configured")
	}
	// Trailing slash trimmed, or every request path ends up with a double slash.
	if cfg.GoalEngine.BaseURL != "https://goals.wingman.test" {
		t.Errorf("base URL = %q; want the trailing slash trimmed", cfg.GoalEngine.BaseURL)
	}
	if !cfg.GoalEngine.ReportSpend {
		t.Error("spend reporting was off; wiring a goal engine up is asking for cost to be a metric")
	}
	if cfg.GoalEngine.SpendMetric != defaultSpendMetric {
		t.Errorf("spend metric = %q; want %q", cfg.GoalEngine.SpendMetric, defaultSpendMetric)
	}
	if cfg.GoalEngine.Timeout != defaultGoalEngineTimeout {
		t.Errorf("timeout = %s; want %s", cfg.GoalEngine.Timeout, defaultGoalEngineTimeout)
	}
}

// A configured goal engine core cannot authenticate to is the worst of the three
// states: the kill switch reads as unreadable, which means engaged, which means
// every run halts. Refusing to boot says why once, instead of leaving that to be
// diagnosed from halted runs.
func TestLoad_refusesAGoalEngineItCannotAuthenticateTo(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("GOAL_ENGINE_BASE_URL", "https://goals.wingman.test")
	t.Setenv("GOAL_ENGINE_API_KEY", "")

	// Act
	_, err := Load()

	// Assert
	if err == nil {
		t.Fatal("a goal engine with no credential was accepted")
	}
	if !strings.Contains(err.Error(), "GOAL_ENGINE_API_KEY") {
		t.Errorf("error = %v; want it to name GOAL_ENGINE_API_KEY", err)
	}
}

func TestLoad_rejectsAnIncoherentGoalEngineConfiguration(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		env   map[string]string
		wants string
	}{
		"reporting spend with nowhere to send it": {
			env:   map[string]string{"GOAL_ENGINE_REPORT_SPEND": "true"},
			wants: "GOAL_ENGINE_BASE_URL",
		},
		"a base URL that is not http": {
			env: map[string]string{
				"GOAL_ENGINE_BASE_URL": "goals.wingman.test",
				"GOAL_ENGINE_API_KEY":  "engine-bot-key-0123456789abcdef",
			},
			wants: "http",
		},
		"a timeout of zero": {
			env: map[string]string{
				"GOAL_ENGINE_BASE_URL": "https://goals.wingman.test",
				"GOAL_ENGINE_API_KEY":  "engine-bot-key-0123456789abcdef",
				"GOAL_ENGINE_TIMEOUT":  "0s",
			},
			wants: "GOAL_ENGINE_TIMEOUT",
		},
		"a report-spend flag that is not a boolean": {
			env:   map[string]string{"GOAL_ENGINE_REPORT_SPEND": "sometimes"},
			wants: "GOAL_ENGINE_REPORT_SPEND",
		},
	}

	// Act & Assert
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatal("expected the configuration rejected")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v; want it to mention %q", err, tc.wants)
			}
		})
	}
}

// Spend reporting can be declined while the kill switch stays wired up. They are
// separate decisions and the config must let them be made separately.
func TestLoad_allowsAGoalEngineWithSpendReportingDeclined(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("GOAL_ENGINE_BASE_URL", "http://127.0.0.1:8080")
	t.Setenv("GOAL_ENGINE_API_KEY", "engine-bot-key-0123456789abcdef")
	t.Setenv("GOAL_ENGINE_REPORT_SPEND", "false")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.GoalEngine.Configured() {
		t.Error("the kill switch lost its base URL along with spend reporting")
	}
	if cfg.GoalEngine.ReportSpend {
		t.Error("spend reporting stayed on after being turned off")
	}
}

func TestLoad_appliesRunLimitDefaults(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := domain.RunLimits{}.WithDefaults()
	if cfg.Limits != want {
		t.Errorf("limits = %+v; want %+v", cfg.Limits, want)
	}
	// Zero reads as "no limit" to nobody here: Decide treats a zero as a stop, and
	// this is the only place a zero becomes a working number.
	if cfg.Limits.MaxIterations == 0 || cfg.Limits.MaxToolCalls == 0 || cfg.Limits.MaxTokensPerRun == 0 {
		t.Errorf("a limit came through as zero: %+v", cfg.Limits)
	}
}

func TestLoad_parsesRunLimitOverrides(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("RUN_MAX_ITERATIONS", "40")
	t.Setenv("RUN_MAX_TOOL_CALLS", "100")
	t.Setenv("RUN_MAX_TOKENS", "500000")
	t.Setenv("USER_DAILY_TOKEN_CAP", "5000000")
	t.Setenv("RUN_STEP_TIMEOUT", "5m")
	t.Setenv("SANDBOX_TIMEOUT", "2m")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := domain.RunLimits{
		MaxIterations:       40,
		MaxToolCalls:        100,
		MaxTokensPerRun:     500_000,
		MaxTokensPerUserDay: 5_000_000,
		StepTimeout:         5 * time.Minute,
		SandboxTimeout:      2 * time.Minute,
	}
	if cfg.Limits != want {
		t.Errorf("limits = %+v; want %+v", cfg.Limits, want)
	}
}

// Removing the daily cap has to be written out as the sentinel. Any other negative
// number is a typo, and reading a typo as "unlimited" is the most expensive possible
// interpretation of one.
func TestLoad_removingTheUserDailyCapMustBeWrittenOut(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("USER_DAILY_TOKEN_CAP", "-1")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Limits.MaxTokensPerUserDay != domain.NoUserDailyCap {
		t.Errorf("user daily cap = %d; want the sentinel %d", cfg.Limits.MaxTokensPerUserDay, domain.NoUserDailyCap)
	}

	// Act again, with a different negative number.
	t.Setenv("USER_DAILY_TOKEN_CAP", "-500")
	if _, err := Load(); err == nil {
		t.Error("a negative cap that is not the sentinel was accepted as unlimited")
	} else if !strings.Contains(err.Error(), "USER_DAILY_TOKEN_CAP") {
		t.Errorf("error = %v; want it to name USER_DAILY_TOKEN_CAP", err)
	}
}

// The domain reports its own field names; the error an operator reads has to name
// the variable they can actually edit.
func TestLoad_reportsABadLimitUnderItsEnvironmentVariableName(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"RUN_MAX_ITERATIONS":   "9000",
		"RUN_MAX_TOOL_CALLS":   "-1",
		"RUN_MAX_TOKENS":       "10",
		"USER_DAILY_TOKEN_CAP": "10",
		"RUN_STEP_TIMEOUT":     "1s",
		"SANDBOX_TIMEOUT":      "10h",
	}

	// Act & Assert
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(key, value)

			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%q was accepted", key, value)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %v; want it to name %s rather than a Go field", err, key)
			}
		})
	}
}

func TestLoad_boundsTheRunnerPool(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Runner.Workers != defaultRunWorkers || cfg.Runner.QueuePoll != defaultQueuePoll {
		t.Errorf("runner = %+v; want %d workers polling every %s", cfg.Runner, defaultRunWorkers, defaultQueuePoll)
	}

	// Arrange & Act & Assert — each worker is a concurrent model conversation with a
	// container behind it, so the ceiling is a real one.
	cases := map[string]struct{ workers, poll string }{
		"no workers":                           {"0", ""},
		"negative workers":                     {"-2", ""},
		"more workers than one host can serve": {"1000", ""},
		"workers not an integer":               {"a few", ""},
		"polling faster than once a second":    {"", "100ms"},
		"poll not a duration":                  {"", "often"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("RUN_WORKERS", tc.workers)
			t.Setenv("RUN_QUEUE_POLL", tc.poll)

			if _, err := Load(); err == nil {
				t.Fatalf("workers=%q poll=%q was accepted", tc.workers, tc.poll)
			}
		})
	}
}

func TestLoad_rejectsMissingOrMalformedRequiredValues(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		env   map[string]string
		wants string
	}{
		"no database url":     {map[string]string{"DATABASE_URL": ""}, "DATABASE_URL"},
		"bad port":            {map[string]string{"PORT": "not-a-number"}, "PORT"},
		"port out of range":   {map[string]string{"PORT": "70000"}, "PORT"},
		"port zero":           {map[string]string{"PORT": "0"}, "PORT"},
		"unknown timezone":    {map[string]string{"TIMEZONE": "Mars/Olympus_Mons"}, "TIMEZONE"},
		"bad automigrate":     {map[string]string{"AUTO_MIGRATE": "maybe"}, "AUTO_MIGRATE"},
		"bad shutdown grace":  {map[string]string{"SHUTDOWN_GRACE": "a moment"}, "SHUTDOWN_GRACE"},
		"zero rate limit":     {map[string]string{"RATE_LIMIT_RPS": "0"}, "RATE_LIMIT_RPS"},
		"rate limit in words": {map[string]string{"RATE_LIMIT_RPS": "fast"}, "RATE_LIMIT_RPS"},
		"infinite rate limit": {map[string]string{"RATE_LIMIT_RPS": "Inf"}, "finite"},
		"nan rate limit":      {map[string]string{"RATE_LIMIT_RPS": "NaN"}, "finite"},
		"zero burst":          {map[string]string{"RATE_LIMIT_BURST": "0"}, "RATE_LIMIT_BURST"},
		"fractional burst":    {map[string]string{"RATE_LIMIT_BURST": "3.5"}, "RATE_LIMIT_BURST"},
		"token cap in words":  {map[string]string{"RUN_MAX_TOKENS": "lots"}, "RUN_MAX_TOKENS"},
	}

	// Act & Assert
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if err == nil {
				t.Fatalf("expected %s to fail validation", name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v; want it to mention %q", err, tc.wants)
			}
		})
	}
}

// A service an operator configures by hand over SSH should not need one round of
// "fix it and try again" per field.
func TestLoad_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CORE_API_KEYS", "")
	t.Setenv("PORT", "abc")
	t.Setenv("DEFAULT_MODEL", "")
	t.Setenv("RUN_MAX_ITERATIONS", "9000")

	// Act
	_, err := Load()

	// Assert
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"DATABASE_URL", "CORE_API_KEYS", "PORT", "DEFAULT_MODEL", "RUN_MAX_ITERATIONS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error did not mention %q: %v", want, err)
		}
	}
}

func TestLoad_parsesDeploymentOverrides(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("PORT", "9091")
	t.Setenv("TIMEZONE", "UTC")
	t.Setenv("AUTO_MIGRATE", "false")
	t.Setenv("MIGRATIONS_DIR", "/srv/wingman/core/migrations")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.7, 10.0.0.8")
	t.Setenv("SHUTDOWN_GRACE", "45s")
	t.Setenv("TOOLS_CONFIG_PATH", "/etc/wingman/tools.yaml")
	t.Setenv("MCP_CONFIG_PATH", "/etc/wingman/mcp.yaml")
	t.Setenv("HTTP_TOOLS_CONFIG_PATH", "/etc/wingman/http-tools.yaml")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("expected production env")
	}
	if cfg.Port != 9091 || cfg.Location.String() != "UTC" {
		t.Errorf("port = %d, zone = %s; want 9091 and UTC", cfg.Port, cfg.Location)
	}
	if cfg.AutoMigrate {
		t.Error("expected migrations to be left to the operator")
	}
	if cfg.MigrationsDir != "/srv/wingman/core/migrations" {
		t.Errorf("migrations dir = %q", cfg.MigrationsDir)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0] != "10.0.0.7" || cfg.TrustedProxies[1] != "10.0.0.8" {
		t.Errorf("trusted proxies = %v; want both, trimmed", cfg.TrustedProxies)
	}
	if cfg.ShutdownGrace != 45*time.Second {
		t.Errorf("shutdown grace = %s", cfg.ShutdownGrace)
	}
	if cfg.Tools.GrantsPath != "/etc/wingman/tools.yaml" || cfg.Tools.MCPPath != "/etc/wingman/mcp.yaml" ||
		cfg.Tools.HTTPPath != "/etc/wingman/http-tools.yaml" {
		t.Errorf("tool paths = %+v", cfg.Tools)
	}
}

func TestFingerprintName_identifiesAKeyWithoutRevealingIt(t *testing.T) {
	// Act
	name := fingerprintName(operatorKey)

	// Assert
	if !strings.HasPrefix(name, "key-") {
		t.Errorf("name = %q; want a key- prefix", name)
	}
	if strings.Contains(name, operatorKey) || len(name) != len("key-")+8 {
		t.Errorf("name = %q; want eight hex characters of the hash and nothing of the key", name)
	}
	if fingerprintName(operatorKey) != name {
		t.Error("the same key fingerprinted two ways; an audit trail needs it stable")
	}
	if fingerprintName(botKey) == name {
		t.Error("two different keys share a fingerprint")
	}
}

func rolesOf(keys []APIKey) []Role {
	roles := make([]Role, 0, len(keys))
	for _, k := range keys {
		roles = append(roles, k.Role)
	}
	return roles
}
