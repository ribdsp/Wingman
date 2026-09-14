package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// Bounds on the three fields a notification carries.
//
// None is a security property on its own; together they stop a bug next door from turning
// one goal's headline into a message a platform refuses, or into a wall of text somebody
// has to scroll past on a phone at midnight. A notice that arrives truncated has failed at
// its job, so an over-long one is refused rather than trimmed.
const (
	maxNoticeSubjectLength  = 200
	maxNoticeHeadlineLength = 500
	maxNoticeLinkLength     = 500
)

// NoticeKind names what happened. It mirrors goalengine's set field for field, because the
// engine is the only caller and this is the wire.
//
// A closed set rather than free text: core does not act on the kind beyond logging it, but
// an unknown one means the two services disagree about their contract, and finding that out
// from a rejected request beats finding it out from a message nobody understands.
type NoticeKind string

const (
	// NoticeTrigger says an agent was woken for a goal that fell behind.
	NoticeTrigger NoticeKind = "trigger"
	// NoticeApprovalPending says a spend is waiting for a human.
	NoticeApprovalPending NoticeKind = "approval_pending"
)

// Valid reports whether this is a kind core knows.
func (k NoticeKind) Valid() bool {
	switch k {
	case NoticeTrigger, NoticeApprovalPending:
		return true
	default:
		return false
	}
}

// Notice is something that happened next door which a person should hear about.
//
// It carries no amount, no currency and no pace figure, and core neither adds one nor
// misses one: the engine decides what may leave the box, and a chat message is retained on
// somebody else's servers for as long as they like. The link is what makes an operator open
// the console, where the numbers are.
type Notice struct {
	Kind NoticeKind
	// SubjectID is the goal or the approval this is about. It is in the log line, so
	// "was I told about this one?" is answerable without opening anything.
	SubjectID string
	// Headline is the sentence a person reads, written by the engine.
	Headline string
	// Link is where to go, and may be empty — an engine with no WEB_BASE_URL configured
	// has nowhere to point.
	Link string
}

// Delivered is how many chat accounts heard about it.
//
// The count and not the platforms. A caller that learned which platforms the operator has
// connected would be a caller that could enumerate them by sending notices, and the engine
// has no use for the answer: what it does with a failure is log it.
type Delivered struct {
	Recipients int
}

// NotificationsDeps is what the notifier needs.
type NotificationsDeps struct {
	Directory ChannelDirectory
	Sender    DirectSender

	// OwnerID is the account CORE_UNATTENDED_OWNER names, resolved to an id by cmd at
	// boot — the same value Tasks files unattended work against.
	//
	// It is the recipient, and there is deliberately no other way to choose one: a
	// request field naming who to message would let whoever holds a bot key send a
	// message from this instance to any chat account linked to it. Empty is allowed, and
	// means there is nobody to tell.
	OwnerID string

	Logger zerolog.Logger
}

// Notifications tells the operator that something happened, on a chat platform they already
// use.
//
// It is the smallest service in the package and deliberately the least powerful: it reads
// one account's chat identities and writes to none of them. It cannot revoke a link, cannot
// start a run, cannot read a chat and cannot see anybody's data but the owner's — which is
// why it takes ChannelDirectory rather than ChannelLinks.
//
// Nothing here can affect a decision. A notification is sent after the engine has already
// recorded what it decided, so every failure below is a lost message and never a lost audit
// row, a re-run trigger or an approval that resolved itself.
type Notifications struct {
	directory ChannelDirectory
	sender    DirectSender
	ownerID   string
	log       zerolog.Logger
}

// NewNotifications validates its wiring and returns a ready service.
func NewNotifications(deps NotificationsDeps) (*Notifications, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Directory != nil, "Directory")
	require(deps.Sender != nil, "Sender")
	if len(missing) > 0 {
		return nil, fmt.Errorf("notifications: missing dependencies: %v", missing)
	}

	// OwnerID is not required. An instance nobody configured an owner for is an instance
	// with nobody to notify, which is a normal way to run this and not a reason to refuse
	// to boot — cmd already warns about the unset variable for the dispatch path.
	return &Notifications{
		directory: deps.Directory,
		sender:    deps.Sender,
		ownerID:   strings.TrimSpace(deps.OwnerID),
		log:       deps.Logger,
	}, nil
}

// Notify delivers one notice to every chat account the unattended owner has linked.
//
// Best effort by design. One platform failing does not stop the others, nothing is retried,
// and nothing is queued: the approval is still in the queue, the console still shows it and
// its TTL still expires it, so a lost message costs a prompt and never a decision.
func (n *Notifications) Notify(ctx context.Context, in Notice, actor Actor) (Delivered, error) {
	actor, err := actor.prepare()
	if err != nil {
		return Delivered{}, err
	}
	// An operator or the goal engine, never a person. The same rule as dispatch, for the
	// same reason: a signed-in human must not be able to make this instance send messages
	// to the operator's own phone.
	if err := actor.requireDispatcher("sending a notification"); err != nil {
		return Delivered{}, err
	}
	notice, err := in.prepare()
	if err != nil {
		return Delivered{}, err
	}

	if n.ownerID == "" {
		n.log.Warn().Str("noticeKind", string(notice.Kind)).Str("subjectId", notice.SubjectID).
			Msg("nobody to notify: CORE_UNATTENDED_OWNER is not set")
		return Delivered{}, nil
	}

	identities, err := n.directory.ListForUser(ctx, n.ownerID)
	if err != nil {
		// Not "nobody is linked". A directory that cannot be read is a fault, and saying so
		// is what keeps a broken database from looking like an operator who connected
		// nothing.
		return Delivered{}, fmt.Errorf("read the owner's chat accounts: %w", err)
	}

	delivered, attempted, failures := n.fanOut(ctx, notice, identities)
	switch {
	case attempted == 0:
		n.log.Warn().Str("noticeKind", string(notice.Kind)).Str("subjectId", notice.SubjectID).
			Msg("nobody to notify: the unattended owner has no connected chat account")
		return Delivered{}, nil
	case delivered == 0:
		return Delivered{}, fmt.Errorf("notify %s about %s: every platform failed: %w",
			notice.Kind, notice.SubjectID, errors.Join(failures...))
	}

	// The kind, the subject and how many heard. Not the text, not the conversation and not
	// the platforms — the same line Replier writes, for the same reason.
	n.log.Info().Str("noticeKind", string(notice.Kind)).Str("subjectId", notice.SubjectID).
		Int("recipients", delivered).Msg("notification delivered")
	return Delivered{Recipients: delivered}, nil
}

// fanOut sends to every live identity and reports how it went.
//
// attempted counts the live ones, which is what tells "nobody is linked" apart from "nobody
// could be reached": the first is somebody's setup and the second is a fault.
func (n *Notifications) fanOut(ctx context.Context, notice Notice, identities []repository.ChannelIdentity) (delivered, attempted int, failures []error) {
	text := notice.text()
	for _, identity := range identities {
		// ListForUser returns revoked rows, because a person needs to see the ones they
		// disconnected. A revoked identity is somebody who asked not to be messaged there
		// again, and this is where that is honoured.
		if !identity.Live() {
			continue
		}
		attempted++

		if err := n.sender.SendDirect(ctx, identity.Kind, identity.ExternalID, text); err != nil {
			// Warned per platform and carried on. A blocked bot on one platform is that
			// person's own choice and must not cost them the message on the other.
			n.log.Warn().Err(err).Str("channel", string(identity.Kind)).
				Str("noticeKind", string(notice.Kind)).Str("subjectId", notice.SubjectID).
				Msg("a notification could not be delivered on this platform")
			failures = append(failures, fmt.Errorf("%s: %w", identity.Kind, err))
			continue
		}
		delivered++
	}
	return delivered, attempted, failures
}

// prepare trims a notice and refuses one that cannot be delivered usefully.
func (n Notice) prepare() (Notice, error) {
	n.Kind = NoticeKind(strings.TrimSpace(string(n.Kind)))
	n.SubjectID = strings.TrimSpace(n.SubjectID)
	n.Headline = strings.TrimSpace(n.Headline)
	n.Link = strings.TrimSpace(n.Link)

	switch {
	case !n.Kind.Valid():
		return Notice{}, fmt.Errorf("%w: %q is not something core knows how to announce", ErrValidation, n.Kind)
	case n.SubjectID == "":
		return Notice{}, fmt.Errorf("%w: a notification must say what it is about", ErrValidation)
	case len(n.SubjectID) > maxNoticeSubjectLength:
		return Notice{}, fmt.Errorf("%w: that subject id is too long", ErrValidation)
	case n.Headline == "":
		return Notice{}, fmt.Errorf("%w: a notification needs a headline", ErrValidation)
	case len(n.Headline) > maxNoticeHeadlineLength:
		return Notice{}, fmt.Errorf("%w: that headline is longer than a notification may be", ErrValidation)
	case len(n.Link) > maxNoticeLinkLength:
		return Notice{}, fmt.Errorf("%w: that link is too long", ErrValidation)
	}

	if n.Link != "" && !webAddress(n.Link) {
		// The link is the one part of a notification somebody is invited to act on, and it
		// arrives carrying this instance's credibility. A javascript:, file: or
		// scheme-relative one is a phishing message with Wingman's name on it.
		return Notice{}, fmt.Errorf("%w: a notification's link must be an http or https address", ErrValidation)
	}
	return n, nil
}

// text is what gets sent: the headline as the engine wrote it, then the link on a line of
// its own.
//
// Core adds no wording. What may leave this box was decided next door, where the numbers
// are, and a second author here would be a second place for that rule to drift.
func (n Notice) text() string {
	if n.Link == "" {
		return n.Headline
	}
	return n.Headline + "\n" + n.Link
}

// webAddress reports whether a link is one a chat client will open as a web page.
//
// The scheme is compared case-insensitively because a scheme is case-insensitive, and the
// check is a prefix rather than a parse: url.Parse accepts every scheme there is, so it
// would answer yes to the two this refuses.
func webAddress(link string) bool {
	lower := strings.ToLower(link)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")
}
