package handler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// In-memory stands-in for every port the services need.
//
// The handler tests drive real services rather than mocking them, because what
// they are checking is the wiring: that a role reaches the right actor, that an
// error reaches the right status, and that no field the API should not publish
// makes it into a body. A mocked service would let all three pass while broken.

var errStorage = errors.New("connection refused")

// --- goals ---

type memGoals struct {
	mu        sync.Mutex
	byID      map[string]repository.GoalRecord
	order     []string
	nextID    int
	createErr error
	listErr   error
	patchErr  error
	getErr    error
}

func newMemGoals() *memGoals {
	return &memGoals{byID: map[string]repository.GoalRecord{}}
}

func (m *memGoals) Create(_ context.Context, goal domain.Goal, createdBy string) (repository.GoalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return repository.GoalRecord{}, m.createErr
	}
	m.nextID++
	goal.ID = "goal-" + itoa(m.nextID)
	record := repository.GoalRecord{
		Goal:      goal,
		CreatedBy: createdBy,
		CreatedAt: testNow,
	}
	m.byID[goal.ID] = record
	m.order = append(m.order, goal.ID)
	return record, nil
}

func (m *memGoals) GetByID(_ context.Context, id string) (repository.GoalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return repository.GoalRecord{}, m.getErr
	}
	record, ok := m.byID[id]
	if !ok {
		return repository.GoalRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func (m *memGoals) List(_ context.Context, filter repository.GoalFilter) ([]repository.GoalRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	matched := []repository.GoalRecord{}
	for _, id := range m.order {
		record := m.byID[id]
		if filter.Product != "" && record.Product != filter.Product {
			continue
		}
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		if filter.MetricKey != "" && record.MetricKey != filter.MetricKey {
			continue
		}
		matched = append(matched, record)
	}
	total := len(matched)
	if filter.Offset >= total {
		return nil, total, nil
	}
	end := total
	if filter.Limit > 0 && filter.Offset+filter.Limit < end {
		end = filter.Offset + filter.Limit
	}
	return matched[filter.Offset:end], total, nil
}

func (m *memGoals) Patch(_ context.Context, id string, patch repository.GoalPatch) (repository.GoalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.patchErr != nil {
		return repository.GoalRecord{}, m.patchErr
	}
	record, ok := m.byID[id]
	if !ok {
		return repository.GoalRecord{}, repository.ErrNotFound
	}
	if patch.Title != nil {
		record.Title = *patch.Title
	}
	if patch.TargetValue != nil {
		record.TargetValue = *patch.TargetValue
	}
	if patch.PeriodEnd != nil {
		record.PeriodEnd = *patch.PeriodEnd
	}
	if patch.Status != nil {
		record.Status = *patch.Status
	}
	if patch.ToleranceRatio != nil {
		record.ToleranceRatio = *patch.ToleranceRatio
	}
	if patch.TriggerCooldown != nil {
		record.TriggerCooldown = *patch.TriggerCooldown
	}
	if patch.MaxTriggersPerPeriod != nil {
		record.MaxTriggersPerPeriod = *patch.MaxTriggersPerPeriod
	}
	if patch.BotID != nil {
		record.BotID = *patch.BotID
	}
	if patch.ChannelID != nil {
		record.ChannelID = *patch.ChannelID
	}
	if patch.SourceText != nil {
		record.SourceText = *patch.SourceText
	}
	record.UpdatedAt = testNow
	m.byID[id] = record
	return record, nil
}

// monitorGoals is the narrower slice of goal storage the monitor uses.
type monitorGoals struct{ due []repository.GoalRecord }

func (m *monitorGoals) ListDue(context.Context, time.Time, int) ([]repository.GoalRecord, error) {
	return m.due, nil
}
func (m *monitorGoals) SetBaseline(context.Context, string, float64) error { return nil }
func (m *monitorGoals) UpdateStatus(context.Context, string, domain.GoalStatus) error {
	return nil
}

// --- approvals ---

type memApprovals struct {
	mu        sync.Mutex
	byID      map[string]repository.ApprovalRecord
	order     []string
	nextID    int
	listErr   error
	getErr    error
	expired   int
	expireErr error
}

func newMemApprovals() *memApprovals {
	return &memApprovals{byID: map[string]repository.ApprovalRecord{}}
}

func (m *memApprovals) Create(_ context.Context, input repository.ApprovalInput) (repository.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	record := repository.ApprovalRecord{
		ID:             "approval-" + itoa(m.nextID),
		ActionType:     input.ActionType,
		Amount:         input.Amount,
		Currency:       input.Currency,
		RequestedBy:    input.RequestedBy,
		GoalID:         input.GoalID,
		IdempotencyKey: input.IdempotencyKey,
		Outcome:        input.Outcome,
		PolicyReason:   input.PolicyReason,
		Payload:        input.Payload,
		CreatedAt:      testNow,
		ExpiresAt:      input.ExpiresAt,
	}
	m.byID[record.ID] = record
	m.order = append(m.order, record.ID)
	return record, nil
}

func (m *memApprovals) GetByID(_ context.Context, id string) (repository.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return repository.ApprovalRecord{}, m.getErr
	}
	record, ok := m.byID[id]
	if !ok {
		return repository.ApprovalRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func (m *memApprovals) GetByIdempotencyKey(_ context.Context, key string) (repository.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		if record := m.byID[id]; record.IdempotencyKey == key {
			return record, nil
		}
	}
	return repository.ApprovalRecord{}, repository.ErrNotFound
}

func (m *memApprovals) List(_ context.Context, filter repository.ApprovalFilter) ([]repository.ApprovalRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	matched := []repository.ApprovalRecord{}
	for _, id := range m.order {
		record := m.byID[id]
		if filter.ActionType != "" && record.ActionType != filter.ActionType {
			continue
		}
		if filter.Outcome != "" && record.Outcome != filter.Outcome {
			continue
		}
		if filter.OpenOnly && !record.IsOpen() {
			continue
		}
		matched = append(matched, record)
	}
	return matched, len(matched), nil
}

func (m *memApprovals) Resolve(_ context.Context, id string, resolution repository.ApprovalResolution, resolvedBy, note string) (repository.ApprovalRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.byID[id]
	if !ok {
		return repository.ApprovalRecord{}, repository.ErrNotFound
	}
	at := testNow
	record.Resolution = &resolution
	record.ResolvedBy = &resolvedBy
	record.ResolvedAt = &at
	record.ResolutionNote = note
	m.byID[id] = record
	return record, nil
}

func (m *memApprovals) ExpireOverdue(context.Context, time.Time) (int, error) {
	if m.expireErr != nil {
		return 0, m.expireErr
	}
	return m.expired, nil
}

// --- spend ---

// memSpend is both the store and the locked ledger, mirroring the real
// repository, where the handle passed to the callback is the same type.
type memSpend struct {
	mu       sync.Mutex
	recorded []repository.SpendInput
	spent    float64
}

func (m *memSpend) WithinActionLock(ctx context.Context, _ string, fn func(repository.SpendLedger) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(unlockedSpend{m})
}

func (m *memSpend) SpentSince(context.Context, string, string, time.Time) (float64, error) {
	return m.spent, nil
}

// unlockedSpend is the ledger handle. It does not take the mutex again: the lock
// is already held by WithinActionLock, exactly as the advisory lock is in the real
// repository.
type unlockedSpend struct{ owner *memSpend }

func (u unlockedSpend) Record(_ context.Context, input repository.SpendInput) (repository.SpendRecord, error) {
	u.owner.recorded = append(u.owner.recorded, input)
	u.owner.spent += input.Amount
	return repository.SpendRecord{ID: int64(len(u.owner.recorded)), Amount: input.Amount}, nil
}

func (u unlockedSpend) SpentSince(context.Context, string, string, time.Time) (float64, error) {
	return u.owner.spent, nil
}

// --- policies ---

type memPolicies struct {
	byAction map[string]domain.ApprovalPolicy
}

func (m *memPolicies) Lookup(actionType string) *domain.ApprovalPolicy {
	policy, ok := m.byAction[actionType]
	if !ok {
		return nil
	}
	return &policy
}

// --- flags ---

type memFlags struct {
	mu      sync.Mutex
	byKey   map[string]repository.Flag
	getErr  error
	setErr  error
	listErr error
}

func newMemFlags() *memFlags {
	return &memFlags{byKey: map[string]repository.Flag{}}
}

func (m *memFlags) Get(_ context.Context, key string) (repository.Flag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return repository.Flag{}, m.getErr
	}
	flag, ok := m.byKey[key]
	if !ok {
		return repository.Flag{}, repository.ErrNotFound
	}
	return flag, nil
}

func (m *memFlags) Set(_ context.Context, key string, enabled bool, reason, updatedBy string) (repository.Flag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return repository.Flag{}, m.setErr
	}
	flag := repository.Flag{
		Key:       key,
		Enabled:   enabled,
		Reason:    reason,
		UpdatedBy: updatedBy,
		UpdatedAt: testNow,
	}
	m.byKey[key] = flag
	return flag, nil
}

func (m *memFlags) List(context.Context) ([]repository.Flag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	flags := make([]repository.Flag, 0, len(m.byKey))
	for _, flag := range m.byKey {
		flags = append(flags, flag)
	}
	return flags, nil
}

func (m *memFlags) KillSwitchEngaged(ctx context.Context) (bool, error) {
	flag, err := m.Get(ctx, repository.KillSwitchKey)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return flag.Enabled, nil
}

// --- audit ---

type memAudit struct {
	mu      sync.Mutex
	events  []repository.AuditEvent
	listErr error
}

func (m *memAudit) Append(_ context.Context, event repository.AuditEvent) (repository.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	event.ID = int64(len(m.events) + 1)
	m.events = append(m.events, event)
	return event, nil
}

func (m *memAudit) List(_ context.Context, filter repository.AuditFilter) ([]repository.AuditEvent, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	matched := []repository.AuditEvent{}
	for _, event := range m.events {
		if filter.Action != "" && event.Action != filter.Action {
			continue
		}
		if filter.ActorType != "" && event.ActorType != filter.ActorType {
			continue
		}
		matched = append(matched, event)
	}
	return matched, len(matched), nil
}

// --- monitor's remaining ports ---

// memSamples is the metric time series. It serves as both the store and the
// reader, because a value posted through the API has to be readable by the same
// path a monitor tick reads it.
type memSamples struct {
	records []repository.SampleRecord
	err     error
}

func newMemSamples() *memSamples { return &memSamples{} }

func (m *memSamples) Insert(_ context.Context, input repository.SampleInput) (repository.SampleRecord, error) {
	if m.err != nil {
		return repository.SampleRecord{}, m.err
	}
	record := repository.SampleRecord{
		ID:         int64(len(m.records) + 1),
		MetricKey:  input.MetricKey,
		Value:      input.Value,
		ObservedAt: input.ObservedAt,
		Source:     input.Source,
	}
	m.records = append(m.records, record)
	return record, nil
}

func (m *memSamples) Latest(_ context.Context, metricKey string) (repository.SampleRecord, error) {
	if m.err != nil {
		return repository.SampleRecord{}, m.err
	}
	for i := len(m.records) - 1; i >= 0; i-- {
		if m.records[i].MetricKey == metricKey {
			return m.records[i], nil
		}
	}
	return repository.SampleRecord{}, repository.ErrNotFound
}

type memEvaluations struct {
	mu      sync.Mutex
	records []repository.EvaluationRecord
	listErr error
}

func newMemEvaluations() *memEvaluations { return &memEvaluations{} }

func (m *memEvaluations) Insert(_ context.Context, eval domain.Evaluation, sampleID *int64) (repository.EvaluationRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := repository.EvaluationRecord{
		ID:         "eval-" + itoa(len(m.records)+1),
		Evaluation: eval,
		SampleID:   sampleID,
		CreatedAt:  testNow,
	}
	m.records = append(m.records, record)
	return record, nil
}

func (m *memEvaluations) ListByGoal(_ context.Context, goalID string, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	matched := []repository.EvaluationRecord{}
	// Newest first, matching the query this stands in for.
	for i := len(m.records) - 1; i >= 0; i-- {
		if m.records[i].GoalID == goalID {
			matched = append(matched, m.records[i])
		}
	}
	return pageOfEvaluations(matched, limit, offset), len(matched), nil
}

func (m *memEvaluations) LatestPerGoal(_ context.Context, limit, offset int) ([]repository.EvaluationRecord, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	// One row per goal, the newest, newest goal first — what DISTINCT ON plus the
	// outer ORDER BY produce. The fake reproduces the shape rather than the SQL,
	// which is what the handler tests are about; the query itself is asserted in
	// internal/repository.
	seen := map[string]bool{}
	latest := []repository.EvaluationRecord{}
	for i := len(m.records) - 1; i >= 0; i-- {
		record := m.records[i]
		if seen[record.GoalID] {
			continue
		}
		seen[record.GoalID] = true
		latest = append(latest, record)
	}
	return pageOfEvaluations(latest, limit, offset), len(latest), nil
}

func pageOfEvaluations(records []repository.EvaluationRecord, limit, offset int) []repository.EvaluationRecord {
	if offset >= len(records) {
		return nil
	}
	end := len(records)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return records[offset:end]
}

type memDispatches struct{}

func (memDispatches) History(context.Context, string, time.Time) (repository.DispatchHistory, error) {
	return repository.DispatchHistory{}, nil
}
func (memDispatches) Create(_ context.Context, input repository.DispatchInput) (repository.DispatchRecord, error) {
	return repository.DispatchRecord{ID: "dispatch-1", GoalID: input.GoalID}, nil
}
func (memDispatches) MarkSent(context.Context, string, int, string) error { return nil }
func (memDispatches) MarkFailed(context.Context, string, *int, string) error {
	return nil
}

type memSampler struct {
	value float64
	err   error
}

func (m *memSampler) Sample(_ context.Context, def metrics.Definition) (metrics.Sample, error) {
	if m.err != nil {
		return metrics.Sample{}, m.err
	}
	return metrics.Sample{MetricKey: def.Key, Value: m.value, ObservedAt: testNow, Source: def.Source}, nil
}

type memTasks struct{ requests []core.TaskRequest }

func (m *memTasks) CreateTask(_ context.Context, req core.TaskRequest) (core.TaskResponse, error) {
	m.requests = append(m.requests, req)
	return core.TaskResponse{TaskID: "task-1"}, nil
}

// itoa avoids pulling strconv into every fake for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
