package agent

import (
	"context"
	"time"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The loop's dependencies, one interface per thing it needs and no wider.
//
// They live here rather than in internal/service/ports.go because the loop is not a
// service: it is called by one. Same rule either way — the agent depends on these
// and never on a repository, which is what lets every branch of the ladder be tested
// against a fake in milliseconds. Repositories satisfy most of them as they stand;
// the two that need a three-line adapter say so.

// Transcript is the run's record of what it did. Every model turn and every tool
// call goes through it, and the counters the ladder reads are bumped by the same
// statement that writes the row — see repository.RunRepository.AppendStep.
type Transcript interface {
	NextStepIndex(ctx context.Context, runID string) (int, error)
	AppendStep(ctx context.Context, step domain.Step) error
}

// Runs is the run row: whether somebody asked for it to stop, and the one write that
// ends it.
//
// Finish is satisfied by repository.RunRepository directly. CancelRequested needs an
// adapter over Get, because a repository that answered a bool would be a repository
// deciding what "cancelled" means.
type Runs interface {
	CancelRequested(ctx context.Context, runID string) (bool, error)
	Finish(ctx context.Context, runID string, stop domain.StopReason, reason string, at time.Time) error
}

// Charge is one model step's cost, as the loop knows it.
type Charge struct {
	UserID    string
	RunID     string
	Provider  string
	Model     string
	TokensIn  int64
	TokensOut int64
	At        time.Time
}

// Budget is the user's daily spend and where this run's own spend is booked.
//
// Ledger is satisfied by repository.SpendRepository directly, including its rule
// that an unreadable ledger comes back as domain.Ledger{Readable: false} — the loop
// stops on that rather than guessing a number.
type Budget interface {
	Ledger(ctx context.Context, userID string, now time.Time) (domain.Ledger, error)
	Charge(ctx context.Context, charge Charge) error
}

// Halt is the goal engine's kill switch.
//
// One method, because the loop asks one question. An error is not a false: the caller
// reads an unreadable switch as engaged, which is the rule the goal engine applies to
// its own flag and the reason it is a bool-and-error rather than a bool.
type Halt interface {
	Engaged(ctx context.Context) (bool, error)
}

// SpendRequest is what the loop files with the goal engine's spending gate.
//
// IdempotencyKey is derived from the run and the call, so a retried step files the
// same request rather than a second one against the same daily cap.
type SpendRequest struct {
	ActionType     string
	Amount         float64
	Currency       string
	RunID          string
	ToolName       string
	IdempotencyKey string
}

// SpendDecision is the gate's answer. It mirrors goal-engine's three outcomes,
// deliberately: core does not add a fourth reading of them.
type SpendDecision struct {
	Outcome domain.ApprovalOutcome
	Reason  string
}

// SpendGate is the goal engine's deny-biased spending ladder.
//
// Core has no second gate. This port exists so the loop can be tested without one,
// not so it can be replaced by one: a nil SpendGate means a spending call is refused,
// never that it goes ahead.
type SpendGate interface {
	Request(ctx context.Context, req SpendRequest) (SpendDecision, error)
}

// Provider answers one model turn. It is provider.Client, restated so this package
// declares what it uses.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req provider.Request) (provider.Response, error)
}

// Tools is the per-run snapshot of what may be called. tool.Offering satisfies it;
// see tool.Registry.Offer for why the set is taken once and not per call.
type Tools interface {
	Definitions() []tool.Definition
	Unavailable() []tool.RunnerFailure
	Conflicting() []tool.Conflict
	Call(ctx context.Context, invocation tool.Invocation) (tool.Result, error)
}
