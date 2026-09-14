package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func channelRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "user_id", "kind", "external_id", "display_name", "linked_at", "revoked_at",
	})
}

func TestChannelRepository_link_attachesAChannelAccountToAWingmanAccount(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`INSERT INTO channel_identities \(user_id, kind, external_id, display_name\)`).
		WithArgs("usr_01", "telegram", "584213907", "Rina").
		WillReturnRows(channelRows().
			AddRow("chi_01", "usr_01", "telegram", "584213907", "Rina", fixedNow, nil))

	// Act
	identity, err := repo.Link(context.Background(), ChannelIdentity{
		UserID: "usr_01", Kind: ChannelTelegram,
		ExternalID: "  584213907 ", DisplayName: "Rina",
	})

	// Assert
	if err != nil {
		t.Fatalf("link identity: %v", err)
	}
	// The external id is trimmed before the write. A trailing newline from a pasted id
	// would otherwise store an identity that no inbound message ever resolves to,
	// leaving a link that looks connected and never works.
	if identity.ExternalID != "584213907" {
		t.Errorf("externalId = %q; want it trimmed", identity.ExternalID)
	}
	if !identity.Live() {
		t.Error("a freshly linked identity came back revoked")
	}
}

// The conflict deliberately says nothing about who holds the identity. Telling the caller
// "that Telegram account belongs to usr_02" is an account-existence oracle: anybody who
// can attempt a link could enumerate which channel accounts are already registered here.
func TestChannelRepository_link_anIdentityAlreadyHeldIsAConflictThatNamesNobody(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`INSERT INTO channel_identities`).
		WithArgs("usr_02", "telegram", "584213907", "").
		WillReturnError(pgError(pgUniqueViolation))

	// Act
	_, err := repo.Link(context.Background(), ChannelIdentity{
		UserID: "usr_02", Kind: ChannelTelegram, ExternalID: "584213907",
	})

	// Assert
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("link a held identity = %v; want ErrConflict", err)
	}
	// Re-pointing rather than refusing would hand one person's conversations to another,
	// which is why the unique constraint exists and is not upserted around.
	if strings.Contains(err.Error(), "usr_01") {
		t.Errorf("the conflict named the holding account: %v", err)
	}
}

func TestChannelRepository_link_refusesAnIdentityWithNoExternalIDWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewChannelRepository(db)

	// Act
	_, err := repo.Link(context.Background(), ChannelIdentity{
		UserID: "usr_01", Kind: ChannelTelegram, ExternalID: "   ",
	})

	// Assert
	// An empty external id is a link no message can match, and it would occupy the
	// unique slot for the empty string on that channel.
	if err == nil {
		t.Fatal("an identity with no external id was accepted")
	}
}

// Resolve is what every inbound message passes through before anything else happens, and
// this test pins the revoked exclusion as SQL. Somebody who unlinked their Telegram
// account has withdrawn permission for it to spend their tokens.
func TestChannelRepository_resolve_findsTheAccountBehindALiveSender(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`FROM channel_identities\s+WHERE kind = \$1 AND external_id = \$2 AND revoked_at IS NULL`).
		WithArgs("telegram", "584213907").
		WillReturnRows(channelRows().
			AddRow("chi_01", "usr_01", "telegram", "584213907", "Rina", fixedNow, nil))

	// Act
	identity, err := repo.Resolve(context.Background(), ChannelTelegram, "584213907")

	// Assert
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	// This is the only thing that ties an inbound message to an account, and therefore
	// to a token budget. A resolve that returned the wrong account would spend somebody
	// else's allowance.
	if identity.UserID != "usr_01" {
		t.Errorf("resolved to %q; want usr_01", identity.UserID)
	}
}

func TestChannelRepository_resolve_aRevokedLinkResolvesToNobody(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`WHERE kind = \$1 AND external_id = \$2 AND revoked_at IS NULL`).
		WithArgs("telegram", "584213907").
		WillReturnRows(channelRows())

	// Act
	_, err := repo.Resolve(context.Background(), ChannelTelegram, "584213907")

	// Assert
	// The message arrives as if from a stranger, and the channel handler's answer to
	// that is a link code — not a session, and not a task.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve a revoked identity = %v; want ErrNotFound", err)
	}
}

// A database that is down must not read as "this sender is not linked". Folding the two
// together would make an outage look like every user having unlinked at once — and, worse,
// would send every inbound message down the link-code path as if it were a new person.
func TestChannelRepository_resolve_anOutageIsNotAnUnlinkedSender(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`FROM channel_identities`).
		WithArgs("slack", "U024BE7LH").
		WillReturnError(errors.New("connection refused"))

	// Act
	_, err := repo.Resolve(context.Background(), ChannelSlack, "U024BE7LH")

	// Assert
	if err == nil {
		t.Fatal("a failed resolve was reported as a successful one")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("an outage was reported as an unlinked sender: %v", err)
	}
}

func TestChannelRepository_listForUser_includesRevokedLinks(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	revoked := fixedNow
	mock.ExpectQuery(`FROM channel_identities\s+WHERE user_id = \$1\s+ORDER BY kind ASC, linked_at DESC`).
		WithArgs("usr_01").
		WillReturnRows(channelRows().
			AddRow("chi_02", "usr_01", "slack", "U024BE7LH", "rina", fixedNow, nil).
			AddRow("chi_01", "usr_01", "telegram", "584213907", "Rina", fixedNow, revoked))

	// Act
	identities, err := repo.ListForUser(context.Background(), "usr_01")

	// Assert
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("got %d identities; want 2", len(identities))
	}
	// "This was connected and then disconnected" is something a person needs to be able
	// to see. Hiding it would make an unlink somebody did not perform invisible.
	if identities[1].Live() {
		t.Error("the revoked link came back live")
	}
	if !identities[0].Live() {
		t.Error("the live link came back revoked")
	}
}

// The row is updated rather than deleted, and this test matches the statement to prove it.
// A delete would free the unique slot, and then any account could claim an identity
// another one had disconnected.
func TestChannelRepository_revoke_keepsTheRowSoTheIdentityStaysHeld(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectExec(`UPDATE channel_identities SET revoked_at = \$3\s+WHERE id = \$2 AND user_id = \$1 AND revoked_at IS NULL`).
		WithArgs("usr_01", "chi_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.Revoke(context.Background(), "usr_01", "chi_01", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("revoke identity: %v", err)
	}
}

func TestChannelRepository_revoke_anotherAccountsIdentityIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectExec(`WHERE id = \$2 AND user_id = \$1 AND revoked_at IS NULL`).
		WithArgs("usr_02", "chi_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.Revoke(context.Background(), "usr_02", "chi_01", fixedNow)

	// Assert
	// Not found, not yours and already revoked all answer the same. The last of those is
	// deliberate: the caller wanted it unlinked, and it is.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke another account's identity = %v; want ErrNotFound", err)
	}
}

func TestChannelRepository_revoke_refusesAZeroTimeWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewChannelRepository(db)

	// Act
	err := repo.Revoke(context.Background(), "usr_01", "chi_01", time.Time{})

	// Assert
	// revoked_at is what Resolve reads to decide the link is dead. A zero timestamp
	// written into it would be a revocation dated year one — still non-NULL, so the link
	// would stop working, but with no usable record of when permission was withdrawn.
	if err == nil {
		t.Fatal("a revocation with no time was accepted")
	}
}

func TestChannelRepository_relink_isScopedToTheAccountThatHeldTheIdentity(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`UPDATE channel_identities\s+SET revoked_at = NULL, linked_at = \$4\s+WHERE user_id = \$1 AND kind = \$2 AND external_id = \$3 AND revoked_at IS NOT NULL`).
		WithArgs("usr_01", "telegram", "584213907", fixedNow).
		WillReturnRows(channelRows().
			AddRow("chi_01", "usr_01", "telegram", "584213907", "Rina", fixedNow, nil))

	// Act
	identity, err := repo.Relink(context.Background(), "usr_01", ChannelTelegram, "584213907", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("relink identity: %v", err)
	}
	// The alternative to this method is deleting the revoked row so Link can succeed,
	// and that delete is exactly what lets a different account claim the identity. The
	// user_id in the WHERE clause is the thing being pinned here.
	if !identity.Live() || identity.UserID != "usr_01" {
		t.Errorf("identity = %+v; want a live link owned by usr_01", identity)
	}
}

func TestChannelRepository_relink_anIdentityAnotherAccountRevokedIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`WHERE user_id = \$1 AND kind = \$2 AND external_id = \$3 AND revoked_at IS NOT NULL`).
		WithArgs("usr_02", "telegram", "584213907", fixedNow).
		WillReturnRows(channelRows())

	// Act
	_, err := repo.Relink(context.Background(), "usr_02", ChannelTelegram, "584213907", fixedNow)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("relink another account's identity = %v; want ErrNotFound", err)
	}
}

func TestChannelRepository_relink_aLinkThatWasNeverRevokedIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`revoked_at IS NOT NULL`).
		WithArgs("usr_01", "discord", "312455901", fixedNow).
		WillReturnRows(channelRows())

	// Act
	_, err := repo.Relink(context.Background(), "usr_01", ChannelDiscord, "312455901", fixedNow)

	// Assert
	// `revoked_at IS NOT NULL` keeps this from being a general-purpose way to move
	// linked_at on a live link, which would misdate when the connection was made.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("relink a live link = %v; want ErrNotFound", err)
	}
}

func TestChannelRepository_relink_refusesAZeroTimeWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewChannelRepository(db)

	// Act
	_, err := repo.Relink(context.Background(), "usr_01", ChannelTelegram, "584213907", time.Time{})

	// Assert
	if err == nil {
		t.Fatal("a relink with no time was accepted")
	}
}

// WhatsApp is the value this is really about: it is spelled in the PRD and in the
// roadmap, so it is the one somebody will reach for before the account-risk decision
// behind it has been made. The enum refuses it, and refusing here means the error names
// the value instead of arriving as a 22P02 from the driver.
func TestChannelRepository_everyEntryPointRefusesAKindTheEnumDoesNotHave(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewChannelRepository(db)
	whatsapp := ChannelKind("whatsapp")

	// Act
	_, linkErr := repo.Link(context.Background(), ChannelIdentity{
		UserID: "usr_01", Kind: whatsapp, ExternalID: "628123456789",
	})
	_, resolveErr := repo.Resolve(context.Background(), whatsapp, "628123456789")
	_, relinkErr := repo.Relink(context.Background(), "usr_01", whatsapp, "628123456789", fixedNow)

	// Assert
	// No query is expected on any of the three; the mock's expectation check fails the
	// test if one were made.
	for name, err := range map[string]error{
		"link": linkErr, "resolve": resolveErr, "relink": relinkErr,
	} {
		if err == nil {
			t.Errorf("%s accepted a kind the channel_kind enum does not have", name)
			continue
		}
		if !strings.Contains(err.Error(), "whatsapp") {
			t.Errorf("%s error does not name the offending value: %v", name, err)
		}
	}
}

func TestChannelRepository_resolve_trimsTheSenderIDBeforeMatching(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChannelRepository(db)
	mock.ExpectQuery(`FROM channel_identities`).
		WithArgs("discord", "312455901").
		WillReturnRows(channelRows().
			AddRow("chi_03", "usr_01", "discord", "312455901", "rina", fixedNow, nil))

	// Act
	identity, err := repo.Resolve(context.Background(), ChannelDiscord, " 312455901\n")

	// Assert
	// Link trims on the way in, so Resolve has to trim on the way out or an id that
	// arrived with whitespace would never match the row it created.
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if identity.ID != "chi_03" {
		t.Errorf("identity id = %q; want chi_03", identity.ID)
	}
}
