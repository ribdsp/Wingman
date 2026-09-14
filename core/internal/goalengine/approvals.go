package goalengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// SpendRequest is a spending call filed with the goal engine's gate.
//
// It is not agent.SpendRequest. The loop's type carries what the loop knows — a run
// id and a tool name — and this one carries what the gate's API accepts; keeping them
// apart is what stops this package from importing the loop, and the adapter between
// them lives in cmd where the wiring already is.
type SpendRequest struct {
	// ActionType is the operator's declared action, which is what the policy
	// registry matches on. The model does not choose it.
	ActionType string
	Amount     float64
	// Currency is compared against the policy's own currency by the engine, so a
	// blank one cannot match a policy and is refused here instead.
	Currency string
	// IdempotencyKey makes a retried call return the original decision rather than
	// opening a second request against the same daily cap.
	IdempotencyKey string
	// Detail is the action's context, shown to whoever has to decide. The engine
	// bounds it at 16 KiB and refuses a larger one, which stops the run — the safe
	// direction, and the reason this is a handful of short fields rather than a
	// place to park a document.
	Detail map[string]string
}

// Decision is the gate's answer.
//
// Outcome is domain.ApprovalOutcome rather than a string so the value the loop acts
// on is the value the engine sent, with no mapping in between to drift. An outcome
// this version does not recognise is passed through untouched: domain.DecideAfterGate
// stops the run on anything it cannot interpret, and that is the right place for that
// judgement, not here.
type Decision struct {
	// ID is the approval row, so a transcript can point at what a human was asked.
	ID      string
	Outcome domain.ApprovalOutcome
	// Reason is the engine's own sentence — which policy, which threshold.
	Reason string
}

// approvalBody is the engine's POST /v1/approvals request.
//
// requestedBy is deliberately absent. The engine records a bot under the credential
// it authenticated with and ignores any name a bot supplies, because a caller that
// could name itself could split its spend across identities and a cap that can be
// split is not a cap. Sending the field would be asking for something we would not be
// given.
type approvalBody struct {
	ActionType     string          `json:"actionType"`
	Amount         float64         `json:"amount"`
	Currency       string          `json:"currency"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// RequestSpend files a spending call and returns the gate's decision.
//
// It never waits. The engine answers 201 with an outcome inside — auto_approved,
// pending or denied — and pending means a human has been asked, not that this call
// should block until they answer.
func (c *Client) RequestSpend(ctx context.Context, req SpendRequest) (Decision, error) {
	// Checked here rather than left to the engine, because each of these would come
	// back as a denial recorded against the wrong thing: an audit row blaming an
	// amount of zero reads as a policy decision about zero rather than as a caller
	// that never filled the field in.
	if strings.TrimSpace(req.ActionType) == "" {
		return Decision{}, errors.New("goalengine: a spending request needs an action type")
	}
	if strings.TrimSpace(req.Currency) == "" {
		return Decision{}, errors.New("goalengine: a spending request needs a currency")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return Decision{}, errors.New("goalengine: a spending request needs an idempotency key")
	}
	if req.Amount <= 0 {
		return Decision{}, fmt.Errorf("goalengine: a spending request needs a positive amount, got %v", req.Amount)
	}

	body := approvalBody{
		ActionType:     strings.TrimSpace(req.ActionType),
		Amount:         req.Amount,
		Currency:       strings.TrimSpace(req.Currency),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
	}
	if len(req.Detail) > 0 {
		detail, err := json.Marshal(req.Detail)
		if err != nil {
			return Decision{}, fmt.Errorf("goalengine: encode the spending detail: %w", err)
		}
		body.Payload = detail
	}

	var envelope struct {
		Data struct {
			ID           string `json:"id"`
			Outcome      string `json:"outcome"`
			PolicyReason string `json:"policyReason"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, ApprovalsPath, body, &envelope); err != nil {
		return Decision{}, fmt.Errorf("goalengine: file a spending request: %w", err)
	}

	outcome := strings.TrimSpace(envelope.Data.Outcome)
	if outcome == "" {
		// Fail closed for the same reason the kill switch does: an envelope with no
		// outcome in it is an answer we did not get, and the empty string would fall
		// through DecideAfterGate's default anyway. Saying so here names the fault.
		return Decision{}, errors.New("goalengine: the spending gate answered with no outcome; refusing the call")
	}

	return Decision{
		ID:      strings.TrimSpace(envelope.Data.ID),
		Outcome: domain.ApprovalOutcome(outcome),
		Reason:  strings.TrimSpace(envelope.Data.PolicyReason),
	}, nil
}
