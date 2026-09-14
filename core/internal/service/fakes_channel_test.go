package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// The channel fakes. They are in this file rather than in fakes_test.go because that one
// is already at the length this project treats as a ceiling, and because these nine ports
// are one subject: what a message arriving from a platform is allowed to reach.
//
// Same rules as the others — in-memory, single-threaded, explicit fail* fields, and the
// two repository behaviours the services are built around reproduced faithfully: a query
// scoped by user id answers ErrNotFound for somebody else's row, and a channel account
// that is already taken answers ErrConflict.

var (
	_ ChannelResolver  = (*fakeIdentities)(nil)
	_ ChannelLinker    = (*fakeIdentities)(nil)
	_ ChannelLinks     = (*fakeIdentities)(nil)
	_ LinkCodeMinter   = (*fakeLinkCodes)(nil)
	_ LinkCodeRedeemer = (*fakeLinkCodes)(nil)
	_ LinkCodeJanitor  = (*fakeLinkCodes)(nil)
	_ ChannelChats     = (*fakeChats)(nil)
	_ ChannelWork      = (*fakeChannelWork)(nil)
	_ ChannelReplier   = (*fakeReplier)(nil)
	_ ChannelDirectory = (*fakeIdentities)(nil)
	_ DirectSender     = (*fakeDirectSender)(nil)
	_ agent.Halt       = (*fakeHalt)(nil)
)

// ---------------------------------------------------------------------------
// channel identities

// fakeIdentities is ChannelResolver, ChannelLinker and ChannelLinks. One fake for three
// ports for the reason fakeUsers is one for three: a test wiring several views of the same
// table is a test about that table.
type fakeIdentities struct {
	identities map[string]*repository.ChannelIdentity
	order      []string
	nextID     int

	failResolve error
	failLink    error
	failRelink  error
	failList    error
	failRevoke  error

	resolveCalls int
}

func newFakeIdentities() *fakeIdentities {
	return &fakeIdentities{identities: map[string]*repository.ChannelIdentity{}}
}

// seed adds a live link, and returns the row so a test can name its id.
func (f *fakeIdentities) seed(userID string, kind repository.ChannelKind, externalID string) repository.ChannelIdentity {
	f.nextID++
	identity := repository.ChannelIdentity{
		ID:          fmt.Sprintf("identity-%d", f.nextID),
		UserID:      userID,
		Kind:        kind,
		ExternalID:  externalID,
		DisplayName: "seeded",
		LinkedAt:    testNow,
	}
	f.identities[identity.ID] = &identity
	f.order = append(f.order, identity.ID)
	return identity
}

// seedRevoked adds a link somebody disconnected, which is the state Relink exists for.
func (f *fakeIdentities) seedRevoked(userID string, kind repository.ChannelKind, externalID string) repository.ChannelIdentity {
	identity := f.seed(userID, kind, externalID)
	revoked := testNow.Add(-time.Hour)
	f.identities[identity.ID].RevokedAt = &revoked
	return *f.identities[identity.ID]
}

func (f *fakeIdentities) find(kind repository.ChannelKind, externalID string) *repository.ChannelIdentity {
	for _, id := range f.order {
		identity := f.identities[id]
		if identity.Kind == kind && identity.ExternalID == strings.TrimSpace(externalID) {
			return identity
		}
	}
	return nil
}

func (f *fakeIdentities) Resolve(_ context.Context, kind repository.ChannelKind, externalID string) (repository.ChannelIdentity, error) {
	f.resolveCalls++
	if f.failResolve != nil {
		return repository.ChannelIdentity{}, f.failResolve
	}
	// Revoked rows are excluded, exactly as the real query excludes them: somebody who
	// unlinked has withdrawn permission, so their next message is a stranger's.
	identity := f.find(kind, externalID)
	if identity == nil || !identity.Live() {
		return repository.ChannelIdentity{}, repository.ErrNotFound
	}
	return *identity, nil
}

func (f *fakeIdentities) Link(_ context.Context, in repository.ChannelIdentity) (repository.ChannelIdentity, error) {
	if f.failLink != nil {
		return repository.ChannelIdentity{}, f.failLink
	}
	// The unique constraint is on the channel account, not on the pair, so a revoked row
	// conflicts too — which is what sends the service to Relink.
	if f.find(in.Kind, in.ExternalID) != nil {
		return repository.ChannelIdentity{}, fmt.Errorf("link identity: %w", repository.ErrConflict)
	}

	f.nextID++
	identity := repository.ChannelIdentity{
		ID:          fmt.Sprintf("identity-%d", f.nextID),
		UserID:      in.UserID,
		Kind:        in.Kind,
		ExternalID:  strings.TrimSpace(in.ExternalID),
		DisplayName: in.DisplayName,
		LinkedAt:    testNow,
	}
	f.identities[identity.ID] = &identity
	f.order = append(f.order, identity.ID)
	return identity, nil
}

func (f *fakeIdentities) Relink(_ context.Context, userID string, kind repository.ChannelKind, externalID string, at time.Time) (repository.ChannelIdentity, error) {
	if f.failRelink != nil {
		return repository.ChannelIdentity{}, f.failRelink
	}
	identity := f.find(kind, externalID)
	// Scoped to the owner and to a revoked row, both of which the real UPDATE has in its
	// WHERE clause. Somebody else's revoked identity is not found, not taken over.
	if identity == nil || identity.UserID != userID || identity.Live() {
		return repository.ChannelIdentity{}, repository.ErrNotFound
	}
	identity.RevokedAt = nil
	identity.LinkedAt = at
	return *identity, nil
}

func (f *fakeIdentities) ListForUser(_ context.Context, userID string) ([]repository.ChannelIdentity, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	out := []repository.ChannelIdentity{}
	for _, id := range f.order {
		if identity := f.identities[id]; identity.UserID == userID {
			out = append(out, *identity)
		}
	}
	return out, nil
}

func (f *fakeIdentities) Revoke(_ context.Context, userID, identityID string, at time.Time) error {
	if f.failRevoke != nil {
		return f.failRevoke
	}
	identity, ok := f.identities[identityID]
	if !ok || identity.UserID != userID || !identity.Live() {
		return repository.ErrNotFound
	}
	identity.RevokedAt = &at
	return nil
}

// ---------------------------------------------------------------------------
// link codes

// fakeLinkCodes is LinkCodeMinter, LinkCodeRedeemer and LinkCodeJanitor over a map keyed
// by the stored hash. The plaintext never appears in it, because the plaintext never
// reaches the real repository either.
type fakeLinkCodes struct {
	codes  map[string]*repository.LinkCode
	nextID int

	failMint    error
	failConsume error
	failDelete  error

	minted   int
	consumed int
}

func newFakeLinkCodes() *fakeLinkCodes {
	return &fakeLinkCodes{codes: map[string]*repository.LinkCode{}}
}

// seedCode records a live code for a hash a test computed itself.
func (f *fakeLinkCodes) seedCode(userID, hash string, expiresAt time.Time) repository.LinkCode {
	f.nextID++
	code := repository.LinkCode{
		ID:        fmt.Sprintf("code-%d", f.nextID),
		UserID:    userID,
		CreatedAt: testNow,
		ExpiresAt: expiresAt,
	}
	f.codes[hash] = &code
	return code
}

func (f *fakeLinkCodes) Mint(_ context.Context, in repository.NewLinkCode) (repository.LinkCode, error) {
	if f.failMint != nil {
		return repository.LinkCode{}, f.failMint
	}
	// Whatever the account had outstanding goes, in the same call — one live code per
	// person, which the real Mint does in one statement.
	for hash, code := range f.codes {
		if code.UserID == in.UserID && code.ConsumedAt == nil {
			delete(f.codes, hash)
		}
	}
	f.minted++
	return f.seedCode(in.UserID, in.CodeHash, in.ExpiresAt), nil
}

func (f *fakeLinkCodes) Consume(_ context.Context, codeHash string, at time.Time) (string, error) {
	if f.failConsume != nil {
		return "", f.failConsume
	}
	code, ok := f.codes[codeHash]
	// Unknown, expired and already spent are one answer here as they are there, so a
	// sender cannot tell the three apart by what comes back.
	if !ok || !code.Live(at) {
		return "", repository.ErrNotFound
	}
	code.ConsumedAt = &at
	f.consumed++
	return code.UserID, nil
}

func (f *fakeLinkCodes) DeleteSpent(_ context.Context, before time.Time) (int, error) {
	if f.failDelete != nil {
		return 0, f.failDelete
	}
	deleted := 0
	for hash, code := range f.codes {
		spent := code.ConsumedAt != nil && code.ConsumedAt.Before(before)
		if spent || code.ExpiresAt.Before(before) {
			delete(f.codes, hash)
			deleted++
		}
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// the chat a channel conversation lands in

// EnsureChannelChat is on fakeChats rather than on a fake of its own, because it is the
// same table the other two ports read: a channel message and a web message end up in the
// same chats, which is the point of the whole arrangement.
//
// Keyed by the account, the platform and the conversation, so the second message from a
// person's Telegram DM finds the chat the first one made instead of starting another.
func (f *fakeChats) EnsureChannelChat(_ context.Context, in repository.ChannelChat) (repository.Chat, error) {
	if f.failEnsure != nil {
		return repository.Chat{}, f.failEnsure
	}
	key := in.UserID + "|" + string(in.Kind) + "|" + in.ConversationID
	if id, ok := f.channelChats[key]; ok {
		// The title is not updated. Somebody who renamed the thread keeps their name for
		// it, which is what the real upsert's DO UPDATE deliberately does not touch.
		return *f.chats[id], nil
	}

	chat, err := f.CreateChat(context.Background(), in.UserID, in.Title)
	if err != nil {
		return repository.Chat{}, err
	}
	if f.channelChats == nil {
		f.channelChats = map[string]string{}
	}
	f.channelChats[key] = chat.ID
	return chat, nil
}

// ---------------------------------------------------------------------------
// the rest of the inbound path

// fakeChannelWork is ChannelWork: where an accepted message goes. It records what it was
// handed, because "the owner Inbox resolved is the owner the task was filed against" is
// the property most of the inbox tests are about.
type fakeChannelWork struct {
	calls []ChannelMessage
	fail  error
}

func (f *fakeChannelWork) FromChannel(_ context.Context, in ChannelMessage) (Sent, error) {
	f.calls = append(f.calls, in)
	if f.fail != nil {
		return Sent{}, f.fail
	}
	return Sent{
		Chat:    repository.Chat{ID: "chat-1", UserID: in.UserID},
		Message: repository.Message{ID: "message-1", ChatID: "chat-1", UserID: in.UserID},
		Task: domain.Task{
			ID:          "task-1",
			OwnerUserID: in.UserID,
			Source:      domain.TaskSourceChannel,
			Status:      domain.TaskStatusQueued,
		},
	}, nil
}

// fakeHalt is the kill switch. Its own fake rather than a field on another, because the
// two things a test needs from it — engaged, and unreadable — are the two halves of the
// fail-closed rule.
type fakeHalt struct {
	engaged bool
	fail    error
	calls   int
}

func (f *fakeHalt) Engaged(_ context.Context) (bool, error) {
	f.calls++
	if f.fail != nil {
		return false, f.fail
	}
	return f.engaged, nil
}

// fakeReplier is ChannelReplier: the delivery of a run's answer back to the platform it
// was asked on.
type fakeReplier struct {
	calls []channelReply
	fail  error
}

type channelReply struct {
	userID string
	chatID string
	text   string
}

func (f *fakeReplier) Reply(_ context.Context, userID, chatID, text string) error {
	f.calls = append(f.calls, channelReply{userID: userID, chatID: chatID, text: text})
	return f.fail
}

// fakeDirectSender is DirectSender: a message to one person on one platform.
//
// failOn is per platform rather than a single fail field, because the property the
// notification tests are about is that one platform being unreachable does not cost the
// others their message — which cannot be expressed with an all-or-nothing failure.
type fakeDirectSender struct {
	calls  []directSend
	failOn map[repository.ChannelKind]error
}

type directSend struct {
	kind           repository.ChannelKind
	externalUserID string
	text           string
}

func (f *fakeDirectSender) SendDirect(_ context.Context, kind repository.ChannelKind, externalUserID, text string) error {
	f.calls = append(f.calls, directSend{kind: kind, externalUserID: externalUserID, text: text})
	return f.failOn[kind]
}

// sentOn is what was delivered on one platform, or the zero value if nothing was.
func (f *fakeDirectSender) sentOn(kind repository.ChannelKind) directSend {
	for _, call := range f.calls {
		if call.kind == kind {
			return call
		}
	}
	return directSend{}
}
