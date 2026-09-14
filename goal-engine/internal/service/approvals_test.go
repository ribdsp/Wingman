package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

const testAction = "ads.budget.set"

// testPolicy is a normal operator policy: small amounts run unattended, large
// ones ask, and there is a ceiling nothing gets past.
func testPolicy() domain.ApprovalPolicy {
	return domain.ApprovalPolicy{
		ActionType:       testAction,
		Currency:         "IDR",
		AutoApproveBelow: 100_000,
		HardCap:          5_000_000,
		DailyCap:         1_000_000,
		Enabled:          true,
	}
}

type approvalsFixture struct {
	gate      *Approvals
	approvals *fakeApprovals
	spend     *fakeSpend
	policies  *fakePolicies
	flags     *fakeFlags
	audit     *fakeAudit
	notifier  *fakeNotifier
}

// newApprovalsFixture wires the gate with one policy in place.
func newApprovalsFixture(t *testing.T, policies ...domain.ApprovalPolicy) *approvalsFixture {
	t.Helper()
	byAction := map[string]domain.ApprovalPolicy{}
	for _, p := range policies {
		byAction[p.ActionType] = p
	}
	f := &approvalsFixture{
		approvals: newFakeApprovals(),
		spend:     &fakeSpend{},
		policies:  &fakePolicies{policies: byAction},
		flags:     &fakeFlags{},
		audit:     &fakeAudit{},
		notifier:  &fakeNotifier{},
	}

	gate, err := NewApprovals(ApprovalsDeps{
		Approvals: f.approvals,
		Spend:     f.spend,
		Policies:  f.policies,
		Flags:     f.flags,
		Audit:     f.audit,
		Notices:   NewNotices(NoticesDeps{Notifier: f.notifier, ConsoleBaseURL: "https://console.wingman.test"}),
		Clock:     fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a gate, got %v", err)
	}
	f.gate = gate
	return f
}

// spendReq is a well-formed request the tests vary one field of at a time.
func spendReq(amount float64) SpendRequest {
	return SpendRequest{
		ActionType:  testAction,
		Amount:      amount,
		Currency:    "IDR",
		RequestedBy: "bot-1",
		Payload:     `{"campaign":"launch"}`,
	}
}

func TestNewApprovalsNamesEveryMissingDependency(t *testing.T) {
	_, err := NewApprovals(ApprovalsDeps{})
	if err == nil {
		t.Fatal("expected an incomplete gate to be refused")
	}
	for _, name := range []string{"Approvals", "Spend", "Policies", "Flags", "Audit"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("expected %s to be named in %v", name, err)
		}
	}
}

func TestNewApprovalsFillsInItsOptionalDefaults(t *testing.T) {
	f := newApprovalsFixture(t)
	if f.gate.ttl != DefaultApprovalTTL {
		t.Fatalf("expected the default TTL, got %v", f.gate.ttl)
	}

	gate, err := NewApprovals(ApprovalsDeps{
		Approvals: f.approvals, Spend: f.spend, Policies: f.policies,
		Flags: f.flags, Audit: f.audit, TTL: -time.Hour,
	})
	if err != nil {
		t.Fatalf("expected a gate, got %v", err)
	}
	if gate.ttl != DefaultApprovalTTL || gate.clock == nil {
		t.Fatalf("expected meaningless values to be replaced, got %+v", gate)
	}
}

func TestRequestAutoApprovesASmallAmountAndPutsItOnTheLedger(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	record, err := f.gate.Request(context.Background(), spendReq(50_000))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalAutoApproved {
		t.Fatalf("expected auto-approval, got %q (%s)", record.Outcome, record.PolicyReason)
	}
	// Spend that happens with nobody watching must still be counted, or the daily
	// cap is measuring only the amounts a human happened to see.
	if len(f.spend.recorded) != 1 || f.spend.recorded[0].Amount != 50_000 {
		t.Fatalf("expected the amount on the ledger, got %+v", f.spend.recorded)
	}
	if f.spend.recorded[0].ApprovalID == nil || *f.spend.recorded[0].ApprovalID != record.ID {
		t.Fatalf("expected the ledger row to name the approval, got %+v", f.spend.recorded[0])
	}
	if !f.audit.has(ActionApprovalDecided) {
		t.Fatalf("expected the decision to be audited, got %v", f.audit.actions())
	}
}

func TestRequestRecordsAutoApprovedSpendInsideTheLock(t *testing.T) {
	// Recording after the lock releases would let a burst of small amounts each
	// read a stale total and slip past the daily cap together.
	f := newApprovalsFixture(t, testPolicy())

	if _, err := f.gate.Request(context.Background(), spendReq(50_000)); err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if f.spend.recordedInLock != 1 || f.spend.recordedOutLock != 0 {
		t.Fatalf("expected the ledger write inside the lock, got in=%d out=%d",
			f.spend.recordedInLock, f.spend.recordedOutLock)
	}
	if f.spend.spentInsideLock != 1 || f.spend.spentOutsideLock != 0 {
		t.Fatalf("expected today's total read inside the lock, got in=%d out=%d",
			f.spend.spentInsideLock, f.spend.spentOutsideLock)
	}
	if len(f.spend.locks) != 1 || f.spend.locks[0] != testAction {
		t.Fatalf("expected one lock on the action type, got %v", f.spend.locks)
	}
}

func TestRequestSendsALargeAmountToAHumanWithADeadline(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	record, err := f.gate.Request(context.Background(), spendReq(500_000))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected a human to be asked, got %q", record.Outcome)
	}
	// A pending request with no expiry becomes a question nobody ever closes.
	if record.ExpiresAt == nil || !record.ExpiresAt.Equal(testNow.Add(DefaultApprovalTTL)) {
		t.Fatalf("expected an expiry one TTL out, got %v", record.ExpiresAt)
	}
	// Nothing is committed until a human says yes.
	if len(f.spend.recorded) != 0 {
		t.Fatalf("expected nothing on the ledger yet, got %+v", f.spend.recorded)
	}
}

func TestRequestAsksAHumanWhenNoPolicyIsConfigured(t *testing.T) {
	// Deny-by-default: an action type nobody has written a policy for is not free,
	// it is unreviewed.
	f := newApprovalsFixture(t)

	record, err := f.gate.Request(context.Background(), spendReq(1))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected an unknown action type to need a human, got %q", record.Outcome)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing to be spent under no policy")
	}
}

func TestRequestDeniesWhenTheKillSwitchIsEngaged(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	f.flags.engaged = true

	record, err := f.gate.Request(context.Background(), spendReq(1))
	if err != nil {
		t.Fatalf("expected a recorded denial, got %v", err)
	}
	if record.Outcome != domain.ApprovalDenied {
		t.Fatalf("expected a denial, got %q", record.Outcome)
	}
	// The denial is recorded rather than silently dropped: an operator needs to see
	// what the agent tried to do while the engine was stopped.
	if len(f.approvals.created) != 1 {
		t.Fatalf("expected the attempt to be recorded, got %+v", f.approvals.created)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing to be spent while halted")
	}
}

func TestRequestRefusesToDecideWhenTheKillSwitchCannotBeRead(t *testing.T) {
	// The decision core reads an engaged switch as a denial, so a failed read must
	// surface as an error rather than a silent false that lets spend through.
	f := newApprovalsFixture(t, testPolicy())
	f.flags.err = errBoom

	if _, err := f.gate.Request(context.Background(), spendReq(1)); err == nil {
		t.Fatal("expected an unreadable kill switch to block the decision")
	}
	if len(f.approvals.created) != 0 || len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing to be recorded")
	}
}

func TestRequestDeniesAmountsOverTheHardCap(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	record, err := f.gate.Request(context.Background(), spendReq(9_000_000))
	if err != nil {
		t.Fatalf("expected a recorded denial, got %v", err)
	}
	if record.Outcome != domain.ApprovalDenied {
		t.Fatalf("expected the hard cap to deny, got %q", record.Outcome)
	}
	// A hard cap breach never becomes a card a tired operator can wave through.
	if record.ExpiresAt != nil {
		t.Fatalf("expected no expiry on a denial, got %v", record.ExpiresAt)
	}
}

func TestRequestDeniesWhenTodaysSpendWouldPassTheDailyCap(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	f.spend.spentToday = 950_000

	record, err := f.gate.Request(context.Background(), spendReq(80_000))
	if err != nil {
		t.Fatalf("expected a recorded denial, got %v", err)
	}
	if record.Outcome != domain.ApprovalDenied {
		t.Fatalf("expected the daily cap to deny, got %q (%s)", record.Outcome, record.PolicyReason)
	}
	if !strings.Contains(strings.ToLower(record.PolicyReason), "daily") {
		t.Fatalf("expected the reason to name the daily cap, got %q", record.PolicyReason)
	}
}

func TestRequestDeniesADisabledActionType(t *testing.T) {
	policy := testPolicy()
	policy.Enabled = false
	f := newApprovalsFixture(t, policy)

	record, err := f.gate.Request(context.Background(), spendReq(1))
	if err != nil {
		t.Fatalf("expected a recorded denial, got %v", err)
	}
	if record.Outcome != domain.ApprovalDenied {
		t.Fatalf("expected a disabled action type to be denied, got %q", record.Outcome)
	}
}

func TestRequestDeniesACurrencyThePolicyDoesNotCover(t *testing.T) {
	// A policy denominated in IDR says nothing about 5,000 USD.
	f := newApprovalsFixture(t, testPolicy())
	req := spendReq(5_000)
	req.Currency = "USD"

	record, err := f.gate.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("expected a recorded denial, got %v", err)
	}
	if record.Outcome != domain.ApprovalDenied {
		t.Fatalf("expected a currency mismatch to be denied, got %q", record.Outcome)
	}
}

func TestRequestNormalisesTheCurrencyBeforeDeciding(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	req := spendReq(50_000)
	req.Currency = " idr "

	record, err := f.gate.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Currency != "IDR" {
		t.Fatalf("expected a normalised currency, got %q", record.Currency)
	}
	if record.Outcome != domain.ApprovalAutoApproved {
		t.Fatalf("expected whitespace not to change the outcome, got %q", record.Outcome)
	}
}

func TestRequestRejectsAnUndecidableRequest(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	cases := map[string]SpendRequest{
		"no action type": {Amount: 1, Currency: "IDR", RequestedBy: "bot-1"},
		"no currency":    {ActionType: testAction, Amount: 1, RequestedBy: "bot-1"},
		"no requester":   {ActionType: testAction, Amount: 1, Currency: "IDR"},
		"blank action":   {ActionType: "   ", Amount: 1, Currency: "IDR", RequestedBy: "bot-1"},
	}
	for name, req := range cases {
		_, err := f.gate.Request(context.Background(), req)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: expected a validation error, got %v", name, err)
		}
	}
	if len(f.approvals.created) != 0 {
		t.Fatal("expected nothing to be recorded for a malformed request")
	}
}

func TestRequestDeniesAnAmountThatIsNotRealMoney(t *testing.T) {
	// NaN compares false against every threshold, so an unguarded gate would read
	// a broken amount as under the limit.
	f := newApprovalsFixture(t, testPolicy())
	for _, amount := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		record, err := f.gate.Request(context.Background(), spendReq(amount))
		if err != nil {
			t.Fatalf("amount %v: expected a recorded denial, got %v", amount, err)
		}
		if record.Outcome != domain.ApprovalDenied {
			t.Fatalf("amount %v: expected a denial, got %q", amount, record.Outcome)
		}
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing to reach the ledger")
	}
}

func TestRequestReturnsTheExistingDecisionForARetriedKey(t *testing.T) {
	// Two open requests for one action are how an operator ends up approving the
	// same spend twice.
	f := newApprovalsFixture(t, testPolicy())
	req := spendReq(500_000)
	req.IdempotencyKey = "task-77:budget"

	first, err := f.gate.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	second, err := f.gate.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("expected the retry to be answered, got %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the same request back, got %q and %q", first.ID, second.ID)
	}
	if len(f.approvals.created) != 1 {
		t.Fatalf("expected only one request to be opened, got %d", len(f.approvals.created))
	}
	// The retry short-circuits before the lock: re-deciding would risk a second
	// ledger row for one intent.
	if len(f.spend.locks) != 1 {
		t.Fatalf("expected the retry not to re-decide, got %v", f.spend.locks)
	}
}

func TestRequestFailsWhenTheIdempotencyLookupBreaks(t *testing.T) {
	// Deciding anyway could open a second request for spend already approved.
	f := newApprovalsFixture(t, testPolicy())
	f.approvals.byKeyErr = errBoom
	req := spendReq(1)
	req.IdempotencyKey = "task-77:budget"

	if _, err := f.gate.Request(context.Background(), req); err == nil {
		t.Fatal("expected the lookup failure to block the decision")
	}
	if len(f.approvals.created) != 0 {
		t.Fatal("expected no request to be opened")
	}
}

func TestRequestFailsWhenTodaysSpendCannotBeRead(t *testing.T) {
	// Without today's total the daily cap is unknown, and deciding without it means
	// deciding without the cap.
	f := newApprovalsFixture(t, testPolicy())
	f.spend.spentErr = errBoom

	if _, err := f.gate.Request(context.Background(), spendReq(1)); err == nil {
		t.Fatal("expected an unreadable ledger to block the decision")
	}
	if len(f.approvals.created) != 0 {
		t.Fatal("expected no decision to be recorded")
	}
}

func TestRequestFailsWhenTheLockCannotBeTaken(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	f.spend.lockErr = errBoom

	if _, err := f.gate.Request(context.Background(), spendReq(1)); err == nil {
		t.Fatal("expected a failed lock to block the decision")
	}
}

func TestRequestFailsWhenTheDecisionCannotBeRecorded(t *testing.T) {
	// An unrecorded auto-approval is spend with no paper trail at all.
	f := newApprovalsFixture(t, testPolicy())
	f.approvals.createErr = errBoom

	if _, err := f.gate.Request(context.Background(), spendReq(50_000)); err == nil {
		t.Fatal("expected the decision to fail")
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing on the ledger without a recorded decision")
	}
}

func TestRequestFailsWhenAutoApprovedSpendCannotBeRecorded(t *testing.T) {
	// Letting this through would return an approval whose amount the daily cap
	// never counts.
	f := newApprovalsFixture(t, testPolicy())
	f.spend.recordErr = errBoom

	if _, err := f.gate.Request(context.Background(), spendReq(50_000)); err == nil {
		t.Fatal("expected an unrecordable auto-approval to fail")
	}
}

func TestRequestStillDecidesWhenTheAuditWriteFails(t *testing.T) {
	// By the time the audit entry is written the decision is already recorded;
	// failing here would make the caller retry a decision that must not repeat.
	f := newApprovalsFixture(t, testPolicy())
	f.audit.err = errBoom

	record, err := f.gate.Request(context.Background(), spendReq(50_000))
	if err != nil {
		t.Fatalf("expected the decision to stand, got %v", err)
	}
	if record.Outcome != domain.ApprovalAutoApproved {
		t.Fatalf("expected auto-approval, got %q", record.Outcome)
	}
}

func TestRequestKeepsTheRequestingBotOutOfTheDecision(t *testing.T) {
	// The bot's identity is recorded for the audit trail; it must never widen a
	// limit.
	f := newApprovalsFixture(t, testPolicy())
	req := spendReq(500_000)
	req.RequestedBy = "bot-with-a-trustworthy-name"

	record, err := f.gate.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected the amount alone to decide, got %q", record.Outcome)
	}
	if record.RequestedBy != "bot-with-a-trustworthy-name" {
		t.Fatalf("expected the requester to be recorded, got %q", record.RequestedBy)
	}
}

// --- Resolve ---

// pendingApproval opens a request that is waiting for a human.
func pendingApproval(t *testing.T, f *approvalsFixture, amount float64) repository.ApprovalRecord {
	t.Helper()
	record, err := f.gate.Request(context.Background(), spendReq(amount))
	if err != nil {
		t.Fatalf("expected a pending request, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected a pending request, got %q", record.Outcome)
	}
	return record
}

func TestResolveApprovalPutsTheAmountOnTheLedger(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	resolved, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "checked the campaign", testActor())
	if err != nil {
		t.Fatalf("expected the approval to be recorded, got %v", err)
	}
	if resolved.Resolution == nil || *resolved.Resolution != repository.ResolutionApproved {
		t.Fatalf("expected an approved resolution, got %+v", resolved.Resolution)
	}
	if len(f.spend.recorded) != 1 || f.spend.recorded[0].Amount != 500_000 {
		t.Fatalf("expected the amount on the ledger, got %+v", f.spend.recorded)
	}
	if f.spend.recordedInLock != 1 {
		t.Fatalf("expected the ledger write inside the lock, got %d", f.spend.recordedInLock)
	}
	if !f.audit.has(ActionApprovalResolved) || !f.audit.has(ActionSpendRecorded) {
		t.Fatalf("expected both entries to be audited, got %v", f.audit.actions())
	}
}

func TestResolveRejectionLeavesTheLedgerAlone(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	resolved, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionRejected, "wrong campaign", testActor())
	if err != nil {
		t.Fatalf("expected the rejection to be recorded, got %v", err)
	}
	if resolved.Resolution == nil || *resolved.Resolution != repository.ResolutionRejected {
		t.Fatalf("expected a rejected resolution, got %+v", resolved.Resolution)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatalf("expected nothing on the ledger, got %+v", f.spend.recorded)
	}
}

func TestResolveRefusesAnAnonymousDecision(t *testing.T) {
	// The audit trail has to name who cleared the spend.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	_, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", Actor{Type: repository.ActorUser, ID: "   "})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected nothing to be spent on an anonymous approval")
	}
}

func TestResolveRefusesAResolutionThatIsNotAHumanAnswer(t *testing.T) {
	// "Expired" is something the system does when nobody answers, not an answer.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	for _, resolution := range []repository.ApprovalResolution{repository.ResolutionExpired, "", "maybe"} {
		if _, err := f.gate.Resolve(context.Background(), pending.ID, resolution, "", testActor()); !errors.Is(err, ErrValidation) {
			t.Fatalf("resolution %q: expected a validation error, got %v", resolution, err)
		}
	}
}

func TestResolveRefusesABlankID(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	if _, err := f.gate.Resolve(context.Background(), "  ", repository.ResolutionApproved, "", testActor()); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected a validation error, got %v", err)
	}
}

func TestResolveReportsAnUnknownApproval(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	if _, err := f.gate.Resolve(context.Background(), "approval-404", repository.ResolutionApproved, "", testActor()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestResolveRefusesToAnswerTwice(t *testing.T) {
	// A second answer would overwrite a decision that has already been acted on.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	if _, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", testActor()); err != nil {
		t.Fatalf("expected the first answer to stand, got %v", err)
	}
	_, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionRejected, "", Actor{Type: repository.ActorUser, ID: "someone-else"})
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("expected the second answer to be refused, got %v", err)
	}
	if len(f.spend.recorded) != 1 {
		t.Fatalf("expected one ledger row, got %+v", f.spend.recorded)
	}
}

func TestResolveTreatsARaceAsAlreadyResolved(t *testing.T) {
	// Somebody answered between the read and the write. Their answer stands.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)
	f.approvals.resolveErr = repository.ErrConflict

	_, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", testActor())
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("expected the race to read as already resolved, got %v", err)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatal("expected no ledger row for a resolution that did not land")
	}
}

func TestResolveReportsWhenApprovedSpendCannotBeRecorded(t *testing.T) {
	// The approval stands — a human said yes and that is recorded — but the daily
	// cap is now under-counting, so the caller has to hear about it.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)
	f.spend.recordErr = errBoom

	resolved, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", testActor())
	if err == nil {
		t.Fatal("expected the ledger failure to be reported")
	}
	if resolved.ID != pending.ID {
		t.Fatalf("expected the approval to be returned alongside the error, got %+v", resolved)
	}
	if resolved.Resolution == nil || *resolved.Resolution != repository.ResolutionApproved {
		t.Fatalf("expected the human's answer to stand, got %+v", resolved.Resolution)
	}
}

func TestResolveTruncatesAnOverlongNote(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	resolved, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved,
		strings.Repeat("é", maxNoteLength+500), testActor())
	if err != nil {
		t.Fatalf("expected the approval to stand, got %v", err)
	}
	if got := len([]rune(resolved.ResolutionNote)); got != maxNoteLength {
		t.Fatalf("expected the note capped at %d runes, got %d", maxNoteLength, got)
	}
}

func TestResolveReportsAFailureToReadTheApproval(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)
	f.approvals.byIDErr = errBoom

	if _, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", testActor()); err == nil {
		t.Fatal("expected the read failure to be reported")
	}
}

func TestResolveRefusesToLetABotClearItsOwnSpend(t *testing.T) {
	// This is the whole approval gate in one test. If an agent can answer its own
	// pending request, "over the threshold, ask a human" is a comment rather than a
	// control, and every other safeguard here is downstream of a decision the agent
	// already made for itself.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	_, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "looks fine to me", testBot())

	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected the bot refused, got %v", err)
	}
	if len(f.approvals.resolved) != 0 {
		t.Fatalf("expected the request left pending, got %+v", f.approvals.resolved)
	}
	if len(f.spend.recorded) != 0 {
		t.Fatalf("expected nothing on the ledger, got %+v", f.spend.recorded)
	}
}

func TestResolveRefusesAnActorItCannotPlace(t *testing.T) {
	// An unrecognised actor type normalises to bot — the least privileged reading —
	// so a caller cannot reach a human's decision by inventing a role.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	for _, actor := range []Actor{
		{Type: "superuser", ID: "who"},
		{Type: repository.ActorSystem, ID: "goal-engine"},
	} {
		_, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", actor)
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("actor %q: expected ErrForbidden, got %v", actor.Type, err)
		}
	}
}

func TestResolveTiesTheDecisionToTheRequestThatMadeIt(t *testing.T) {
	// "ops approved 5 million" is only half an answer six weeks later. The request
	// id is what connects it to the rest of that minute's log.
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	if _, err := f.gate.Resolve(context.Background(), pending.ID, repository.ResolutionApproved, "", testActor()); err != nil {
		t.Fatalf("expected the approval to stand, got %v", err)
	}

	entry := f.audit.last(t, ActionApprovalResolved)
	if entry.RequestID != "req-1" {
		t.Fatalf("expected the request id recorded, got %q", entry.RequestID)
	}
	if entry.ActorType != repository.ActorUser || entry.ActorID != "ops" {
		t.Fatalf("expected the human named, got %s/%s", entry.ActorType, entry.ActorID)
	}
}

// --- Get / List / ExpireOverdue ---

func TestGetReturnsARequestAndDistinguishesAMissingOne(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	pending := pendingApproval(t, f, 500_000)

	record, err := f.gate.Get(context.Background(), pending.ID)
	if err != nil {
		t.Fatalf("expected the request back, got %v", err)
	}
	if record.ID != pending.ID {
		t.Fatalf("expected %q, got %q", pending.ID, record.ID)
	}

	if _, err := f.gate.Get(context.Background(), "approval-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}

	f.approvals.byIDErr = errBoom
	_, err = f.gate.Get(context.Background(), pending.ID)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a read failure to be distinct from not-found, got %v", err)
	}
}

func TestListReturnsTheMatchesAndTheTotal(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	f.approvals.listed = []repository.ApprovalRecord{{ID: "approval-1"}, {ID: "approval-2"}}
	f.approvals.listTotal = 7

	records, total, err := f.gate.List(context.Background(), repository.ApprovalFilter{Limit: 2})
	if err != nil {
		t.Fatalf("expected a page, got %v", err)
	}
	if len(records) != 2 || total != 7 {
		t.Fatalf("expected 2 of 7, got %d of %d", len(records), total)
	}

	f.approvals.listErr = errBoom
	if _, _, err := f.gate.List(context.Background(), repository.ApprovalFilter{}); err == nil {
		t.Fatal("expected the list failure to be reported")
	}
}

func TestExpireOverdueLapsesUnansweredRequests(t *testing.T) {
	// "Waiting for a human" must not become a permanent state a later operator
	// mistakes for a live question.
	f := newApprovalsFixture(t, testPolicy())
	f.approvals.expired = 3

	count, err := f.gate.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("expected the sweep to run, got %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 expiries, got %d", count)
	}
	if !f.audit.has(ActionApprovalResolved) {
		t.Fatalf("expected the expiries to be audited, got %v", f.audit.actions())
	}
}

func TestExpireOverdueStaysQuietWhenThereIsNothingToExpire(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	count, err := f.gate.ExpireOverdue(context.Background())
	if err != nil {
		t.Fatalf("expected the sweep to run, got %v", err)
	}
	if count != 0 || len(f.audit.actions()) != 0 {
		t.Fatalf("expected no audit churn, got %d expiries and %v", count, f.audit.actions())
	}
}

func TestExpireOverdueReportsAFailedSweep(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())
	f.approvals.expireErr = errBoom

	if _, err := f.gate.ExpireOverdue(context.Background()); err == nil {
		t.Fatal("expected the sweep failure to be reported")
	}
}

// --- notifications ---
//
// A notification is the last thing Request does and the least important. These
// tests fix which outcomes are worth waking somebody for, and prove that the
// message cannot change the decision it describes.

func TestRequestNotifiesAHumanOnlyWhenOneIsBeingAsked(t *testing.T) {
	f := newApprovalsFixture(t, testPolicy())

	record, err := f.gate.Request(context.Background(), spendReq(500_000))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected a human to be asked, got %q", record.Outcome)
	}
	if len(f.notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %+v", f.notifier.sent)
	}
	sent := f.notifier.sent[0]
	if sent.Kind != core.NotifyApprovalPending {
		t.Fatalf("expected an approval notification, got %q", sent.Kind)
	}
	if sent.SubjectID != record.ID {
		t.Fatalf("expected the notification to name the approval, got %q", sent.SubjectID)
	}
	// The deck, not a per-request screen: its first panel is the queue, which is what
	// somebody woken by this message needs open.
	if sent.Link != "https://console.wingman.test" {
		t.Fatalf("expected a link to the console deck, got %q", sent.Link)
	}
	// The action type is a policy name an operator chose, and the difference between
	// "the familiar gate again" and "something I have never seen".
	if !strings.Contains(sent.Headline, testAction) {
		t.Fatalf("expected the headline to name the action type, got %q", sent.Headline)
	}
}

func TestRequestNotifiesNobodyAboutAnAutoApprovedSpend(t *testing.T) {
	// Nobody is needed, and a stream of messages about spend that needed nobody is
	// how the one that does need somebody gets skimmed past.
	f := newApprovalsFixture(t, testPolicy())

	record, err := f.gate.Request(context.Background(), spendReq(50_000))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalAutoApproved {
		t.Fatalf("expected auto-approval, got %q", record.Outcome)
	}
	if len(f.notifier.sent) != 0 {
		t.Fatalf("expected no notification, got %+v", f.notifier.sent)
	}
}

func TestRequestNotifiesNobodyAboutADenial(t *testing.T) {
	// A denial is already over. There is nothing to decide and nothing a recipient
	// could do about it from a phone; it is in the audit log for whoever reviews.
	for _, tc := range []struct {
		name  string
		setup func(*approvalsFixture)
		req   SpendRequest
	}{
		{"over the hard cap", func(*approvalsFixture) {}, spendReq(9_000_000)},
		{"kill switch engaged", func(f *approvalsFixture) { f.flags.engaged = true }, spendReq(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalsFixture(t, testPolicy())
			tc.setup(f)

			record, err := f.gate.Request(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("expected a recorded denial, got %v", err)
			}
			if record.Outcome != domain.ApprovalDenied {
				t.Fatalf("expected a denial, got %q", record.Outcome)
			}
			if len(f.notifier.sent) != 0 {
				t.Fatalf("expected no notification, got %+v", f.notifier.sent)
			}
		})
	}
}

func TestRequestNotificationCarriesNoAmountOrCurrency(t *testing.T) {
	// The message is retained on somebody else's servers, and a message complete
	// enough to approve from invites approving from it. The figures are one click
	// away, in the console, next to the policy and the day's total.
	f := newApprovalsFixture(t, testPolicy())

	if _, err := f.gate.Request(context.Background(), spendReq(500_000)); err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if len(f.notifier.sent) != 1 {
		t.Fatalf("expected one notification, got %+v", f.notifier.sent)
	}
	headline := f.notifier.sent[0].Headline
	for _, r := range headline {
		if r >= '0' && r <= '9' {
			t.Fatalf("expected no figure in a headline, got %q", headline)
		}
	}
	if strings.Contains(headline, "IDR") {
		t.Fatalf("expected no currency in a headline, got %q", headline)
	}
}

func TestRequestDecidesIdenticallyWhenTheNotifierFails(t *testing.T) {
	// The decision is durable before the message is attempted. A chat platform being
	// down must not turn a recorded pending request into an error the agent retries.
	f := newApprovalsFixture(t, testPolicy())
	f.notifier.err = errBoom

	record, err := f.gate.Request(context.Background(), spendReq(500_000))
	if err != nil {
		t.Fatalf("expected the failed send to be swallowed, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected the outcome to stand, got %q", record.Outcome)
	}
	if record.ExpiresAt == nil || !record.ExpiresAt.Equal(testNow.Add(DefaultApprovalTTL)) {
		t.Fatalf("expected the deadline to stand, got %v", record.ExpiresAt)
	}
	if !f.audit.has(ActionApprovalDecided) {
		t.Fatalf("expected the decision to be audited, got %v", f.audit.actions())
	}
	// Attempted, not skipped: the fake records before it fails.
	if len(f.notifier.sent) != 1 {
		t.Fatalf("expected the send to have been attempted, got %+v", f.notifier.sent)
	}
}

func TestRequestDecidesIdenticallyWithNotificationsTurnedOff(t *testing.T) {
	// NOTIFY_ENABLED=false is a nil *Notices, and the gate must not carry a question
	// about messaging into the decision path at all.
	f := newApprovalsFixture(t, testPolicy())
	gate, err := NewApprovals(ApprovalsDeps{
		Approvals: f.approvals, Spend: f.spend, Policies: f.policies,
		Flags: f.flags, Audit: f.audit, Clock: fixedClock(testNow),
	})
	if err != nil {
		t.Fatalf("expected a gate, got %v", err)
	}

	record, err := gate.Request(context.Background(), spendReq(500_000))
	if err != nil {
		t.Fatalf("expected a decision, got %v", err)
	}
	if record.Outcome != domain.ApprovalPending {
		t.Fatalf("expected a human to be asked, got %q", record.Outcome)
	}
	if len(f.notifier.sent) != 0 {
		t.Fatalf("expected nothing sent, got %+v", f.notifier.sent)
	}
}

func TestStartOfDayResetsWithTheOperatorsDayNotUTCs(t *testing.T) {
	// A cap called "daily" that rolls over at 07:00 local is not a daily cap.
	jakarta := time.FixedZone("WIB", 7*60*60)
	local := time.Date(2026, 9, 11, 2, 30, 0, 0, jakarta)

	got := startOfDay(local)
	if got.Hour() != 0 || got.Day() != 11 || got.Location() != jakarta {
		t.Fatalf("expected local midnight on the 11th, got %v", got)
	}
	// 02:30 WIB is still the 10th in UTC, which is exactly the day a UTC-based
	// truncation would have charged the spend to.
	if got.UTC().Day() != 10 {
		t.Fatalf("expected the fixture to straddle the UTC date boundary, got %v", got.UTC())
	}
}
