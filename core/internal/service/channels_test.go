package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// channelsFixture wires the signed-in half of linking over the two fakes, both kept to
// hand: what a code looks like from outside is one assertion, and what was stored for it
// is the other.
type channelsFixture struct {
	channels   *Channels
	codes      *fakeLinkCodes
	identities *fakeIdentities
}

func newChannelsFixture(t *testing.T, ttl time.Duration, withJanitor bool) channelsFixture {
	t.Helper()

	codes := newFakeLinkCodes()
	identities := newFakeIdentities()
	deps := ChannelsDeps{
		Codes:   codes,
		Links:   identities,
		CodeTTL: ttl,
		Clock:   fixedClock(testNow),
		Logger:  silentLogger(),
	}
	if withJanitor {
		deps.Janitor = codes
	}
	channels, err := NewChannels(deps)
	if err != nil {
		t.Fatalf("NewChannels: %v", err)
	}
	return channelsFixture{channels: channels, codes: codes, identities: identities}
}

func TestNewChannels_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewChannels(ChannelsDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Codes", "Links"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestNewChannels_withoutAJanitorIsAValidInstance(t *testing.T) {
	// Arrange — sweeping is hygiene: a spent or expired code is refused by Consume whether
	// or not its row is still in the table.
	codes := newFakeLinkCodes()

	// Act
	_, err := NewChannels(ChannelsDeps{Codes: codes, Links: newFakeIdentities()})

	// Assert
	if err != nil {
		t.Fatalf("NewChannels: %v", err)
	}
}

func TestNewChannels_clampsTheCodeLifetimeAtBothEnds(t *testing.T) {
	// Arrange — zero is unset and takes the default, never "no expiry"; the floor and the
	// ceiling are enforced here as well as in config, because a bound that only exists in
	// the parser is a bound the next caller does not have.
	cases := []struct {
		name  string
		given time.Duration
		want  time.Duration
	}{
		{"unset takes the default", 0, defaultLinkCodeTTL},
		{"negative takes the default", -time.Hour, defaultLinkCodeTTL},
		{"below the floor is raised", time.Millisecond, minLinkCodeTTL},
		{"in range is honoured", 5 * time.Minute, 5 * time.Minute},
		{"above the ceiling is lowered", 24 * time.Hour, maxLinkCodeTTL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newChannelsFixture(t, tc.given, false)

			// Act
			minted, err := f.channels.MintLinkCode(context.Background(), userActor("user-1"))

			// Assert
			if err != nil {
				t.Fatalf("MintLinkCode: %v", err)
			}
			if got := minted.ExpiresAt.Sub(testNow); got != tc.want {
				t.Errorf("lifetime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChannelsMintLinkCode_storesOnlyTheHashOfWhatItShows(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)

	// Act
	minted, err := f.channels.MintLinkCode(context.Background(), userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("MintLinkCode: %v", err)
	}
	// Displayed grouped, so it can be read aloud and typed back, and it has to normalise
	// to the value that was hashed — otherwise what is on screen is a code nobody can use.
	canonical, ok := domain.NormaliseLinkCode(minted.Code)
	if !ok {
		t.Fatalf("code = %q, want something the inbound path can read back", minted.Code)
	}
	if strings.Count(minted.Code, "-") != 2 {
		t.Errorf("code = %q, want it grouped for reading", minted.Code)
	}

	stored, ok := f.codes.codes[auth.HashToken(canonical)]
	if !ok {
		t.Fatal("no row was stored under the hash of the code that was shown")
	}
	if stored.UserID != "user-1" {
		t.Errorf("userId = %q, want the caller's", stored.UserID)
	}
	// The code itself is nowhere in the table. A dump of it is a list of codes nobody can
	// use, which is the only reason the hash exists.
	for hash := range f.codes.codes {
		if hash == canonical || hash == minted.Code {
			t.Error("the code was stored in plaintext")
		}
	}
}

func TestChannelsMintLinkCode_replacesWhateverTheAccountHadOutstanding(t *testing.T) {
	// Arrange — one live code per person, so a code read over somebody's shoulder is dead
	// as soon as they ask for another.
	f := newChannelsFixture(t, 0, false)
	first, err := f.channels.MintLinkCode(context.Background(), userActor("user-1"))
	if err != nil {
		t.Fatalf("MintLinkCode: %v", err)
	}

	// Act
	second, err := f.channels.MintLinkCode(context.Background(), userActor("user-1"))
	if err != nil {
		t.Fatalf("MintLinkCode: %v", err)
	}

	// Assert
	firstCanonical, _ := domain.NormaliseLinkCode(first.Code)
	if _, err := f.codes.Consume(context.Background(), auth.HashToken(firstCanonical), testNow); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("consuming the replaced code: err = %v, want it gone", err)
	}
	secondCanonical, _ := domain.NormaliseLinkCode(second.Code)
	if _, err := f.codes.Consume(context.Background(), auth.HashToken(secondCanonical), testNow); err != nil {
		t.Errorf("consuming the new code: %v", err)
	}
}

func TestChannelsMintLinkCode_isNotSomethingAMachineKeyCanDo(t *testing.T) {
	// Arrange — a link attaches a chat account to a Wingman account, and a machine key has
	// none. There is no account for it to mint against.
	f := newChannelsFixture(t, 0, false)

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			_, err := f.channels.MintLinkCode(context.Background(), actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if f.codes.minted != 0 {
				t.Errorf("minted = %d, want nothing issued", f.codes.minted)
			}
		})
	}
}

func TestChannelsMintLinkCode_reportsAStoreFailureAsItself(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)
	f.codes.failMint = errBoom

	// Act
	_, err := f.channels.MintLinkCode(context.Background(), userActor("user-1"))

	// Assert — not turned into a validation error: nothing about the request was wrong.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Error("a store failure was reported as a bad request")
	}
}

func TestChannelsList_showsTheCallersOwnConnectionsIncludingRevokedOnes(t *testing.T) {
	// Arrange — two of the caller's, one of them released, and one of somebody else's.
	f := newChannelsFixture(t, 0, false)
	f.identities.seed("user-1", domain.ChannelTelegram, "tg-1")
	f.identities.seedRevoked("user-1", domain.ChannelSlack, "slack-1")
	f.identities.seed("user-2", domain.ChannelDiscord, "discord-1")

	// Act
	got, err := f.channels.List(context.Background(), userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The revoked row is included on purpose: it is the answer to "why can I not connect
	// this again", which nobody can work out from a list that hides it.
	if len(got) != 2 {
		t.Fatalf("identities = %d, want the caller's two", len(got))
	}
	for _, identity := range got {
		if identity.UserID != "user-1" {
			t.Errorf("userId = %q, want only the caller's rows", identity.UserID)
		}
	}
}

func TestChannelsList_isNotSomethingAMachineKeyCanRead(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			_, err := f.channels.List(context.Background(), actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestChannelsUnlink_releasesTheCallersOwnConnectionAtTheClock(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)
	identity := f.identities.seed("user-1", domain.ChannelTelegram, "tg-1")

	// Act
	err := f.channels.Unlink(context.Background(), identity.ID, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	stored := f.identities.identities[identity.ID]
	if stored.Live() {
		t.Fatal("the identity is still live after being unlinked")
	}
	if !stored.RevokedAt.Equal(testNow) {
		t.Errorf("revokedAt = %v, want the service's clock %v", stored.RevokedAt, testNow)
	}
}

func TestChannelsUnlink_somebodyElsesConnectionIsSimplyNotFound(t *testing.T) {
	// Arrange — the id exists, and it is not the caller's.
	f := newChannelsFixture(t, 0, false)
	identity := f.identities.seed("user-2", domain.ChannelTelegram, "tg-1")

	// Act
	err := f.channels.Unlink(context.Background(), identity.ID, userActor("user-1"))

	// Assert — not forbidden: "you may not touch this one" confirms it exists, and a
	// caller sweeping ids should learn nothing from the difference.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !f.identities.identities[identity.ID].Live() {
		t.Error("somebody else's connection was revoked")
	}
}

func TestChannelsUnlink_anUnknownIdAndAnEmptyIdAreDifferentAnswers(t *testing.T) {
	// Arrange — an empty id is a client that did not fill in the request; an unknown one is
	// a well-formed request about a row that is not there.
	f := newChannelsFixture(t, 0, false)

	// Act
	empty := f.channels.Unlink(context.Background(), "  ", userActor("user-1"))
	unknown := f.channels.Unlink(context.Background(), "identity-404", userActor("user-1"))

	// Assert
	if !errors.Is(empty, ErrValidation) {
		t.Errorf("empty id: err = %v, want ErrValidation", empty)
	}
	if !errors.Is(unknown, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", unknown)
	}
}

func TestChannelsUnlink_isNotSomethingAMachineKeyCanDo(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)
	identity := f.identities.seed("user-1", domain.ChannelTelegram, "tg-1")

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			err := f.channels.Unlink(context.Background(), identity.ID, actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestChannelsUnlink_reportsAStoreFailureAsItself(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)
	identity := f.identities.seed("user-1", domain.ChannelTelegram, "tg-1")
	f.identities.failRevoke = errBoom

	// Act
	err := f.channels.Unlink(context.Background(), identity.ID, userActor("user-1"))

	// Assert — a store that is down is not a row that is missing. Reporting it as
	// ErrNotFound would tell somebody their connection is already gone when it is not.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a store failure was reported as an absent row")
	}
}

func TestChannelsSweep_withoutAJanitorDoesNothingAndSaysSo(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, false)
	f.codes.seedCode("user-1", "hash-expired", testNow.Add(-time.Hour))

	// Act
	deleted, err := f.channels.Sweep(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 from an instance with no janitor", deleted)
	}
	if len(f.codes.codes) != 1 {
		t.Errorf("codes = %d, want the row left alone", len(f.codes.codes))
	}
}

func TestChannelsSweep_deletesWhatIsSpentOrExpiredAndNothingLive(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, true)
	f.codes.seedCode("user-1", "hash-live", testNow.Add(time.Hour))
	f.codes.seedCode("user-2", "hash-expired", testNow.Add(-time.Minute))
	// Spent but not yet expired: a code somebody used a minute ago is finished with,
	// whatever its expiry says.
	f.codes.seedCode("user-3", "hash-spent", testNow.Add(time.Hour))
	consumed := testNow.Add(-time.Minute)
	f.codes.codes["hash-spent"].ConsumedAt = &consumed

	// Act
	deleted, err := f.channels.Sweep(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want the expired one and the spent one", deleted)
	}
	if _, ok := f.codes.codes["hash-live"]; !ok {
		t.Error("a live code was swept")
	}
}

func TestChannelsSweep_wrapsAJanitorFailureWithWhatItWasDoing(t *testing.T) {
	// Arrange
	f := newChannelsFixture(t, 0, true)
	f.codes.failDelete = errBoom

	// Act
	deleted, err := f.channels.Sweep(context.Background())

	// Assert — the ticker logs this without a request id to correlate against, so the
	// message has to say what was being attempted.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the janitor's failure", err)
	}
	if !strings.Contains(err.Error(), "sweep spent link codes") {
		t.Errorf("err = %v, want it to say what it was doing", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 on a failure", deleted)
	}
}
