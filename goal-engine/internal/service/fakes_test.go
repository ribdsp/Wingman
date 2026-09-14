package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// Fakes for every port. They are hand-written rather than generated because each
// one has to be able to fail on demand: most of what this package is responsible
// for is behaving correctly when a dependency does not.

var errBoom = errors.New("boom")

// --- goals ---

type fakeGoals struct {
	due       []repository.GoalRecord
	listErr   error
	listCalls int

	baselines   map[string]float64
	baselineErr error

	statuses  map[string]domain.GoalStatus
	statusErr error
}

func newFakeGoals(due ...repository.GoalRecord) *fakeGoals {
	return &fakeGoals{
		due:       due,
		baselines: map[string]float64{},
		statuses:  map[string]domain.GoalStatus{},
	}
}

func (f *fakeGoals) ListDue(_ context.Context, _ time.Time, _ int) ([]repository.GoalRecord, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.due, nil
}

func (f *fakeGoals) SetBaseline(_ context.Context, id string, value float64) error {
	if f.baselineErr != nil {
		return f.baselineErr
	}
	f.baselines[id] = value
	return nil
}

func (f *fakeGoals) UpdateStatus(_ context.Context, id string, status domain.GoalStatus) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.statuses[id] = status
	return nil
}

// --- samples ---

type fakeSamples struct {
	inserted []repository.SampleInput
	records  []repository.SampleRecord
	err      error
	nextID   int64

	// latest overrides what a reader sees, so a test can present a reported value
	// nobody inserted — which is exactly the situation a push metric is in.
	latest    *repository.SampleRecord
	latestErr error
}

func (f *fakeSamples) Insert(_ context.Context, input repository.SampleInput) (repository.SampleRecord, error) {
	if f.err != nil {
		return repository.SampleRecord{}, f.err
	}
	f.inserted = append(f.inserted, input)
	f.nextID++
	record := repository.SampleRecord{
		ID:         f.nextID,
		MetricKey:  input.MetricKey,
		Value:      input.Value,
		ObservedAt: input.ObservedAt,
		Source:     input.Source,
	}
	f.records = append(f.records, record)
	return record, nil
}

// Latest makes the same fake serve as a SampleReader. Without an override it
// returns the most recent insert for the key, which is how the real table
// behaves; with one, it stands in for a value that arrived by being reported.
func (f *fakeSamples) Latest(_ context.Context, metricKey string) (repository.SampleRecord, error) {
	if f.latestErr != nil {
		return repository.SampleRecord{}, f.latestErr
	}
	if f.latest != nil {
		return *f.latest, nil
	}
	for i := len(f.records) - 1; i >= 0; i-- {
		if f.records[i].MetricKey == metricKey {
			return f.records[i], nil
		}
	}
	return repository.SampleRecord{}, repository.ErrNotFound
}

// --- evaluations ---

type fakeEvaluations struct {
	inserted  []domain.Evaluation
	sampleIDs []*int64
	err       error
	nextID    int
}

func (f *fakeEvaluations) Insert(_ context.Context, eval domain.Evaluation, sampleID *int64) (repository.EvaluationRecord, error) {
	if f.err != nil {
		return repository.EvaluationRecord{}, f.err
	}
	f.inserted = append(f.inserted, eval)
	f.sampleIDs = append(f.sampleIDs, sampleID)
	f.nextID++
	return repository.EvaluationRecord{
		ID:         "eval-" + itoa(f.nextID),
		Evaluation: eval,
	}, nil
}

func (f *fakeEvaluations) last() domain.Evaluation {
	if len(f.inserted) == 0 {
		return domain.Evaluation{}
	}
	return f.inserted[len(f.inserted)-1]
}

// --- dispatches ---

type fakeDispatches struct {
	history    repository.DispatchHistory
	historyErr error

	created   []repository.DispatchInput
	createErr error

	sent     []string
	sentErr  error
	failed   []string
	failures []string
	failErr  error

	nextID int
}

func (f *fakeDispatches) History(_ context.Context, _ string, _ time.Time) (repository.DispatchHistory, error) {
	if f.historyErr != nil {
		return repository.DispatchHistory{}, f.historyErr
	}
	return f.history, nil
}

func (f *fakeDispatches) Create(_ context.Context, input repository.DispatchInput) (repository.DispatchRecord, error) {
	if f.createErr != nil {
		return repository.DispatchRecord{}, f.createErr
	}
	f.created = append(f.created, input)
	f.nextID++
	return repository.DispatchRecord{
		ID:             "dispatch-" + itoa(f.nextID),
		GoalID:         input.GoalID,
		IdempotencyKey: input.IdempotencyKey,
		Status:         repository.DispatchPending,
	}, nil
}

func (f *fakeDispatches) MarkSent(_ context.Context, id string, _ int, _ string) error {
	if f.sentErr != nil {
		return f.sentErr
	}
	f.sent = append(f.sent, id)
	return nil
}

func (f *fakeDispatches) MarkFailed(_ context.Context, id string, _ *int, message string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.failed = append(f.failed, id)
	f.failures = append(f.failures, message)
	return nil
}

// --- flags ---

type fakeFlags struct {
	engaged bool
	err     error
	calls   int
}

func (f *fakeFlags) KillSwitchEngaged(context.Context) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.engaged, nil
}

// --- audit ---

type fakeAudit struct {
	mu     sync.Mutex
	events []repository.AuditEvent
	err    error
}

func (f *fakeAudit) Append(_ context.Context, event repository.AuditEvent) (repository.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return repository.AuditEvent{}, f.err
	}
	f.events = append(f.events, event)
	return event, nil
}

// actions returns the recorded action names in order.
func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.events))
	for _, e := range f.events {
		names = append(names, e.Action)
	}
	return names
}

func (f *fakeAudit) has(action string) bool {
	for _, name := range f.actions() {
		if name == action {
			return true
		}
	}
	return false
}

// last returns the most recent entry recorded for an action, failing the test if
// there is none. Tests that care about who did something and under which request
// need the whole entry, not just that the action appeared.
func (f *fakeAudit) last(t *testing.T, action string) repository.AuditEvent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		if f.events[i].Action == action {
			return f.events[i]
		}
	}
	t.Fatalf("no audit entry for %q, got %v", action, f.events)
	return repository.AuditEvent{}
}

// --- metrics ---

type fakeMetrics struct {
	defs map[string]metrics.Definition
}

func newFakeMetrics(keys ...string) *fakeMetrics {
	defs := map[string]metrics.Definition{}
	for _, key := range keys {
		defs[key] = metrics.Definition{Key: key, Source: metrics.SourceSQL, Unit: "IDR"}
	}
	return &fakeMetrics{defs: defs}
}

func (f *fakeMetrics) Get(key string) (metrics.Definition, bool) {
	def, ok := f.defs[key]
	return def, ok
}

// declare adds a metric with an explicit source. The source is the whole
// difference between a number the engine reads for itself and one it waits to be
// told, so a push test starts here.
func (f *fakeMetrics) declare(key string, source metrics.SourceType) {
	f.defs[key] = metrics.Definition{Key: key, Source: source, Unit: "IDR"}
}

// --- sampler ---

type fakeSampler struct {
	value    float64
	at       time.Time
	err      error
	observed []string
}

func (f *fakeSampler) Sample(_ context.Context, def metrics.Definition) (metrics.Sample, error) {
	f.observed = append(f.observed, def.Key)
	if f.err != nil {
		return metrics.Sample{}, f.err
	}
	return metrics.Sample{
		MetricKey:  def.Key,
		Value:      f.value,
		ObservedAt: f.at,
		Source:     def.Source,
	}, nil
}

// --- core tasks ---

type fakeTasks struct {
	requests []core.TaskRequest
	response core.TaskResponse
	err      error
}

func (f *fakeTasks) CreateTask(_ context.Context, req core.TaskRequest) (core.TaskResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return core.TaskResponse{}, f.err
	}
	return f.response, nil
}

func (f *fakeTasks) last() core.TaskRequest {
	if len(f.requests) == 0 {
		return core.TaskRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// --- notifications ---

type fakeNotifier struct {
	sent []core.NotifyRequest
	err  error
}

func (f *fakeNotifier) Notify(_ context.Context, req core.NotifyRequest) error {
	// Recorded before the failure, so a test can assert that a doomed send was
	// still attempted rather than skipped.
	f.sent = append(f.sent, req)
	return f.err
}

// --- spend ---

// fakeSpend is both the store and the locked ledger, which mirrors the real
// repository: the handle handed to the callback is the same type.
type fakeSpend struct {
	spentToday float64
	spentErr   error
	recorded   []repository.SpendInput
	recordErr  error
	lockErr    error
	locks      []string
	// lockDepth counts how many callbacks are running, so a test can assert the
	// ledger write happens inside the lock rather than after it.
	lockDepth        int
	recordedInLock   int
	recordedOutLock  int
	spentInsideLock  int
	spentOutsideLock int
}

func (f *fakeSpend) WithinActionLock(ctx context.Context, actionType string, fn func(repository.SpendLedger) error) error {
	f.locks = append(f.locks, actionType)
	if f.lockErr != nil {
		return f.lockErr
	}
	f.lockDepth++
	defer func() { f.lockDepth-- }()
	return fn(f)
}

func (f *fakeSpend) SpentSince(_ context.Context, _, _ string, _ time.Time) (float64, error) {
	if f.lockDepth > 0 {
		f.spentInsideLock++
	} else {
		f.spentOutsideLock++
	}
	if f.spentErr != nil {
		return 0, f.spentErr
	}
	return f.spentToday, nil
}

func (f *fakeSpend) Record(_ context.Context, input repository.SpendInput) (repository.SpendRecord, error) {
	if f.lockDepth > 0 {
		f.recordedInLock++
	} else {
		f.recordedOutLock++
	}
	if f.recordErr != nil {
		return repository.SpendRecord{}, f.recordErr
	}
	f.recorded = append(f.recorded, input)
	return repository.SpendRecord{ID: int64(len(f.recorded)), ActionType: input.ActionType, Amount: input.Amount}, nil
}

// --- approvals ---

type fakeApprovals struct {
	created    []repository.ApprovalInput
	createErr  error
	byKey      map[string]repository.ApprovalRecord
	byKeyErr   error
	byID       map[string]repository.ApprovalRecord
	byIDErr    error
	resolveErr error
	resolved   []string
	listed     []repository.ApprovalRecord
	listTotal  int
	listErr    error
	expired    int
	expireErr  error
	nextID     int
}

func newFakeApprovals() *fakeApprovals {
	return &fakeApprovals{
		byKey: map[string]repository.ApprovalRecord{},
		byID:  map[string]repository.ApprovalRecord{},
	}
}

func (f *fakeApprovals) Create(_ context.Context, input repository.ApprovalInput) (repository.ApprovalRecord, error) {
	if f.createErr != nil {
		return repository.ApprovalRecord{}, f.createErr
	}
	f.created = append(f.created, input)
	f.nextID++
	record := repository.ApprovalRecord{
		ID:             "approval-" + itoa(f.nextID),
		ActionType:     input.ActionType,
		Amount:         input.Amount,
		Currency:       input.Currency,
		RequestedBy:    input.RequestedBy,
		GoalID:         input.GoalID,
		IdempotencyKey: input.IdempotencyKey,
		Outcome:        input.Outcome,
		PolicyReason:   input.PolicyReason,
		Payload:        input.Payload,
		ExpiresAt:      input.ExpiresAt,
	}
	f.byID[record.ID] = record
	if input.IdempotencyKey != "" {
		f.byKey[input.IdempotencyKey] = record
	}
	return record, nil
}

func (f *fakeApprovals) GetByID(_ context.Context, id string) (repository.ApprovalRecord, error) {
	if f.byIDErr != nil {
		return repository.ApprovalRecord{}, f.byIDErr
	}
	record, ok := f.byID[id]
	if !ok {
		return repository.ApprovalRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func (f *fakeApprovals) GetByIdempotencyKey(_ context.Context, key string) (repository.ApprovalRecord, error) {
	if f.byKeyErr != nil {
		return repository.ApprovalRecord{}, f.byKeyErr
	}
	record, ok := f.byKey[key]
	if !ok {
		return repository.ApprovalRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func (f *fakeApprovals) List(_ context.Context, _ repository.ApprovalFilter) ([]repository.ApprovalRecord, int, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listed, f.listTotal, nil
}

func (f *fakeApprovals) Resolve(_ context.Context, id string, resolution repository.ApprovalResolution, resolvedBy, note string) (repository.ApprovalRecord, error) {
	if f.resolveErr != nil {
		return repository.ApprovalRecord{}, f.resolveErr
	}
	record, ok := f.byID[id]
	if !ok {
		return repository.ApprovalRecord{}, repository.ErrNotFound
	}
	f.resolved = append(f.resolved, id)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	record.Resolution = &resolution
	record.ResolvedBy = &resolvedBy
	record.ResolvedAt = &now
	record.ResolutionNote = note
	f.byID[id] = record
	return record, nil
}

func (f *fakeApprovals) ExpireOverdue(_ context.Context, _ time.Time) (int, error) {
	if f.expireErr != nil {
		return 0, f.expireErr
	}
	return f.expired, nil
}

// --- policies ---

type fakePolicies struct {
	policies map[string]domain.ApprovalPolicy
}

func (f *fakePolicies) Lookup(actionType string) *domain.ApprovalPolicy {
	policy, ok := f.policies[actionType]
	if !ok {
		return nil
	}
	return &policy
}

// itoa avoids importing strconv into every fake for one digit.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
