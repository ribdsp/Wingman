package handler

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/service"
)

// The channel fakes, in a file of their own because fakes_test.go is already at the length
// this project treats as a ceiling.
//
// Only the three ports the *signed-in* half needs are here: minting a code, listing what an
// account has connected, disconnecting one. The inbound half — resolving a stranger's chat
// account, redeeming a code, queueing the work — has no route, so nothing in this package
// can reach it, and a fake for it here would be a fake for something untested.
//
// Like the others these enforce nothing except user_id scoping, which is what makes
// "somebody else's identity id answers 404" a real assertion rather than a coincidence.

var (
	_ service.LinkCodeMinter   = (*memChannels)(nil)
	_ service.ChannelLinks     = (*memChannels)(nil)
	_ service.LinkCodeJanitor  = (*memChannels)(nil)
	_ service.ChannelDirectory = (*memChannels)(nil)
	_ service.DirectSender     = (*memDirectSender)(nil)
)

// memChannels is the channel_identities and channel_link_codes tables.
//
// Codes are keyed by their stored hash, never by the plaintext, because the plaintext never
// reaches the real repository either — which is the reason no route can read a code back.
type memChannels struct {
	mu    sync.Mutex
	seq   int
	byID  map[string]repository.ChannelIdentity
	order []string
	codes map[string]repository.LinkCode

	err   error            // returned by every method when set
	failN map[string]error // method name → error, for one-method failures
}

func newMemChannels() *memChannels {
	return &memChannels{
		byID:  map[string]repository.ChannelIdentity{},
		codes: map[string]repository.LinkCode{},
		failN: map[string]error{},
	}
}

func (m *memChannels) fault(method string) error {
	if m.err != nil {
		return m.err
	}
	return m.failN[method]
}

// seed adds a live connection and returns it, so a test can name its id.
func (m *memChannels) seed(userID string, kind repository.ChannelKind, externalID, displayName string) repository.ChannelIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	identity := repository.ChannelIdentity{
		ID:          "identity-" + strconv.Itoa(m.seq),
		UserID:      userID,
		Kind:        kind,
		ExternalID:  externalID,
		DisplayName: displayName,
		LinkedAt:    testNow,
	}
	m.byID[identity.ID] = identity
	m.order = append(m.order, identity.ID)
	return identity
}

// seedRevoked adds one somebody disconnected, so the rendering of a revoked row can be
// asserted rather than assumed.
func (m *memChannels) seedRevoked(userID string, kind repository.ChannelKind, externalID, displayName string) repository.ChannelIdentity {
	identity := m.seed(userID, kind, externalID, displayName)

	m.mu.Lock()
	defer m.mu.Unlock()
	revoked := testNow.Add(-time.Hour)
	identity.RevokedAt = &revoked
	m.byID[identity.ID] = identity
	return identity
}

func (m *memChannels) Mint(_ context.Context, input repository.NewLinkCode) (repository.LinkCode, error) {
	if err := m.fault("Mint"); err != nil {
		return repository.LinkCode{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// One live code per person, discarded in the same call, because the real Mint discards
	// the outstanding one in the same statement as the insert.
	for hash, code := range m.codes {
		if code.UserID == input.UserID && code.ConsumedAt == nil {
			delete(m.codes, hash)
		}
	}

	m.seq++
	code := repository.LinkCode{
		ID:        "code-" + strconv.Itoa(m.seq),
		UserID:    input.UserID,
		CreatedAt: testNow,
		ExpiresAt: input.ExpiresAt,
	}
	m.codes[input.CodeHash] = code
	return code, nil
}

func (m *memChannels) ListForUser(_ context.Context, userID string) ([]repository.ChannelIdentity, error) {
	if err := m.fault("ListForUser"); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := []repository.ChannelIdentity{}
	for _, id := range m.order {
		if identity := m.byID[id]; identity.UserID == userID {
			out = append(out, identity)
		}
	}
	return out, nil
}

func (m *memChannels) Revoke(_ context.Context, userID, identityID string, at time.Time) error {
	if err := m.fault("Revoke"); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	identity, ok := m.byID[identityID]
	// Scoped to the owner and to a live row, both of which the real UPDATE has in its WHERE
	// clause. Somebody else's connection is not found, which is what the 404 is made of.
	if !ok || identity.UserID != userID || !identity.Live() {
		return repository.ErrNotFound
	}
	identity.RevokedAt = &at
	m.byID[identityID] = identity
	return nil
}

func (m *memChannels) DeleteSpent(_ context.Context, before time.Time) (int, error) {
	if err := m.fault("DeleteSpent"); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	deleted := 0
	for hash, code := range m.codes {
		spent := code.ConsumedAt != nil && code.ConsumedAt.Before(before)
		if spent || code.ExpiresAt.Before(before) {
			delete(m.codes, hash)
			deleted++
		}
	}
	return deleted, nil
}

// memDirectSender stands in for the hub: a notification's last step, where a message would
// leave for Telegram, Slack or Discord.
//
// It records the text, which is what makes "a notification carries no figure" assertable at
// the boundary the engine actually calls — and it never has a real token, so no test in this
// package can send anything anywhere.
type memDirectSender struct {
	mu    sync.Mutex
	calls []directSend
	err   error
}

type directSend struct {
	kind           repository.ChannelKind
	externalUserID string
	text           string
}

func (m *memDirectSender) SendDirect(_ context.Context, kind repository.ChannelKind, externalUserID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, directSend{kind: kind, externalUserID: externalUserID, text: text})
	return m.err
}

// sent is a copy of what was delivered, safe to read after the request.
func (m *memDirectSender) sent() []directSend {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]directSend(nil), m.calls...)
}
