package service

import (
	"context"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// Fakes for the registry-side ports: goal CRUD, the flag setter, and the audit
// reader. They live apart from the monitor's fakes because they answer a different
// question — what the API layer is allowed to do — and mixing the two would make
// either file hard to follow.

// --- goal registry ---

type fakeRegistry struct {
	created   []domain.Goal
	createdBy []string
	createErr error

	record   repository.GoalRecord
	getErr   error
	getCalls int

	listed    []repository.GoalFilter
	listTotal int
	listErr   error

	patched    []repository.GoalPatch
	patchedIDs []string
	patchErr   error
}

func newFakeRegistry(record repository.GoalRecord) *fakeRegistry {
	return &fakeRegistry{record: record}
}

func (f *fakeRegistry) Create(_ context.Context, goal domain.Goal, createdBy string) (repository.GoalRecord, error) {
	f.created = append(f.created, goal)
	f.createdBy = append(f.createdBy, createdBy)
	if f.createErr != nil {
		return repository.GoalRecord{}, f.createErr
	}
	return repository.GoalRecord{
		Goal:      goal,
		CreatedBy: createdBy,
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}, nil
}

func (f *fakeRegistry) GetByID(_ context.Context, id string) (repository.GoalRecord, error) {
	f.getCalls++
	if f.getErr != nil {
		return repository.GoalRecord{}, f.getErr
	}
	record := f.record
	record.ID = id
	return record, nil
}

func (f *fakeRegistry) List(_ context.Context, filter repository.GoalFilter) ([]repository.GoalRecord, int, error) {
	f.listed = append(f.listed, filter)
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return []repository.GoalRecord{f.record}, f.listTotal, nil
}

func (f *fakeRegistry) Patch(_ context.Context, id string, patch repository.GoalPatch) (repository.GoalRecord, error) {
	f.patchedIDs = append(f.patchedIDs, id)
	f.patched = append(f.patched, patch)
	if f.patchErr != nil {
		return repository.GoalRecord{}, f.patchErr
	}
	patched, _ := applyGoalPatch(f.record.Goal, patch)
	record := f.record
	record.Goal = patched
	record.ID = id
	record.UpdatedAt = testNow
	return record, nil
}

// --- flag admin ---

type fakeFlagAdmin struct {
	flags map[string]repository.Flag

	getErr  error
	setErr  error
	listErr error

	sets []repository.Flag
}

func newFakeFlagAdmin() *fakeFlagAdmin {
	return &fakeFlagAdmin{flags: map[string]repository.Flag{}}
}

func (f *fakeFlagAdmin) Get(_ context.Context, key string) (repository.Flag, error) {
	if f.getErr != nil {
		return repository.Flag{}, f.getErr
	}
	flag, ok := f.flags[key]
	if !ok {
		return repository.Flag{}, repository.ErrNotFound
	}
	return flag, nil
}

func (f *fakeFlagAdmin) Set(_ context.Context, key string, enabled bool, reason, updatedBy string) (repository.Flag, error) {
	flag := repository.Flag{
		Key:       key,
		Enabled:   enabled,
		Reason:    reason,
		UpdatedBy: updatedBy,
		UpdatedAt: testNow,
	}
	f.sets = append(f.sets, flag)
	if f.setErr != nil {
		return repository.Flag{}, f.setErr
	}
	f.flags[key] = flag
	return flag, nil
}

func (f *fakeFlagAdmin) List(context.Context) ([]repository.Flag, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]repository.Flag, 0, len(f.flags))
	for _, flag := range f.flags {
		out = append(out, flag)
	}
	return out, nil
}

// --- audit reader ---

type fakeAuditReader struct {
	filters []repository.AuditFilter
	events  []repository.AuditEvent
	total   int
	err     error
}

func (f *fakeAuditReader) List(_ context.Context, filter repository.AuditFilter) ([]repository.AuditEvent, int, error) {
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.events, f.total, nil
}

// last returns the filter the service actually passed down.
func (f *fakeAuditReader) last() repository.AuditFilter {
	if len(f.filters) == 0 {
		return repository.AuditFilter{}
	}
	return f.filters[len(f.filters)-1]
}

// --- helpers ---

func timePtr(t time.Time) *time.Time                   { return &t }
func stringPtr(s string) *string                       { return &s }
func intPtr(i int) *int                                { return &i }
func durationPtr(d time.Duration) *time.Duration       { return &d }
func statusPtr(s domain.GoalStatus) *domain.GoalStatus { return &s }
