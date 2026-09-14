package domain

import (
	"fmt"
	"strings"
)

// ToolClass says what a tool is able to do, and therefore how much scrutiny a
// call to it deserves. The operator declares it; the run does not get to say.
type ToolClass string

const (
	// ToolClassRead observes without changing anything outside the sandbox:
	// reading a file, querying a report, fetching a page.
	ToolClassRead ToolClass = "read"
	// ToolClassWrite changes state somebody else can see — posting a message,
	// editing a record, sending mail. Reversible in principle, embarrassing in
	// practice.
	ToolClassWrite ToolClass = "write"
	// ToolClassSpend moves money. Core never decides one of these on its own; see
	// ClassifyTool.
	ToolClassSpend ToolClass = "spend"
)

// ToolSpend is what the operator declared about a tool that moves money: enough
// for the goal engine's spending gate to decide, and nothing the model chooses.
//
// The gate needs an action type to find the policy and an amount to measure
// against its caps. Neither can be inferred from a tool name, and neither can be
// left to the model to assert, so both are pinned here: the operator says which
// policy governs this tool and which of its arguments carries the amount. What the
// model fills into that argument is read, not trusted — it is handed to the gate,
// and the gate's caps are what decide.
type ToolSpend struct {
	// ActionType is the key the goal engine's spending policy is written against.
	// A name with no policy escalates to a human there, which is the deny-biased
	// default that file ships with.
	ActionType string
	// AmountArgument is the field in the tool's own arguments holding the amount.
	AmountArgument string
	// Currency the amount is denominated in. The gate refuses a request whose
	// currency does not match its policy, so a mistake here is a denial rather
	// than a payment in the wrong unit.
	Currency string
}

// ToolGrant is what the operator declared about one tool. Grants come from
// operator-owned configuration, never from an API — the same rule metric
// definitions follow in the goal engine, and for the same reason: a list of what
// an autonomous agent may do is not a list an autonomous agent may edit.
type ToolGrant struct {
	Name  string
	Class ToolClass
	// Enabled false denies. A grant is left in place and switched off rather than
	// deleted so the record of what was once permitted survives.
	Enabled bool
	// Spend is required for ToolClassSpend and must be absent otherwise. A spend
	// grant without one is denied rather than filed: an approval request with no
	// amount is a request the gate cannot decide, and it would be refused there
	// for a reason that names the missing number rather than the missing
	// configuration.
	Spend *ToolSpend
}

// ToolVerdict is the decision about one tool call.
type ToolVerdict struct {
	Allowed bool
	// NeedsApproval means the call is legitimate but not permitted as it stands.
	// Allowed stays false until something outside this decision says otherwise:
	// "waiting on a human" is not "permitted".
	//
	// What that something is depends on the grant, and the caller reads it from the
	// grant rather than from this field. A spend is filed with the goal engine's
	// gate under the contract the grant declares. An unattended write has no such
	// contract and no gate denominated in anything a write can be measured in, so
	// it is refused for that run and the model is told why — which leaves the run
	// able to finish and report, rather than losing the reading it had already done.
	NeedsApproval bool
	// Stop is the reason to record if the run gives up here. It is empty when the
	// call is allowed outright.
	Stop   StopReason
	Reason string
}

// ClassifyTool decides whether a run may call a tool now.
//
// The order, again as the safety model:
//
//	1  no name, or no grant for this name     denied
//	2  the grant is switched off              denied
//	3  the class is read                      allowed
//	4  the class is write, attended run       allowed
//	5  the class is write, unattended run     approval required
//	6  the class is spend, contract declared  approval required, via the goal engine
//	7  the class is spend, no contract        denied
//	8  no usable class                        denied
//
// Row 1 is the load-bearing one. An unknown tool is denied, not passed through:
// tools arrive from MCP servers at runtime, so a server that adds a capability
// overnight must not gain it silently. The registry is the operator's list, and a
// name absent from it has never been considered.
//
// Row 5 uses attended to mean somebody is reading the reply as it happens. A person
// in a chat can undo a wrong message in seconds; a goal-engine trigger firing at
// 04:00 cannot. Same tool, same class, different exposure.
//
// Row 6 sends money decisions somewhere else on purpose. The goal engine already
// has a deny-biased spending ladder with caps that deny rather than escalate, a
// missing policy meaning "ask a human", and an audit row per outcome. A second gate
// in core would be a weaker copy of it, and the weaker of two gates is the one that
// decides.
//
// Row 7 is the configuration mistake that would otherwise reach that gate as a
// request with no amount. Denying here says the grant is incomplete; filing it
// would produce a refusal that blames the number instead.
func ClassifyTool(grants map[string]ToolGrant, name string, attended bool) ToolVerdict {
	deny := func(format string, args ...any) ToolVerdict {
		return ToolVerdict{
			Stop:   StopToolDenied,
			Reason: fmt.Sprintf(format, args...),
		}
	}

	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return deny("no tool name was given")
	}

	grant, declared := grants[trimmed]
	if !declared {
		return deny("%q is not a declared tool", trimmed)
	}
	if !grant.Enabled {
		return deny("%q is declared but switched off", trimmed)
	}

	switch grant.Class {
	case ToolClassRead:
		return ToolVerdict{Allowed: true, Reason: fmt.Sprintf("%q only reads", trimmed)}

	case ToolClassWrite:
		if attended {
			return ToolVerdict{Allowed: true, Reason: fmt.Sprintf("%q writes, and somebody is watching this run", trimmed)}
		}
		return ToolVerdict{
			NeedsApproval: true,
			Reason:        fmt.Sprintf("%q writes and this run is unattended", trimmed),
		}

	case ToolClassSpend:
		if grant.Spend == nil || strings.TrimSpace(grant.Spend.ActionType) == "" ||
			strings.TrimSpace(grant.Spend.AmountArgument) == "" {
			return deny("%q is declared as spend but carries no spending contract", trimmed)
		}
		return ToolVerdict{
			NeedsApproval: true,
			Reason:        fmt.Sprintf("%q spends money, which the goal engine decides", trimmed),
		}

	default:
		// A grant with an unrecognised class is a configuration mistake. Reading it
		// as the most permissive class available is how a typo becomes an outage.
		return deny("%q has no usable class (%q)", trimmed, grant.Class)
	}
}

// Attended reports whether a human is present for work from this source.
//
// It is a method on the source rather than a field on the run so the answer cannot
// drift: there is one place that decides what "unattended" means, and adding a
// fourth source forces a decision here rather than defaulting to permissive.
func (s TaskSource) Attended() bool {
	switch s {
	case TaskSourceUser, TaskSourceChannel:
		// Somebody typed and is waiting for a reply.
		return true
	default:
		// Including the empty source. Work whose origin is unclear is treated as
		// unattended, which is the more careful of the two readings.
		return false
	}
}
