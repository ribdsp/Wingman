package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// notifyRequest is the goal engine's payload, field for field.
//
// The same kind of contract as dispatchRequest: these names are the ones
// goal-engine/internal/core/client.go writes, and renaming one here silently stops
// notifications rather than failing, because an omitted field decodes as empty.
type notifyRequest struct {
	Kind      string `json:"kind"`
	SubjectID string `json:"subjectId"`
	Headline  string `json:"headline"`
	Link      string `json:"link"`
}

// deliveredView is how many of the owner's chat accounts heard about it.
//
// A count and not a list. Which platforms somebody has connected is their own business, and
// a caller holding a bot key has no use for it: what the engine does with this answer is
// write a log line.
type deliveredView struct {
	Recipients int `json:"recipients"`
}

// Notify tells the operator that something happened next door.
//
// No route guard, on the tasks group's precedent: the service refuses a signed-in person
// itself, because who may make this instance send a message is one decision and belongs in
// one place. A 200 with no recipients is a real answer — an owner who has linked no chat
// account, or no owner configured at all, is an ordinary instance and not a failed request.
func (h *Handler) Notify(c *gin.Context) {
	var req notifyRequest
	if !bindJSON(c, &req) {
		return
	}

	delivered, err := h.notifications.Notify(c.Request.Context(), service.Notice{
		Kind:      service.NoticeKind(req.Kind),
		SubjectID: req.SubjectID,
		Headline:  req.Headline,
		Link:      req.Link,
	}, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}

	if delivered.Recipients == 0 {
		// Said plainly, because the caller's alternative is to assume it worked. The engine
		// logs this line, and it is how an operator who turned notifications on and heard
		// nothing finds out that the linking step is still outstanding.
		utils.Success(c, http.StatusOK,
			"Nobody was notified: no chat account is connected to the unattended owner.",
			deliveredView{})
		return
	}
	utils.Success(c, http.StatusOK, "Notification delivered.",
		deliveredView{Recipients: delivered.Recipients})
}
