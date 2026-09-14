package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// How long a link code stays good for.
//
// Short, because this is the only credential in the system that travels through a third
// party's servers: it is typed into Telegram or Slack, where it stays in a history that
// its owner cannot delete from both sides. Fifteen minutes is long enough to switch
// windows and paste, and short enough that the copy left behind is worthless.
//
// Zero means unset and takes the default, never "no expiry", and both bounds are
// enforced here as well as in config — the same rule every other limit in core follows,
// because a limit that only exists in the parser is a limit the next caller does not
// have.
const (
	defaultLinkCodeTTL = 15 * time.Minute
	minLinkCodeTTL     = time.Minute
	maxLinkCodeTTL     = time.Hour
)

// ChannelsDeps is what the channel service needs.
type ChannelsDeps struct {
	Codes LinkCodeMinter
	Links ChannelLinks
	// Janitor is optional, on SessionsDeps' precedent: without one Sweep is a no-op,
	// and an instance that never sweeps is untidy rather than unsafe, because a spent
	// code is refused by Consume whether or not the row is still there.
	Janitor LinkCodeJanitor

	// CodeTTL is CHANNEL_LINK_CODE_TTL, clamped by the constants above.
	CodeTTL time.Duration

	Clock  Clock
	Logger zerolog.Logger
}

// Channels is the signed-in half of connecting a chat account: asking for a code,
// reading back what is connected, and disconnecting one.
//
// Making a link is not here. That happens on the inbound path, in Inbox, because the
// proof that a chat account belongs to somebody is a code arriving *from* it — and a
// service reached by a session must not be able to attach an arbitrary external id to
// the account holding it. The two halves of linking are two services on purpose.
type Channels struct {
	codes   LinkCodeMinter
	links   ChannelLinks
	janitor LinkCodeJanitor

	ttl time.Duration

	clock Clock
	log   zerolog.Logger
}

// NewChannels validates its wiring and returns a ready service.
func NewChannels(deps ChannelsDeps) (*Channels, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Codes != nil, "Codes")
	require(deps.Links != nil, "Links")
	if len(missing) > 0 {
		return nil, fmt.Errorf("channels: missing dependencies: %v", missing)
	}

	c := &Channels{
		codes:   deps.Codes,
		links:   deps.Links,
		janitor: deps.Janitor,
		ttl:     deps.CodeTTL,
		clock:   deps.Clock,
		log:     deps.Logger,
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	switch {
	case c.ttl <= 0:
		c.ttl = defaultLinkCodeTTL
	case c.ttl < minLinkCodeTTL:
		c.ttl = minLinkCodeTTL
	case c.ttl > maxLinkCodeTTL:
		c.ttl = maxLinkCodeTTL
	}
	return c, nil
}

// Minted is a link code, as the person who asked for it is shown it.
//
// Once. There is no route that reads a code back: the database holds only the hash, so
// this struct is the only place the value exists after the request that made it, and
// somebody who loses it mints another. That is cheaper than a way to retrieve one, which
// would turn a session into a permanent supply of channel credentials.
type Minted struct {
	// Code is grouped for reading aloud and typing back — WGM-ABCDE-FGHJK. The inbound
	// path normalises the grouping away, so what is displayed and what is stored are
	// the same code.
	Code      string
	ExpiresAt time.Time
}

// MintLinkCode issues a code for the caller's own account.
func (c *Channels) MintLinkCode(ctx context.Context, actor Actor) (Minted, error) {
	actor, err := actor.prepare()
	if err != nil {
		return Minted{}, err
	}
	userID, err := actor.Owner()
	if err != nil {
		return Minted{}, err
	}

	plaintext, stored, err := auth.NewLinkCode()
	if err != nil {
		return Minted{}, fmt.Errorf("mint link code: %w", err)
	}

	// Mint discards whatever this account had outstanding, in the same statement. One
	// live code per person means a code read over somebody's shoulder is dead as soon as
	// they ask for another.
	code, err := c.codes.Mint(ctx, repository.NewLinkCode{
		UserID:    userID,
		CodeHash:  stored,
		ExpiresAt: c.clock().Add(c.ttl),
	})
	if err != nil {
		return Minted{}, err
	}

	c.log.Info().Str("userId", userID).Time("expiresAt", code.ExpiresAt).
		Msg("channel link code minted")
	// The row's expiry rather than the one just computed: what is returned is what will
	// be enforced, even if the two ever disagree.
	return Minted{Code: domain.FormatLinkCode(plaintext), ExpiresAt: code.ExpiresAt}, nil
}

// List is the caller's own connected chat accounts, including the ones they revoked.
//
// Revoked rows come back because they are the answer to "why can I not connect this
// again": a chat account that was linked and released is a row somebody has to be able
// to see to understand. The repository decides that; this service only scopes it.
func (c *Channels) List(ctx context.Context, actor Actor) ([]repository.ChannelIdentity, error) {
	actor, err := actor.prepare()
	if err != nil {
		return nil, err
	}
	userID, err := actor.Owner()
	if err != nil {
		return nil, err
	}
	return c.links.ListForUser(ctx, userID)
}

// Unlink disconnects one of the caller's chat accounts.
//
// After this the sender is a stranger again: their next message gets the same answer any
// unlinked sender gets, and nothing they send starts a run. That is the whole reason this
// route exists — it is how somebody takes their phone out of the loop.
func (c *Channels) Unlink(ctx context.Context, identityID string, actor Actor) error {
	actor, err := actor.prepare()
	if err != nil {
		return err
	}
	userID, err := actor.Owner()
	if err != nil {
		return err
	}
	if strings.TrimSpace(identityID) == "" {
		return fmt.Errorf("%w: an identity id is required", ErrValidation)
	}

	if err := c.links.Revoke(ctx, userID, identityID, c.clock()); err != nil {
		return mapAbsence(err)
	}

	c.log.Info().Str("userId", userID).Str("identityId", identityID).
		Msg("channel identity revoked")
	return nil
}

// Sweep deletes link codes that were spent or expired, and reports how many.
//
// Like Sessions.Sweep it takes no Actor, because no request reaches it: cmd runs it on a
// ticker. A code that outlives its expiry is already refused by Consume, so this is
// hygiene — the table is meant to hold at most one live row per account, and without a
// sweep it would instead hold every code ever minted.
func (c *Channels) Sweep(ctx context.Context) (int, error) {
	if c.janitor == nil {
		return 0, nil
	}
	deleted, err := c.janitor.DeleteSpent(ctx, c.clock())
	if err != nil {
		return 0, fmt.Errorf("sweep spent link codes: %w", err)
	}
	if deleted > 0 {
		c.log.Info().Int("linkCodesDeleted", deleted).Msg("spent link codes swept")
	}
	return deleted, nil
}
