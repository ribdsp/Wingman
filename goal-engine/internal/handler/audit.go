package handler

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// auditView is the rendered form of one audit entry.
type auditView struct {
	ID          int64     `json:"id"`
	At          time.Time `json:"at"`
	ActorType   string    `json:"actorType"`
	ActorID     string    `json:"actorId"`
	Action      string    `json:"action"`
	SubjectType string    `json:"subjectType"`
	SubjectID   string    `json:"subjectId"`
	Outcome     string    `json:"outcome"`
	// Detail is already JSON in storage, so it is emitted as-is rather than
	// re-encoded into a quoted string a client would have to unwrap twice.
	Detail    jsonRaw `json:"detail,omitempty"`
	RequestID string  `json:"requestId,omitempty"`
}

func viewAuditEvent(event repository.AuditEvent) auditView {
	return auditView{
		ID:          event.ID,
		At:          event.At,
		ActorType:   string(event.ActorType),
		ActorID:     event.ActorID,
		Action:      event.Action,
		SubjectType: event.SubjectType,
		SubjectID:   event.SubjectID,
		Outcome:     event.Outcome,
		Detail:      jsonRaw(event.Detail),
		RequestID:   event.RequestID,
	}
}

// ListAudit reads a page of the business audit log.
//
// This log is the answer to "who moved that target", so it is readable by both
// roles and writable by neither: the service exposes no append method at all.
// Entries are written as a side effect of the actions they describe.
func (h *Handler) ListAudit(c *gin.Context) {
	limit, offset := page(c)

	since, _, err := timeQuery(c, "since")
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	until, _, err := timeQuery(c, "until")
	if err != nil {
		badRequest(c, err.Error())
		return
	}

	filter := repository.AuditFilter{
		ActorType:   repository.ActorType(strings.TrimSpace(c.Query("actorType"))),
		Action:      strings.TrimSpace(c.Query("action")),
		SubjectType: strings.TrimSpace(c.Query("subjectType")),
		SubjectID:   strings.TrimSpace(c.Query("subjectId")),
		Since:       since,
		Until:       until,
		Limit:       limit,
		Offset:      offset,
	}

	events, total, err := h.audit.List(c.Request.Context(), filter)
	if err != nil {
		h.respondError(c, err)
		return
	}
	views := make([]auditView, 0, len(events))
	for _, event := range events {
		views = append(views, viewAuditEvent(event))
	}
	paged(c, "Audit log listed.", views, limit, offset, total)
}
