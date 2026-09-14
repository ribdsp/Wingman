package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// linkCodeView is a freshly minted code, rendered the only time it exists.
//
// There is no route that reads one back, and this is why: the database holds a hash, so
// after this response the value exists nowhere except in the hands of whoever asked. A
// person who loses it mints another, which is cheaper than an endpoint that would turn a
// session into a standing supply of channel credentials.
type linkCodeView struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// channelIdentityView is one connected chat account, as its owner sees it.
//
// The external id is rendered because it is the only thing that tells two accounts on one
// platform apart, and the person reading this list is the person deciding which to
// disconnect. It is their own id on their own list — the same reasoning that puts an IP
// address on the session list.
//
// Revoked rows are included, and isLive is rendered rather than left to a client to derive
// from revokedAt. Two clients working out "can this still send me anything?" independently
// is two chances to work it out differently.
type channelIdentityView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	ExternalID  string     `json:"externalId"`
	DisplayName string     `json:"displayName"`
	LinkedAt    time.Time  `json:"linkedAt"`
	RevokedAt   *time.Time `json:"revokedAt"`
	IsLive      bool       `json:"isLive"`
}

func viewChannelIdentity(identity repository.ChannelIdentity) channelIdentityView {
	return channelIdentityView{
		ID:          identity.ID,
		Kind:        string(identity.Kind),
		ExternalID:  identity.ExternalID,
		DisplayName: identity.DisplayName,
		LinkedAt:    identity.LinkedAt,
		RevokedAt:   identity.RevokedAt,
		IsLive:      identity.Live(),
	}
}

func viewChannelIdentities(identities []repository.ChannelIdentity) []channelIdentityView {
	views := make([]channelIdentityView, 0, len(identities))
	for _, identity := range identities {
		views = append(views, viewChannelIdentity(identity))
	}
	return views
}

// MintLinkCode issues a code the caller sends to the bot from the chat account they want to
// connect.
//
// It takes no body. Not even a platform: the code is good on whichever channel it arrives
// over, because what proves the link is the message carrying it, and asking the caller to
// declare a platform first would add a field that nothing can check and that a mismatch
// would only turn into a confusing refusal.
func (h *Handler) MintLinkCode(c *gin.Context) {
	minted, err := h.channels.MintLinkCode(c.Request.Context(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated,
		"Send this code to the bot from the chat account you want to connect. It works once.",
		linkCodeView{Code: minted.Code, ExpiresAt: minted.ExpiresAt})
}

// ListChannels renders the caller's own connected chat accounts.
//
// No pagination block, on sessions' precedent: the service does not count the table, and a
// person has a handful of these rather than a page of them.
func (h *Handler) ListChannels(c *gin.Context) {
	identities, err := h.channels.List(c.Request.Context(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Connected channels listed.", viewChannelIdentities(identities))
}

// Unlink disconnects one of them.
//
// An id belonging to somebody else answers 404 rather than 403, because the service scopes
// the update on the caller's account instead of reading the row and comparing. A 403 would
// confirm the id exists.
func (h *Handler) Unlink(c *gin.Context) {
	if err := h.channels.Unlink(c.Request.Context(), c.Param("id"), actorFrom(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "That chat account is disconnected. Messages from it will no longer be acted on.",
		gin.H{"id": c.Param("id"), "revoked": true})
}
