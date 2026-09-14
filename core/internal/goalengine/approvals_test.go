package goalengine

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// The spending gate. Core has no gate of its own, so every test here is about filing
// the question faithfully and reading the answer without softening it.

// testSpend is a spending call the gate would recognise: an operator-declared action,
// a positive amount, and a key derived from the run and the call.
func testSpend() SpendRequest {
	return SpendRequest{
		ActionType:     "supplier.invoice",
		Amount:         1500,
		Currency:       "IDR",
		IdempotencyKey: "run_1:call_1",
		Detail:         map[string]string{"runId": "run_1", "toolName": "pay_invoice"},
	}
}

func TestRequestSpend_filesTheCallAsTheGateExpectsIt(t *testing.T) {
	// Arrange
	fake, client := newEngine(t, http.StatusCreated,
		approvalBodyFor("auto_approved", "below the auto-approve threshold of 250000.00 IDR"))

	// Act
	decision, err := client.RequestSpend(context.Background(), testSpend())

	// Assert
	if err != nil {
		t.Fatalf("RequestSpend() = %v; want a decision", err)
	}
	if decision.Outcome != domain.ApprovalAutoApproved {
		t.Errorf("outcome = %q; want %q", decision.Outcome, domain.ApprovalAutoApproved)
	}
	if decision.ID != "apr_7" {
		t.Errorf("id = %q; want the approval row, so a transcript can point at it", decision.ID)
	}
	// The engine's own sentence, not a summary of it. It names the threshold, and that
	// is what an operator reading the run needs.
	if !strings.Contains(decision.Reason, "250000.00 IDR") {
		t.Errorf("reason = %q; want the policy's own words", decision.Reason)
	}

	got := fake.only(t)
	if got.method != http.MethodPost || got.path != ApprovalsPath {
		t.Errorf("request = %s %s; want POST %s", got.method, got.path, ApprovalsPath)
	}
	if got.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", got.header.Get("Content-Type"))
	}

	body := got.decode(t)
	if body["actionType"] != "supplier.invoice" {
		t.Errorf("actionType = %v; want the operator's declared action", body["actionType"])
	}
	if body["amount"] != float64(1500) {
		t.Errorf("amount = %v; want 1500", body["amount"])
	}
	if body["currency"] != "IDR" {
		t.Errorf("currency = %v; want IDR, which is what the policy is compared against", body["currency"])
	}
	if body["idempotencyKey"] != "run_1:call_1" {
		t.Errorf("idempotencyKey = %v; want the key derived from the run and the call", body["idempotencyKey"])
	}
	// requestedBy is not sent. The engine records a bot under the credential it
	// authenticated with and ignores a name a bot supplies, because a caller that
	// could name itself could split its spend across identities.
	if _, ok := body["requestedBy"]; ok {
		t.Errorf("the request names its own requester: %v", body["requestedBy"])
	}

	// The detail is what a human sees in the approval queue. Without it they are
	// looking at an amount and an action type with no idea which run wants it.
	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v; want the run's context as an object", body["payload"])
	}
	if payload["runId"] != "run_1" || payload["toolName"] != "pay_invoice" {
		t.Errorf("payload = %v; want the run and the tool named", payload)
	}
}

func TestRequestSpend_noDetail_sendsNoPayloadRatherThanAnEmptyOne(t *testing.T) {
	// Arrange
	// An empty object in the approval queue reads as detail somebody recorded. Nothing
	// at all reads as nothing to say, which is the truth.
	fake, client := newEngine(t, http.StatusCreated, approvalBodyFor("auto_approved", "under the threshold"))
	req := testSpend()
	req.Detail = nil

	// Act
	if _, err := client.RequestSpend(context.Background(), req); err != nil {
		t.Fatalf("RequestSpend() = %v; want a decision", err)
	}

	// Assert
	if body := fake.only(t).decode(t); body["payload"] != nil {
		t.Errorf("payload = %v; want it absent", body["payload"])
	}
}

func TestRequestSpend_eachOfTheGatesAnswers_reachesTheCallerUntranslated(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		want    domain.ApprovalOutcome
	}{
		{name: "auto approved", outcome: "auto_approved", want: domain.ApprovalAutoApproved},
		// Pending is not a wait. The client returns it and the loop tells the model,
		// rather than holding a worker open against somebody's attention.
		{name: "pending", outcome: "pending", want: domain.ApprovalPending},
		{name: "denied", outcome: "denied", want: domain.ApprovalDenied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			_, client := newEngine(t, http.StatusCreated, approvalBodyFor(test.outcome, "the policy said so"))

			// Act
			decision, err := client.RequestSpend(context.Background(), testSpend())

			// Assert
			if err != nil {
				t.Fatalf("RequestSpend() = %v; want a decision", err)
			}
			if decision.Outcome != test.want {
				t.Errorf("outcome = %q; want %q", decision.Outcome, test.want)
			}
		})
	}
}

func TestRequestSpend_anOutcomeThisVersionDoesNotKnow_isPassedOnForTheDomainToRefuse(t *testing.T) {
	// Arrange
	// A newer goal engine inventing a fourth outcome. Mapping it to something here
	// would be this package deciding what an answer it cannot read means, and that
	// decision belongs in one place — the same place the other three are read.
	_, client := newEngine(t, http.StatusCreated, approvalBodyFor("queued_for_review", "a new policy did something new"))

	// Act
	decision, err := client.RequestSpend(context.Background(), testSpend())

	// Assert
	if err != nil {
		t.Fatalf("RequestSpend() = %v; want the answer as it stands", err)
	}
	if decision.Outcome != domain.ApprovalOutcome("queued_for_review") {
		t.Errorf("outcome = %q; want it carried through unchanged", decision.Outcome)
	}
	// And the domain refuses it, which is the half of this that matters.
	if verdict := domain.DecideAfterGate(decision.Outcome, decision.Reason); verdict.Action != domain.GateStop {
		t.Errorf("the domain answered %q for an outcome it does not recognise; want %q", verdict.Action, domain.GateStop)
	}
}

func TestRequestSpend_anEnvelopeWithNoOutcome_isRefused(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "an empty decision", body: `{"success":true,"code":201,"data":{"id":"apr_7"}}`},
		{name: "a blank outcome", body: `{"success":true,"code":201,"data":{"id":"apr_7","outcome":"  "}}`},
		{name: "no data at all", body: `{"success":true,"code":201}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			_, client := newEngine(t, http.StatusCreated, test.body)

			// Act
			decision, err := client.RequestSpend(context.Background(), testSpend())

			// Assert
			if err == nil {
				t.Fatalf("RequestSpend() = %+v, nil; want a refusal", decision)
			}
			if !strings.Contains(err.Error(), "no outcome") {
				t.Errorf("error = %v; want it to say the answer carried no outcome", err)
			}
		})
	}
}

func TestRequestSpend_aCallTheGateCouldOnlyDeny_isNotSentAtAll(t *testing.T) {
	// Each of these would come back denied, and the denial would be recorded against
	// the wrong thing: an audit row about an amount of zero reads as a decision about
	// zero rather than as a caller that never filled the field in.
	tests := []struct {
		name string
		req  func() SpendRequest
		want string
	}{
		{
			name: "no action type",
			req:  func() SpendRequest { r := testSpend(); r.ActionType = " "; return r },
			want: "action type",
		},
		{
			name: "no currency",
			req:  func() SpendRequest { r := testSpend(); r.Currency = ""; return r },
			want: "currency",
		},
		{
			// Without it, a retried step files a second request against the same daily
			// cap and the run's own retry becomes double spending.
			name: "no idempotency key",
			req:  func() SpendRequest { r := testSpend(); r.IdempotencyKey = "\t"; return r },
			want: "idempotency key",
		},
		{
			name: "no amount",
			req:  func() SpendRequest { r := testSpend(); r.Amount = 0; return r },
			want: "positive amount",
		},
		{
			name: "a negative amount",
			req:  func() SpendRequest { r := testSpend(); r.Amount = -20; return r },
			want: "positive amount",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			fake, client := newEngine(t, http.StatusCreated, approvalBodyFor("auto_approved", "under the threshold"))

			// Act
			decision, err := client.RequestSpend(context.Background(), test.req())

			// Assert
			if err == nil {
				t.Fatalf("RequestSpend() = %+v, nil; want a refusal", decision)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to name %q", err, test.want)
			}
			if len(fake.requests) != 0 {
				t.Errorf("the engine received %d requests; want none — nothing worth deciding was sent", len(fake.requests))
			}
		})
	}
}

func TestRequestSpend_theGateCouldNotBeAsked_isAnErrorAndNeverADecision(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
	}{
		{
			name:          "the engine failed",
			status:        http.StatusInternalServerError,
			body:          `{"success":false,"error":{"code":"internal_error"}}`,
			wantRetryable: true,
		},
		{
			name:          "the engine is rate limiting core",
			status:        http.StatusTooManyRequests,
			body:          `{"success":false,"error":{"code":"rate_limited"}}`,
			wantRetryable: true,
		},
		{
			// A payload too large, an unknown currency: our fault, and it will be our
			// fault again on the next attempt.
			name:          "the engine rejected the request",
			status:        http.StatusBadRequest,
			body:          `{"success":false,"error":{"code":"validation_error","message":"payload is too large"}}`,
			wantRetryable: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			_, client := newEngine(t, test.status, test.body)

			// Act
			decision, err := client.RequestSpend(context.Background(), testSpend())

			// Assert
			if err == nil {
				t.Fatalf("RequestSpend() = %+v, nil; want a refusal", decision)
			}
			if decision.Outcome != "" {
				t.Errorf("outcome = %q alongside an error; want nothing that could be read as permission", decision.Outcome)
			}
			if got := IsRetryable(err); got != test.wantRetryable {
				t.Errorf("IsRetryable() = %v; want %v", got, test.wantRetryable)
			}
			if !strings.Contains(err.Error(), "spending request") {
				t.Errorf("error = %v; want it to say which call failed", err)
			}
		})
	}
}
