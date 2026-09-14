package channel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// telegramTestToken has the shape telego insists on — digits, a colon, thirty-five more
// characters — without being one Telegram ever issued.
const telegramTestToken = "1234567890:AAF-testTokenValueForUnitTests-0000"

// telegramSelf is the bot these tests are connected as, as Run would have filled it in.
var telegramSelf = telegramBot{id: 900, username: "wingmanbot"}

func TestNewTelegram_refusesToBuildWithoutWhatItNeeds(t *testing.T) {
	// Arrange — an adapter with no handler would read updates and drop every one of them,
	// which looks exactly like a working connection to a quiet bot.
	cases := map[string]TelegramConfig{
		"no token":   {Handler: &fakeHandler{}},
		"no handler": {Token: telegramTestToken},
		"a token that is not one": {
			Token:   "not-a-telegram-token",
			Handler: &fakeHandler{},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := NewTelegram(cfg)

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the adapter refused")
			}
			if strings.Contains(err.Error(), cfg.Token) && cfg.Token != "" {
				t.Errorf("err = %v, want it not to quote the token", err)
			}
		})
	}
}

func TestNewTelegram_doesNotTouchTheNetwork(t *testing.T) {
	// Arrange — cmd builds every configured adapter before anything is running. A
	// constructor that connected would mean a Telegram outage stopping core from booting,
	// and taking the HTTP API and the other channels down with it.

	// Act
	tg, err := NewTelegram(TelegramConfig{Token: telegramTestToken, Handler: &fakeHandler{}, Logger: testLogger()})

	// Assert
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}
	if tg.Kind() != domain.ChannelTelegram {
		t.Errorf("Kind() = %s, want telegram", tg.Kind())
	}
	// Nothing was asked of Telegram, so nothing is known about who this bot is yet — that
	// is Run's first job.
	if tg.self != (telegramBot{}) {
		t.Errorf("self = %+v, want it unknown until Run", tg.self)
	}
}

func TestInboundFromTelegram_acceptsAPrivateMessage(t *testing.T) {
	// Arrange — the ordinary case. A private chat needs no mention: there is nobody else in
	// it to have been talking to.
	update := telego.Update{Message: &telego.Message{
		Chat: telego.Chat{ID: -100, Type: telego.ChatTypePrivate},
		From: &telego.User{ID: 42, FirstName: "Rama", LastName: "Putra", Username: "ramap"},
		Text: "what happened to revenue",
	}}

	// Act
	in, ok := inboundFromTelegram(update, telegramSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message accepted")
	}
	want := Inbound{
		Kind:           domain.ChannelTelegram,
		SenderID:       "42",
		SenderName:     "Rama Putra",
		ConversationID: "-100",
		Text:           "what happened to revenue",
		Direct:         true,
	}
	if in != want {
		t.Errorf("in = %+v, want %+v", in, want)
	}
}

func TestInboundFromTelegram_readsTheCaptionOfAMessageThatIsAnAttachment(t *testing.T) {
	// Arrange — a photograph of an invoice with the instruction written underneath it. The
	// attachment is not something core can do anything with; the caption is the brief.
	update := telego.Update{Message: &telego.Message{
		Chat:    telego.Chat{ID: 7, Type: telego.ChatTypePrivate},
		From:    &telego.User{ID: 42, FirstName: "Rama"},
		Caption: "reconcile this against the ledger",
	}}

	// Act
	in, ok := inboundFromTelegram(update, telegramSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the caption read as the message")
	}
	if in.Text != "reconcile this against the ledger" {
		t.Errorf("text = %q, want the caption", in.Text)
	}
}

func TestInboundFromTelegram_marksAMessageFromAnotherBot(t *testing.T) {
	// Arrange — two bots in one chat answering each other is a loop that spends real money
	// on both sides. The fact is recorded here; the decision belongs to the domain.
	update := telego.Update{Message: &telego.Message{
		Chat: telego.Chat{ID: 7, Type: telego.ChatTypePrivate},
		From: &telego.User{ID: 999, FirstName: "Another", IsBot: true},
		Text: "revenue is up",
	}}

	// Act
	in, ok := inboundFromTelegram(update, telegramSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message normalised so the domain can refuse it")
	}
	if !in.FromBot {
		t.Error("fromBot = false, want the platform's own answer carried through")
	}
}

func TestInboundFromTelegram_ignoresAnUpdateWithNothingToActOn(t *testing.T) {
	// Arrange — only message updates were asked for, but Telegram adds types and an
	// anonymous group admin posts as the group itself, with no user to link an account to.
	cases := map[string]telego.Update{
		"an update that is not a message": {},
		"a message with no author": {Message: &telego.Message{
			Chat: telego.Chat{ID: 7, Type: telego.ChatTypePrivate},
			Text: "what happened to revenue",
		}},
	}

	for name, update := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromTelegram(update, telegramSelf)

			// Assert
			if ok {
				t.Error("ok = true, want the update ignored")
			}
		})
	}
}

func TestInboundFromTelegram_inAGroupForwardsOnlyAMessageAddressedToTheBot(t *testing.T) {
	// Arrange — a bot in a group receives every message in it. Forwarding those would mean
	// a room's whole conversation reaching a model, on the account of whoever linked first.
	group := telego.Chat{ID: -1001, Type: "supergroup"}
	author := &telego.User{ID: 42, FirstName: "Rama"}
	cases := map[string]struct {
		message  *telego.Message
		want     string
		accepted bool
	}{
		"mentioned by username": {
			message:  &telego.Message{Chat: group, From: author, Text: "@wingmanbot what happened to revenue"},
			want:     "what happened to revenue",
			accepted: true,
		},
		"a reply to something the bot said": {
			message: &telego.Message{Chat: group, From: author, Text: "and last month?",
				ReplyToMessage: &telego.Message{From: &telego.User{ID: telegramSelf.id, IsBot: true}}},
			want:     "and last month?",
			accepted: true,
		},
		"overheard": {
			message: &telego.Message{Chat: group, From: author, Text: "what happened to revenue"},
		},
		"a reply to somebody else": {
			message: &telego.Message{Chat: group, From: author, Text: "and last month?",
				ReplyToMessage: &telego.Message{From: &telego.User{ID: 77}}},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			in, ok := inboundFromTelegram(telego.Update{Message: tc.message}, telegramSelf)

			// Assert
			if ok != tc.accepted {
				t.Fatalf("ok = %v, want %v", ok, tc.accepted)
			}
			if !tc.accepted {
				return
			}
			if in.Text != tc.want {
				t.Errorf("text = %q, want %q", in.Text, tc.want)
			}
			// Not direct, and that is what makes the message answerable in front of other
			// people: the domain reads it and applies the group rule.
			if in.Direct {
				t.Error("direct = true, want a group message marked as such")
			}
		})
	}
}

func TestInboundFromTelegram_aBotWithNoUsernameCanOnlyBeRepliedTo(t *testing.T) {
	// Arrange — a bot can exist without a username. Matching a bare "@" instead would make
	// every pasted email address in the room an instruction.
	nameless := telegramBot{id: 900}
	group := telego.Chat{ID: -1001, Type: "supergroup"}
	author := &telego.User{ID: 42, FirstName: "Rama"}

	// Act
	_, mentioned := inboundFromTelegram(telego.Update{Message: &telego.Message{
		Chat: group, From: author, Text: "email finance@example.com about revenue",
	}}, nameless)
	_, replied := inboundFromTelegram(telego.Update{Message: &telego.Message{
		Chat: group, From: author, Text: "and last month?",
		ReplyToMessage: &telego.Message{From: &telego.User{ID: nameless.id, IsBot: true}},
	}}, nameless)

	// Assert
	if mentioned {
		t.Error("a message containing an @ was read as addressing a bot with no username")
	}
	if !replied {
		t.Error("a reply to the bot was not read as addressing it")
	}
}

func TestTelegramSenderName_fallsBackToTheHandleAndThenToNothing(t *testing.T) {
	// Arrange — a person sees this in their own list of connected accounts, to know which
	// one to revoke. Both names are optional, and a bot account often has neither.
	cases := map[string]struct {
		from *telego.User
		want string
	}{
		"both names":     {&telego.User{FirstName: "Rama", LastName: "Putra", Username: "ramap"}, "Rama Putra"},
		"first only":     {&telego.User{FirstName: "Rama", Username: "ramap"}, "Rama"},
		"neither name":   {&telego.User{Username: "ramap"}, "ramap"},
		"nothing at all": {&telego.User{}, ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act & Assert
			if got := telegramSenderName(tc.from); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTelegramSend_refusesAConversationThatCannotBeAChatId(t *testing.T) {
	// Arrange — a Telegram chat id is a number, so a stored value that is not one is a
	// corrupted row or a caller's bug rather than a chat that has gone away. Naming it is
	// the only way to find either, and it is refused before any request is made.
	tg, err := NewTelegram(TelegramConfig{Token: telegramTestToken, Handler: &fakeHandler{}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}

	// Act
	err = tg.Send(context.Background(), Reply{ConversationID: "slack-chat-1", Text: "Revenue is up."})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the send refused")
	}
	if !strings.Contains(err.Error(), "slack-chat-1") {
		t.Errorf("err = %v, want it to name the value", err)
	}
}

func TestTelegramDirectTarget_isTheUserIdItselfBecauseAPrivateChatSharesIt(t *testing.T) {
	// Arrange — a link stores only the sender's own id, and on Telegram the private chat
	// with a person has that same id. So there is nothing to open and no request to make:
	// a call here would put Telegram's availability in front of a notification that has
	// not been attempted yet.
	tg, err := NewTelegram(TelegramConfig{Token: telegramTestToken, Handler: &fakeHandler{}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}

	// Act
	got, err := tg.DirectTarget(context.Background(), "  987654321  ")

	// Assert
	if err != nil {
		t.Fatalf("DirectTarget: %v", err)
	}
	if got != "987654321" {
		t.Errorf("conversation = %q, want the user id itself", got)
	}
}

func TestTelegramDirectTarget_refusesAnIdThatCannotBeAChatId(t *testing.T) {
	// Arrange — a Slack-shaped id in a Telegram row is a corrupted link. Refusing it here
	// names the value; letting it through would surface as a 400 from Telegram about a
	// message nobody can trace back to a row.
	tg, err := NewTelegram(TelegramConfig{Token: telegramTestToken, Handler: &fakeHandler{}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}

	// Act
	_, err = tg.DirectTarget(context.Background(), "U123ABC")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the recipient refused")
	}
	if !strings.Contains(err.Error(), "U123ABC") {
		t.Errorf("err = %v, want it to name the value", err)
	}
	if strings.Contains(err.Error(), telegramTestToken) {
		t.Error("the error carries the bot token")
	}
}

func TestTelegramClose_releasesNothingAndSaysSo(t *testing.T) {
	// Arrange — long polling holds no session, so shutdown is the cancellation of Run's
	// context. Close exists because the interface has it, and it must not fail.
	tg, err := NewTelegram(TelegramConfig{Token: telegramTestToken, Handler: &fakeHandler{}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewTelegram: %v", err)
	}

	// Act & Assert
	if err := tg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestTelegoLogger_keepsTheTokenOutOfWhatTheSdkLogs(t *testing.T) {
	// Arrange — the SDK's own long-polling loop reports a rejected getUpdates through this
	// interface, and its debug lines are documented as containing the bot token. Discarding
	// everything would be safe and would also hide an outage; this is the middle.
	var buf bytes.Buffer
	logger := telegoLogger{log: zerolog.New(&buf), token: telegramTestToken}

	// Act
	logger.Debugf("getting updates from https://api.telegram.org/bot%s/getUpdates", telegramTestToken)
	logger.Errorf("failed to get updates from https://api.telegram.org/bot%s/getUpdates: 401", telegramTestToken)

	// Assert
	out := buf.String()
	if strings.Contains(out, telegramTestToken) {
		t.Fatal("the log carries the bot token")
	}
	if !strings.Contains(out, "401") {
		t.Errorf("log = %q, want the failure still visible", out)
	}
	// One line, not two: the debug line was dropped rather than scrubbed, because it is
	// formatted on the other side of a module boundary.
	if got := strings.Count(strings.TrimSpace(out), "\n") + 1; got != 1 {
		t.Errorf("lines = %d, want only the error", got)
	}
}
