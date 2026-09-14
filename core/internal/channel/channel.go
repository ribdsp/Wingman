// Package channel is core's edge on the chat platforms a person already uses.
//
// Everything here is transport. What may happen to an inbound message is decided by
// domain.ClassifyInbound and carried out by service.Inbox; an adapter's whole job is to
// turn one platform's update into an Inbound, hand it to a Handler, and send back
// whatever text comes out.
//
// All three adapters connect outbound — Telegram by long polling, Slack over Socket
// Mode, Discord over its gateway. That is a deliberate choice over webhooks: core needs
// no public address, no TLS certificate and no route that verifies a third party's
// signature, so an instance behind a home router works with nothing forwarded and
// nothing exposed.
package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// ErrIncomplete is returned for a message that no verdict could be reached about. It is
// an adapter bug, not something a sender did.
var ErrIncomplete = errors.New("inbound message is incomplete")

// Inbound is one message, as the adapter for its platform normalised it.
//
// Nothing platform-shaped survives this struct: no update id, no thread, no
// attachments, no raw payload. A fact that a decision needs belongs here and in
// domain.InboundFacts; a fact no decision needs only widens what the rest of core would
// have to understand about three separate APIs.
type Inbound struct {
	Kind domain.ChannelKind
	// SenderID is the platform's own id for the author, and the value a link is
	// recorded against. It has to be the stable one — a Telegram user id, a Slack
	// user id, a Discord snowflake — and never a display name, because people change
	// those and a link that followed a name would follow it to somebody else.
	SenderID string
	// SenderName is shown to a person reading their own connected accounts, so they
	// recognise which one to revoke. Nothing is ever looked up by it.
	SenderName string
	// ConversationID is where an answer goes: the chat, channel or room the message
	// arrived in, which in a group is not the sender.
	ConversationID string
	Text           string
	// Direct is false in a group, a channel or a server room. An adapter forwards a
	// group message only when the bot was addressed, so false here means somebody
	// called it deliberately in a place other people can read.
	Direct bool
	// FromBot is what the platform says about the author being an application —
	// including this one, seeing its own message echoed back.
	FromBot bool
}

// Validate refuses an Inbound that cannot be acted on at all.
//
// A message with no author cannot be attached to an account, and one with no
// conversation has nowhere to be answered. Both are refused here rather than failing
// deeper in, where the error would name a column instead of the adapter.
func (in Inbound) Validate() error {
	missing := []string{}
	if !in.Kind.Valid() {
		missing = append(missing, "kind")
	}
	if strings.TrimSpace(in.SenderID) == "" {
		missing = append(missing, "senderId")
	}
	if strings.TrimSpace(in.ConversationID) == "" {
		missing = append(missing, "conversationId")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", ErrIncomplete, missing)
	}
	return nil
}

// Handled is what the adapter does next.
//
// Reply is the whole instruction: send it if it is there, send nothing if it is not.
// The verdict comes along for the adapter's log and for platform touches that are not
// text — a typing indicator on an accepted message, say — and never to be re-decided.
type Handled struct {
	Verdict domain.InboundVerdict
	Reply   string
}

// Handler decides what happens to an inbound message. service.Inbox implements it.
//
// The interface is declared here, next to Inbound, so that an adapter depends on the
// decision and not on the package that makes it. An error means the message could not
// be acted on at all — Validate's answer, an adapter bug. Everything else, up to and
// including a database being down mid-link, comes back as a Handled with something to
// say, because there is a person waiting on the other end and an adapter has nothing
// better to do with an error than log it.
type Handler interface {
	Handle(ctx context.Context, in Inbound) (Handled, error)
}

// Reply is one outbound message. Splitting it to fit the platform's size limit is the
// adapter's job, because the limit is the platform's.
type Reply struct {
	ConversationID string
	Text           string
}

// Channel is one platform's connection, from core's side.
//
// Run blocks until the context is cancelled or the connection fails, so it is what
// cmd/core puts in a goroutine. Send is called from a different goroutine — the agent
// runner's — which every implementation here has to be safe for. Close releases the
// connection; it is separate from cancelling Run because two of the three SDKs want an
// explicit disconnect.
type Channel interface {
	Kind() domain.ChannelKind
	Run(ctx context.Context) error
	Send(ctx context.Context, out Reply) error
	// DirectTarget turns a person's own id on this platform into a conversation a message
	// can be sent to, opening one if the platform needs that.
	//
	// It exists because a link records only who somebody is (repository.ChannelIdentity's
	// ExternalID) and on Discord that is not somewhere a message goes. Two of the three
	// adapters answer without a request; the third has to make one. Keeping the difference
	// behind this method means nothing above the hub has to know which is which.
	DirectTarget(ctx context.Context, externalUserID string) (conversationID string, err error)
	Close() error
}
