package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// maxPayloadBytes bounds the action detail stored with an approval request. The
// payload exists so a human can see what they are approving; it is not a place to
// park a document.
const maxPayloadBytes = 16 * 1024

// spendRequestBody is a bot asking permission to spend.
type spendRequestBody struct {
	ActionType string  `json:"actionType"`
	Amount     float64 `json:"amount"`
	Currency   string  `json:"currency"`
	GoalID     *string `json:"goalId"`
	// IdempotencyKey lets a bot retry without opening a second request. A retried
	// request returns the original decision rather than asking again.
	IdempotencyKey string `json:"idempotencyKey"`
	// Payload is the action's own detail, shown to whoever has to decide.
	Payload json.RawMessage `json:"payload"`
	// RequestedBy is accepted but only honoured for an operator filing on an
	// agent's behalf. A bot is recorded under the credential it authenticated
	// with, never under a name it supplied — that is the field a compromised
	// agent would use to spread its spend across identities.
	RequestedBy string `json:"requestedBy"`
}

// resolutionBody is a human's answer to a pending request.
type resolutionBody struct {
	// Resolution is "approved" or "rejected". Anything else is refused by the
	// service rather than coerced here.
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

// approvalView is the rendered form of an approval request.
type approvalView struct {
	ID             string     `json:"id"`
	ActionType     string     `json:"actionType"`
	Amount         float64    `json:"amount"`
	Currency       string     `json:"currency"`
	RequestedBy    string     `json:"requestedBy"`
	GoalID         *string    `json:"goalId"`
	IdempotencyKey string     `json:"idempotencyKey,omitempty"`
	Outcome        string     `json:"outcome"`
	PolicyReason   string     `json:"policyReason,omitempty"`
	Resolution     *string    `json:"resolution"`
	ResolvedBy     *string    `json:"resolvedBy"`
	ResolvedAt     *time.Time `json:"resolvedAt"`
	ResolutionNote string     `json:"resolutionNote,omitempty"`
	Payload        string     `json:"payload,omitempty"`
	IsOpen         bool       `json:"isOpen"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      *time.Time `json:"expiresAt"`
}

func viewApproval(record repository.ApprovalRecord) approvalView {
	view := approvalView{
		ID:             record.ID,
		ActionType:     record.ActionType,
		Amount:         record.Amount,
		Currency:       record.Currency,
		RequestedBy:    record.RequestedBy,
		GoalID:         record.GoalID,
		IdempotencyKey: record.IdempotencyKey,
		Outcome:        string(record.Outcome),
		PolicyReason:   record.PolicyReason,
		ResolvedBy:     record.ResolvedBy,
		ResolvedAt:     record.ResolvedAt,
		ResolutionNote: record.ResolutionNote,
		Payload:        record.Payload,
		IsOpen:         record.IsOpen(),
		CreatedAt:      record.CreatedAt,
		ExpiresAt:      record.ExpiresAt,
	}
	if record.Resolution != nil {
		resolution := string(*record.Resolution)
		view.Resolution = &resolution
	}
	return view
}

func viewApprovals(records []repository.ApprovalRecord) []approvalView {
	views := make([]approvalView, 0, len(records))
	for _, record := range records {
		views = append(views, viewApproval(record))
	}
	return views
}

// RequestApproval asks whether an amount may be spent, and records the answer.
//
// The response is always a 201 with the decision inside it, including when the
// decision is "denied": the request was accepted and recorded, and the caller
// needs to read the outcome either way. An HTTP error here would be about the
// request failing, which is a different fact from the spend being refused.
func (h *Handler) RequestApproval(c *gin.Context) {
	var body spendRequestBody
	if !bindJSON(c, &body) {
		return
	}
	if len(body.Payload) > maxPayloadBytes {
		badRequest(c, "payload is too large")
		return
	}

	actor := actorFrom(c)
	req := service.SpendRequest{
		ActionType:     strings.TrimSpace(body.ActionType),
		Amount:         body.Amount,
		Currency:       body.Currency,
		RequestedBy:    requesterFor(actor, body.RequestedBy),
		GoalID:         body.GoalID,
		IdempotencyKey: strings.TrimSpace(body.IdempotencyKey),
		Payload:        string(body.Payload),
	}

	record, err := h.approvals.Request(c.Request.Context(), req)
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, approvalMessage(record.Outcome), viewApproval(record))
}

// requesterFor decides whose name goes on the request.
//
// An agent is recorded under the credential it authenticated with, full stop. A
// bot allowed to name its own requester could file each spend under a different
// identity, and a daily cap that can be split across identities is not a cap. An
// operator filing on an agent's behalf may name it, because a human is already
// trusted with the decision they are recording.
func requesterFor(actor service.Actor, claimed string) string {
	if actor.IsOperator() {
		if named := strings.TrimSpace(claimed); named != "" {
			return named
		}
	}
	return actor.ID
}

// approvalMessage puts the decision in the envelope's message, so an operator
// reading a log of responses can see what happened without parsing the body.
func approvalMessage(outcome domain.ApprovalOutcome) string {
	switch outcome {
	case domain.ApprovalAutoApproved:
		return "Auto-approved under policy."
	case domain.ApprovalDenied:
		return "Denied by policy."
	default:
		return "Waiting for a human."
	}
}

// ResolveApproval records a human's answer. Operator only, enforced on the route
// and again in the service: an agent that could answer its own request would have
// found a way to spend without asking.
func (h *Handler) ResolveApproval(c *gin.Context) {
	var body resolutionBody
	if !bindJSON(c, &body) {
		return
	}

	resolution := repository.ApprovalResolution(strings.ToLower(strings.TrimSpace(body.Resolution)))
	record, err := h.approvals.Resolve(c.Request.Context(), c.Param("id"), resolution, body.Note, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Approval resolved.", viewApproval(record))
}

// GetApproval reads one approval request.
func (h *Handler) GetApproval(c *gin.Context) {
	record, err := h.approvals.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Approval found.", viewApproval(record))
}

// ListApprovals reads a page of approval requests. ?openOnly=true is the queue a
// human has to work through.
func (h *Handler) ListApprovals(c *gin.Context) {
	limit, offset := page(c)
	filter := repository.ApprovalFilter{
		ActionType: strings.TrimSpace(c.Query("actionType")),
		Outcome:    domain.ApprovalOutcome(strings.TrimSpace(c.Query("outcome"))),
		OpenOnly:   boolQuery(c, "openOnly"),
		Limit:      limit,
		Offset:     offset,
	}

	records, total, err := h.approvals.List(c.Request.Context(), filter)
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Approvals listed.", viewApprovals(records), limit, offset, total)
}

// ExpireApprovals lapses pending requests nobody answered in time.
//
// It is exposed so an operator can run it on demand and so a scheduler outside
// this process can drive it; the monitor's own loop calls the same service
// method. Silence is not consent, and an unanswered request that sits forever
// eventually gets approved by somebody who has forgotten the context.
func (h *Handler) ExpireApprovals(c *gin.Context) {
	count, err := h.approvals.ExpireOverdue(c.Request.Context())
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Overdue approvals expired.", gin.H{"expired": count})
}

// boolQuery reads a boolean query parameter. Only an explicit true enables a
// filter; anything else is absence, so a typo cannot silently widen a listing.
func boolQuery(c *gin.Context, name string) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query(name))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}
