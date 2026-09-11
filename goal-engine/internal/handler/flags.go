package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// killSwitchBody is a request to stop or restart the engine. The reason is not
// optional: this is the entry somebody reads while working out why the engine
// went quiet.
type killSwitchBody struct {
	Engaged *bool  `json:"engaged"`
	Reason  string `json:"reason"`
}

// flagView is the rendered form of an operational flag.
type flagView struct {
	Key       string     `json:"key"`
	Enabled   bool       `json:"enabled"`
	Reason    string     `json:"reason,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

func viewFlag(flag repository.Flag) flagView {
	view := flagView{
		Key:       flag.Key,
		Enabled:   flag.Enabled,
		Reason:    flag.Reason,
		UpdatedBy: flag.UpdatedBy,
	}
	if !flag.UpdatedAt.IsZero() {
		updated := flag.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

// GetKillSwitch reports whether the engine is halted.
func (h *Handler) GetKillSwitch(c *gin.Context) {
	flag, err := h.flags.KillSwitch(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Kill switch read.", viewFlag(flag))
}

// SetKillSwitch engages or releases the engine's stop button.
//
// Anyone holding a credential may engage it. An agent that has worked out it is
// doing damage should be able to stop the fleet, and the worst case is an outage
// a human undoes in one call. Releasing it is a human's alone, which is why only
// this direction is guarded — a switch an agent can turn off is no switch at all,
// and the case it exists for is precisely the one where the agent's judgement is
// what went wrong. The rule is enforced in the service; the route stays open so
// an agent can still reach the engage path.
func (h *Handler) SetKillSwitch(c *gin.Context) {
	var body killSwitchBody
	if !bindJSON(c, &body) {
		return
	}
	if body.Engaged == nil {
		// Defaulting this would mean a malformed request could release the switch.
		badRequest(c, "engaged is required and must be true or false")
		return
	}

	flag, err := h.flags.SetKillSwitch(c.Request.Context(), *body.Engaged, body.Reason, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}

	message := "Kill switch released. The engine will act again."
	if flag.Enabled {
		message = "Kill switch engaged. No autonomous action will be taken."
	}
	utils.Success(c, http.StatusOK, message, viewFlag(flag))
}

// ListFlags reads every operational flag.
func (h *Handler) ListFlags(c *gin.Context) {
	flags, err := h.flags.List(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}
	views := make([]flagView, 0, len(flags))
	for _, flag := range flags {
		views = append(views, viewFlag(flag))
	}
	utils.Success(c, http.StatusOK, "Flags listed.", views)
}
