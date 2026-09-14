package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/channel"
	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/sandbox"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// testLogger discards everything. What is asserted here is behaviour, and a test that
// printed the service's log lines would bury it.
func testLogger() zerolog.Logger { return zerolog.Nop() }

// Most of this package is wiring, and wiring is proved by the build. What is tested here
// is the handful of decisions that are not wiring: how long a run may be in flight before
// the sweep calls its worker dead, what a spending request tells whoever has to approve
// it, and how a password reaches the account it creates.

func TestStaleAfter_isLongerThanTheLongestRunTheLimitsAllow(t *testing.T) {
	// Arrange: the shipped defaults — fifteen iterations, three minutes of model time and
	// ninety seconds of sandbox time each.
	limits := domain.RunLimits{}.WithDefaults()
	longestLegitimateRun := time.Duration(limits.MaxIterations) * (limits.StepTimeout + limits.SandboxTimeout)

	// Act
	got := staleAfter(limits)

	// Assert
	if got <= longestLegitimateRun {
		t.Errorf("staleAfter = %s, want more than the %s a run may legitimately take: "+
			"the sweep would requeue work that was still being done", got, longestLegitimateRun)
	}
	if want := 135 * time.Minute; got != want {
		t.Errorf("staleAfter = %s, want %s", got, want)
	}
}

func TestStaleAfter_zeroLimitsUseTheDefaults_notZero(t *testing.T) {
	// Arrange + Act: limits nobody configured. Zero would make every run in flight look
	// abandoned to the first sweep.
	got := staleAfter(domain.RunLimits{})

	// Assert
	if want := staleAfter(domain.RunLimits{}.WithDefaults()); got != want {
		t.Errorf("staleAfter of unset limits = %s, want the defaulted %s", got, want)
	}
}

func TestStaleAfter_isNotCapped_soALongRunIsLeftAlone(t *testing.T) {
	// Arrange: an operator who allows the maximum has runs that legitimately take days.
	limits := domain.RunLimits{
		MaxIterations:  200,
		StepTimeout:    15 * time.Minute,
		SandboxTimeout: 30 * time.Minute,
	}.WithDefaults()

	// Act
	got := staleAfter(limits)

	// Assert: a fixed ceiling here would have the sweep close live runs and pay for their
	// work twice.
	if got < 24*time.Hour {
		t.Errorf("staleAfter = %s, want days: a capped value requeues runs that are still working", got)
	}
}

func TestProviderKey_picksTheCredentialForTheConfiguredProvider(t *testing.T) {
	// Arrange
	both := config.ProvidersConfig{AnthropicAPIKey: "anthropic-key", OpenAIAPIKey: "openai-key"}

	cases := map[string]struct {
		defaultProvider string
		want            string
	}{
		"anthropic": {defaultProvider: config.ProviderAnthropic, want: "anthropic-key"},
		"openai":    {defaultProvider: config.ProviderOpenAI, want: "openai-key"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			providers := both
			providers.Default = tc.defaultProvider

			// Assert
			if got := providerKey(providers); got != tc.want {
				t.Errorf("providerKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewSandbox_refusesABackendItDoesNotImplement(t *testing.T) {
	// Arrange
	workspaces, err := sandbox.NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}

	// Act: config would have refused this already. Reaching here means a backend was
	// added to config and not to the wiring, and running on the host instead is the one
	// outcome that must not happen quietly.
	_, err = newSandbox(config.SandboxConfig{Backend: "e2b"}, workspaces)

	// Assert
	if err == nil {
		t.Fatal("newSandbox accepted an unimplemented backend")
	}

	// And the backend that is implemented is built.
	if _, err := newSandbox(config.SandboxConfig{Backend: config.SandboxLocal}, workspaces); err != nil {
		t.Errorf("newSandbox(local) = %v, want a working backend", err)
	}
}

// stubBox is a sandbox that reports whatever the test wants.
type stubBox struct {
	result sandbox.ExecResult
	err    error
}

func (s stubBox) Exec(context.Context, string, string) (sandbox.ExecResult, error) {
	return s.result, s.err
}

func TestSandboxTool_passesTheBackendsAnswerThroughUnchanged(t *testing.T) {
	// Arrange: a command that ran and failed. The exit code is the model's to read, so it
	// must survive the adapter — a translation that dropped it would turn every failed
	// command into a successful one.
	adapter := sandboxTool{box: stubBox{result: sandbox.ExecResult{
		Stdout:   "out",
		Stderr:   "err",
		ExitCode: 2,
	}}}

	// Act
	got, err := adapter.Exec(context.Background(), "/workspace", "false")

	// Assert
	if err != nil {
		t.Fatalf("Exec = %v, want no error: a non-zero exit is output, not a failure", err)
	}
	if want := (tool.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 2}); got != want {
		t.Errorf("Exec = %+v, want %+v", got, want)
	}
}

func TestSandboxTool_reportsACommandThatCouldNotBeRunAtAll(t *testing.T) {
	// Arrange: no daemon, no shell — the failure the model cannot correct.
	boom := errors.New("docker: not installed")
	adapter := sandboxTool{box: stubBox{err: boom}}

	// Act
	_, err := adapter.Exec(context.Background(), "/workspace", "ls")

	// Assert
	if !errors.Is(err, boom) {
		t.Errorf("Exec error = %v, want the backend's own %v", err, boom)
	}
}

func TestShouldTouch_writesLastSeenOncePerInterval(t *testing.T) {
	// Arrange
	store := newSessionStore(nil, testLogger())
	const token = "stored-token-hash"
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

	// Act + Assert: the first use of a session is worth a write.
	if !store.shouldTouch(token, now) {
		t.Fatal("shouldTouch = false on first use, want true")
	}
	// The next request a second later is not. Resolve runs on every authenticated
	// request, and last seen is for a person recognising their own devices.
	if store.shouldTouch(token, now.Add(time.Second)) {
		t.Error("shouldTouch = true a second later, want false: that is a write per request")
	}
	// Once it is stale again, it is.
	if !store.shouldTouch(token, now.Add(touchInterval+time.Second)) {
		t.Error("shouldTouch = false after the interval, want true")
	}
	// A different session is judged on its own.
	if !store.shouldTouch("another-hash", now.Add(time.Second)) {
		t.Error("shouldTouch = false for a second session, want true")
	}
}

func TestShouldTouch_forgetsRatherThanGrowingWithoutBound(t *testing.T) {
	// Arrange
	store := newSessionStore(nil, testLogger())
	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

	// Act: more sessions than the throttle tracks. A long-lived process serving many
	// sign-ins must not accumulate one entry per token for ever.
	for i := 0; i < maxTouchTracked+10; i++ {
		store.shouldTouch(strconv.Itoa(i), now)
	}

	// Assert: the cost of forgetting is one extra write; the cost of not forgetting is a
	// process that has to be restarted.
	if len(store.touched) > maxTouchTracked {
		t.Errorf("tracked %d tokens, want at most %d", len(store.touched), maxTouchTracked)
	}
}

func TestReadPassword_takesTheLineAsItIsGiven(t *testing.T) {
	// Arrange: a passphrase with spaces in it, sent through a pipe. Trimming those would
	// silently store a different password from the one the operator typed.
	cases := map[string]struct {
		stdin string
		want  string
	}{
		"a line":                                   {stdin: "correct horse battery\n", want: "correct horse battery"},
		"crlf, as a pipe on windows may send":      {stdin: "correct horse\r\n", want: "correct horse"},
		"no trailing newline, as printf sends":     {stdin: "correct horse", want: "correct horse"},
		"trailing spaces are part of a passphrase": {stdin: "correct horse  \n", want: "correct horse  "},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got, err := readPassword(strings.NewReader(tc.stdin))

			// Assert
			if err != nil {
				t.Fatalf("readPassword = %v, want no error", err)
			}
			if got != tc.want {
				t.Errorf("readPassword = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadPassword_refusesAnEmptyOne(t *testing.T) {
	// Arrange + Act: an operator who forgot the pipe. Accepting this would create an
	// account whose password is the empty string.
	for name, stdin := range map[string]string{"nothing at all": "", "a bare newline": "\n"} {
		t.Run(name, func(t *testing.T) {
			_, err := readPassword(strings.NewReader(stdin))

			// Assert
			if err == nil {
				t.Error("readPassword accepted an empty password")
			}
		})
	}
}

func TestShippedToolConfig_parsesAndPermitsNothing(t *testing.T) {
	// Arrange: the three files this package loads before it touches a credential. They
	// are parsed with KnownFields(true), so a misspelled key in one of them is a boot
	// failure — including in the commented examples somebody uncomments.
	shipped := filepath.Join("..", "..", "config")

	// Act
	grants, grantsErr := tool.LoadGrants(filepath.Join(shipped, "tools.yaml"))
	servers, serversErr := tool.LoadMCPServers(filepath.Join(shipped, "mcp.yaml"))
	services, servicesErr := tool.LoadHTTPServices(filepath.Join(shipped, "http-tools.yaml"))

	// Assert
	if grantsErr != nil || serversErr != nil || servicesErr != nil {
		t.Fatalf("the shipped config does not load: tools.yaml: %v; mcp.yaml: %v; http-tools.yaml: %v",
			grantsErr, serversErr, servicesErr)
	}

	// And it grants nothing. A fresh instance can think, read its own workspace and
	// answer; it cannot act until an operator writes down what it may do. Shipping one
	// granted tool would make that decision for every person who installs this.
	if grants.Len() != 0 {
		t.Errorf("the shipped tools.yaml grants %v", grants.Names())
	}
	if len(servers) != 0 {
		t.Errorf("the shipped mcp.yaml declares %d servers, want none", len(servers))
	}
	if len(services) != 0 {
		t.Errorf("the shipped http-tools.yaml declares %d services, want none", len(services))
	}
}

func TestSpendGate_tellsTheGateWhichRunAndWhichTool_andNothingElse(t *testing.T) {
	// Arrange
	var received map[string]any
	engineAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("the engine could not parse this body: %v; body: %s", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"data":{"id":"approval-1","outcome":"pending","policyReason":"above the auto-approve threshold"}}`)
	}))
	t.Cleanup(engineAPI.Close)

	engine, err := goalengine.New(engineAPI.URL, "engine-key")
	if err != nil {
		t.Fatalf("goalengine.New: %v", err)
	}

	// Act
	decision, err := spendGate{engine: engine}.Request(context.Background(), agent.SpendRequest{
		ActionType:     "ads_topup",
		Amount:         250,
		Currency:       "IDR",
		RunID:          "run-7",
		ToolName:       "mcp__ads__topup",
		IdempotencyKey: "run-7:step-3",
	})

	// Assert: the engine's answer is passed through, pending included. Pending means a
	// human was asked, not that this call may proceed.
	if err != nil {
		t.Fatalf("Request = %v, want the gate's decision", err)
	}
	if decision.Outcome != domain.ApprovalPending {
		t.Errorf("outcome = %q, want %q", decision.Outcome, domain.ApprovalPending)
	}
	if decision.Reason == "" {
		t.Error("reason is empty, want the engine's own sentence")
	}

	// The payload names the run and the tool. It is stored on the approval row and shown
	// to whoever decides, so it must carry no brief, no model output and no tool
	// arguments.
	payload, ok := received["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v, want the run and the tool", received["payload"])
	}
	if payload["runId"] != "run-7" || payload["toolName"] != "mcp__ads__topup" {
		t.Errorf("payload = %v, want runId and toolName", payload)
	}
	if len(payload) != 2 {
		t.Errorf("payload = %v, want exactly the run and the tool", payload)
	}
}

// nothingInbound stands in for service.Inbox.
//
// Nothing below reaches it: a hub is built and inspected, never run, because running one
// opens a connection to somebody else's servers.
type nothingInbound struct{}

func (nothingInbound) Handle(context.Context, channel.Inbound) (channel.Handled, error) {
	return channel.Handled{}, nil
}

func TestNewChannelHub_connectsToExactlyThePlatformsWithCredentials(t *testing.T) {
	// Arrange
	//
	// These are not credentials and would not work as any — the Telegram one is only
	// shaped like one, because telego checks the shape. What is asserted is the mapping
	// from a configuration field to a platform, which the build cannot check for us: a
	// token read from the wrong field compiles, and a platform whose branch was never
	// written would simply be missing.
	const (
		telegramToken = "123456789:AAHfiqksKZ8WmoWZ7Q4tAgh1234567890ab"
		slackBotToken = "not-a-slack-bot-token"
		slackAppToken = "not-a-slack-app-token"
		discordToken  = "not-a-discord-token"
	)

	cases := map[string]struct {
		cfg  config.ChannelsConfig
		want []domain.ChannelKind
	}{
		// The ordinary instance: reached over HTTP, on no chat platform. A hub is still built,
		// so nothing above it has to hold a nil.
		"nothing configured": {want: []domain.ChannelKind{}},
		"telegram alone": {
			cfg:  config.ChannelsConfig{TelegramToken: telegramToken},
			want: []domain.ChannelKind{domain.ChannelTelegram},
		},
		"slack alone": {
			cfg:  config.ChannelsConfig{SlackBotToken: slackBotToken, SlackAppToken: slackAppToken},
			want: []domain.ChannelKind{domain.ChannelSlack},
		},
		"discord alone": {
			cfg:  config.ChannelsConfig{DiscordToken: discordToken},
			want: []domain.ChannelKind{domain.ChannelDiscord},
		},
		"all three": {
			cfg: config.ChannelsConfig{
				TelegramToken: telegramToken,
				SlackBotToken: slackBotToken,
				SlackAppToken: slackAppToken,
				DiscordToken:  discordToken,
			},
			want: domain.AllChannelKinds(),
		},
		// Half a Slack app connects to nothing, so it contributes nothing. Config refuses this
		// combination before a hub is ever asked for; the branch here has to agree with it
		// rather than build a client that cannot open a socket.
		"a slack bot token with no app token": {
			cfg:  config.ChannelsConfig{SlackBotToken: slackBotToken},
			want: []domain.ChannelKind{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			hub, err := newChannelHub(tc.cfg, nothingInbound{}, testLogger())

			// Assert
			if err != nil {
				t.Fatalf("newChannelHub: %v", err)
			}
			if got := kindNames(hub.Kinds()); !slicesEqual(got, kindNames(tc.want)) {
				t.Errorf("connected = %v, want %v", got, kindNames(tc.want))
			}
		})
	}
}

func TestNewChannelHub_refusesAMalformedTokenAtBootRatherThanOnTheFirstPoll(t *testing.T) {
	// Arrange & Act: a token pasted with a piece missing. Telegram's own token shape is
	// checkable without asking Telegram, so it is checked here.
	_, err := newChannelHub(config.ChannelsConfig{TelegramToken: "AAHfiqksKZ8WmoWZ7Q4tAgh"},
		nothingInbound{}, testLogger())

	// Assert: refused, and refused before the process claims to be serving. The operator is
	// at a terminal now; on the first poll they are not.
	if err == nil {
		t.Fatal("newChannelHub accepted a token that is not a Telegram token")
	}
	// And it says which platform. Four tokens can be configured and the message is the
	// whole diagnosis — while carrying none of the token itself.
	if !strings.Contains(err.Error(), "telegram") {
		t.Errorf("error = %q, want it to name the platform", err)
	}
	if strings.Contains(err.Error(), "AAHfiqksKZ8WmoWZ7Q4tAgh") {
		t.Errorf("error = %q, want it not to echo the token", err)
	}
}

func TestConnectedKinds_saysNothingRatherThanGuessingWhenThereIsNoHub(t *testing.T) {
	// Arrange, Act & Assert
	//
	// An instance on no platform has no hub at all, and /v1/reference still has to answer.
	// Nil is what the handler renders as an empty array.
	if got := connectedKinds(nil); got != nil {
		t.Errorf("connectedKinds(nil) = %v, want nil", got)
	}

	hub, err := newChannelHub(config.ChannelsConfig{TelegramToken: "123456789:AAHfiqksKZ8WmoWZ7Q4tAgh1234567890ab"},
		nothingInbound{}, testLogger())
	if err != nil {
		t.Fatalf("newChannelHub: %v", err)
	}
	if got := connectedKinds(hub); !slicesEqual(got, []string{string(domain.ChannelTelegram)}) {
		t.Errorf("connectedKinds = %v, want just telegram", got)
	}
}

func TestDirectSender_withNoPlatformConnectedRefusesRatherThanReportingADelivery(t *testing.T) {
	// Arrange — an instance reached only over HTTP has no hub at all. The nil could not be
	// handed to the notifier directly: a typed nil in an interface passes a non-nil check, so
	// the constructor would accept it and the failure would surface as a panic on the first
	// notification instead of as a refusal here.
	sender := directSender(nil)

	// Act
	err := sender.SendDirect(context.Background(), domain.ChannelTelegram, "987654321",
		"A spend is waiting for a decision.")

	// Assert — refused, and refused as "not connected" rather than silently succeeding.
	// Reporting a delivery nobody received is the one lie this path must not tell: the
	// operator's next move is to stop waiting for a message that never comes.
	if !errors.Is(err, channel.ErrNotConnected) {
		t.Fatalf("err = %v, want channel.ErrNotConnected", err)
	}
	if !strings.Contains(err.Error(), string(domain.ChannelTelegram)) {
		t.Errorf("err = %v, want it to name the platform", err)
	}
}

func TestDirectSender_withAHubIsTheHub(t *testing.T) {
	// Arrange
	hub, err := newChannelHub(config.ChannelsConfig{TelegramToken: "123456789:AAHfiqksKZ8WmoWZ7Q4tAgh1234567890ab"},
		nothingInbound{}, testLogger())
	if err != nil {
		t.Fatalf("newChannelHub: %v", err)
	}

	// Act
	sender := directSender(hub)

	// Assert — the hub itself, not a wrapper. A layer between the notifier and the adapters
	// would be somewhere for the message to be rephrased, and what may leave the box was
	// decided next door.
	if sender != service.DirectSender(hub) {
		t.Error("directSender returned something other than the hub")
	}

	// And a platform the hub is not connected to is still refused, which is what makes the
	// no-hub case above a special case of one rule rather than a second one.
	if err := sender.SendDirect(context.Background(), domain.ChannelSlack, "U123ABC", "A goal fell behind."); !errors.Is(err, channel.ErrNotConnected) {
		t.Errorf("err = %v, want channel.ErrNotConnected", err)
	}
}

func kindNames(kinds []domain.ChannelKind) []string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return names
}

func slicesEqual(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
