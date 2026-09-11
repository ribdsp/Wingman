package domain

import (
	"fmt"
	"strings"
)

// DecideApproval applies threshold policy to one spend request.
//
// The ladder is deliberately deny-biased. A missing policy escalates to a human
// rather than auto-approving, and anything above a hard or daily cap is refused
// outright instead of being put in front of a human — an operator who is asked
// to rubber-stamp a cap breach will eventually rubber-stamp it. Raising a cap is
// a configuration change, not an approval.
func DecideApproval(req ApprovalRequest) ApprovalDecision {
	if req.KillSwitchEngaged {
		return denied("kill switch engaged: no spend may be executed")
	}

	if !isFinite(req.Amount) || req.Amount <= 0 {
		return denied(fmt.Sprintf("amount %v is not a positive finite number", req.Amount))
	}

	if !isFinite(req.SpentToday) || req.SpentToday < 0 {
		return denied(fmt.Sprintf("today's recorded spend %v is not a valid amount: refusing to decide against a broken ledger", req.SpentToday))
	}

	if req.Policy == nil {
		return pending(fmt.Sprintf("no approval policy configured for action %q: escalating to a human", req.ActionType))
	}
	p := *req.Policy

	if !p.Enabled {
		return denied(fmt.Sprintf("action %q is disabled by policy", p.ActionType))
	}

	if p.Currency != "" && !strings.EqualFold(p.Currency, req.Currency) {
		return denied(fmt.Sprintf("currency mismatch: policy for %q is denominated in %s, request used %q",
			p.ActionType, p.Currency, req.Currency))
	}

	if p.HardCap > 0 && req.Amount > p.HardCap {
		return denied(fmt.Sprintf("amount %.2f exceeds the hard cap of %.2f %s for %q",
			req.Amount, p.HardCap, p.Currency, p.ActionType))
	}

	if p.DailyCap > 0 && req.SpentToday+req.Amount > p.DailyCap {
		return denied(fmt.Sprintf("amount %.2f would push today's spend to %.2f, past the daily cap of %.2f %s for %q",
			req.Amount, req.SpentToday+req.Amount, p.DailyCap, p.Currency, p.ActionType))
	}

	if req.Amount < p.AutoApproveBelow {
		return autoApproved(fmt.Sprintf("amount %.2f is below the auto-approve threshold of %.2f %s for %q",
			req.Amount, p.AutoApproveBelow, p.Currency, p.ActionType))
	}

	return pending(fmt.Sprintf("amount %.2f is at or above the auto-approve threshold of %.2f %s for %q: human approval required",
		req.Amount, p.AutoApproveBelow, p.Currency, p.ActionType))
}

func autoApproved(reason string) ApprovalDecision {
	return ApprovalDecision{Outcome: ApprovalAutoApproved, Reason: reason}
}

func pending(reason string) ApprovalDecision {
	return ApprovalDecision{Outcome: ApprovalPending, Reason: reason, RequiresHuman: true}
}

func denied(reason string) ApprovalDecision {
	return ApprovalDecision{Outcome: ApprovalDenied, Reason: reason}
}
