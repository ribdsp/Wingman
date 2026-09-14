package config

import (
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// The chat-platform half of the environment, in a file of its own because config_test.go
// is already at the length this project treats as a ceiling.
//
// These are not credentials, and they deliberately do not carry a real token's *shape*
// either. Nothing in config parses one — `Validate` only asks whether a pair is present
// and whether the two differ — so realism buys nothing here, and it costs something: a
// fixture shaped like `xoxb-<digits>-<digits>-<alnum>` is what GitHub's secret scanning
// looks for, and a public push carrying it is rejected. The way out of that is to allow a
// scanner finding, which is a bad habit to acquire on a repository whose subject is
// credential handling. So each value names its platform, stays distinctive enough that the
// "no problem echoes a token" assertion is a real search rather than one that would pass
// against any error text, and looks like nothing a scanner should care about.
const (
	telegramToken = "sample-telegram-token-not-a-real-credential"
	slackBotToken = "sample-slack-bot-token-not-a-real-credential"
	slackAppToken = "sample-slack-app-token-not-a-real-credential"
	discordToken  = "sample-discord-token-not-a-real-credential"
)

// No channel connected is the ordinary instance: reached over HTTP by a client and by the
// goal engine, listening on no chat platform at all. Everything that bounds an inbound
// message still has a value, because a platform can be switched on without the operator
// having thought about throttling.
func TestLoad_connectsToNoChannelByDefault(t *testing.T) {
	// Arrange
	setValidEnv(t)

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Channels.Configured() {
		t.Errorf("channels = %+v; want none connected", cfg.Channels)
	}
	if cfg.Channels.LinkCodeTTL != defaultLinkCodeTTL {
		t.Errorf("link code ttl = %s; want %s", cfg.Channels.LinkCodeTTL, defaultLinkCodeTTL)
	}
	if cfg.Channels.MinInterval != domain.MinInboundInterval {
		t.Errorf("min interval = %s; want the domain floor %s", cfg.Channels.MinInterval, domain.MinInboundInterval)
	}
	// Off by default is the safety property: in a shared room the linked person's token
	// budget is spendable by anybody who can type there.
	if cfg.Channels.AllowGroups {
		t.Error("groups were allowed by default")
	}
}

func TestLoad_acceptsEachPlatformOnItsOwn(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		env       map[string]string
		wantSlack bool
	}{
		"telegram alone": {env: map[string]string{"CHANNEL_TELEGRAM_TOKEN": telegramToken}},
		"discord alone":  {env: map[string]string{"CHANNEL_DISCORD_TOKEN": discordToken}},
		"slack, both tokens": {
			env: map[string]string{
				"CHANNEL_SLACK_BOT_TOKEN": slackBotToken,
				"CHANNEL_SLACK_APP_TOKEN": slackAppToken,
			},
			wantSlack: true,
		},
		"all three": {
			env: map[string]string{
				"CHANNEL_TELEGRAM_TOKEN":  telegramToken,
				"CHANNEL_SLACK_BOT_TOKEN": slackBotToken,
				"CHANNEL_SLACK_APP_TOKEN": slackAppToken,
				"CHANNEL_DISCORD_TOKEN":   discordToken,
			},
			wantSlack: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			// Act
			cfg, err := Load()

			// Assert
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !cfg.Channels.Configured() {
				t.Error("Configured() = false with a token set")
			}
			if got := cfg.Channels.SlackConfigured(); got != tc.wantSlack {
				t.Errorf("SlackConfigured() = %v; want %v", got, tc.wantSlack)
			}
		})
	}
}

// Half a Slack app is the one combination that is a mistake rather than a choice, and it
// is worth refusing at boot: left alone it fails when the socket opens, as a 401 with
// nothing in it naming which of the two credentials is missing.
func TestLoad_refusesHalfASlackApp(t *testing.T) {
	// Arrange
	cases := map[string]struct {
		bot, app string
		wantName string
	}{
		"bot token only": {bot: slackBotToken, wantName: "CHANNEL_SLACK_APP_TOKEN"},
		"app token only": {app: slackAppToken, wantName: "CHANNEL_SLACK_BOT_TOKEN"},
		// One value pasted into both. The two are issued separately and are not
		// interchangeable, so this is never a working app.
		"the same value twice": {bot: slackBotToken, app: slackBotToken, wantName: "must be different"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("CHANNEL_SLACK_BOT_TOKEN", tc.bot)
			t.Setenv("CHANNEL_SLACK_APP_TOKEN", tc.app)

			// Act
			_, err := Load()

			// Assert
			if err == nil {
				t.Fatal("a half-configured Slack app was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error = %v; want it to name %q", err, tc.wantName)
			}
		})
	}
}

func TestLoad_rejectsALinkCodeTTLOutsideItsBounds(t *testing.T) {
	// Arrange
	//
	// The bounds are the ones internal/service/channels.go clamps to. A code is a bearer
	// credential — whoever sends it from a chat account attaches that account to the
	// person who minted it — so a day-long one is a day-long window for the wrong person
	// to use a code left in a scrollback.
	cases := map[string]string{
		"far too short":      "10s",
		"far too long":       "24h",
		"zero":               "0s",
		"negative":           "-15m",
		"not a duration":     "quarter of an hour",
		"only just short":    (minLinkCodeTTL - time.Second).String(),
		"only just too long": (maxLinkCodeTTL + time.Second).String(),
	}

	// Act & Assert
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("CHANNEL_LINK_CODE_TTL", value)

			if _, err := Load(); err == nil {
				t.Fatalf("CHANNEL_LINK_CODE_TTL=%q was accepted", value)
			} else if !strings.Contains(err.Error(), "CHANNEL_LINK_CODE_TTL") {
				t.Errorf("error = %v; want it to name CHANNEL_LINK_CODE_TTL", err)
			}
		})
	}
}

// Zero does not mean "no throttle" here, and there is no way to ask for that. Every
// accepted message is a run, and a run spends the linked person's tokens, so an
// unthrottled sender is a way to empty somebody else's daily cap by typing quickly.
func TestLoad_refusesAnInboundThrottleBelowTheDomainFloor(t *testing.T) {
	// Arrange
	cases := map[string]string{
		"zero":     "0s",
		"below it": "100ms",
		"negative": "-1s",
	}

	// Act & Assert
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("CHANNEL_MIN_INTERVAL", value)

			if _, err := Load(); err == nil {
				t.Fatalf("CHANNEL_MIN_INTERVAL=%q was accepted", value)
			} else if !strings.Contains(err.Error(), "CHANNEL_MIN_INTERVAL") {
				t.Errorf("error = %v; want it to name CHANNEL_MIN_INTERVAL", err)
			}
		})
	}
}

func TestLoad_acceptsAnInboundThrottleAtOrAboveTheFloor(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("CHANNEL_MIN_INTERVAL", "5s")
	t.Setenv("CHANNEL_ALLOW_GROUPS", "true")

	// Act
	cfg, err := Load()

	// Assert
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Channels.MinInterval != 5*time.Second {
		t.Errorf("min interval = %s; want 5s", cfg.Channels.MinInterval)
	}
	// An operator who asked for groups gets them. The refusal is the default, not a rule
	// they cannot lift — a private Slack channel with three colleagues in it is a
	// reasonable place to want the agent.
	if !cfg.Channels.AllowGroups {
		t.Error("an operator who allowed groups did not get them")
	}
}

// Not one of the four tokens appears in the aggregated problem list, even though the
// failure being reported is on a path that has all four in hand.
func TestLoad_neverEchoesAChannelTokenInAProblem(t *testing.T) {
	// Arrange
	setValidEnv(t)
	t.Setenv("CHANNEL_TELEGRAM_TOKEN", telegramToken)
	t.Setenv("CHANNEL_SLACK_BOT_TOKEN", slackBotToken)
	t.Setenv("CHANNEL_SLACK_APP_TOKEN", slackAppToken)
	t.Setenv("CHANNEL_DISCORD_TOKEN", discordToken)
	// Something else wrong, so there is a problem list at all.
	t.Setenv("CHANNEL_LINK_CODE_TTL", "48h")

	// Act
	_, err := Load()

	// Assert
	if err == nil {
		t.Fatal("expected the configuration to be refused")
	}
	secrets := map[string]string{
		"Telegram bot token": telegramToken,
		"Slack bot token":    slackBotToken,
		"Slack app token":    slackAppToken,
		"Discord bot token":  discordToken,
	}
	for what, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the %s appears in the configuration error", what)
		}
	}
}
