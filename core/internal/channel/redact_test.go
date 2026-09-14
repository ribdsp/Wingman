package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedact_takesTheTokenOutOfTheErrorText(t *testing.T) {
	// Arrange — this is the real case, not a hypothetical one: Telegram puts the bot token
	// in the URL path, so a transport failure against it produces an error string with the
	// credential in the middle of it. An operator pasting that into an issue has published
	// a token that can read every message the bot receives.
	token := "1234567890:AAF-thisIsTheBotTokenAndItMustNotAppear"
	inner := fmt.Errorf("Post \"https://api.telegram.org/bot%s/getMe\": dial tcp: timeout", token)

	// Act
	err := redact(fmt.Errorf("telegram: identify this bot: %w", inner), token)

	// Assert
	if strings.Contains(err.Error(), token) {
		t.Fatal("the error still carries the token")
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("err = %v, want the token replaced rather than the message emptied", err)
	}
	// And what is left still says what went wrong, because an error nobody can act on is
	// only marginally better than a leaked one.
	for _, want := range []string{"identify this bot", "api.telegram.org", "timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to still mention %q", err, want)
		}
	}
}

func TestRedact_keepsTheWrappedErrorRecognisable(t *testing.T) {
	// Arrange — the hub decides whether a channel stopped or crashed with errors.Is against
	// context.Canceled. A wrapper that hid what it holds would turn every ordinary shutdown
	// into a logged failure.
	err := redact(fmt.Errorf("slack: socket mode: %w", context.Canceled), "xoxb-a-token-long-enough")

	// Act & Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false for %v", err)
	}
}

func TestRedact_withNothingWorthHidingReturnsTheErrorItself(t *testing.T) {
	// Arrange — an unset optional token is the empty string, and a short value is not a
	// credential any of these platforms issues. Substituting either would either do nothing
	// or replace every character of the message.
	inner := errors.New("dial tcp: connection refused")
	cases := map[string][]string{
		"no secrets at all": nil,
		"an unset token":    {""},
		"a short value":     {"abc"},
	}

	for name, secrets := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			err := redact(inner, secrets...)

			// Assert
			if err.Error() != inner.Error() {
				t.Errorf("err = %v, want the message unchanged", err)
			}
			if strings.Contains(err.Error(), "[redacted]") {
				t.Error("a value too short to be a credential was substituted out")
			}
		})
	}
}

func TestRedact_ofNothingIsNothing(t *testing.T) {
	// Arrange — every call site wraps a possibly-nil error, so returning a non-nil wrapper
	// around nil would turn every success into a failure.

	// Act & Assert
	if err := redact(nil, "a-token-long-enough"); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestScrub_hidesEverySecretItWasGivenEverywhereTheyAppear(t *testing.T) {
	// Arrange — Slack holds two credentials at once and either can turn up in a line the
	// SDK formats itself, which is the other way a token reaches a log file.
	bot, app := "xoxb-bot-token-value", "xapp-app-token-value"
	line := fmt.Sprintf("auth failed for %s using %s, retrying with %s", bot, app, bot)

	// Act
	got := scrub(line, bot, app, "")

	// Assert
	for _, secret := range []string{bot, app} {
		if strings.Contains(got, secret) {
			t.Errorf("got %q, want %q gone", got, secret)
		}
	}
	if !strings.Contains(got, "auth failed") {
		t.Errorf("got %q, want the reason kept", got)
	}
}
