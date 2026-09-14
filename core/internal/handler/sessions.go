package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// sessionView is one row of "your devices".
//
// It carries no token and no hash of one. repository.Session does not hold the stored
// token either, which is what makes this list safe to render at all — a session list that
// included credentials would be a way to escalate one stolen session into all of them.
//
// The address and the user agent are included because recognising an unfamiliar sign-in is
// the entire purpose of the list.
type sessionView struct {
	ID         string     `json:"id"`
	UserAgent  string     `json:"userAgent"`
	CreatedIP  string     `json:"createdIp"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	LastSeenAt *time.Time `json:"lastSeenAt"`
	// IsLive is rendered rather than left to the client to work out from the three
	// timestamps above. Two clients deriving "is this still usable?" independently is two
	// chances to derive it differently.
	IsLive bool `json:"isLive"`
}

func viewSession(session repository.Session, now time.Time) sessionView {
	return sessionView{
		ID:         session.ID,
		UserAgent:  session.UserAgent,
		CreatedIP:  session.CreatedIP,
		CreatedAt:  session.CreatedAt,
		ExpiresAt:  session.ExpiresAt,
		RevokedAt:  session.RevokedAt,
		LastSeenAt: session.LastSeenAt,
		IsLive:     session.Live(now),
	}
}

func viewSessions(sessions []repository.Session, now time.Time) []sessionView {
	views := make([]sessionView, 0, len(sessions))
	for _, session := range sessions {
		views = append(views, viewSession(session, now))
	}
	return views
}

// ListSessions renders the caller's own sessions, live ones and ended ones alike.
//
// The response carries no total. The service does not count the table, and rendering a
// pagination block with a made-up total would be worse than not offering one — a client
// paging on it would stop early or never stop.
func (h *Handler) ListSessions(c *gin.Context) {
	limit, offset := page(c)

	sessions, err := h.sessions.List(c.Request.Context(), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Sessions listed.", viewSessions(sessions, h.now()))
}

// RevokeSession ends one of the caller's sessions by id.
//
// An id belonging to somebody else answers 404, because the service scopes the update on
// the caller's account rather than reading the row and comparing. A 403 here would confirm
// that the id exists.
func (h *Handler) RevokeSession(c *gin.Context) {
	if err := h.sessions.Revoke(c.Request.Context(), c.Param("id"), actorFrom(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Session ended.", gin.H{"id": c.Param("id"), "revoked": true})
}

// RevokeAllSessions is sign out everywhere, and it includes the session making the
// request. See the service for why sparing the current device would defeat the button.
func (h *Handler) RevokeAllSessions(c *gin.Context) {
	revoked, err := h.sessions.RevokeAll(c.Request.Context(), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Every session has been ended, including this one.",
		gin.H{"sessionsRevoked": revoked})
}
