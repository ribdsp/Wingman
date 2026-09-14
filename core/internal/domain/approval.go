package domain

import "fmt"

// ApprovalOutcome is what the goal engine's spending gate answered. The three
// values are its values, spelled the same way on purpose: core reads that gate's
// decision, it does not translate it into a vocabulary of its own where a mapping
// could drift.
type ApprovalOutcome string

const (
	// ApprovalAutoApproved means the amount was below the operator's threshold.
	ApprovalAutoApproved ApprovalOutcome = "auto_approved"
	// ApprovalPending means a human has been asked. Nothing has been permitted.
	ApprovalPending ApprovalOutcome = "pending"
	// ApprovalDenied means no, and the gate does not ask a human about a cap
	// breach — raising a cap is a configuration change, not an approval.
	ApprovalDenied ApprovalOutcome = "denied"
)

// GateAction is what a run does next, having filed a spending call.
type GateAction string

const (
	// GateProceed runs the tool.
	GateProceed GateAction = "proceed"
	// GateReport tells the model the call was not made and why, and lets the run
	// carry on. The work already done is still worth finishing and reporting.
	GateReport GateAction = "report"
	// GateStop ends the run and records StopToolDenied.
	GateStop GateAction = "stop"
)

// GateVerdict is the decision, with the sentence that goes in the transcript.
type GateVerdict struct {
	Action GateAction
	Reason string
}

// DecideAfterGate turns the spending gate's answer into what the run does next.
//
//	auto_approved   the tool runs
//	pending         the model is told a human was asked; the run continues
//	denied          the run stops, recording tool_denied
//	anything else   the run stops, as though denied
//
// Pending is not a wait. Core does not hold a run open against a human's attention:
// a run parked mid-loop holds a worker, a sandbox and a place in somebody's daily
// budget for as long as nobody looks at the queue. Telling the model instead leaves
// a transcript that says what it wanted to do and why it could not, which is what
// the person approving the card needs to read anyway.
//
// The last row is the reason this is a function rather than a switch at the call
// site. An outcome core does not recognise means the gate answered something a newer
// version of it invented, and the only safe reading of an answer we cannot interpret
// is no.
func DecideAfterGate(outcome ApprovalOutcome, gateReason string) GateVerdict {
	switch outcome {
	case ApprovalAutoApproved:
		return GateVerdict{Action: GateProceed, Reason: gateReason}

	case ApprovalPending:
		return GateVerdict{
			Action: GateReport,
			Reason: fmt.Sprintf("a human has been asked to approve this spend and the run did not wait: %s", gateReason),
		}

	case ApprovalDenied:
		return GateVerdict{Action: GateStop, Reason: gateReason}

	default:
		return GateVerdict{
			Action: GateStop,
			Reason: fmt.Sprintf("the spending gate answered %q, which this version does not recognise: refusing", outcome),
		}
	}
}
