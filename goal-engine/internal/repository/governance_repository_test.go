package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
)

// --- dispatches ---

func dispatchSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "goal_id", "evaluation_id", "bot_id", "channel_id",
		"idempotency_key", "brief", "request_payload", "status", "attempts",
		"response_status", "external_task_id", "last_error", "created_at", "sent_at",
	})
}

func addDispatchRow(rows *sqlmock.Rows, status string) *sqlmock.Rows {
	return rows.AddRow(
		"33333333-3333-3333-3333-333333333333", "goal-1", "eval-1", "bot-1", "chan-1",
		"goal-1:2026-09", "MRR is behind pace", `{"goalId":"goal-1"}`, status, 0,
		nil, nil, nil, testPeriodStart, nil,
	)
}

func testDispatch() DispatchInput {
	return DispatchInput{
		GoalID:         "goal-1",
		EvaluationID:   "eval-1",
		BotID:          "bot-1",
		ChannelID:      "chan-1",
		IdempotencyKey: "goal-1:2026-09",
		Brief:          "MRR is behind pace",
		RequestPayload: `{"goalId":"goal-1"}`,
	}
}

func TestDispatchRepositoryCreateStoresTheClaim(t *testing.T) {
	// Arrange
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("INSERT INTO trigger_dispatches").
		WithArgs("goal-1", "eval-1", "bot-1", "chan-1", "goal-1:2026-09",
			"MRR is behind pace", `{"goalId":"goal-1"}`).
		WillReturnRows(addDispatchRow(dispatchSQLRows(), "pending"))

	// Act
	record, err := repo.Create(context.Background(), testDispatch())

	// Assert
	if err != nil {
		t.Fatalf("expected the dispatch to be created, got %v", err)
	}
	if record.Status != DispatchPending {
		t.Fatalf("expected a pending dispatch, got %q", record.Status)
	}
	if record.ResponseStatus != nil || record.SentAt != nil {
		t.Fatalf("expected an unsent dispatch, got %+v", record)
	}
	if record.RequestPayload != `{"goalId":"goal-1"}` {
		t.Fatalf("expected the payload to round-trip as text, got %q", record.RequestPayload)
	}
}

func TestDispatchRepositoryCreateRefusesAnUnkeyedDispatchWithoutAskingTheDatabase(t *testing.T) {
	// No expectation is registered on purpose. Without an idempotency key there is
	// nothing stopping a retry from waking a second agent for the same shortfall,
	// so the write must be refused before it reaches the database.
	db, _ := newTestDB(t)
	repo := NewDispatchRepository(db)
	input := testDispatch()
	input.IdempotencyKey = ""

	if _, err := repo.Create(context.Background(), input); err == nil {
		t.Fatal("expected an unkeyed dispatch to be refused")
	}
}

func TestDispatchRepositoryCreateDefaultsAnEmptyPayloadToAnObject(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	input := testDispatch()
	input.RequestPayload = ""
	mock.ExpectQuery("INSERT INTO trigger_dispatches").
		WithArgs("goal-1", "eval-1", "bot-1", "chan-1", "goal-1:2026-09",
			"MRR is behind pace", "{}").
		WillReturnRows(addDispatchRow(dispatchSQLRows(), "pending"))

	if _, err := repo.Create(context.Background(), input); err != nil {
		t.Fatalf("expected the dispatch to be created, got %v", err)
	}
}

func TestDispatchRepositoryCreateReportsADuplicateAsAConflict(t *testing.T) {
	// A duplicate is the healthy outcome of a retry: the caller has to be able to
	// tell it apart from a real failure, or it will retry forever.
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("INSERT INTO trigger_dispatches").
		WillReturnError(&pq.Error{Code: pgUniqueViolation})

	_, err := repo.Create(context.Background(), testDispatch())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestDispatchRepositoryGetByIdempotencyKeyReportsAMissingDispatch(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("WHERE idempotency_key = \\$1").
		WithArgs("goal-1:2026-09").
		WillReturnRows(dispatchSQLRows())

	_, err := repo.GetByIdempotencyKey(context.Background(), "goal-1:2026-09")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDispatchRepositoryMarkSentCountsTheAttempt(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectExec("SET status = 'sent', attempts = attempts \\+ 1").
		WithArgs("dispatch-1", 202, "task-9").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkSent(context.Background(), "dispatch-1", 202, "task-9"); err != nil {
		t.Fatalf("expected the dispatch to be marked sent, got %v", err)
	}
}

func TestDispatchRepositoryMarkSentReportsAMissingDispatch(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectExec("UPDATE trigger_dispatches").
		WithArgs("missing", 202, nil).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.MarkSent(context.Background(), "missing", 202, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDispatchRepositoryMarkFailedStoresAnAbsentStatusAsNull(t *testing.T) {
	// A transport failure has no HTTP status at all, which is different from a
	// zero status and has to stay distinguishable in the record.
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectExec("SET status = 'failed'").
		WithArgs("dispatch-1", nil, "connection refused").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkFailed(context.Background(), "dispatch-1", nil, "connection refused"); err != nil {
		t.Fatalf("expected the dispatch to be marked failed, got %v", err)
	}
}

func TestDispatchRepositoryMarkFailedKeepsAResponseStatus(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	status := 503
	mock.ExpectExec("SET status = 'failed'").
		WithArgs("dispatch-1", 503, "core unavailable").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.MarkFailed(context.Background(), "dispatch-1", &status, "core unavailable"); err != nil {
		t.Fatalf("expected the dispatch to be marked failed, got %v", err)
	}
}

func TestDispatchRepositoryMarkAbandonedReportsAMissingDispatch(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectExec("SET status = 'abandoned'").
		WithArgs("missing", "gave up after 5 attempts").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.MarkAbandoned(context.Background(), "missing", "gave up after 5 attempts")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDispatchRepositoryHistoryOnlyCountsDispatchesThatReachedAnAgent(t *testing.T) {
	// This is the trigger budget. Counting failed dispatches would let one outage
	// burn a goal's entire allowance without a single agent ever waking up, so the
	// status filter is asserted rather than assumed.
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	lastAt := time.Date(2026, 9, 10, 6, 0, 0, 0, time.UTC)
	mock.ExpectQuery("status IN \\('pending', 'sent'\\)").
		WithArgs("goal-1", testPeriodStart).
		WillReturnRows(sqlmock.NewRows([]string{"last_at", "total"}).AddRow(lastAt, 2))

	history, err := repo.History(context.Background(), "goal-1", testPeriodStart)
	if err != nil {
		t.Fatalf("expected a history, got %v", err)
	}
	if history.CountThisPeriod != 2 {
		t.Fatalf("expected 2 triggers this period, got %d", history.CountThisPeriod)
	}
	if history.LastTriggeredAt == nil || !history.LastTriggeredAt.Equal(lastAt) {
		t.Fatalf("expected the last trigger time, got %v", history.LastTriggeredAt)
	}
}

func TestDispatchRepositoryHistoryLeavesNeverTriggeredAsNil(t *testing.T) {
	// max() over no rows is NULL. Reading that as the zero time would put the last
	// trigger in year 1 and make every cooldown look expired — which is safe here,
	// but only by accident, so nil is kept explicit.
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("FROM trigger_dispatches").
		WithArgs("goal-1", testPeriodStart).
		WillReturnRows(sqlmock.NewRows([]string{"last_at", "total"}).AddRow(nil, 0))

	history, err := repo.History(context.Background(), "goal-1", testPeriodStart)
	if err != nil {
		t.Fatalf("expected a history, got %v", err)
	}
	if history.LastTriggeredAt != nil {
		t.Fatalf("expected no last trigger, got %v", *history.LastTriggeredAt)
	}
	if history.CountThisPeriod != 0 {
		t.Fatalf("expected no triggers, got %d", history.CountThisPeriod)
	}
}

func TestDispatchRepositoryListByGoalPaginates(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM trigger_dispatches WHERE goal_id = \\$1").
		WithArgs("goal-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("LIMIT \\$2 OFFSET \\$3").
		WithArgs("goal-1", defaultPageLimit, 0).
		WillReturnRows(addDispatchRow(dispatchSQLRows(), "sent"))

	records, total, err := repo.ListByGoal(context.Background(), "goal-1", 0, 0)
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 3 || len(records) != 1 {
		t.Fatalf("unexpected result: total=%d records=%d", total, len(records))
	}
}

func TestDispatchRepositoryListRetryableExcludesExhaustedAttempts(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewDispatchRepository(db)
	mock.ExpectQuery("status IN \\('pending', 'failed'\\) AND attempts < \\$1").
		WithArgs(5, 20).
		WillReturnRows(addDispatchRow(dispatchSQLRows(), "failed"))

	records, err := repo.ListRetryable(context.Background(), 5, 20)
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if len(records) != 1 || records[0].Status != DispatchFailed {
		t.Fatalf("unexpected records %+v", records)
	}
}

// --- approvals ---

func approvalSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "action_type", "amount", "currency", "requested_by", "goal_id",
		"idempotency_key", "outcome", "policy_reason", "resolution", "resolved_by",
		"resolved_at", "resolution_note", "payload", "created_at", "expires_at",
	})
}

func addApprovalRow(rows *sqlmock.Rows, outcome string, resolution, resolvedBy any, resolvedAt any) *sqlmock.Rows {
	return rows.AddRow(
		"44444444-4444-4444-4444-444444444444", "ads.topup", 750000.0, "USD", "bot-1",
		"goal-1", "goal-1:topup:1", outcome, "above auto-approve threshold",
		resolution, resolvedBy, resolvedAt, "", `{"campaign":"x"}`, testPeriodStart, nil,
	)
}

func testApproval() ApprovalInput {
	return ApprovalInput{
		ActionType:     "ads.topup",
		Amount:         750000,
		Currency:       "usd",
		RequestedBy:    "bot-1",
		IdempotencyKey: "goal-1:topup:1",
		Outcome:        domain.ApprovalPending,
		PolicyReason:   "above auto-approve threshold",
		Payload:        `{"campaign":"x"}`,
	}
}

func TestApprovalRepositoryCreateNormalisesTheCurrency(t *testing.T) {
	// Currency comparisons decide whether a policy applies at all, so a lowercase
	// request must not create a row the policy will never match.
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	mock.ExpectQuery("INSERT INTO approval_requests").
		WithArgs("ads.topup", 750000.0, "USD", "bot-1", nil, "goal-1:topup:1",
			"pending", "above auto-approve threshold", `{"campaign":"x"}`, nil).
		WillReturnRows(addApprovalRow(approvalSQLRows(), "pending", nil, nil, nil))

	record, err := repo.Create(context.Background(), testApproval())
	if err != nil {
		t.Fatalf("expected the request to be recorded, got %v", err)
	}
	if record.Currency != "USD" {
		t.Fatalf("expected USD, got %q", record.Currency)
	}
	if !record.IsOpen() {
		t.Fatalf("expected an open request, got %+v", record)
	}
}

func TestApprovalRepositoryCreateLinksAGoalWhenGiven(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	input := testApproval()
	goalID := "goal-1"
	expiresAt := testPeriodEnd
	input.GoalID = &goalID
	input.ExpiresAt = &expiresAt

	mock.ExpectQuery("INSERT INTO approval_requests").
		WithArgs("ads.topup", 750000.0, "USD", "bot-1", "goal-1", "goal-1:topup:1",
			"pending", "above auto-approve threshold", `{"campaign":"x"}`, expiresAt).
		WillReturnRows(addApprovalRow(approvalSQLRows(), "pending", nil, nil, nil))

	record, err := repo.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("expected the request to be recorded, got %v", err)
	}
	if record.GoalID == nil || *record.GoalID != "goal-1" {
		t.Fatalf("expected the goal link to round-trip, got %v", record.GoalID)
	}
}

func TestApprovalRepositoryCreateRefusesBrokenInputWithoutAskingTheDatabase(t *testing.T) {
	// No expectation is registered: an unkeyed or non-finite spend request must be
	// refused before it can become a row a human might approve.
	db, _ := newTestDB(t)
	repo := NewApprovalRepository(db)

	unkeyed := testApproval()
	unkeyed.IdempotencyKey = ""
	if _, err := repo.Create(context.Background(), unkeyed); err == nil {
		t.Fatal("expected an unkeyed request to be refused")
	}

	broken := testApproval()
	broken.Amount = math.NaN()
	if _, err := repo.Create(context.Background(), broken); err == nil {
		t.Fatal("expected a NaN amount to be refused")
	}
}

func TestApprovalRepositoryGetByIdempotencyKeyReportsAMissingRequest(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	mock.ExpectQuery("WHERE idempotency_key = \\$1").
		WithArgs("goal-1:topup:1").
		WillReturnRows(approvalSQLRows())

	_, err := repo.GetByIdempotencyKey(context.Background(), "goal-1:topup:1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestApprovalRepositoryListOpenOnlyAddsNoPlaceholder(t *testing.T) {
	// The open filter is a fixed clause with no bound value. If it consumed a
	// placeholder anyway, the paging arguments would land in the wrong slots.
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	mock.ExpectQuery("WHERE 1 = 1 AND outcome = 'pending' AND resolution IS NULL").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("LIMIT \\$1 OFFSET \\$2").
		WithArgs(defaultPageLimit, 0).
		WillReturnRows(addApprovalRow(approvalSQLRows(), "pending", nil, nil, nil))

	records, total, err := repo.List(context.Background(), ApprovalFilter{OpenOnly: true})
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 1 || len(records) != 1 {
		t.Fatalf("unexpected result: total=%d records=%d", total, len(records))
	}
}

func TestApprovalRepositoryListFiltersByActionTypeAndOutcome(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	mock.ExpectQuery("AND action_type = \\$1 AND outcome = \\$2").
		WithArgs("ads.topup", "denied").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("LIMIT \\$3 OFFSET \\$4").
		WithArgs("ads.topup", "denied", 5, 10).
		WillReturnRows(approvalSQLRows())

	if _, _, err := repo.List(context.Background(), ApprovalFilter{
		ActionType: "ads.topup",
		Outcome:    domain.ApprovalDenied,
		Limit:      5,
		Offset:     10,
	}); err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
}

func TestApprovalRepositoryResolveRecordsTheHumanDecision(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	resolvedAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery("WHERE id = \\$1 AND outcome = 'pending' AND resolution IS NULL").
		WithArgs("approval-1", "approved", "ops", "ok, go ahead").
		WillReturnRows(addApprovalRow(approvalSQLRows(), "pending", "approved", "ops", resolvedAt))

	record, err := repo.Resolve(context.Background(), "approval-1", ResolutionApproved, "ops", "ok, go ahead")
	if err != nil {
		t.Fatalf("expected the request to be resolved, got %v", err)
	}
	if record.Resolution == nil || *record.Resolution != ResolutionApproved {
		t.Fatalf("expected an approved resolution, got %v", record.Resolution)
	}
	if record.ResolvedBy == nil || *record.ResolvedBy != "ops" {
		t.Fatalf("expected the resolver to be recorded, got %v", record.ResolvedBy)
	}
	if record.IsOpen() {
		t.Fatal("expected a resolved request to no longer be open")
	}
}

func TestApprovalRepositoryResolveInsistsOnAnAttributableDecision(t *testing.T) {
	// No expectation is registered: an anonymous approval is worse than none,
	// because the audit trail would show a spend nobody authorised.
	db, _ := newTestDB(t)
	repo := NewApprovalRepository(db)

	if _, err := repo.Resolve(context.Background(), "approval-1", ResolutionApproved, "", ""); err == nil {
		t.Fatal("expected an unattributed resolution to be refused")
	}
}

func TestApprovalRepositoryResolveReportsAnAlreadyDecidedRequestAsAConflict(t *testing.T) {
	// Two operators can click at the same moment. The loser has to learn that the
	// decision was already made rather than silently overwrite it.
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	resolvedAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery("UPDATE approval_requests").
		WithArgs("approval-1", "rejected", "ops", "").
		WillReturnRows(approvalSQLRows())
	mock.ExpectQuery("SELECT .+ FROM approval_requests WHERE id = \\$1").
		WithArgs("approval-1").
		WillReturnRows(addApprovalRow(approvalSQLRows(), "pending", "approved", "someone-else", resolvedAt))

	_, err := repo.Resolve(context.Background(), "approval-1", ResolutionRejected, "ops", "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestApprovalRepositoryResolveReportsAMissingRequest(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	mock.ExpectQuery("UPDATE approval_requests").
		WithArgs("missing", "approved", "ops", "").
		WillReturnRows(approvalSQLRows())
	mock.ExpectQuery("SELECT .+ FROM approval_requests WHERE id = \\$1").
		WithArgs("missing").
		WillReturnRows(approvalSQLRows())

	_, err := repo.Resolve(context.Background(), "missing", ResolutionApproved, "ops", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestApprovalRepositoryExpireOverdueReportsHowManyLapsed(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewApprovalRepository(db)
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	mock.ExpectExec("SET resolution = 'expired'").
		WithArgs(now).
		WillReturnResult(sqlmock.NewResult(0, 4))

	count, err := repo.ExpireOverdue(context.Background(), now)
	if err != nil {
		t.Fatalf("expected the sweep to run, got %v", err)
	}
	if count != 4 {
		t.Fatalf("expected 4 expired requests, got %d", count)
	}
}

// --- spend ---

func spendSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "action_type", "amount", "currency", "approval_id", "bot_id",
		"occurred_at", "note",
	})
}

func TestSpendRepositoryWithinActionLockSerialisesTheCapCheck(t *testing.T) {
	// The lock is the whole reason a daily cap means anything: without it two
	// concurrent requests read the same total, both find room, and both commit.
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock\\(\\$1, hashtext\\(\\$2\\)\\)").
		WithArgs(spendLockClass, "ads.topup").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("coalesce\\(sum\\(amount\\), 0\\)").
		WithArgs("ads.topup", testPeriodStart, "USD").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(1500000.0))
	mock.ExpectCommit()

	var spent float64
	err := repo.WithinActionLock(context.Background(), "ads.topup", func(locked SpendLedger) error {
		total, err := locked.SpentSince(context.Background(), "ads.topup", "usd", testPeriodStart)
		spent = total
		return err
	})
	if err != nil {
		t.Fatalf("expected the locked section to commit, got %v", err)
	}
	if spent != 1500000 {
		t.Fatalf("expected the total to reach the caller, got %v", spent)
	}
}

func TestSpendRepositoryWithinActionLockRollsBackOnFailure(t *testing.T) {
	// A cap check that failed must leave nothing behind, or a partial write would
	// count against the very budget it never cleared.
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").
		WithArgs(spendLockClass, "ads.topup").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	wanted := errors.New("over the daily cap")
	err := repo.WithinActionLock(context.Background(), "ads.topup", func(SpendLedger) error {
		return wanted
	})
	if !errors.Is(err, wanted) {
		t.Fatalf("expected the caller's error back unwrapped, got %v", err)
	}
}

func TestSpendRepositoryWithinActionLockSurfacesALockFailure(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	called := false
	err := repo.WithinActionLock(context.Background(), "ads.topup", func(SpendLedger) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected the lock failure to surface")
	}
	if called {
		t.Fatal("expected the callback to be skipped when the lock was never held")
	}
}

func TestSpendRepositoryRecordStoresACommittedSpend(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	approvalID := "approval-1"
	occurredAt := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO spend_ledger").
		WithArgs("ads.topup", 750000.0, "USD", "approval-1", "bot-1", occurredAt, "topup").
		WillReturnRows(spendSQLRows().AddRow(1, "ads.topup", 750000.0, "USD", "approval-1", "bot-1", occurredAt, "topup"))

	record, err := repo.Record(context.Background(), SpendInput{
		ActionType: "ads.topup",
		Amount:     750000,
		Currency:   "usd",
		ApprovalID: &approvalID,
		BotID:      "bot-1",
		OccurredAt: occurredAt,
		Note:       "topup",
	})
	if err != nil {
		t.Fatalf("expected the spend to be recorded, got %v", err)
	}
	if record.ApprovalID == nil || *record.ApprovalID != "approval-1" {
		t.Fatalf("expected the approval link to round-trip, got %v", record.ApprovalID)
	}
}

func TestSpendRepositoryRecordRefusesBrokenAmountsWithoutAskingTheDatabase(t *testing.T) {
	db, _ := newTestDB(t)
	repo := NewSpendRepository(db)

	if _, err := repo.Record(context.Background(), SpendInput{ActionType: "ads.topup", Amount: math.Inf(1)}); err == nil {
		t.Fatal("expected an infinite amount to be refused")
	}
}

func TestSpendRepositoryRecordStoresAnUnlinkedSpendAsNull(t *testing.T) {
	// Spend without an approval happens — an auto-approved action below the
	// threshold — and must still be countable against the daily cap.
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectQuery("INSERT INTO spend_ledger").
		WithArgs("ads.topup", 1000.0, "USD", nil, "", sqlmock.AnyArg(), "").
		WillReturnRows(spendSQLRows().AddRow(2, "ads.topup", 1000.0, "USD", nil, "", time.Now(), ""))

	record, err := repo.Record(context.Background(), SpendInput{
		ActionType: "ads.topup",
		Amount:     1000,
		Currency:   "USD",
	})
	if err != nil {
		t.Fatalf("expected the spend to be recorded, got %v", err)
	}
	if record.ApprovalID != nil {
		t.Fatalf("expected no approval link, got %v", *record.ApprovalID)
	}
}

func TestSpendRepositorySpentSinceSumsEveryCurrencyWhenNoneIsNamed(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectQuery("\\(\\$3 = '' OR currency = \\$3\\)").
		WithArgs("ads.topup", testPeriodStart, "").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0.0))

	total, err := repo.SpentSince(context.Background(), "ads.topup", "", testPeriodStart)
	if err != nil {
		t.Fatalf("expected a total, got %v", err)
	}
	if total != 0 {
		t.Fatalf("expected an empty ledger to total zero, got %v", total)
	}
}

func TestSpendRepositoryListSinceClampsTheLimit(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewSpendRepository(db)
	mock.ExpectQuery("FROM spend_ledger").
		WithArgs("", testPeriodStart, maxPageLimit).
		WillReturnRows(spendSQLRows())

	if _, err := repo.ListSince(context.Background(), "", testPeriodStart, 5000); err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
}

// --- flags ---

func flagSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"key", "enabled", "reason", "updated_by", "updated_at"})
}

func TestFlagRepositorySetDefaultsTheAuthor(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewFlagRepository(db)
	mock.ExpectQuery("INSERT INTO system_flags").
		WithArgs(KillSwitchKey, true, "incident 2026-09-11", "system").
		WillReturnRows(flagSQLRows().AddRow(KillSwitchKey, true, "incident 2026-09-11", "system", testPeriodStart))

	flag, err := repo.Set(context.Background(), KillSwitchKey, true, "incident 2026-09-11", "")
	if err != nil {
		t.Fatalf("expected the flag to be set, got %v", err)
	}
	if !flag.Enabled || flag.UpdatedBy != "system" {
		t.Fatalf("unexpected flag %+v", flag)
	}
}

func TestFlagRepositoryListReturnsEveryFlag(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewFlagRepository(db)
	mock.ExpectQuery("FROM system_flags ORDER BY key ASC").
		WillReturnRows(flagSQLRows().
			AddRow(KillSwitchKey, false, "seeded disabled at install", "system", testPeriodStart).
			AddRow("monitor_paused", true, "maintenance", "ops", testPeriodStart))

	flags, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if len(flags) != 2 {
		t.Fatalf("expected 2 flags, got %d", len(flags))
	}
}

func TestFlagRepositoryKillSwitchReportsItsState(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
	}{
		{"engaged", true},
		{"released", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newTestDB(t)
			repo := NewFlagRepository(db)
			mock.ExpectQuery("FROM system_flags WHERE key = \\$1").
				WithArgs(KillSwitchKey).
				WillReturnRows(flagSQLRows().AddRow(KillSwitchKey, tc.enabled, "", "ops", testPeriodStart))

			engaged, err := repo.KillSwitchEngaged(context.Background())
			if err != nil {
				t.Fatalf("expected a reading, got %v", err)
			}
			if engaged != tc.enabled {
				t.Fatalf("expected %v, got %v", tc.enabled, engaged)
			}
		})
	}
}

func TestFlagRepositoryKillSwitchRefusesToGuessWhenTheRowIsGone(t *testing.T) {
	// Defaulting a missing kill switch to "off" would let agents keep acting during
	// exactly the incident the switch exists for. An error is the safe answer,
	// because the caller can then do nothing this tick at no cost.
	db, mock := newTestDB(t)
	repo := NewFlagRepository(db)
	mock.ExpectQuery("FROM system_flags WHERE key = \\$1").
		WithArgs(KillSwitchKey).
		WillReturnRows(flagSQLRows())

	engaged, err := repo.KillSwitchEngaged(context.Background())
	if err == nil {
		t.Fatal("expected a missing kill switch to be an error")
	}
	if engaged {
		t.Fatal("expected the failed reading not to claim the switch is engaged")
	}
	if !contains(err.Error(), KillSwitchKey) {
		t.Fatalf("expected the flag name in the error, got %v", err)
	}
}

func TestFlagRepositoryKillSwitchSurfacesAReadFailure(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewFlagRepository(db)
	mock.ExpectQuery("FROM system_flags WHERE key = \\$1").
		WillReturnError(errors.New("connection reset"))

	if _, err := repo.KillSwitchEngaged(context.Background()); err == nil {
		t.Fatal("expected the read failure to surface")
	}
}

// --- audit ---

func auditSQLRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "at", "actor_type", "actor_id", "action", "subject_type",
		"subject_id", "outcome", "detail", "request_id",
	})
}

func TestAuditRepositoryAppendDefaultsTheActorAndTime(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewAuditRepository(db)
	mock.ExpectQuery("INSERT INTO audit_events").
		WithArgs(sqlmock.AnyArg(), "system", "", "goal.triggered", "goal", "goal-1", "sent", "{}", "req-1").
		WillReturnRows(auditSQLRows().AddRow(1, testPeriodStart, "system", "", "goal.triggered", "goal", "goal-1", "sent", "{}", "req-1"))

	event, err := repo.Append(context.Background(), AuditEvent{
		Action:      "goal.triggered",
		SubjectType: "goal",
		SubjectID:   "goal-1",
		Outcome:     "sent",
		RequestID:   "req-1",
	})
	if err != nil {
		t.Fatalf("expected the event to be appended, got %v", err)
	}
	if event.ActorType != ActorSystem {
		t.Fatalf("expected the system actor, got %q", event.ActorType)
	}
	if event.At.IsZero() {
		t.Fatal("expected a timestamp")
	}
}

func TestAuditRepositoryAppendKeepsAnExplicitActor(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewAuditRepository(db)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery("INSERT INTO audit_events").
		WithArgs(at, "user", "ops", "approval.resolved", "approval", "approval-1", "approved", `{"note":"ok"}`, "req-2").
		WillReturnRows(auditSQLRows().AddRow(2, at, "user", "ops", "approval.resolved", "approval", "approval-1", "approved", `{"note":"ok"}`, "req-2"))

	event, err := repo.Append(context.Background(), AuditEvent{
		At:          at,
		ActorType:   ActorUser,
		ActorID:     "ops",
		Action:      "approval.resolved",
		SubjectType: "approval",
		SubjectID:   "approval-1",
		Outcome:     "approved",
		Detail:      `{"note":"ok"}`,
		RequestID:   "req-2",
	})
	if err != nil {
		t.Fatalf("expected the event to be appended, got %v", err)
	}
	if event.ActorType != ActorUser || event.ActorID != "ops" {
		t.Fatalf("unexpected actor %+v", event)
	}
}

func TestAuditRepositoryListNumbersEveryFilterInOrder(t *testing.T) {
	db, mock := newTestDB(t)
	repo := NewAuditRepository(db)
	since := testPeriodStart
	until := testPeriodEnd
	mock.ExpectQuery("AND actor_type = \\$1 AND action = \\$2 AND subject_type = \\$3 AND subject_id = \\$4 AND at >= \\$5 AND at <= \\$6").
		WithArgs("bot", "spend.recorded", "goal", "goal-1", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(11))
	mock.ExpectQuery("LIMIT \\$7 OFFSET \\$8").
		WithArgs("bot", "spend.recorded", "goal", "goal-1", since, until, 10, 0).
		WillReturnRows(auditSQLRows().AddRow(3, since, "bot", "bot-1", "spend.recorded", "goal", "goal-1", "ok", "{}", ""))

	events, total, err := repo.List(context.Background(), AuditFilter{
		ActorType:   ActorBot,
		Action:      "spend.recorded",
		SubjectType: "goal",
		SubjectID:   "goal-1",
		Since:       since,
		Until:       until,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("expected a listing, got %v", err)
	}
	if total != 11 || len(events) != 1 {
		t.Fatalf("unexpected result: total=%d events=%d", total, len(events))
	}
}
