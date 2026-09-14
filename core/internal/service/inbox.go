package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/channel"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// maxTrackedSenders bounds the throttle's memory.
//
// The map is keyed by a value strangers control, so it needs a ceiling: anybody who can
// find the bot can add a key by sending it one message. When it is reached the whole map
// is dropped rather than pruned, following the session touch tracker in cmd/core — the
// cost of forgetting is one extra accepted message per sender, and the cost of not
// forgetting is a process somebody has to restart.
const maxTrackedSenders = 4096

// What is said back, in one place.
//
// Every refusal answers with something, because a self-hosted assistant that goes quiet
// is indistinguishable from one that is broken and the person on the other end has no
// logs to read. None of them names an account, a database, a user id or which of several
// reasons applied — a sender who can tell "expired" from "never existed" from "somebody
// else has it" is a sender who has been handed the difference.
const (
	replyGroupRefused = "I only work in direct messages on this instance. Message me directly and I will pick it up there."

	replyLinkRequired = "This chat is not connected to a Wingman account. Sign in to Wingman, ask for a link code, and send the code here on its own."

	replyCodeRefused = "That code will not work. A code can be used once and expires a few minutes after it is made, so ask for a fresh one and send it here."

	replyLinked = "Connected. This chat is on your Wingman account now — tell me what you need."

	replyRelinked = "Reconnected. This chat is on your Wingman account again."

	replyLinkHeld = "This chat account was connected to a different Wingman account and released. An operator has to clear it before it can be connected here; you will need a fresh code once they have."

	replyTooLongFormat = "That is longer than I can take in one message (%d characters). Send a shorter instruction, or split it up."

	replyHalted = "Wingman is halted right now, so I am not starting anything. An operator has to release the kill switch."

	replyUnavailable = "Something went wrong on my end and I could not act on that. Try again in a moment."
)

// InboxDeps is what the inbound path needs.
type InboxDeps struct {
	Identities ChannelResolver
	Linker     ChannelLinker
	Codes      LinkCodeRedeemer
	Work       ChannelWork
	// Halt is the goal engine's kill switch, read through the same port the agent loop
	// uses. Required, not optional: an instance with no engine to ask is an instance
	// whose brakes cannot be read, and the rule for that is halt.
	Halt agent.Halt

	// AllowGroups is CHANNEL_ALLOW_GROUPS, off by default. In a shared room the linked
	// person's token budget is spendable by anybody who can type there.
	AllowGroups bool
	// MinInterval is CHANNEL_MIN_INTERVAL, floored by domain.MinInboundInterval.
	MinInterval time.Duration

	Clock  Clock
	Logger zerolog.Logger
}

// Inbox is what happens to a message that arrived on a chat platform.
//
// It takes no Actor, and it is the only service here that does not. Every other entry
// point is reached by a caller who presented a credential; this one is reached by
// whoever found the bot, and the only authorisation in it is a channel identity that a
// signed-in person created themselves by sending a code. That is why the ports it holds
// are the narrow ones — a resolver that cannot list or revoke, a redeemer that cannot
// mint, a queue that cannot dispatch.
//
// Every decision is domain.ClassifyInbound's. This type establishes facts in the order
// that ladder consults them and re-runs it as each becomes known, which is what keeps
// the expensive facts — who the sender is, whether the brakes are on — from being
// established for messages that were never going to get that far. It decides nothing
// itself.
//
// Safe for concurrent use: three adapters call Handle from their own goroutines.
type Inbox struct {
	identities ChannelResolver
	linker     ChannelLinker
	codes      LinkCodeRedeemer
	work       ChannelWork
	halt       agent.Halt

	allowGroups bool
	minInterval time.Duration

	mu       sync.Mutex
	accepted map[string]time.Time

	clock Clock
	log   zerolog.Logger
}

// NewInbox validates its wiring and returns a ready service.
func NewInbox(deps InboxDeps) (*Inbox, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Identities != nil, "Identities")
	require(deps.Linker != nil, "Linker")
	require(deps.Codes != nil, "Codes")
	require(deps.Work != nil, "Work")
	require(deps.Halt != nil, "Halt")
	if len(missing) > 0 {
		return nil, fmt.Errorf("inbox: missing dependencies: %v", missing)
	}

	i := &Inbox{
		identities:  deps.Identities,
		linker:      deps.Linker,
		codes:       deps.Codes,
		work:        deps.Work,
		halt:        deps.Halt,
		allowGroups: deps.AllowGroups,
		minInterval: deps.MinInterval,
		accepted:    make(map[string]time.Time),
		clock:       deps.Clock,
		log:         deps.Logger,
	}
	if i.clock == nil {
		i.clock = time.Now
	}
	return i, nil
}

// Handle decides what happens to one inbound message and carries it out.
//
// Three passes over the same ladder, each adding the facts the previous verdict made
// worth establishing. The order is not an optimisation: rung 1 is loop prevention and
// rung 3 is about the room, and both have to be able to refuse a message before anything
// on this side looks the sender up or asks the goal engine anything at all.
func (i *Inbox) Handle(ctx context.Context, in channel.Inbound) (channel.Handled, error) {
	// The only error this method returns. An adapter that produced a message with no
	// author or nowhere to answer has a bug, and no verdict about it would mean anything.
	if err := in.Validate(); err != nil {
		return channel.Handled{}, err
	}

	now := i.clock()
	facts := domain.InboundFacts{
		Kind:        in.Kind,
		FromBot:     in.FromBot,
		Text:        in.Text,
		Direct:      in.Direct,
		AllowGroups: i.allowGroups,
		MinInterval: i.minInterval,
	}

	// Pass one: what the message itself says. Linked is false and the switch is unread,
	// so anything settled here was settled without a query.
	decision := domain.ClassifyInbound(facts, now)

	// Pass two: who sent it.
	var identity repository.ChannelIdentity
	if identityCouldChange(decision.Verdict) {
		resolved, err := i.identities.Resolve(ctx, in.Kind, in.SenderID)
		switch {
		case err == nil:
			identity = resolved
			facts.Linked = resolved.Live()
			facts.LastAccepted = i.lastAccepted(senderKey(in))
		case errors.Is(err, repository.ErrNotFound):
			// A stranger, which is the ordinary case and not a fault.
		default:
			// Not run through the ladder as "not linked": that would tell somebody who
			// *is* linked to go and get a code, and sending them to mint one during an
			// outage is sending them somewhere else that cannot answer either.
			return i.broke(in, decision.Verdict, "resolve the sender", err), nil
		}
		decision = domain.ClassifyInbound(facts, now)
	}

	// Pass three: the brakes. Only an accept can become a halt — rung 7 is the last one
	// — so only an accept is worth a request to the goal engine.
	if decision.Verdict == domain.InboundAccept {
		engaged, err := i.halt.Engaged(ctx)
		if err != nil {
			// An error is not a false. Unreadable means engaged, the same rule the run
			// ladder and the goal engine both apply, and it goes back through the ladder
			// as that fact rather than becoming a second decision made here.
			i.log.Warn().Err(err).Msg("the kill switch could not be read; treating it as engaged")
			engaged = true
		}
		facts.KillSwitchEngaged = engaged
		decision = domain.ClassifyInbound(facts, now)
	}

	return i.act(ctx, in, identity, decision), nil
}

// act carries out a verdict. It is the only place a verdict turns into a side effect, and
// it never re-decides one.
func (i *Inbox) act(ctx context.Context, in channel.Inbound, identity repository.ChannelIdentity, decision domain.InboundDecision) channel.Handled {
	if decision.Verdict.Silent() {
		i.log.Debug().Str("channel", string(in.Kind)).Str("verdict", string(decision.Verdict)).
			Dur("retryAfter", decision.RetryAfter).Msg("inbound message dropped without a reply")
		return channel.Handled{Verdict: decision.Verdict}
	}

	switch decision.Verdict {
	case domain.InboundGroupRefused:
		return handled(decision.Verdict, replyGroupRefused)
	case domain.InboundLinkRequired:
		return handled(decision.Verdict, replyLinkRequired)
	case domain.InboundLinkAttempt:
		return i.link(ctx, in, decision)
	case domain.InboundTooLong:
		return handled(decision.Verdict, fmt.Sprintf(replyTooLongFormat, domain.MaxBriefLength))
	case domain.InboundHalted:
		return handled(decision.Verdict, replyHalted)
	case domain.InboundAccept:
		return i.accept(ctx, in, identity.UserID, decision)
	default:
		// A verdict this switch does not know is a rung somebody added without deciding
		// what it does. Saying nothing useful is the safe half of that mistake; queueing
		// work would be the other one.
		i.log.Error().Str("verdict", string(decision.Verdict)).
			Msg("no handling for this inbound verdict; nothing was queued")
		return handled(decision.Verdict, replyUnavailable)
	}
}

// link redeems a code and attaches the chat account that sent it.
func (i *Inbox) link(ctx context.Context, in channel.Inbound, decision domain.InboundDecision) channel.Handled {
	at := decision.DecidedAt

	// The hash, never the code. The plaintext exists in this process for the length of
	// this call and is not logged, returned or stored anywhere by it.
	userID, err := i.codes.Consume(ctx, auth.HashToken(decision.LinkCode), at)
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrNotFound):
		// Unknown, expired and already spent are one answer, decided in the repository.
		i.log.Info().Str("channel", string(in.Kind)).Msg("a link code was refused")
		return handled(decision.Verdict, replyCodeRefused)
	default:
		return i.broke(in, decision.Verdict, "redeem a link code", err)
	}

	identity, err := i.linker.Link(ctx, repository.ChannelIdentity{
		UserID:      userID,
		Kind:        in.Kind,
		ExternalID:  in.SenderID,
		DisplayName: in.SenderName,
	})
	switch {
	case err == nil:
		i.log.Info().Str("userId", userID).Str("identityId", identity.ID).
			Str("channel", string(in.Kind)).Msg("channel identity linked")
		return handled(decision.Verdict, replyLinked)
	case errors.Is(err, repository.ErrConflict):
		// A row for this chat account exists, and it cannot be a live one: this rung is
		// only reached when the resolver found nothing live. So it is a revoked link,
		// and somebody is picking it up again.
		return i.relink(ctx, in, userID, decision)
	default:
		return i.broke(in, decision.Verdict, "link the sender", err)
	}
}

// relink claims a revoked identity, but only for the account that revoked it.
func (i *Inbox) relink(ctx context.Context, in channel.Inbound, userID string, decision domain.InboundDecision) channel.Handled {
	_, err := i.linker.Relink(ctx, userID, in.Kind, in.SenderID, decision.DecidedAt)
	switch {
	case err == nil:
		i.log.Info().Str("userId", userID).Str("channel", string(in.Kind)).
			Msg("channel identity relinked")
		return handled(decision.Verdict, replyRelinked)
	case errors.Is(err, repository.ErrNotFound):
		// Relink is scoped to the account that just redeemed the code, so nothing found
		// means the revoked row belongs to somebody else. The reply does not say whose:
		// which Wingman account a chat account was once attached to is not this sender's
		// business, and this sender may be the one trying to find out. The code is spent
		// either way, which is what single use means.
		i.log.Warn().Str("userId", userID).Str("channel", string(in.Kind)).
			Msg("a link code was redeemed for a chat account another account holds")
		return handled(decision.Verdict, replyLinkHeld)
	default:
		return i.broke(in, decision.Verdict, "relink the sender", err)
	}
}

// accept files the work and says nothing.
//
// Nothing, because the answer is the run's: it arrives in this conversation when the run
// finishes, delivered through ChannelReplier. An acknowledgement here would double the
// traffic on every message to say something the next message already says.
func (i *Inbox) accept(ctx context.Context, in channel.Inbound, userID string, decision domain.InboundDecision) channel.Handled {
	// Recorded before the work is filed rather than after. The throttle bounds how often
	// a stranger can make this side do expensive things, and an attempt that failed
	// halfway through cost that anyway.
	i.recordAccepted(senderKey(in), decision.DecidedAt)

	sent, err := i.work.FromChannel(ctx, ChannelMessage{
		UserID:         userID,
		Kind:           in.Kind,
		ConversationID: in.ConversationID,
		Text:           in.Text,
	})
	if err != nil {
		return i.broke(in, decision.Verdict, "queue the message", err)
	}

	i.log.Info().Str("taskId", sent.Task.ID).Str("chatId", sent.Chat.ID).
		Str("userId", userID).Str("channel", string(in.Kind)).
		Msg("channel message accepted")
	return channel.Handled{Verdict: decision.Verdict}
}

// broke turns a dependency failure into something to say.
//
// Logged here and not returned, because an adapter has nothing to do with an error
// except log it, and this is closer to the cause. What goes back names nothing: a person
// on Telegram cannot act on a constraint name, and the failure may be about somebody
// else's row.
func (i *Inbox) broke(in channel.Inbound, verdict domain.InboundVerdict, what string, err error) channel.Handled {
	i.log.Error().Err(err).Str("channel", string(in.Kind)).Str("verdict", string(verdict)).
		Msg("could not " + what)
	return handled(verdict, replyUnavailable)
}

// lastAccepted is when this sender last had a message accepted, or the zero time, which
// the ladder reads as never.
func (i *Inbox) lastAccepted(key string) time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.accepted[key]
}

// recordAccepted remembers a sender, forgetting everybody if the map is full.
//
// In memory and not in the database on purpose: the throttle is a flood control, a
// restart forgets it, and the cost of forgetting is one extra accepted message per
// sender. A row per inbound message would be a write per message a stranger can cause.
func (i *Inbox) recordAccepted(key string, at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.accepted) >= maxTrackedSenders {
		i.accepted = make(map[string]time.Time, maxTrackedSenders)
	}
	i.accepted[key] = at
}

// senderKey identifies a sender for throttling.
//
// The platform and its own id for them — not the user id, because an unlinked sender has
// none and is exactly who the throttle is for, and not the conversation, because the same
// person talking in two rooms is one sender spending one budget.
func senderKey(in channel.Inbound) string {
	return string(in.Kind) + ":" + in.SenderID
}

// identityCouldChange reports whether resolving the sender could change the verdict.
//
// Rung 4 is the only rung that reads Linked, and these two verdicts are what it produces,
// so a first pass that ended anywhere else ended above it and the sender is nobody's
// business yet. A new rung that reads Linked has to be named here too.
func identityCouldChange(verdict domain.InboundVerdict) bool {
	return verdict == domain.InboundLinkAttempt || verdict == domain.InboundLinkRequired
}

func handled(verdict domain.InboundVerdict, reply string) channel.Handled {
	return channel.Handled{Verdict: verdict, Reply: reply}
}
