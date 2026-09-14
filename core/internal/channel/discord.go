package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// DiscordConfig is what the Discord adapter needs to exist.
type DiscordConfig struct {
	// Token is CHANNEL_DISCORD_TOKEN, the bot token from the developer portal.
	Token   string
	Handler Handler
	Logger  zerolog.Logger
}

// Discord is core over Discord's gateway.
//
// The gateway is a websocket this side opens, so as with the other two platforms there is
// no public address, no certificate and no inbound port. Safe for concurrent use: the SDK's
// session is, and Send holds no state.
type Discord struct {
	session *discordgo.Session
	token   string
	handler Handler
	log     zerolog.Logger

	// self is filled in by Run before the gateway is opened, and read afterwards by the
	// goroutine the SDK delivers messages on.
	self discordBot
}

// discordBot is who this adapter is connected as.
type discordBot struct {
	// id is this bot's own snowflake — what a mention of it is written with, and what
	// identifies its own messages coming back.
	id string
}

// NewDiscord validates its configuration and returns an adapter that has not connected yet.
// Nothing here touches the network, so a Discord outage cannot stop core booting.
func NewDiscord(cfg DiscordConfig) (*Discord, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("discord: a bot token is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("discord: a handler is required")
	}

	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, redact(fmt.Errorf("discord: %w", err), cfg.Token)
	}

	// Three intents and no more. Guild and direct messages are the two places a message can
	// arrive from; message content is the privileged one that has to be enabled in the
	// developer portal as well, and without it every message arrives with an empty body.
	// Nothing asks for presence, membership or reactions: an intent not asked for is data
	// that never reaches this process.
	session.Identify.Intents = discordgo.IntentGuildMessages |
		discordgo.IntentDirectMessages |
		discordgo.IntentMessageContent

	// Handlers on the gateway's own goroutine rather than one goroutine per event, which is
	// this SDK's default. Handling a message files work and returns, and doing that one at a
	// time means a flood waits on Discord's side instead of becoming a hundred concurrent
	// database calls. Heartbeats have their own goroutine, so holding the read loop for the
	// length of one message does not drop the connection.
	session.SyncEvents = true

	return &Discord{session: session, token: cfg.Token, handler: cfg.Handler, log: cfg.Logger}, nil
}

// Kind is domain.ChannelDiscord.
func (d *Discord) Kind() domain.ChannelKind { return domain.ChannelDiscord }

// Run connects and stays connected until ctx is cancelled.
func (d *Discord) Run(ctx context.Context) error {
	// Who this bot is, over the REST API and before the gateway. Its own id is what a
	// mention looks like in a server room, and this is the call that fails loudly on a bad
	// token — a gateway that will not authenticate reconnects quietly instead.
	me, err := d.session.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return redact(fmt.Errorf("discord: identify this bot: %w", err), d.token)
	}
	d.self = discordBot{id: me.ID}

	// Registered after self is known, so no message can be handled before there is something
	// to compare a mention against.
	remove := d.session.AddHandler(func(_ *discordgo.Session, event *discordgo.MessageCreate) {
		in, ok := inboundFromDiscord(event, d.self)
		if !ok {
			return
		}
		dispatch(ctx, d, d.handler, d.log, in)
	})
	defer remove()

	if err := d.session.Open(); err != nil {
		return redact(fmt.Errorf("discord: open the gateway: %w", err), d.token)
	}
	d.log.Info().Str("channel", string(domain.ChannelDiscord)).
		Str("username", me.Username).Msg("connected as bot")

	// The SDK reconnects on its own, so there is nothing to do here but wait: this returns
	// when core is shutting down, and Close is what takes the connection down.
	<-ctx.Done()
	return ctx.Err()
}

// Send delivers one message. Chunking has already happened above it.
func (d *Discord) Send(ctx context.Context, out Reply) error {
	// Nothing in an answer may mention anybody. A model that writes "@everyone" into a
	// summary would otherwise ping a whole server, and one that writes "@someone" would
	// notify a person who never asked for anything: the text still reads as written, it
	// simply does not become a notification.
	_, err := d.session.ChannelMessageSendComplex(out.ConversationID, &discordgo.MessageSend{
		Content:         out.Text,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}, discordgo.WithContext(ctx))
	if err != nil {
		return redact(fmt.Errorf("discord: send: %w", err), d.token)
	}
	return nil
}

// DirectTarget opens the direct-message channel with a person and returns its id.
//
// This is the platform the method on the interface exists for. A user snowflake is not a
// channel: handing one to Send would have Discord read it as a channel id and refuse. The
// call is idempotent — Discord hands back the existing DM channel when there is one — so
// there is nothing to cache here and nothing to clean up afterwards.
func (d *Discord) DirectTarget(ctx context.Context, externalUserID string) (string, error) {
	id := strings.TrimSpace(externalUserID)
	if id == "" {
		// Refused before the network: an empty id asks Discord to open a conversation with
		// nobody, which is a request that can only fail.
		return "", errors.New("discord: a user id is required to open a direct message")
	}

	conversation, err := d.session.UserChannelCreate(id, discordgo.WithContext(ctx))
	if err != nil {
		// A person may have blocked the bot or closed their DMs, which is their answer and
		// not a fault here. The caller decides what a lost message means; this only says so
		// without the token.
		return "", redact(fmt.Errorf("discord: open a direct message: %w", err), d.token)
	}
	return conversation.ID, nil
}

// Close takes the gateway down. Unlike the other two, this one has a websocket of its own
// to close, and closing it is what tells Discord the bot went offline deliberately.
func (d *Discord) Close() error {
	if err := d.session.Close(); err != nil {
		return redact(fmt.Errorf("discord: close the gateway: %w", err), d.token)
	}
	return nil
}

// inboundFromDiscord normalises one message, or reports that there is nothing to act on.
//
// Pure and separately tested: an event either becomes an Inbound or it does not, and that is
// the whole of this adapter's own logic.
func inboundFromDiscord(event *discordgo.MessageCreate, self discordBot) (Inbound, bool) {
	if event == nil || event.Message == nil {
		return Inbound{}, false
	}
	message := event.Message
	if message.Author == nil {
		// A webhook post or a system notice. There is no account to link a chat identity
		// to, so there is nothing this side could do with it.
		return Inbound{}, false
	}
	switch message.Type {
	case discordgo.MessageTypeDefault, discordgo.MessageTypeReply:
		// Somebody typing, and somebody typing in a thread. Every other type is a join
		// notice, a pin, a boost or a call — a message Discord wrote, not a person.
	default:
		return Inbound{}, false
	}

	// A guild is a server. No guild is a direct message: Discord gives a DM its own channel
	// with two people in it, and the channel id is what an answer goes back to.
	direct := message.GuildID == ""

	text := message.Content
	if !direct {
		addressed, remaining := addressedDiscord(message, text, self)
		if !addressed {
			// Overheard, not asked. A bot in a server room receives every message in it, and
			// forwarding those would mean a room's whole conversation reaching a model.
			return Inbound{}, false
		}
		text = remaining
	}

	return Inbound{
		Kind:           domain.ChannelDiscord,
		SenderID:       message.Author.ID,
		SenderName:     message.Author.DisplayName(),
		ConversationID: message.ChannelID,
		Text:           strings.TrimSpace(text),
		Direct:         direct,
		FromBot:        message.Author.Bot,
	}, true
}

// addressedDiscord reports whether a message in a server room was aimed at this bot, and
// returns the message without the mention.
func addressedDiscord(message *discordgo.Message, text string, self discordBot) (bool, string) {
	if self.id == "" {
		return false, text
	}

	// An @everyone or a role ping is not being spoken to. It reaches everybody in the room by
	// design, and treating it as an instruction would mean one person's announcement starting
	// a run on somebody else's budget.
	named := false
	for _, mentioned := range message.Mentions {
		if mentioned != nil && mentioned.ID == self.id {
			named = true
			break
		}
	}
	if !named {
		return false, text
	}

	// Two forms, because a mention is written with a bang by older clients and without by
	// newer ones, and the same message may contain either. Whether it was found is already
	// known from the mention list above — this is only about taking it out of the text.
	remaining, _ := stripMention(text, "<@"+self.id+">", "<@!"+self.id+">")
	return true, remaining
}
