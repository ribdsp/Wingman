package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// slackDirectPrefix is what Slack starts the id of a one-to-one conversation with. Public
// channels get C, private ones G, and a direct message D — which is how a mention in a DM
// is told apart from a mention in a room without a second API call to ask.
const slackDirectPrefix = "D"

// SlackConfig is what the Slack adapter needs to exist.
type SlackConfig struct {
	// BotToken is CHANNEL_SLACK_BOT_TOKEN, the xoxb- token used for the Web API — reading
	// who this app is, and sending messages.
	BotToken string
	// AppToken is CHANNEL_SLACK_APP_TOKEN, the xapp- token that opens the socket. Two
	// separate credentials because Slack issues them separately, and Socket Mode needs
	// both.
	AppToken string
	Handler  Handler
	Logger   zerolog.Logger
}

// Slack is core over Slack, in Socket Mode.
//
// Socket Mode and not the Events API over HTTP: the connection is outbound, so core needs
// no public URL, no TLS certificate and no route that verifies Slack's request signature.
// Safe for concurrent use — the SDK's client is, and Send holds no state.
type Slack struct {
	api     *slack.Client
	socket  *socketmode.Client
	handler Handler
	log     zerolog.Logger

	botToken string
	appToken string

	// self is filled in by Run before the first event is read, and read only by the
	// goroutine that filled it.
	self slackBot
}

// slackBot is who this adapter is connected as.
type slackBot struct {
	// userID is this app's own user id — the U… that a mention of it is written with, and
	// the one to recognise when Slack echoes something this app itself posted.
	userID string
	botID  string
}

// NewSlack validates its configuration and returns an adapter that has not connected yet.
// Nothing here touches the network, so a Slack outage cannot stop core booting.
func NewSlack(cfg SlackConfig) (*Slack, error) {
	missing := []string{}
	if strings.TrimSpace(cfg.BotToken) == "" {
		missing = append(missing, "a bot token")
	}
	if strings.TrimSpace(cfg.AppToken) == "" {
		missing = append(missing, "an app token")
	}
	if cfg.Handler == nil {
		missing = append(missing, "a handler")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("slack: %s required", strings.Join(missing, ", "))
	}

	api := slack.New(cfg.BotToken, slack.OptionAppLevelToken(cfg.AppToken))
	return &Slack{
		api:      api,
		socket:   socketmode.New(api),
		handler:  cfg.Handler,
		log:      cfg.Logger,
		botToken: cfg.BotToken,
		appToken: cfg.AppToken,
	}, nil
}

// Kind is domain.ChannelSlack.
func (s *Slack) Kind() domain.ChannelKind { return domain.ChannelSlack }

// Run connects and reads events until ctx is cancelled.
func (s *Slack) Run(ctx context.Context) error {
	// Who this app is, first. Its user id is what a mention of it looks like in a room, and
	// what identifies its own messages coming back — and asking is the call that fails
	// loudly on a bad bot token, rather than leaving a socket that connects and never has
	// anything to say.
	auth, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return redact(fmt.Errorf("slack: identify this app: %w", err), s.botToken, s.appToken)
	}
	s.self = slackBot{userID: auth.UserID, botID: auth.BotID}
	s.log.Info().Str("channel", string(domain.ChannelSlack)).
		Str("team", auth.Team).Str("userId", auth.UserID).Msg("connected as app")

	// The SDK's own loop owns the socket and reconnects on its own; this side reads what it
	// publishes. Both have to be watched, because a socket that has given up entirely shows
	// up as RunContext returning rather than as an event.
	stopped := make(chan error, 1)
	go func() { stopped <- s.socket.RunContext(ctx) }()

	for {
		select {
		case err := <-stopped:
			switch {
			case err != nil && !errors.Is(err, context.Canceled):
				return redact(fmt.Errorf("slack: socket mode: %w", err), s.botToken, s.appToken)
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				return errors.New("slack: the socket connection ended")
			}
		case event := <-s.socket.Events:
			s.handle(ctx, event)
		}
	}
}

// handle deals with one event from the socket.
func (s *Slack) handle(ctx context.Context, event socketmode.Event) {
	kind := string(domain.ChannelSlack)

	switch event.Type {
	case socketmode.EventTypeEventsAPI:
		// Acknowledged first, and whatever happens next. Slack redelivers anything it has
		// not heard back about within three seconds, so a message that takes a moment to
		// file would otherwise arrive again and be answered twice.
		if event.Request != nil {
			if err := s.socket.Ack(*event.Request); err != nil {
				s.log.Warn().Err(err).Str("channel", kind).
					Msg("an event could not be acknowledged; slack may send it again")
			}
		}
		payload, ok := event.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		in, ok := inboundFromSlack(payload, s.self)
		if !ok {
			return
		}
		dispatch(ctx, s, s.handler, s.log, in)

	case socketmode.EventTypeConnected:
		s.log.Info().Str("channel", kind).Msg("socket connected")

	case socketmode.EventTypeInvalidAuth:
		// Worth its own line at error level: everything else here is transient and this one
		// never is. Nothing will arrive until an operator fixes the token.
		s.log.Error().Str("channel", kind).
			Msg("slack rejected the credentials; no messages will be received")

	case socketmode.EventTypeConnectionError, socketmode.EventTypeIncomingError,
		socketmode.EventTypeErrorWriteFailed, socketmode.EventTypeErrorBadMessage:
		s.log.Warn().Str("channel", kind).Str("event", string(event.Type)).
			Msg("socket trouble; the sdk will reconnect")
	}
}

// Send delivers one message. Chunking has already happened above it.
func (s *Slack) Send(ctx context.Context, out Reply) error {
	// Escaped, not trusted. Slack reads &, < and > in message text as markup, so an answer
	// containing them would render as something other than what the model wrote — and a
	// <http://…|label> assembled out of model output is a link a person did not ask for.
	if _, _, err := s.api.PostMessageContext(ctx, out.ConversationID,
		slack.MsgOptionText(out.Text, true),
	); err != nil {
		return redact(fmt.Errorf("slack: send: %w", err), s.botToken, s.appToken)
	}
	return nil
}

// DirectTarget is the user id itself: chat.postMessage takes one where a channel id goes
// and opens the direct conversation on Slack's side. Calling conversations.open first would
// be a second request, a second thing that can be rate limited and a second scope to ask an
// operator to grant.
func (s *Slack) DirectTarget(_ context.Context, externalUserID string) (string, error) {
	id := strings.TrimSpace(externalUserID)
	if id == "" {
		// A corrupted link rather than a conversation that has gone away. An empty channel
		// argument is not a place anybody chose to be messaged.
		return "", errors.New("slack: a user id is required to open a direct message")
	}
	return id, nil
}

// Close releases nothing of its own: the socket belongs to the SDK's loop, which ends when
// the context passed to Run is cancelled. It exists because the interface has it, and
// because two of the three platforms here do need an explicit disconnect.
func (s *Slack) Close() error { return nil }

// inboundFromSlack normalises one Events API payload, or reports that there is nothing to
// act on.
//
// Pure and separately tested. Two event types matter and they divide the work between them:
// a message event is how a direct message arrives, and an app mention is how being spoken
// to in a room arrives. Anything else — a reaction, a file share, a channel join, an
// assistant thread — is something this side has no verdict for.
func inboundFromSlack(event slackevents.EventsAPIEvent, self slackBot) (Inbound, bool) {
	switch inner := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		// Direct messages only. A message in a room arrives here too, for every room this
		// app is in, and forwarding those would mean a channel's whole conversation
		// reaching a model; being spoken to in a room comes back as an app mention instead.
		if inner.ChannelType != slackevents.ChannelTypeIM {
			return Inbound{}, false
		}
		if inner.SubType != "" {
			// An edit, a deletion, a thread broadcast, a file comment. Each would need its
			// own reading of what the text now means, and none of them is somebody typing.
			return Inbound{}, false
		}
		text, _ := stripMention(inner.Text, slackMention(self.userID))
		return Inbound{
			Kind:           domain.ChannelSlack,
			SenderID:       inner.User,
			SenderName:     slackSenderName(inner.Username, inner.User),
			ConversationID: inner.Channel,
			Text:           strings.TrimSpace(text),
			Direct:         true,
			FromBot:        inner.BotID != "" || inner.User == self.userID,
		}, true

	case *slackevents.AppMentionEvent:
		// A mention inside a direct message arrives twice — once as a message and once as
		// this. The message is the copy that carries the channel type, so this one goes.
		if strings.HasPrefix(inner.Channel, slackDirectPrefix) {
			return Inbound{}, false
		}
		text, _ := stripMention(inner.Text, slackMention(self.userID))
		return Inbound{
			Kind:           domain.ChannelSlack,
			SenderID:       inner.User,
			SenderName:     slackSenderName("", inner.User),
			ConversationID: inner.Channel,
			Text:           strings.TrimSpace(text),
			Direct:         false,
			FromBot:        inner.BotID != "" || inner.User == self.userID,
		}, true

	default:
		return Inbound{}, false
	}
}

// slackMention is how a mention of a user id is written in Slack message text.
func slackMention(userID string) string {
	if userID == "" {
		return ""
	}
	return "<@" + userID + ">"
}

// slackSenderName is what a person sees in their own list of connected accounts.
//
// Slack's events carry a user id and, for most of them, no name at all: a real name needs a
// second API call per sender, on a rate-limited endpoint, for a string that is written down
// once when the account is linked. The id is what goes in instead — it is stable, it is
// what Slack's own admin pages show, and it is enough to tell two linked Slack accounts
// apart. The username is used when the event happens to carry one.
func slackSenderName(username, userID string) string {
	if name := strings.TrimSpace(username); name != "" {
		return name
	}
	return userID
}
