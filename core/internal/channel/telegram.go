package channel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

const (
	// telegramPollTimeout is how long one getUpdates call is allowed to wait for something
	// to happen, in seconds. Long is the point: a held-open request is one request per
	// half-minute of silence instead of one per second, and Telegram answers it the moment
	// a message arrives, so waiting costs nothing in latency.
	telegramPollTimeout = 30

	// telegramUpdateBuffer is how many updates may queue while a message is being handled.
	// Telegram's own long-polling loop blocks when it is full, which is the behaviour to
	// want: a flood waits rather than being dropped, and it waits on Telegram's side.
	telegramUpdateBuffer = 64
)

// TelegramConfig is what the Telegram adapter needs to exist.
type TelegramConfig struct {
	// Token is CHANNEL_TELEGRAM_TOKEN, from BotFather. It appears in the URL of every
	// request this SDK makes, which is why it is kept here and fed to redact.
	Token   string
	Handler Handler
	Logger  zerolog.Logger
}

// Telegram is core over Telegram's Bot API, by long polling.
//
// Long polling and not a webhook: core needs no public address, no certificate and no
// inbound port, so an instance on a laptop or behind a home router works with nothing
// forwarded. Safe for concurrent use — Send holds no state, and the SDK's client is
// safe for use from several goroutines.
type Telegram struct {
	bot     *telego.Bot
	token   string
	handler Handler
	log     zerolog.Logger

	// self is filled in by Run before the first update is read, and read only by the
	// goroutine that filled it. It is what a group mention is matched against.
	self telegramBot
}

// telegramBot is who this adapter is connected as.
type telegramBot struct {
	id       int64
	username string
}

// NewTelegram validates its configuration and returns an adapter that has not connected
// yet. Nothing here touches the network, so a Telegram outage cannot stop core booting.
func NewTelegram(cfg TelegramConfig) (*Telegram, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("telegram: a bot token is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("telegram: a handler is required")
	}

	bot, err := telego.NewBot(cfg.Token, telego.WithLogger(telegoLogger{log: cfg.Logger, token: cfg.Token}))
	if err != nil {
		// telego rejects a malformed token by shape alone, and says so without quoting it.
		// Redacted anyway: this is an error on a path that has the token in hand.
		return nil, redact(fmt.Errorf("telegram: %w", err), cfg.Token)
	}
	return &Telegram{bot: bot, token: cfg.Token, handler: cfg.Handler, log: cfg.Logger}, nil
}

// Kind is domain.ChannelTelegram.
func (t *Telegram) Kind() domain.ChannelKind { return domain.ChannelTelegram }

// Run connects and reads updates until ctx is cancelled.
func (t *Telegram) Run(ctx context.Context) error {
	// Who this bot is, first, and over the network — for two reasons. It is the only way to
	// learn the username a group message has to mention, and it is the one call that fails
	// loudly on a bad token: the SDK's long polling retries a rejected getUpdates for ever,
	// so without this a wrong token would be indistinguishable from a quiet Telegram.
	me, err := t.bot.GetMe(ctx)
	if err != nil {
		return redact(fmt.Errorf("telegram: identify this bot: %w", err), t.token)
	}
	t.self = telegramBot{id: me.ID, username: me.Username}
	t.log.Info().Str("channel", string(domain.ChannelTelegram)).
		Str("username", me.Username).Msg("connected as bot")

	// Only messages. Every other update type — edits, reactions, chat member changes — is
	// something this side has no verdict for, and asking for it means receiving it.
	updates, err := t.bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{telego.MessageUpdates},
		Timeout:        telegramPollTimeout,
	}, telego.WithLongPollingBuffer(telegramUpdateBuffer))
	if err != nil {
		return redact(fmt.Errorf("telegram: start long polling: %w", err), t.token)
	}

	// One at a time, deliberately. Handling a message files work and returns; doing it
	// sequentially means one sender cannot make this process open a hundred database
	// connections by pasting a hundred messages, and the throttle in the inbox is counted
	// per sender rather than per goroutine.
	for update := range updates {
		in, ok := inboundFromTelegram(update, t.self)
		if !ok {
			continue
		}
		dispatch(ctx, t, t.handler, t.log, in)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("telegram: the update stream ended")
}

// Send delivers one message. Chunking has already happened above it.
func (t *Telegram) Send(ctx context.Context, out Reply) error {
	chatID, err := strconv.ParseInt(strings.TrimSpace(out.ConversationID), 10, 64)
	if err != nil {
		// The value is quoted because a Telegram chat id is a number, so a string that is
		// not one cannot be a real conversation — it is a corrupted row or a caller's bug,
		// and naming it is the only way to find either.
		return fmt.Errorf("telegram: %q is not a chat id", out.ConversationID)
	}

	// No parse mode. A model's answer routinely contains asterisks, underscores and
	// backticks that Telegram's Markdown parser rejects as unbalanced, and a 400 on a
	// correct answer is worse than plain text: every word arrives either way.
	if _, err := t.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   out.Text,
	}); err != nil {
		return redact(fmt.Errorf("telegram: send: %w", err), t.token)
	}
	return nil
}

// DirectTarget is the user id itself, because a Telegram private chat has the same id as
// the person in it. Nothing is opened and no request is made: putting a call here would
// make a notification depend on Telegram answering before it has even been attempted.
//
// The id is still parsed, because a value that is not a number cannot be a Telegram
// anything — a Slack id in a Telegram row is a corrupted link, and naming it here is how it
// gets found rather than surfacing as a 400 about a message.
func (t *Telegram) DirectTarget(_ context.Context, externalUserID string) (string, error) {
	id := strings.TrimSpace(externalUserID)
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", fmt.Errorf("telegram: %q is not a user id", externalUserID)
	}
	return id, nil
}

// Close releases nothing, because long polling holds nothing to release: the SDK's loop
// ends when the context passed to Run is cancelled, and there is no session to tear down.
// Telegram's own close method is for handing a token over to another process, which is not
// what a shutdown here is.
func (t *Telegram) Close() error { return nil }

// inboundFromTelegram normalises one update, or reports that there is nothing to act on.
//
// Pure and separately tested: an update either becomes an Inbound or it does not, and that
// is the whole of this adapter's own logic.
func inboundFromTelegram(update telego.Update, self telegramBot) (Inbound, bool) {
	message := update.Message
	if message == nil {
		// An edit, a channel post, a join. Only messages were asked for, so this is a type
		// Telegram added rather than one to handle.
		return Inbound{}, false
	}
	if message.From == nil {
		// An anonymous admin posting as the group itself. There is no account to link a
		// chat identity to, so there is nothing this side could do with it.
		return Inbound{}, false
	}

	text := message.Text
	if text == "" {
		// A photo or a document with the instruction written underneath it.
		text = message.Caption
	}

	direct := message.Chat.Type == telego.ChatTypePrivate
	if !direct {
		addressed, remaining := addressedTelegram(message, text, self)
		if !addressed {
			// Overheard, not asked. A bot in a group receives every message in it, and
			// forwarding those would mean a room's whole conversation reaching a model.
			return Inbound{}, false
		}
		text = remaining
	}

	return Inbound{
		Kind:           domain.ChannelTelegram,
		SenderID:       strconv.FormatInt(message.From.ID, 10),
		SenderName:     telegramSenderName(message.From),
		ConversationID: strconv.FormatInt(message.Chat.ID, 10),
		Text:           strings.TrimSpace(text),
		Direct:         direct,
		FromBot:        message.From.IsBot,
	}, true
}

// addressedTelegram reports whether a group message was aimed at this bot, and returns the
// message without the mention.
func addressedTelegram(message *telego.Message, text string, self telegramBot) (bool, string) {
	// A reply to something the bot said is as deliberate as naming it, and it is how a
	// conversation in a room continues without every turn repeating the username.
	if reply := message.ReplyToMessage; reply != nil && reply.From != nil && reply.From.ID == self.id {
		return true, text
	}
	if self.username == "" {
		// No username to be mentioned by, which happens with a bot that has not been given
		// one. In a room it can then only be reached by replying to it.
		return false, text
	}
	remaining, addressed := stripMention(text, "@"+self.username)
	return addressed, remaining
}

// telegramSenderName is what a person sees in their own list of connected accounts.
func telegramSenderName(from *telego.User) string {
	name := strings.TrimSpace(strings.TrimSpace(from.FirstName) + " " + strings.TrimSpace(from.LastName))
	if name != "" {
		return name
	}
	// Both names are optional on a bot account and on some privacy settings; the @handle is
	// the next most recognisable thing, and after that there is nothing but the id, which
	// the identity row already carries.
	return from.Username
}

// telegoLogger routes the SDK's own logging into ours, without its token.
//
// The SDK offers a discard logger, which is safe and silent — and its long polling retries
// a rejected getUpdates for ever through this interface, so silence there is an outage
// nobody can see. Debug lines are dropped rather than scrubbed, because the SDK documents
// that they contain the bot token and they are formatted by code on the other side of the
// module boundary; error lines are scrubbed, which is belt and braces on top of that.
type telegoLogger struct {
	log   zerolog.Logger
	token string
}

func (l telegoLogger) Debugf(string, ...any) {}

func (l telegoLogger) Errorf(format string, args ...any) {
	l.log.Warn().Str("channel", string(domain.ChannelTelegram)).
		Msg(scrub(fmt.Sprintf(format, args...), l.token))
}
