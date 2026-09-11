// Package service holds Wingman's goal-driven autonomy workflows: the monitor
// tick that decides when to wake an agent, and the approval gate that decides
// whether an agent may spend money.
//
// The rules themselves live in internal/domain and are pure. This package is the
// part that reads the clock, touches storage, and calls out to Wingman core — so
// it is written against small interfaces rather than concrete repositories, which
// keeps the workflows testable without a database.
package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// Clock reads the current time. Injected so the pace rules can be exercised at
// any point in a goal period without waiting for one.
type Clock func() time.Time

// GoalStore is the slice of goal storage the monitor needs.
type GoalStore interface {
	ListDue(ctx context.Context, now time.Time, limit int) ([]repository.GoalRecord, error)
	SetBaseline(ctx context.Context, id string, value float64) error
	UpdateStatus(ctx context.Context, id string, status domain.GoalStatus) error
}

// GoalRegistry is the slice of goal storage the registry service needs. It is
// separate from GoalStore because the two callers want different powers: the
// monitor may settle a goal but never rewrite one, and the API may rewrite a goal
// but never capture a baseline.
type GoalRegistry interface {
	Create(ctx context.Context, goal domain.Goal, createdBy string) (repository.GoalRecord, error)
	GetByID(ctx context.Context, id string) (repository.GoalRecord, error)
	List(ctx context.Context, filter repository.GoalFilter) ([]repository.GoalRecord, int, error)
	Patch(ctx context.Context, id string, patch repository.GoalPatch) (repository.GoalRecord, error)
}

// SampleStore records metric observations.
type SampleStore interface {
	Insert(ctx context.Context, input repository.SampleInput) (repository.SampleRecord, error)
}

// SampleReader reads back stored observations.
//
// It is what makes a push metric evaluable at all: nothing can pull a number
// somebody else reports, so the monitor reads the last one that was pushed
// instead of asking the sampler for a fresh one.
type SampleReader interface {
	Latest(ctx context.Context, metricKey string) (repository.SampleRecord, error)
}

// EvaluationStore records every goal check, triggering or not.
type EvaluationStore interface {
	Insert(ctx context.Context, eval domain.Evaluation, sampleID *int64) (repository.EvaluationRecord, error)
}

// DispatchStore tracks agent tasks this service asked core to run.
type DispatchStore interface {
	History(ctx context.Context, goalID string, periodStart time.Time) (repository.DispatchHistory, error)
	Create(ctx context.Context, input repository.DispatchInput) (repository.DispatchRecord, error)
	MarkSent(ctx context.Context, id string, responseStatus int, externalTaskID string) error
	MarkFailed(ctx context.Context, id string, responseStatus *int, message string) error
}

// FlagStore exposes the operator's kill switch.
type FlagStore interface {
	KillSwitchEngaged(ctx context.Context) (bool, error)
}

// FlagAdmin is the read-write side of the operator's flags, used by the endpoint
// that throws the kill switch. It is a separate interface from FlagStore so that
// the monitor and the approval gate — which must only ever read the switch —
// cannot reach the setter at all.
type FlagAdmin interface {
	Get(ctx context.Context, key string) (repository.Flag, error)
	Set(ctx context.Context, key string, enabled bool, reason, updatedBy string) (repository.Flag, error)
	List(ctx context.Context) ([]repository.Flag, error)
}

// AuditSink appends to the append-only business audit log.
type AuditSink interface {
	Append(ctx context.Context, event repository.AuditEvent) (repository.AuditEvent, error)
}

// AuditReader reads the business audit log. Reading and appending are separate
// interfaces because nothing in this service is allowed to do both: the log is
// append-only, and the code that writes it has no business querying it.
type AuditReader interface {
	List(ctx context.Context, filter repository.AuditFilter) ([]repository.AuditEvent, int, error)
}

// MetricLookup resolves a goal's metric key to its operator-declared definition.
// Only a lookup is exposed: nothing in this package may define a new metric.
type MetricLookup interface {
	Get(key string) (metrics.Definition, bool)
}

// Sampler observes one metric.
type Sampler interface {
	Sample(ctx context.Context, def metrics.Definition) (metrics.Sample, error)
}

// TaskCreator wakes an agent in Wingman core.
type TaskCreator interface {
	CreateTask(ctx context.Context, req core.TaskRequest) (core.TaskResponse, error)
}

// SpendStore is the ledger the daily cap is checked against.
type SpendStore interface {
	// WithinActionLock serialises a read-decide-record sequence for one action
	// type, so two concurrent requests cannot both pass the same daily cap.
	WithinActionLock(ctx context.Context, actionType string, fn func(locked repository.SpendLedger) error) error
	SpentSince(ctx context.Context, actionType, currency string, since time.Time) (float64, error)
}

// ApprovalStore stores approval requests and their human resolutions.
type ApprovalStore interface {
	Create(ctx context.Context, input repository.ApprovalInput) (repository.ApprovalRecord, error)
	GetByID(ctx context.Context, id string) (repository.ApprovalRecord, error)
	GetByIdempotencyKey(ctx context.Context, key string) (repository.ApprovalRecord, error)
	List(ctx context.Context, filter repository.ApprovalFilter) ([]repository.ApprovalRecord, int, error)
	Resolve(ctx context.Context, id string, resolution repository.ApprovalResolution, resolvedBy, note string) (repository.ApprovalRecord, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int, error)
}

// PolicyLookup resolves an action type to its operator-configured limits. A nil
// result means no policy is declared, which the decision core reads as "ask a
// human" — never as "no limit".
type PolicyLookup interface {
	Lookup(actionType string) *domain.ApprovalPolicy
}

// Audit action names. They are constants because they are queried: an audit log
// whose action names drift is a log nobody can search.
const (
	ActionGoalCreated      = "goal.created"
	ActionGoalUpdated      = "goal.updated"
	ActionBaselineCaptured = "goal.baseline_captured"
	ActionTriggerDispatch  = "goal.trigger_dispatched"
	ActionTriggerFailed    = "goal.trigger_failed"
	ActionGoalSettled      = "goal.settled"
	ActionKillSwitchSet    = "flag.kill_switch_set"
	ActionApprovalDecided  = "approval.decided"
	ActionApprovalResolved = "approval.resolved"
	ActionSpendRecorded    = "spend.recorded"
	ActionSampleRecorded   = "metric.sample_recorded"
)

// Audit subject types. They pair with an action to say what the action was
// performed on.
const (
	SubjectGoal     = "goal"
	SubjectApproval = "approval"
	SubjectFlag     = "flag"
	SubjectMetric   = "metric"
)

// detailJSON encodes audit detail. Encoding must never be the reason an audit
// entry goes unwritten, so a failure degrades to an empty object rather than an
// error.
func detailJSON(fields map[string]any) string {
	if len(fields) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
