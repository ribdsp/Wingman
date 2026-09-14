package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// ErrNotConnected is returned when there is nothing to deliver on a platform.
//
// It is an error rather than a silent drop, because it means somebody is linked to a
// platform this instance is no longer configured for: an operator removed a token and the
// people who linked to it are still sending messages into it.
var ErrNotConnected = errors.New("channel is not connected")

// Hub is every platform this instance is connected to, and the one thing that knows which.
//
// It exists so nothing above it has to hold three adapters or know how many there are:
// cmd builds whichever are configured, hands them here, and the rest of core asks for a
// platform by kind. Safe for concurrent use — the map is written once at construction and
// only read afterwards, and every adapter's Send is safe for concurrent use in its own
// right.
type Hub struct {
	channels map[domain.ChannelKind]Channel
	log      zerolog.Logger
}

// NewHub takes whichever channels an operator configured. None is a valid answer: an
// instance reached only over HTTP is an ordinary way to run this.
func NewHub(log zerolog.Logger, channels ...Channel) (*Hub, error) {
	h := &Hub{channels: make(map[domain.ChannelKind]Channel, len(channels)), log: log}
	for _, c := range channels {
		if c == nil {
			continue
		}
		kind := c.Kind()
		if !kind.Valid() {
			return nil, fmt.Errorf("hub: %q is not a channel this build knows", kind)
		}
		if _, taken := h.channels[kind]; taken {
			// Two adapters for one platform means two connections consuming the same
			// updates, and a message answered twice.
			return nil, fmt.Errorf("hub: %s was given twice", kind)
		}
		h.channels[kind] = c
	}
	return h, nil
}

// Kinds is what is connected, in a stable order so a reference endpoint and a log line
// agree with each other between restarts.
func (h *Hub) Kinds() []domain.ChannelKind {
	out := []domain.ChannelKind{}
	for _, kind := range domain.AllChannelKinds() {
		if _, ok := h.channels[kind]; ok {
			out = append(out, kind)
		}
	}
	return out
}

// Send delivers text to one conversation, split to fit the platform.
func (h *Hub) Send(ctx context.Context, kind domain.ChannelKind, conversationID, text string) error {
	c, ok := h.channels[kind]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotConnected, kind)
	}
	if strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("send on %s: a conversation is required", kind)
	}
	return deliver(ctx, c, conversationID, text)
}

// SendDirect delivers text to one person, in whatever a direct conversation with them is on
// their platform.
//
// Two steps rather than one because a link records only who somebody is, and on Discord that
// id is not a place a message can go. Asking the adapter first keeps that difference inside
// the three adapters instead of inside whoever has something to tell somebody.
func (h *Hub) SendDirect(ctx context.Context, kind domain.ChannelKind, externalUserID, text string) error {
	c, ok := h.channels[kind]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotConnected, kind)
	}
	if strings.TrimSpace(externalUserID) == "" {
		return fmt.Errorf("send on %s: a recipient is required", kind)
	}
	// Checked before the lookup, not after. On Discord the lookup opens a conversation, and
	// opening one to then say nothing puts an empty DM channel in somebody's client.
	if strings.TrimSpace(text) == "" {
		return nil
	}

	conversationID, err := c.DirectTarget(ctx, externalUserID)
	if err != nil {
		return err
	}
	return deliver(ctx, c, conversationID, text)
}

// Run starts every connected channel and blocks until ctx is cancelled.
//
// A channel that stops on its own is logged and not returned, and the others keep running.
// That is deliberate: a Telegram outage must not take Slack down with it, and a bad token
// on one platform must not stop an instance whose other platforms work. All three SDKs
// reconnect on their own, so a Run that returns has failed in a way retrying inside it
// would not fix.
func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for kind, c := range h.channels {
		wg.Add(1)
		go func(kind domain.ChannelKind, c Channel) {
			defer wg.Done()
			h.log.Info().Str("channel", string(kind)).Msg("channel connected")
			if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				h.log.Error().Err(err).Str("channel", string(kind)).
					Msg("channel stopped; messages on it will not be received until restart")
				return
			}
			h.log.Info().Str("channel", string(kind)).Msg("channel stopped")
		}(kind, c)
	}
	wg.Wait()
}

// Close releases every connection, reporting all the failures rather than the first.
func (h *Hub) Close() error {
	failures := []string{}
	for kind, c := range h.channels {
		if err := c.Close(); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", kind, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("close channels: %s", strings.Join(failures, "; "))
	}
	return nil
}

// Target is where an answer to a chat goes, or the zero value when the chat is not a
// channel conversation at all.
type Target struct {
	Kind           domain.ChannelKind
	ConversationID string
}

// Deliverable reports whether there is a channel to answer on.
func (t Target) Deliverable() bool {
	return t.Kind != "" && t.ConversationID != ""
}

// Targets says where a chat's answers are delivered.
//
// Declared here, with a type of this package's own, so that transport does not learn the
// repository's types: cmd adapts the one to the other in a few lines. The user id is a
// parameter because it is in the query's WHERE clause — a chat id on its own must not
// reveal which Telegram conversation it belongs to.
type Targets interface {
	TargetFor(ctx context.Context, userID, chatID string) (Target, error)
}

// Replier is service.ChannelReplier: it turns "answer in this chat" into a delivery.
//
// The runner never learns what a channel is. It knows the chat it answered in, and this
// looks up where that chat lives — which for most chats is nowhere, because most chats are
// somebody using the web client.
type Replier struct {
	targets Targets
	hub     *Hub
	log     zerolog.Logger
}

// NewReplier validates its wiring and returns a ready replier.
func NewReplier(targets Targets, hub *Hub, log zerolog.Logger) (*Replier, error) {
	if targets == nil || hub == nil {
		return nil, errors.New("replier: a target lookup and a hub are both required")
	}
	return &Replier{targets: targets, hub: hub, log: log}, nil
}

// Reply delivers a run's answer to the channel the chat belongs to.
//
// Nothing to say and nowhere to say it are both ordinary and neither is an error: the
// runner calls this for every chat it answers in, and most of them are web chats.
func (r *Replier) Reply(ctx context.Context, userID, chatID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	target, err := r.targets.TargetFor(ctx, userID, chatID)
	if err != nil {
		return fmt.Errorf("find where to answer chat %s: %w", chatID, err)
	}
	if !target.Deliverable() {
		return nil
	}

	if err := r.hub.Send(ctx, target.Kind, target.ConversationID, text); err != nil {
		return err
	}
	// The chat id, the account and the platform. Not the conversation and not a word of the
	// answer: this is a delivery record, and the answer is in the chat.
	r.log.Info().Str("userId", userID).Str("chatId", chatID).
		Str("channel", string(target.Kind)).Msg("run answer delivered to a channel")
	return nil
}
