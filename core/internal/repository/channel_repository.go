package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// ChannelKind is a connected messaging platform. The set mirrors the channel_kind enum
// in migration 000001; WhatsApp is absent because the account-risk decision behind it
// is unresolved, and a value the database refuses is better than one it stores.
//
// It is an alias rather than a second type: the adapters, the inbound ladder and this
// table all name the same three platforms, and two types that convert cleanly into each
// other are two places for a fourth platform to be added to only one of.
type ChannelKind = domain.ChannelKind

const (
	ChannelTelegram = domain.ChannelTelegram
	ChannelSlack    = domain.ChannelSlack
	ChannelDiscord  = domain.ChannelDiscord
)

// ChannelIdentity links a person's account on a channel to their Wingman account.
type ChannelIdentity struct {
	ID          string
	UserID      string
	Kind        ChannelKind
	ExternalID  string
	DisplayName string
	LinkedAt    time.Time
	RevokedAt   *time.Time
}

// Live reports whether the link is still usable.
func (c ChannelIdentity) Live() bool { return c.RevokedAt == nil }

// ChannelRepository stores the links between channel accounts and Wingman accounts.
//
// It answers the question every inbound message has to pass through before anything
// else happens: whose account is this, and may it spend? An unlinked sender resolves to
// ErrNotFound, and the channel handler's answer to that is a link code — not a session,
// and not a task.
type ChannelRepository struct {
	db *sqlx.DB
}

// NewChannelRepository builds a repository over the given pool.
func NewChannelRepository(db *sqlx.DB) *ChannelRepository {
	return &ChannelRepository{db: db}
}

const channelColumns = "id, user_id, kind, external_id, display_name, linked_at, revoked_at"

// Link attaches a channel account to a Wingman account.
//
// A channel account already linked to somebody returns ErrConflict rather than moving.
// The unique constraint is what enforces it, and the reason it exists is that silently
// re-pointing an identity would hand one person's conversations to another.
func (r *ChannelRepository) Link(ctx context.Context, identity ChannelIdentity) (ChannelIdentity, error) {
	const query = `
		INSERT INTO channel_identities (user_id, kind, external_id, display_name)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + channelColumns

	if err := validChannelKind(identity.Kind); err != nil {
		return ChannelIdentity{}, err
	}
	externalID := strings.TrimSpace(identity.ExternalID)
	if externalID == "" {
		return ChannelIdentity{}, errors.New("channel identity: external id is required")
	}

	var row channelRow
	err := r.db.QueryRowxContext(ctx, query,
		identity.UserID, string(identity.Kind), externalID, identity.DisplayName,
	).StructScan(&row)
	if err != nil {
		// The conflict is not annotated with which account holds the identity. That
		// answer would tell whoever asked something about somebody else's account.
		return ChannelIdentity{}, fmt.Errorf("link %s identity: %w", identity.Kind, classify(err))
	}
	return row.toIdentity(), nil
}

// Resolve finds the account behind a sender on a channel.
//
// This is the hot path for every inbound message, and it deliberately excludes revoked
// links: somebody who unlinked their Telegram account has withdrawn permission for it
// to spend their tokens, so the message arrives as if from a stranger.
func (r *ChannelRepository) Resolve(ctx context.Context, kind ChannelKind, externalID string) (ChannelIdentity, error) {
	const query = `
		SELECT ` + channelColumns + `
		FROM channel_identities
		WHERE kind = $1 AND external_id = $2 AND revoked_at IS NULL`

	if err := validChannelKind(kind); err != nil {
		return ChannelIdentity{}, err
	}

	var row channelRow
	err := r.db.QueryRowxContext(ctx, query, string(kind), strings.TrimSpace(externalID)).StructScan(&row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelIdentity{}, ErrNotFound
		}
		// Wrapped rather than folded into ErrNotFound: a database that is down must
		// not read as "this sender is not linked", or an outage would silently look
		// like every user having unlinked at once.
		return ChannelIdentity{}, fmt.Errorf("resolve %s identity: %w", kind, classify(err))
	}
	return row.toIdentity(), nil
}

// ListForUser returns one account's links, including revoked ones.
//
// Revoked links are included on purpose: "this was connected and then disconnected" is
// something a person needs to be able to see, and hiding it would make an unlink
// somebody did not perform invisible.
func (r *ChannelRepository) ListForUser(ctx context.Context, userID string) ([]ChannelIdentity, error) {
	const query = `
		SELECT ` + channelColumns + `
		FROM channel_identities
		WHERE user_id = $1
		ORDER BY kind ASC, linked_at DESC`

	rows := []channelRow{}
	if err := r.db.SelectContext(ctx, &rows, query, userID); err != nil {
		return nil, fmt.Errorf("list channel identities: %w", classify(err))
	}

	identities := make([]ChannelIdentity, 0, len(rows))
	for _, row := range rows {
		identities = append(identities, row.toIdentity())
	}
	return identities, nil
}

// Revoke unlinks one of a user's channel identities.
//
// The row is kept rather than deleted, so the unique constraint still holds the
// identity: re-linking it to a different account has to be a deliberate act, not
// something that happens because a row went away.
func (r *ChannelRepository) Revoke(ctx context.Context, userID, identityID string, at time.Time) error {
	const query = `
		UPDATE channel_identities SET revoked_at = $3
		WHERE id = $2 AND user_id = $1 AND revoked_at IS NULL`

	if at.IsZero() {
		return errors.New("channel identity: revocation needs a time")
	}

	result, err := r.db.ExecContext(ctx, query, userID, identityID, at)
	if err != nil {
		return fmt.Errorf("revoke channel identity: %w", classify(err))
	}
	// Not found and not yours answer the same. So does already revoked — the caller
	// wanted it unlinked, and it is.
	return requireOneRow(result, identityID)
}

// Relink restores a revoked link the same account previously held.
//
// It is scoped to the owning account, which is the whole point: the alternative to this
// method is deleting the revoked row so Link can succeed, and a delete would let any
// account claim an identity another one had disconnected.
func (r *ChannelRepository) Relink(ctx context.Context, userID string, kind ChannelKind, externalID string, at time.Time) (ChannelIdentity, error) {
	const query = `
		UPDATE channel_identities
		SET revoked_at = NULL, linked_at = $4
		WHERE user_id = $1 AND kind = $2 AND external_id = $3 AND revoked_at IS NOT NULL
		RETURNING ` + channelColumns

	if err := validChannelKind(kind); err != nil {
		return ChannelIdentity{}, err
	}
	if at.IsZero() {
		return ChannelIdentity{}, errors.New("channel identity: relinking needs a time")
	}

	var row channelRow
	err := r.db.QueryRowxContext(ctx, query,
		userID, string(kind), strings.TrimSpace(externalID), at).StructScan(&row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelIdentity{}, ErrNotFound
		}
		return ChannelIdentity{}, fmt.Errorf("relink %s identity: %w", kind, classify(err))
	}
	return row.toIdentity(), nil
}

// validChannelKind refuses a kind the enum does not have.
//
// The database would refuse it too, as a 22P02. Refusing here instead means the error
// names the offending value and does not consume a connection to find out.
func validChannelKind(kind ChannelKind) error {
	if !kind.Valid() {
		return fmt.Errorf("channel identity: %q is not a channel", kind)
	}
	return nil
}

type channelRow struct {
	ID          string       `db:"id"`
	UserID      string       `db:"user_id"`
	Kind        string       `db:"kind"`
	ExternalID  string       `db:"external_id"`
	DisplayName string       `db:"display_name"`
	LinkedAt    time.Time    `db:"linked_at"`
	RevokedAt   sql.NullTime `db:"revoked_at"`
}

func (r channelRow) toIdentity() ChannelIdentity {
	return ChannelIdentity{
		ID:          r.ID,
		UserID:      r.UserID,
		Kind:        ChannelKind(r.Kind),
		ExternalID:  r.ExternalID,
		DisplayName: r.DisplayName,
		LinkedAt:    r.LinkedAt,
		RevokedAt:   timePtr(r.RevokedAt),
	}
}
