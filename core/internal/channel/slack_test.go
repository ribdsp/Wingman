package channel

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/ribdsp/wingman/core/internal/domain"
)

const (
	slackTestBotToken = "xoxb-test-bot-token-value"
	slackTestAppToken = "xapp-test-app-token-value"
)

// slackSelf is the app these tests are connected as, as Run would have filled it in.
var slackSelf = slackBot{userID: "U0WINGMAN", botID: "B0WINGMAN"}

// slackEvent wraps an inner event the way the SDK does — as a pointer, because it builds
// them by reflection and hands back the address.
func slackEvent(inner any) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{InnerEvent: slackevents.EventsAPIInnerEvent{Data: inner}}
}

func TestNewSlack_refusesToBuildWithoutWhatItNeedsAndNamesAllOfIt(t *testing.T) {
	// Arrange — Socket Mode needs two separate credentials, and an operator who set one is
	// far more likely to have missed the other. Reporting them one restart at a time is the
	// pattern this project already refuses elsewhere.

	// Act
	_, err := NewSlack(SlackConfig{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the adapter refused")
	}
	for _, want := range []string{"bot token", "app token", "handler"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention the missing %s", err, want)
		}
	}
}

func TestNewSlack_refusesEachMissingCredentialOnItsOwn(t *testing.T) {
	// Arrange
	cases := map[string]SlackConfig{
		"no bot token": {AppToken: slackTestAppToken, Handler: &fakeHandler{}},
		"no app token": {BotToken: slackTestBotToken, Handler: &fakeHandler{}},
		"no handler":   {BotToken: slackTestBotToken, AppToken: slackTestAppToken},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := NewSlack(cfg)

			// Assert
			if err == nil {
				t.Fatal("err = nil, want the adapter refused")
			}
			for _, token := range []string{cfg.BotToken, cfg.AppToken} {
				if token != "" && strings.Contains(err.Error(), token) {
					t.Errorf("err = %v, want it not to quote a credential", err)
				}
			}
		})
	}
}

func TestNewSlack_doesNotTouchTheNetwork(t *testing.T) {
	// Arrange — the same reason as the other two: a Slack outage must not stop core booting.

	// Act
	s, err := NewSlack(SlackConfig{
		BotToken: slackTestBotToken, AppToken: slackTestAppToken,
		Handler: &fakeHandler{}, Logger: testLogger(),
	})

	// Assert
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}
	if s.Kind() != domain.ChannelSlack {
		t.Errorf("Kind() = %s, want slack", s.Kind())
	}
	if s.self != (slackBot{}) {
		t.Errorf("self = %+v, want it unknown until Run", s.self)
	}
	// And Close is safe on an adapter that never connected, because the hub calls it on
	// every registered channel during shutdown whatever happened to each of them.
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestInboundFromSlack_acceptsADirectMessage(t *testing.T) {
	// Arrange — the ordinary case. A direct message needs no mention.
	event := slackEvent(&slackevents.MessageEvent{
		User:        "U042",
		Username:    "rama",
		Text:        "what happened to revenue",
		Channel:     "D0CONV",
		ChannelType: slackevents.ChannelTypeIM,
	})

	// Act
	in, ok := inboundFromSlack(event, slackSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the message accepted")
	}
	want := Inbound{
		Kind:           domain.ChannelSlack,
		SenderID:       "U042",
		SenderName:     "rama",
		ConversationID: "D0CONV",
		Text:           "what happened to revenue",
		Direct:         true,
	}
	if in != want {
		t.Errorf("in = %+v, want %+v", in, want)
	}
}

func TestInboundFromSlack_ignoresAMessageInARoom(t *testing.T) {
	// Arrange — a message event arrives for every room the app is in. Forwarding those would
	// mean a channel's whole conversation reaching a model; being spoken to in a room comes
	// back as an app mention instead, which is the event that carries the intent.
	rooms := map[string]string{
		"a public channel":   slackevents.ChannelTypeChannel,
		"a private channel":  slackevents.ChannelTypeGroup,
		"a group of several": slackevents.ChannelTypeMPIM,
	}

	for name, channelType := range rooms {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromSlack(slackEvent(&slackevents.MessageEvent{
				User: "U042", Text: "what happened to revenue",
				Channel: "C0ROOM", ChannelType: channelType,
			}), slackSelf)

			// Assert
			if ok {
				t.Error("ok = true, want a room message left to the app mention event")
			}
		})
	}
}

func TestInboundFromSlack_ignoresAMessageThatIsNotSomebodyTyping(t *testing.T) {
	// Arrange — every subtype would need its own reading of what the text now means. An edit
	// arrives with the whole new message, so acting on it would run the brief twice; a
	// deletion arrives with the old one, and running that is worse.
	subtypes := []string{"message_changed", "message_deleted", "thread_broadcast", "file_share"}

	for _, subtype := range subtypes {
		t.Run(subtype, func(t *testing.T) {
			// Act
			_, ok := inboundFromSlack(slackEvent(&slackevents.MessageEvent{
				User: "U042", Text: "what happened to revenue",
				Channel: "D0CONV", ChannelType: slackevents.ChannelTypeIM, SubType: subtype,
			}), slackSelf)

			// Assert
			if ok {
				t.Errorf("ok = true, want %s ignored", subtype)
			}
		})
	}
}

func TestInboundFromSlack_marksAMessageThatCameFromAnApp(t *testing.T) {
	// Arrange — including this one seeing its own answer echoed back, which is a loop that
	// spends money on every turn. Both of Slack's ways of saying so are read.
	cases := map[string]*slackevents.MessageEvent{
		"another app": {User: "U099", BotID: "B099", Text: "revenue is up",
			Channel: "D0CONV", ChannelType: slackevents.ChannelTypeIM},
		"this app's own message": {User: slackSelf.userID, Text: "Revenue is up 3%.",
			Channel: "D0CONV", ChannelType: slackevents.ChannelTypeIM},
	}

	for name, inner := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			in, ok := inboundFromSlack(slackEvent(inner), slackSelf)

			// Assert
			if !ok {
				t.Fatal("ok = false, want the message normalised so the domain can refuse it")
			}
			if !in.FromBot {
				t.Error("fromBot = false, want it recognised as an app's message")
			}
		})
	}
}

func TestInboundFromSlack_acceptsAMentionInARoomWithoutTheMention(t *testing.T) {
	// Arrange — this is how being spoken to in a room arrives, and the mention has to come
	// out: what reaches the model should be the instruction, not the address.
	event := slackEvent(&slackevents.AppMentionEvent{
		User:    "U042",
		Text:    "<@U0WINGMAN> what happened to revenue",
		Channel: "C0ROOM",
	})

	// Act
	in, ok := inboundFromSlack(event, slackSelf)

	// Assert
	if !ok {
		t.Fatal("ok = false, want the mention accepted")
	}
	if in.Text != "what happened to revenue" {
		t.Errorf("text = %q, want the instruction alone", in.Text)
	}
	if in.Direct {
		t.Error("direct = true, want a room message marked as such")
	}
	if in.ConversationID != "C0ROOM" {
		t.Errorf("conversationId = %q, want the room the answer goes back to", in.ConversationID)
	}
	// No name in the event, so the id stands in — a person revoking a link needs something
	// that tells two Slack accounts apart, and this is the one that costs no API call.
	if in.SenderName != "U042" {
		t.Errorf("senderName = %q, want the user id", in.SenderName)
	}
}

func TestInboundFromSlack_ignoresAMentionInsideADirectMessage(t *testing.T) {
	// Arrange — naming the app in a DM delivers the same message twice, once as each event.
	// Acting on both would run one brief twice and answer it twice, and the message copy is
	// the one that carries the channel type.
	event := slackEvent(&slackevents.AppMentionEvent{
		User:    "U042",
		Text:    "<@U0WINGMAN> what happened to revenue",
		Channel: "D0CONV",
	})

	// Act
	_, ok := inboundFromSlack(event, slackSelf)

	// Assert
	if ok {
		t.Error("ok = true, want the duplicate dropped")
	}
}

func TestInboundFromSlack_ignoresAnEventThereIsNoVerdictFor(t *testing.T) {
	// Arrange — an app subscribed to messages still receives other things, and the SDK adds
	// event types with every Slack release. Anything unrecognised is not somebody typing.
	cases := map[string]any{
		"a reaction":               &slackevents.ReactionAddedEvent{User: "U042", Item: slackevents.Item{Channel: "C0ROOM"}},
		"an app home open":         &slackevents.AppHomeOpenedEvent{User: "U042"},
		"nothing at all":           nil,
		"a value not an sdk event": struct{ Text string }{Text: "what happened to revenue"},
	}

	for name, inner := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			_, ok := inboundFromSlack(slackEvent(inner), slackSelf)

			// Assert
			if ok {
				t.Error("ok = true, want the event ignored")
			}
		})
	}
}

func TestSlackMention_isEmptyWhenThereIsNoIdentityToMatch(t *testing.T) {
	// Arrange — Run fills self in before the first event, but a form of "<@>" would be a
	// string that appears in nothing, and one of "" is what stripMention already refuses.

	// Act & Assert
	if got := slackMention(""); got != "" {
		t.Errorf("got %q, want nothing to match against", got)
	}
	if got := slackMention("U0WINGMAN"); got != "<@U0WINGMAN>" {
		t.Errorf("got %q, want slack's mention form", got)
	}
}

func TestSlackSenderName_usesTheIdWhenTheEventCarriesNoName(t *testing.T) {
	// Arrange — most of Slack's events carry no name at all, and asking costs a call on a
	// rate-limited endpoint per sender for a string written down once at link time.
	cases := map[string]struct {
		username, userID, want string
	}{
		"a name in the event": {"rama", "U042", "rama"},
		"no name":             {"", "U042", "U042"},
		"a blank name":        {"   ", "U042", "U042"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Act & Assert
			if got := slackSenderName(tc.username, tc.userID); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSlackHandle_actsOnlyOnAnEventsApiMessage(t *testing.T) {
	// Arrange
	//
	// The switch in handle is the list of Slack event types that can reach a model at all,
	// and everything not on it must stay off it. A slash command or an interactive component
	// is somebody pressing a button in Slack's own UI: forwarding one would be a run started
	// by a click, with no linked account behind it and nothing that looks like a question.
	//
	// Request is left nil throughout so that nothing acknowledges anything: an Ack writes to
	// the socket, and no test here has one.
	directMessage := socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackEvent(&slackevents.MessageEvent{
			ChannelType: slackevents.ChannelTypeIM,
			User:        "U042",
			Channel:     "D0CONV",
			Text:        "what happened to revenue",
		}),
	}

	cases := map[string]struct {
		event socketmode.Event
		reach bool
	}{
		"a direct message over the events api":     {event: directMessage, reach: true},
		"a socket that has just connected":         {event: socketmode.Event{Type: socketmode.EventTypeConnected}},
		"credentials slack rejected":               {event: socketmode.Event{Type: socketmode.EventTypeInvalidAuth}},
		"a connection error":                       {event: socketmode.Event{Type: socketmode.EventTypeConnectionError}},
		"a message the sdk could not read":         {event: socketmode.Event{Type: socketmode.EventTypeErrorBadMessage}},
		"a slash command":                          {event: socketmode.Event{Type: socketmode.EventTypeSlashCommand}},
		"an interactive component":                 {event: socketmode.Event{Type: socketmode.EventTypeInteractive}},
		"an events api event that is not one":      {event: socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: "a string"}},
		"an events api event with nothing in it":   {event: socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackEvent(nil)}},
		"an event type this version does not know": {event: socketmode.Event{Type: "assistant_thread_started"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler := &fakeHandler{}
			adapter, err := NewSlack(SlackConfig{
				BotToken: slackTestBotToken,
				AppToken: slackTestAppToken,
				Handler:  handler,
				Logger:   zerolog.Nop(),
			})
			if err != nil {
				t.Fatalf("NewSlack: %v", err)
			}
			// As Run would have left it before reading the first event.
			adapter.self = slackSelf

			// Act
			adapter.handle(context.Background(), tc.event)

			// Assert
			if tc.reach != (len(handler.inbounds) == 1) {
				t.Errorf("the handler saw %d messages, want reach=%v", len(handler.inbounds), tc.reach)
			}
		})
	}
}

func TestSlackDirectTarget_isTheUserIdItselfBecausePostMessageAcceptsOne(t *testing.T) {
	// Arrange — Slack's chat.postMessage takes a user id where a channel id goes and opens
	// the direct conversation itself. Calling conversations.open first would be a second
	// request, a second thing that can be rate limited, and a second scope to ask for.
	adapter, err := NewSlack(SlackConfig{
		BotToken: slackTestBotToken,
		AppToken: slackTestAppToken,
		Handler:  &fakeHandler{},
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}

	// Act
	got, err := adapter.DirectTarget(context.Background(), "  U123ABC  ")

	// Assert
	if err != nil {
		t.Fatalf("DirectTarget: %v", err)
	}
	if got != "U123ABC" {
		t.Errorf("conversation = %q, want the user id itself", got)
	}
}

func TestSlackDirectTarget_refusesARecipientThatIsNotThere(t *testing.T) {
	// Arrange — an empty external id is a corrupted link. Sending to it would post into
	// whatever Slack makes of an empty channel argument, which is not a place anybody chose.
	adapter, err := NewSlack(SlackConfig{
		BotToken: slackTestBotToken,
		AppToken: slackTestAppToken,
		Handler:  &fakeHandler{},
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSlack: %v", err)
	}

	// Act
	_, err = adapter.DirectTarget(context.Background(), "   ")

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the recipient refused")
	}
	for _, secret := range []string{slackTestBotToken, slackTestAppToken} {
		if strings.Contains(err.Error(), secret) {
			t.Error("the error carries a slack token")
		}
	}
}
