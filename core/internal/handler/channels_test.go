package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// The three routes a signed-in person uses to connect a chat account, and what they render.
//
// The other half of linking has no test here because it has no route: an external id is
// attached to an account on the inbound path, where the proof is a code arriving over the
// channel. That path's tests live in internal/service.

func TestMintLinkCode_rendersACodeInTheShapeAChannelWillRecognise(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("mint@wingman.test", "Somebody connecting")

	// Act
	env := decode(t, f.do(http.MethodPost, "/v1/channels/link-codes", token, nil), http.StatusCreated)

	// Assert
	var view linkCodeView
	dataInto(t, env, &view)

	// The code the response carries has to be the one the inbound path will accept, so it is
	// asserted through domain's own reader rather than against a regexp copied into the test.
	// It is rendered in the grouped display form, which normalises back — that round trip is
	// the property that matters, because what a person sees is what they will paste.
	normalised, ok := domain.NormaliseLinkCode(view.Code)
	if !ok {
		t.Fatalf("the minted code %q does not normalise, so no channel could redeem it", view.Code)
	}
	if got := domain.FormatLinkCode(normalised); got != view.Code {
		t.Errorf("the rendered code %q is not the display form of what it normalises to (%q)", view.Code, got)
	}
	if want := testNow.Add(fixtureLinkCodeTTL); !view.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want %s", view.ExpiresAt, want)
	}
}

func TestMintLinkCode_neverRendersTheStoredHash(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("hash@wingman.test", "Somebody connecting")

	// Act
	rec := f.do(http.MethodPost, "/v1/channels/link-codes", token, nil)
	decode(t, rec, http.StatusCreated)

	// Assert
	//
	// The plaintext is in the body once, by design — this is the only moment it exists. What
	// must not be there is the value the table holds: a response carrying both would make the
	// hashing pointless, and a log of this response a set of live credentials.
	var stored []string
	f.channels.mu.Lock()
	for hash := range f.channels.codes {
		stored = append(stored, hash)
	}
	f.channels.mu.Unlock()

	if len(stored) != 1 {
		t.Fatalf("stored codes = %d, want exactly 1", len(stored))
	}
	assertBodyOmits(t, rec, map[string]string{"stored link-code hash": stored[0]})
}

func TestMintLinkCode_twiceLeavesOneLiveCode(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("twice@wingman.test", "Somebody impatient")

	var first, second linkCodeView
	dataInto(t, decode(t, f.do(http.MethodPost, "/v1/channels/link-codes", token, nil),
		http.StatusCreated), &first)

	// Act
	dataInto(t, decode(t, f.do(http.MethodPost, "/v1/channels/link-codes", token, nil),
		http.StatusCreated), &second)

	// Assert
	//
	// Asking again is the documented way to recover a lost code, and it has to invalidate the
	// old one: two live codes for one account is two chances for the wrong person to be
	// holding a working credential.
	if first.Code == second.Code {
		t.Error("the second code is the first one again; minting has to produce a new value")
	}
	f.channels.mu.Lock()
	live := len(f.channels.codes)
	f.channels.mu.Unlock()
	if live != 1 {
		t.Errorf("live codes = %d, want 1: minting again has to supersede the outstanding one", live)
	}
}

func TestListChannels_rendersOwnConnectionsWithRevokedOnesMarkedNotLive(t *testing.T) {
	// Arrange
	f := newFixture(t)
	mine, token := f.newAccount("mine@wingman.test", "The owner")
	other, _ := f.newAccount("other@wingman.test", "Somebody else")

	live := f.channels.seed(mine.ID, repository.ChannelTelegram, "tg-24680", "Owner on Telegram")
	gone := f.channels.seedRevoked(mine.ID, repository.ChannelSlack, "U0SLACK1", "Owner on Slack")
	theirs := f.channels.seed(other.ID, repository.ChannelDiscord, "dc-99999", "Somebody else on Discord")

	// Act
	env := decode(t, f.do(http.MethodGet, "/v1/channels", token, nil), http.StatusOK)

	// Assert
	var views []channelIdentityView
	dataInto(t, env, &views)

	if len(views) != 2 {
		t.Fatalf("connections = %d, want 2 (the caller's own, revoked included)", len(views))
	}
	byID := map[string]channelIdentityView{}
	for _, view := range views {
		byID[view.ID] = view
	}

	if got := byID[live.ID]; !got.IsLive || got.RevokedAt != nil {
		t.Errorf("the live connection rendered as isLive=%v revokedAt=%v", got.IsLive, got.RevokedAt)
	}
	// Revoked rows are rendered rather than filtered, because a person who disconnected
	// something wants to see that it is disconnected, not to see it disappear.
	if got := byID[gone.ID]; got.IsLive || got.RevokedAt == nil {
		t.Errorf("the revoked connection rendered as isLive=%v revokedAt=%v", got.IsLive, got.RevokedAt)
	}
	if _, found := byID[theirs.ID]; found {
		t.Error("the listing carried somebody else's connection")
	}
	// No pagination block, on the sessions listing's precedent: the service does not count
	// the table, and rendering a total it did not produce would be an invented number.
	if env.Meta.Pagination != nil {
		t.Error("the listing carried a pagination block")
	}

	// The external id is rendered on purpose — it is the only thing telling two accounts on
	// one platform apart — so assert it is the caller's own and no one else's.
	if got := byID[live.ID].ExternalID; got != "tg-24680" {
		t.Errorf("externalId = %q, want the caller's own", got)
	}
	if strings.Contains(env.Message, theirs.ExternalID) {
		t.Error("the message named another account's chat account")
	}
}

func TestUnlink_revokesTheRowAndSaysSo(t *testing.T) {
	// Arrange
	f := newFixture(t)
	mine, token := f.newAccount("unlink@wingman.test", "The owner")
	identity := f.channels.seed(mine.ID, repository.ChannelTelegram, "tg-13579", "Owner on Telegram")

	// Act
	env := decode(t, f.do(http.MethodDelete, "/v1/channels/"+identity.ID, token, nil), http.StatusOK)

	// Assert
	var view struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}
	dataInto(t, env, &view)
	if view.ID != identity.ID || !view.Revoked {
		t.Errorf("rendered %+v, want the identity id and revoked=true", view)
	}

	// The row, not just the response: a 200 over an unchanged table is the failure mode worth
	// catching, because the person believes they have disconnected.
	after, err := f.channels.ListForUser(context.Background(), mine.ID)
	if err != nil {
		t.Fatalf("read back the connections: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("connections = %d, want 1", len(after))
	}
	if after[0].Live() {
		t.Error("the connection is still live after being disconnected")
	}
}

func TestUnlink_twiceAndUnknownIdBothAnswerNotFound(t *testing.T) {
	// Arrange
	f := newFixture(t)
	mine, token := f.newAccount("twice-unlink@wingman.test", "The owner")
	identity := f.channels.seed(mine.ID, repository.ChannelTelegram, "tg-11223", "Owner on Telegram")
	decode(t, f.do(http.MethodDelete, "/v1/channels/"+identity.ID, token, nil), http.StatusOK)

	// Act + Assert
	//
	// Already revoked and never existed are the same answer, because the query is scoped to a
	// live row owned by the caller and does not read the row back to explain itself. A person
	// who clicks twice gets a 404 and has lost nothing.
	assertNotFound(t, f.do(http.MethodDelete, "/v1/channels/"+identity.ID, token, nil))
	assertNotFound(t, f.do(http.MethodDelete, "/v1/channels/identity-does-not-exist", token, nil))
}

func TestChannels_haveNoUpdateRoute(t *testing.T) {
	// Arrange
	f := newFixture(t)
	mine, token := f.newAccount("no-update@wingman.test", "The owner")
	identity := f.channels.seed(mine.ID, repository.ChannelTelegram, "tg-44556", "Owner on Telegram")

	// Act + Assert
	//
	// Deliberately absent rather than merely unimplemented. A connection is made by proving it
	// and ended by revoking it; an update would be a way to move somebody else's chat account
	// onto this account with a session and an id.
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		assertErrorCode(t, f.do(method, "/v1/channels/"+identity.ID, token, nil),
			http.StatusMethodNotAllowed, "NOT_FOUND")
	}
}

func TestReference_publishesEveryChannelKindAndTheOnesThisInstanceIsOn(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	env := decode(t, f.asOperator(http.MethodGet, "/v1/reference", nil), http.StatusOK)

	// Assert
	var view struct {
		ChannelKinds      []string `json:"channelKinds"`
		ChannelsConnected []string `json:"channelsConnected"`
	}
	dataInto(t, env, &view)

	// Every platform the build knows, in the enum's order, derived from the one list that
	// defines the set — so a kind added to domain and forgotten here fails rather than being
	// silently unpublishable.
	want := []string{}
	for _, kind := range domain.AllChannelKinds() {
		want = append(want, string(kind))
	}
	if strings.Join(view.ChannelKinds, ",") != strings.Join(want, ",") {
		t.Errorf("channelKinds = %v, want %v", view.ChannelKinds, want)
	}

	// And what this instance actually holds a connection to, which is the different question:
	// a code minted for a platform nothing is listening on can never be redeemed.
	if strings.Join(view.ChannelsConnected, ",") != string(domain.ChannelTelegram) {
		t.Errorf("channelsConnected = %v, want just telegram", view.ChannelsConnected)
	}
}

func TestReference_rendersAnEmptyListWhenNoChannelIsConnected(t *testing.T) {
	// Arrange
	f := newFixture(t, withNoChannelsConnected)

	// Act
	env := decode(t, f.asOperator(http.MethodGet, "/v1/reference", nil), http.StatusOK)

	// Assert
	//
	// An empty array, never null. A client that has to tell "none" from "the field is missing"
	// will get it wrong once, and the wrong way round is offering a connect button that cannot
	// work.
	if !strings.Contains(string(env.Data), `"channelsConnected":[]`) {
		t.Errorf("channelsConnected did not render as an empty array; data: %s", env.Data)
	}
}
