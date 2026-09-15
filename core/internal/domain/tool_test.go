package domain

import "testing"

// grants is the operator's declared tool list, one of each class plus one switched
// off.
func grants() map[string]ToolGrant {
	return map[string]ToolGrant{
		"read_report":  {Name: "read_report", Class: ToolClassRead, Enabled: true},
		"post_message": {Name: "post_message", Class: ToolClassWrite, Enabled: true},
		"pay_invoice": {
			Name:    "pay_invoice",
			Class:   ToolClassSpend,
			Enabled: true,
			Spend: &ToolSpend{
				ActionType:     "supplier.invoice",
				AmountArgument: "amount",
				Currency:       "USD",
			},
		},
		"delete_customer": {Name: "delete_customer", Class: ToolClassWrite, Enabled: false},
	}
}

// Branch 1 — the load-bearing one. Tools arrive from MCP servers at runtime, so an
// undeclared name must be denied rather than passed through: a server that gains a
// capability overnight must not gain it silently.
func TestClassifyTool_undeclaredTool_isDenied(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "transfer_funds", true)

	// Assert
	if got.Allowed || got.NeedsApproval {
		t.Fatalf("an undeclared tool was not denied: %+v", got)
	}
	if got.Stop != StopToolDenied {
		t.Errorf("stop reason = %q; want %q", got.Stop, StopToolDenied)
	}
}

func TestClassifyTool_emptyName_isDenied(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "   ", true)

	// Assert
	if got.Allowed || got.NeedsApproval {
		t.Fatalf("an empty tool name was not denied: %+v", got)
	}
}

func TestClassifyTool_emptyRegistry_deniesEverything(t *testing.T) {
	// Arrange
	var registry map[string]ToolGrant

	// Act
	got := ClassifyTool(registry, "read_report", true)

	// Assert
	// A nil registry is a service that failed to load its configuration. Denying is
	// the only reading of that which does not act on an unknown permission set.
	if got.Allowed || got.NeedsApproval {
		t.Fatalf("a nil registry permitted a call: %+v", got)
	}
}

// Branch 2.
func TestClassifyTool_disabledGrant_isDenied(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "delete_customer", true)

	// Assert
	if got.Allowed || got.NeedsApproval {
		t.Fatalf("a switched-off tool was not denied: %+v", got)
	}
	if got.Stop != StopToolDenied {
		t.Errorf("stop reason = %q; want %q", got.Stop, StopToolDenied)
	}
}

// Branch 8.
func TestClassifyTool_grantWithUnrecognisedClass_isDenied(t *testing.T) {
	// Arrange
	registry := map[string]ToolGrant{
		"mystery": {Name: "mystery", Class: ToolClass("wrtie"), Enabled: true},
	}

	// Act
	got := ClassifyTool(registry, "mystery", true)

	// Assert
	// Reading a typo as the most permissive class available is how a misspelling
	// becomes an outage.
	if got.Allowed || got.NeedsApproval {
		t.Fatalf("a grant with a misspelled class permitted a call: %+v", got)
	}
}

// Branch 6 — money never gets decided here.
func TestClassifyTool_spendingTool_requiresApprovalAndIsNotYetAllowed(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "pay_invoice", true)

	// Assert
	if !got.NeedsApproval {
		t.Error("a spending tool did not require approval; core must not be a second, weaker gate")
	}
	// "Waiting on a human" is not "permitted". If Allowed were true here a caller
	// checking one field would spend the money.
	if got.Allowed {
		t.Error("a spending tool was allowed outright while approval was still pending")
	}
	if got.Stop != "" {
		t.Errorf("stop reason = %q; a call awaiting approval has not failed", got.Stop)
	}
}

// Even attended. A person in a chat is not the money owner.
func TestClassifyTool_spendingToolInAttendedRun_stillRequiresApproval(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "pay_invoice", false)
	attended := ClassifyTool(registry, "pay_invoice", true)

	// Assert
	if !got.NeedsApproval || !attended.NeedsApproval {
		t.Error("attendance changed the answer for a spending tool; it must not")
	}
}

// Branch 5 — same tool, same class, different exposure.
func TestClassifyTool_writingToolUnattended_requiresApproval(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "post_message", false)

	// Assert
	// A goal-engine trigger firing at 04:00 has nobody to undo a wrong message.
	if !got.NeedsApproval {
		t.Errorf("an unattended write did not require approval: %+v", got)
	}
	if got.Allowed {
		t.Error("an unattended write was allowed before approval")
	}
}

func TestClassifyTool_writingToolAttended_isAllowed(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "post_message", true)

	// Assert
	if !got.Allowed {
		t.Errorf("an attended write was not allowed: %+v", got)
	}
	if got.NeedsApproval {
		t.Error("an attended write asked for approval; a person is already reading the reply")
	}
}

// Branch 3.
func TestClassifyTool_readingTool_isAllowedEvenUnattended(t *testing.T) {
	// Arrange
	registry := grants()

	// Act
	got := ClassifyTool(registry, "read_report", false)

	// Assert
	if !got.Allowed {
		t.Errorf("a read-only tool was not allowed: %+v", got)
	}
	if got.NeedsApproval {
		t.Error("a read-only tool asked for approval; nothing outside the sandbox changed")
	}
}

// Branch 7 — the incomplete grant.
func TestClassifyTool_spendingToolWithNoContract_isDenied(t *testing.T) {
	// Arrange
	// Three ways for the contract to be missing, all of which leave the goal engine's
	// gate with no policy to look up or no amount to measure. Filing one would produce
	// a refusal blaming the number rather than the configuration, in an audit row an
	// operator then has to reverse-engineer.
	registry := map[string]ToolGrant{
		"none":       {Name: "none", Class: ToolClassSpend, Enabled: true},
		"no_action":  {Name: "no_action", Class: ToolClassSpend, Enabled: true, Spend: &ToolSpend{AmountArgument: "amount"}},
		"no_amount":  {Name: "no_amount", Class: ToolClassSpend, Enabled: true, Spend: &ToolSpend{ActionType: "supplier.invoice"}},
		"whitespace": {Name: "whitespace", Class: ToolClassSpend, Enabled: true, Spend: &ToolSpend{ActionType: "  ", AmountArgument: "  "}},
	}

	// Act & Assert
	for name := range registry {
		got := ClassifyTool(registry, name, true)
		if got.Allowed || got.NeedsApproval {
			t.Errorf("%q was not denied: %+v", name, got)
		}
		if got.Stop != StopToolDenied {
			t.Errorf("%q stop reason = %q; want %q", name, got.Stop, StopToolDenied)
		}
	}
}

func TestClassifyTool_everyVerdictCarriesAReason(t *testing.T) {
	// Arrange
	registry := grants()
	names := []string{"read_report", "post_message", "pay_invoice", "delete_customer", "absent", ""}

	// Act & Assert
	for _, name := range names {
		for _, attended := range []bool{true, false} {
			got := ClassifyTool(registry, name, attended)
			if got.Reason == "" {
				t.Errorf("ClassifyTool(%q, attended=%v) gave no reason; the transcript would not say why", name, attended)
			}
		}
	}
}

func TestTaskSource_attended_onlyWhereSomebodyIsWaiting(t *testing.T) {
	// Arrange
	cases := map[TaskSource]bool{
		TaskSourceUser:       true,
		TaskSourceChannel:    true,
		TaskSourceGoalEngine: false,
		// An unset source is work whose origin is unclear, which is the case for
		// treating it as unattended rather than trusting it.
		TaskSource(""):          false,
		TaskSource("something"): false,
	}

	// Act & Assert
	for source, want := range cases {
		if got := source.Attended(); got != want {
			t.Errorf("TaskSource(%q).Attended() = %v; want %v", source, got, want)
		}
	}
}
