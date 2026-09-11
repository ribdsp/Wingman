package domain

// ApprovalOutcome is the result of applying threshold policy to a spend
// request. Wingman is deny-by-default: anything the policy does not explicitly
// clear ends up in front of a human.
type ApprovalOutcome string

const (
	// ApprovalAutoApproved means the amount is small enough to execute without
	// asking a human.
	ApprovalAutoApproved ApprovalOutcome = "auto_approved"
	// ApprovalPending means a human must decide before execution.
	ApprovalPending ApprovalOutcome = "pending"
	// ApprovalDenied means policy forbids the action outright; no human
	// approval card is raised.
	ApprovalDenied ApprovalOutcome = "denied"
)

// ApprovalPolicy is the operator-configured threshold for one action type.
// Policies are configuration, never agent-writable — an agent that could edit
// its own spending limits has no limits.
type ApprovalPolicy struct {
	// ActionType is a stable identifier such as "ads.budget.set".
	ActionType string
	// Currency is the ISO 4217 code every amount for this action type must use.
	Currency string
	// AutoApproveBelow is the exclusive ceiling for hands-off execution.
	// Zero means nothing auto-approves.
	AutoApproveBelow float64
	// HardCap rejects any single request above it. Zero means no cap.
	HardCap float64
	// DailyCap rejects requests that would push today's cumulative spend past
	// it. Zero means no daily cap.
	DailyCap float64
	// Enabled false blocks the action type entirely.
	Enabled bool
}

// ApprovalRequest is one agent request to spend money.
type ApprovalRequest struct {
	ActionType string
	Amount     float64
	Currency   string
	// SpentToday is the already-committed spend for this action type today,
	// supplied by the caller from the spend ledger.
	SpentToday float64
	// Policy is the matching policy, or nil when none is configured.
	Policy            *ApprovalPolicy
	KillSwitchEngaged bool
}

// ApprovalDecision is the immutable verdict for an ApprovalRequest.
type ApprovalDecision struct {
	Outcome ApprovalOutcome
	// Reason is operator-facing text explaining the verdict; it is written to
	// the audit log and shown on the approval card.
	Reason string
	// RequiresHuman is a convenience mirror of Outcome == ApprovalPending.
	RequiresHuman bool
}
