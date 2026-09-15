package domain

import "testing"

func adsPolicy() ApprovalPolicy {
	return ApprovalPolicy{
		ActionType:       "ads.budget.set",
		Currency:         "USD",
		AutoApproveBelow: 500_000,
		HardCap:          50_000_000,
		DailyCap:         10_000_000,
		Enabled:          true,
	}
}

func TestDecideApprovalAutoApprovesAmountBelowThreshold(t *testing.T) {
	// Arrange
	p := adsPolicy()
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 250_000, Currency: "USD", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalAutoApproved {
		t.Fatalf("outcome = %q, want %q (reason: %s)", got.Outcome, ApprovalAutoApproved, got.Reason)
	}
	if got.RequiresHuman {
		t.Error("RequiresHuman = true, want false")
	}
}

func TestDecideApprovalRequiresHumanAtExactThreshold(t *testing.T) {
	// Arrange: AutoApproveBelow is an exclusive ceiling.
	p := adsPolicy()
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 500_000, Currency: "USD", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalPending {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalPending)
	}
	if !got.RequiresHuman {
		t.Error("RequiresHuman = false, want true")
	}
}

func TestDecideApprovalRequiresHumanAboveThreshold(t *testing.T) {
	// Arrange
	p := adsPolicy()
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 2_000_000, Currency: "USD", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalPending {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalPending)
	}
}

func TestDecideApprovalDeniesRequestWithoutAPolicy(t *testing.T) {
	// Arrange: an unknown action type must never auto-execute.
	req := ApprovalRequest{ActionType: "ads.campaign.launch", Amount: 1_000, Currency: "USD", Policy: nil}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalPending {
		t.Fatalf("outcome = %q, want %q — no policy must fall back to a human", got.Outcome, ApprovalPending)
	}
}

func TestDecideApprovalDeniesDisabledActionType(t *testing.T) {
	// Arrange
	p := adsPolicy()
	p.Enabled = false
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 1_000, Currency: "USD", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalDenied {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalDenied)
	}
}

func TestDecideApprovalDeniesAmountAboveHardCap(t *testing.T) {
	// Arrange
	p := adsPolicy()
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 60_000_000, Currency: "USD", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalDenied {
		t.Fatalf("outcome = %q, want %q — above hard cap must not reach a human", got.Outcome, ApprovalDenied)
	}
}

func TestDecideApprovalDeniesWhenDailyCapWouldBeExceeded(t *testing.T) {
	// Arrange: 9.8M already spent today, another 400k would break the 10M cap.
	p := adsPolicy()
	req := ApprovalRequest{
		ActionType: p.ActionType,
		Amount:     400_000,
		Currency:   "USD",
		SpentToday: 9_800_000,
		Policy:     &p,
	}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalDenied {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalDenied)
	}
}

func TestDecideApprovalAllowsSpendThatExactlyMeetsDailyCap(t *testing.T) {
	// Arrange: landing exactly on the cap is still within budget.
	p := adsPolicy()
	req := ApprovalRequest{
		ActionType: p.ActionType,
		Amount:     200_000,
		Currency:   "USD",
		SpentToday: 9_800_000,
		Policy:     &p,
	}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalAutoApproved {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalAutoApproved)
	}
}

func TestDecideApprovalDeniesEverythingWhileKillSwitchIsEngaged(t *testing.T) {
	// Arrange
	p := adsPolicy()
	req := ApprovalRequest{
		ActionType:        p.ActionType,
		Amount:            1,
		Currency:          "USD",
		Policy:            &p,
		KillSwitchEngaged: true,
	}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalDenied {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalDenied)
	}
}

func TestDecideApprovalDeniesCurrencyMismatch(t *testing.T) {
	// Arrange: a policy denominated in USD must not clear a request in another currency.
	p := adsPolicy()
	req := ApprovalRequest{ActionType: p.ActionType, Amount: 10, Currency: "EUR", Policy: &p}

	// Act
	got := DecideApproval(req)

	// Assert
	if got.Outcome != ApprovalDenied {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalDenied)
	}
}

func TestDecideApprovalDeniesNonPositiveOrInvalidAmounts(t *testing.T) {
	// Arrange
	p := adsPolicy()
	for _, amount := range []float64{0, -1} {
		req := ApprovalRequest{ActionType: p.ActionType, Amount: amount, Currency: "USD", Policy: &p}

		// Act
		got := DecideApproval(req)

		// Assert
		if got.Outcome != ApprovalDenied {
			t.Errorf("amount %v: outcome = %q, want %q", amount, got.Outcome, ApprovalDenied)
		}
	}
}

func TestDecideApprovalAlwaysExplainsItself(t *testing.T) {
	// Arrange
	p := adsPolicy()
	requests := []ApprovalRequest{
		{ActionType: p.ActionType, Amount: 1_000, Currency: "USD", Policy: &p},
		{ActionType: p.ActionType, Amount: 9_000_000, Currency: "USD", Policy: &p},
		{ActionType: p.ActionType, Amount: 99_000_000, Currency: "USD", Policy: &p},
		{ActionType: "unknown", Amount: 1, Currency: "USD"},
	}

	for _, req := range requests {
		// Act
		got := DecideApproval(req)

		// Assert
		if got.Reason == "" {
			t.Errorf("amount %v: Reason is empty; every verdict must be auditable", req.Amount)
		}
		if got.RequiresHuman != (got.Outcome == ApprovalPending) {
			t.Errorf("amount %v: RequiresHuman=%v disagrees with outcome %q", req.Amount, got.RequiresHuman, got.Outcome)
		}
	}
}
