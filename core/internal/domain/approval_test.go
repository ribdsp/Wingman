package domain

import (
	"strings"
	"testing"
)

func TestDecideAfterGate_autoApproved_runsTheTool(t *testing.T) {
	// Arrange, Act
	got := DecideAfterGate(ApprovalAutoApproved, "amount 12.00 is below the auto-approve threshold of 50.00 IDR")

	// Assert
	if got.Action != GateProceed {
		t.Errorf("action = %q; want %q", got.Action, GateProceed)
	}
	// The gate's own sentence, unchanged: it names the threshold, and that is what an
	// operator reading the transcript wants to see.
	if !strings.Contains(got.Reason, "below the auto-approve threshold") {
		t.Errorf("reason = %q; want the gate's own words", got.Reason)
	}
}

func TestDecideAfterGate_pending_tellsTheModelAndDoesNotWait(t *testing.T) {
	// Arrange
	// A run parked against a human's attention holds a worker, a sandbox and a place
	// in somebody's daily budget until the queue is looked at.
	got := DecideAfterGate(ApprovalPending, "no approval policy configured for action \"supplier.invoice\"")

	// Assert
	if got.Action != GateReport {
		t.Errorf("action = %q; want %q", got.Action, GateReport)
	}
	if !strings.Contains(got.Reason, "did not wait") {
		t.Errorf("reason = %q; want it to say the run did not wait", got.Reason)
	}
	if !strings.Contains(got.Reason, "supplier.invoice") {
		t.Errorf("reason = %q; want the gate's reason carried through", got.Reason)
	}
}

func TestDecideAfterGate_denied_stopsTheRun(t *testing.T) {
	// Arrange, Act
	got := DecideAfterGate(ApprovalDenied, "amount 900.00 exceeds the hard cap of 500.00 IDR")

	// Assert
	if got.Action != GateStop {
		t.Errorf("action = %q; want %q", got.Action, GateStop)
	}
}

func TestDecideAfterGate_anOutcomeThisVersionDoesNotKnow_stopsTheRun(t *testing.T) {
	// Arrange
	// A newer goal engine answering something this build has never seen. Reading an
	// answer we cannot interpret as anything other than no is how a spend gets made
	// by a version mismatch.
	for _, outcome := range []ApprovalOutcome{"", "queued", "approved", "AUTO_APPROVED"} {
		// Act
		got := DecideAfterGate(outcome, "")

		// Assert
		if got.Action != GateStop {
			t.Errorf("outcome %q: action = %q; want %q", outcome, got.Action, GateStop)
		}
		if got.Reason == "" {
			t.Errorf("outcome %q: no reason given; the transcript would not say why", outcome)
		}
	}
}
