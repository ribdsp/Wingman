package domain

import (
	"strings"
	"time"
)

// ChannelKind is a chat platform Wingman can be reached on. The set is closed and
// matches the enum in the migrations: a kind stored that this package does not know
// is a row nothing can deliver an answer to.
type ChannelKind string

const (
	ChannelTelegram ChannelKind = "telegram"
	ChannelSlack    ChannelKind = "slack"
	ChannelDiscord  ChannelKind = "discord"
)

// AllChannelKinds is the closed set, in the order the enum declares it.
//
// One definition, for the same reason AllStopReasons has one: the migration, the
// reference endpoint and the adapter registry all have to agree, and a kind added to
// the constants but missing from a list somewhere is a channel that authenticates and
// then silently answers nobody.
func AllChannelKinds() []ChannelKind {
	return []ChannelKind{ChannelTelegram, ChannelSlack, ChannelDiscord}
}

// Valid reports whether the kind is one this build knows how to deliver to.
func (k ChannelKind) Valid() bool {
	switch k {
	case ChannelTelegram, ChannelSlack, ChannelDiscord:
		return true
	default:
		return false
	}
}

// The shape of a link code — the string a person mints while signed in and pastes
// into a chat to prove the account on the other end is theirs.
const (
	// linkCodeMarker is what a code starts with, ahead of any separator. It exists
	// so a person can tell at a glance that the thing in their clipboard belongs to
	// this system, and so prose almost never normalises into a code by accident.
	linkCodeMarker = "WGM"
	// LinkCodePrefix is the marker as it is written.
	LinkCodePrefix = linkCodeMarker + "-"
	// LinkCodeBodyLength is how many characters follow the prefix. Ten characters
	// from a thirty-character alphabet is a little under 49 bits — enough that
	// guessing one inside its few-minute life is not a strategy, and short enough
	// to read off a screen and type into a phone.
	LinkCodeBodyLength = 10
	// LinkCodeAlphabet excludes the characters people confuse when copying a code
	// by eye: 0/O, 1/I/L, and U. A code that cannot be transcribed gets pasted
	// wrong, and every wrong paste is another minute the real one stays live.
	LinkCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
	// linkCodeGroupAt is where the displayed form breaks the body in two.
	linkCodeGroupAt = 5
	// linkCodeSeparators are dropped before a code is read, so the grouping dash,
	// a phone's autocorrected underscore and a pasted line break all normalise to
	// the same value.
	linkCodeSeparators = " -_\t\r\n"
)

// NormaliseLinkCode reads a message as a link code, returning the canonical form
// that gets hashed and looked up.
//
// The whole message has to be the code. A code found inside a sentence is not
// accepted, for two reasons: scanning prose for something code-shaped is a rule
// nobody can predict the behaviour of, and the failure is expensive in the wrong
// direction — a false positive consumes a live credential that may not even belong
// to the sender. Somebody who sends a code with "hi" in front of it gets the linking
// instructions again, which say to send the code on its own.
func NormaliseLinkCode(text string) (string, bool) {
	upper := strings.ToUpper(text)

	var stripped strings.Builder
	stripped.Grow(len(upper))
	for _, r := range upper {
		if strings.ContainsRune(linkCodeSeparators, r) {
			continue
		}
		stripped.WriteRune(r)
	}

	body, found := strings.CutPrefix(stripped.String(), linkCodeMarker)
	if !found || len(body) != LinkCodeBodyLength {
		return "", false
	}
	for _, r := range body {
		if !strings.ContainsRune(LinkCodeAlphabet, r) {
			return "", false
		}
	}
	return LinkCodePrefix + body, true
}

// FormatLinkCode is the canonical form as a person should see it, grouped so it can
// be read aloud and typed back. Anything it does not recognise is returned unchanged:
// this is for display, and inventing a shape for an unrecognised value would put a
// code on screen that will not normalise back.
func FormatLinkCode(code string) string {
	body, found := strings.CutPrefix(code, LinkCodePrefix)
	if !found || len(body) != LinkCodeBodyLength {
		return code
	}
	return LinkCodePrefix + body[:linkCodeGroupAt] + "-" + body[linkCodeGroupAt:]
}

// MinInboundInterval is the shortest gap ClassifyInbound will enforce between two
// accepted messages from the same sender, whatever the caller passed.
//
// It exists because zero is the one value that would read as "no throttle", and this
// is the only decision in core a stranger can drive: anybody who can find the bot can
// send it messages, and each accepted message is a run that spends somebody's tokens.
// The configured value is the real limit; this is the floor under a mistake.
const MinInboundInterval = time.Second

// InboundVerdict is what to do with a message that arrived on a channel.
type InboundVerdict string

const (
	// InboundIgnore means say nothing and do nothing. Reserved for messages that
	// were never addressed to a person: another application's output, and content
	// with no text to act on.
	InboundIgnore InboundVerdict = "ignore"
	// InboundGroupRefused means this instance does not work in shared rooms.
	InboundGroupRefused InboundVerdict = "group_refused"
	// InboundLinkAttempt means the message is a link code and nothing else. The
	// code is in the decision; whether it is live is a database question.
	InboundLinkAttempt InboundVerdict = "link_attempt"
	// InboundLinkRequired means the sender is not connected to any account.
	InboundLinkRequired InboundVerdict = "link_required"
	// InboundThrottled means this sender is talking faster than the interval
	// allows. Silent on purpose — a reply to every throttled message is itself a
	// flood, and it is one this side is paying to send.
	InboundThrottled InboundVerdict = "throttled"
	// InboundTooLong means the text exceeds what a brief may carry.
	InboundTooLong InboundVerdict = "too_long"
	// InboundHalted means the kill switch is engaged, or could not be read.
	InboundHalted InboundVerdict = "halted"
	// InboundAccept means turn it into a task.
	InboundAccept InboundVerdict = "accept"
)

// Silent reports whether the verdict is one that sends nothing back.
//
// Two are: a message that was not addressed to us, and a sender being rate limited.
// Everything else answers, including every refusal — a self-hosted assistant that
// goes quiet is indistinguishable from one that is broken, and the person on the
// other end has no logs to read.
func (v InboundVerdict) Silent() bool {
	return v == InboundIgnore || v == InboundThrottled
}

// InboundFacts is everything ClassifyInbound needs. The adapter for each platform
// normalises its own update into this, which is why the ladder below has no idea
// which of the three it is deciding for.
type InboundFacts struct {
	Kind ChannelKind
	// FromBot is set when the platform says the author is an application — that
	// includes this one, seeing its own message echoed back.
	FromBot bool
	Text    string
	// Direct is false when the message arrived in a group, a channel or a server
	// room. Adapters only forward a group message that addressed the bot, so a
	// false here means somebody deliberately called it in a shared place.
	Direct bool
	// AllowGroups is the operator's decision, from configuration. It is off by
	// default: in a group, the linked person's token budget is spendable by
	// anybody who can type there.
	AllowGroups bool
	// Linked is true when the sender resolves to a live channel identity. A
	// revoked link is not live, so a sender who was disconnected arrives here as
	// false and is a stranger again.
	//
	// An identity that could not be *read* is not passed as false. The caller
	// apologises for that instead of asking for a verdict, because telling somebody
	// who is linked that they are not sends them to mint a code from a database that
	// is equally unreachable. Either way no run starts, which is the property that
	// matters here.
	Linked bool
	// LastAccepted is when this sender last had a message accepted. Zero means
	// never, which is not throttled.
	LastAccepted time.Time
	// MinInterval is the configured gap between accepted messages, floored at
	// MinInboundInterval.
	MinInterval time.Duration
	// KillSwitchEngaged halts the work. An unreadable flag must be passed as true,
	// the same rule the run ladder and the goal engine both apply.
	KillSwitchEngaged bool
}

// InboundDecision is the immutable record of one inbound classification.
type InboundDecision struct {
	Verdict InboundVerdict
	// LinkCode is the normalised code, set only on InboundLinkAttempt.
	LinkCode string
	// RetryAfter is how long until this sender's next message would be accepted,
	// set only on InboundThrottled. Nothing is told to the sender; it is for the
	// log, where "throttled by two seconds" and "throttled by an hour" are two
	// different stories about the same verdict.
	RetryAfter time.Duration
	DecidedAt  time.Time
}

// ClassifyInbound decides what happens to a message that arrived on a channel.
//
// The order of the rungs is the safety model, and it is arranged around three ideas.
//
// Loop prevention is first, because two applications answering each other spends real
// money per exchange and every rung below this one replies. Then the questions about
// the room and the sender, because whether a shared room may be used at all is a fact
// about the room and not about who is typing — telling a stranger in a public channel
// how to link would invite an account credential into a place other people can read.
// Then everything that can be decided from the message itself. Last, and only last,
// the kill switch: it is the single fact here that costs a network call to establish,
// so a flood of messages must not become a flood of requests to the goal engine.
//
// One consequence is deliberate. A person pasting a link code is served while the
// instance is halted, because linking spends nothing and starts no run, and answering
// somebody mid-connection with silence gives them nothing to act on.
func ClassifyInbound(in InboundFacts, now time.Time) InboundDecision {
	decision := InboundDecision{DecidedAt: now}
	verdict := func(v InboundVerdict) InboundDecision {
		decision.Verdict = v
		return decision
	}

	// 1. Another application. Never answer one, whatever else is true.
	if in.FromBot {
		return verdict(InboundIgnore)
	}

	// 2. Nothing to act on: a sticker, a photo with no caption, somebody joining.
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return verdict(InboundIgnore)
	}

	// 3. A shared room, which is off unless an operator turned it on.
	if !in.Direct && !in.AllowGroups {
		return verdict(InboundGroupRefused)
	}

	// 4. An unlinked sender may reach exactly one thing, and this is it.
	if !in.Linked {
		if code, ok := NormaliseLinkCode(text); ok {
			decision.LinkCode = code
			return verdict(InboundLinkAttempt)
		}
		return verdict(InboundLinkRequired)
	}

	// 5. Rate limit, before anything that costs a call. Zero is floored rather than
	// honoured — see MinInboundInterval.
	interval := in.MinInterval
	if interval < MinInboundInterval {
		interval = MinInboundInterval
	}
	if !in.LastAccepted.IsZero() {
		if elapsed := now.Sub(in.LastAccepted); elapsed < interval {
			decision.RetryAfter = interval - elapsed
			return verdict(InboundThrottled)
		}
	}

	// 6. Longer than a brief may be. Refused here rather than truncated: a brief cut
	// in half is an instruction whose second half the agent never sees.
	if len([]rune(text)) > MaxBriefLength {
		return verdict(InboundTooLong)
	}

	// 7. The brakes.
	if in.KillSwitchEngaged {
		return verdict(InboundHalted)
	}

	return verdict(InboundAccept)
}
