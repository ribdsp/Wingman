package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// RunsDeps is everything the run service needs.
type RunsDeps struct {
	Runs    RunReader
	Auditor RunAuditor
	Tasks   TaskReader

	Canceller RunCanceller
	Spend     SpendReader

	// UnattendedOwner is the account unattended work is filed against, the same value
	// Tasks holds. It is what lets the goal engine read back the run its own dispatch
	// started, and nothing else.
	UnattendedOwner string

	Clock  Clock
	Logger zerolog.Logger
}

// Runs is how a run is read back and how one is asked to stop.
//
// It writes nothing about a run except a cancellation request. Starting a run belongs to
// the runner, finishing one belongs to the loop, and charging one belongs to the loop's
// budget port — so the only mutation reachable from a request is the one that asks work
// to stop, which is the only one a person should be able to make.
type Runs struct {
	runs    RunReader
	auditor RunAuditor
	tasks   TaskReader

	canceller RunCanceller
	spend     SpendReader

	unattendedOwner string

	clock Clock
	log   zerolog.Logger
}

// NewRuns validates its wiring and returns a ready service.
func NewRuns(deps RunsDeps) (*Runs, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Runs != nil, "Runs")
	require(deps.Auditor != nil, "Auditor")
	require(deps.Tasks != nil, "Tasks")
	require(deps.Canceller != nil, "Canceller")
	require(deps.Spend != nil, "Spend")
	if len(missing) > 0 {
		return nil, fmt.Errorf("runs: missing dependencies: %v", missing)
	}

	r := &Runs{
		runs:            deps.Runs,
		auditor:         deps.Auditor,
		tasks:           deps.Tasks,
		canceller:       deps.Canceller,
		spend:           deps.Spend,
		unattendedOwner: strings.TrimSpace(deps.UnattendedOwner),
		clock:           deps.Clock,
		log:             deps.Logger,
	}
	if r.clock == nil {
		r.clock = time.Now
	}
	return r, nil
}

// scope is which runs a caller may read.
//
// Three cases, and they are not a hierarchy of one thing: a person reads their own, the
// goal engine reads the unattended account's, and an operator reads every run on the
// instance because auditing what an agent did is what an operator is for. Resolved once,
// here, so each method below branches on it rather than re-deriving it.
type scope struct {
	// userID is the account to scope a query by. Empty when everything is in scope.
	userID string
	// everything is true only for an operator.
	everything bool
}

func (r *Runs) scopeFor(actor Actor, operation string) (scope, error) {
	prepared, err := actor.prepare()
	if err != nil {
		return scope{}, err
	}
	switch prepared.Type {
	case ActorUser:
		userID, err := prepared.Owner()
		if err != nil {
			return scope{}, err
		}
		return scope{userID: userID}, nil
	case ActorOperator:
		return scope{everything: true}, nil
	case ActorBot:
		if r.unattendedOwner == "" {
			// Nothing was ever filed against an unattended account, so there is nothing
			// for this caller to find. ErrNotFound rather than a configuration complaint:
			// the dispatcher cannot fix it and does not need to know about it.
			return scope{}, ErrNotFound
		}
		return scope{userID: r.unattendedOwner}, nil
	default:
		return scope{}, fmt.Errorf("%w: %s is not permitted", ErrForbidden, operation)
	}
}

// Get is one run, with the limits it actually ran under.
func (r *Runs) Get(ctx context.Context, runID string, actor Actor) (repository.RunRecord, error) {
	sc, err := r.scopeFor(actor, "reading a run")
	if err != nil {
		return repository.RunRecord{}, err
	}
	return r.read(ctx, sc, runID)
}

// Steps is a run's transcript, oldest first.
//
// The authorisation is the run's, checked by reading the run first: the step query is not
// user-scoped, because a step has no owner of its own, and going through the run is what
// gives it one.
func (r *Runs) Steps(ctx context.Context, runID string, limit, offset int, actor Actor) ([]domain.Step, error) {
	sc, err := r.scopeFor(actor, "reading a transcript")
	if err != nil {
		return nil, err
	}
	record, err := r.read(ctx, sc, runID)
	if err != nil {
		return nil, err
	}
	return r.runs.Steps(ctx, record.Run.ID, limit, offset)
}

// ListForTask is every run a task caused, newest first.
//
// A task can have more than one: a run abandoned by a crashed worker is requeued, and the
// second attempt is a second run against the same task. Both are in the list, because "it
// was tried twice" is part of what happened.
func (r *Runs) ListForTask(ctx context.Context, taskID string, limit, offset int, actor Actor) ([]repository.RunRecord, error) {
	sc, err := r.scopeFor(actor, "listing a task's runs")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("%w: a task id is required", ErrValidation)
	}

	// The task is fetched first for the same reason as above: ListForTask is not
	// user-scoped, so the ownership check has to happen against something that is.
	if !sc.everything {
		if _, err := r.tasks.GetForUser(ctx, sc.userID, taskID); err != nil {
			return nil, mapAbsence(err)
		}
	}
	return r.runs.ListForTask(ctx, taskID, limit, offset)
}

// Cost is what a run spent, charge by charge.
//
// Charges rather than one total, because a run's cost is several model calls and which
// one was expensive is the useful part. The total is the caller's to sum; repository.Spend
// carries Total() for one charge.
func (r *Runs) Cost(ctx context.Context, runID string, actor Actor) ([]repository.Spend, error) {
	sc, err := r.scopeFor(actor, "reading a run's cost")
	if err != nil {
		return nil, err
	}
	record, err := r.read(ctx, sc, runID)
	if err != nil {
		return nil, err
	}
	return r.spend.ForRun(ctx, record.Run.ID)
}

// Cancel asks a run to stop at its next step boundary.
//
// A request, not a kill. The loop reads the flag before each iteration, so the run ends
// with its transcript intact and its counters matching what it actually spent — where
// killing the process mid-call would leave a charge nobody recorded.
//
// Asking twice succeeds. Somebody clicking cancel again because nothing has visibly
// happened yet is not making a mistake, and the second answer should not tell them the
// run is gone.
func (r *Runs) Cancel(ctx context.Context, runID string, actor Actor) error {
	sc, err := r.scopeFor(actor, "cancelling a run")
	if err != nil {
		return err
	}
	record, err := r.read(ctx, sc, runID)
	if err != nil {
		return err
	}
	if !record.Run.InFlight() {
		return fmt.Errorf("%w: that run has already finished", ErrConflict)
	}
	if record.Cancelled() {
		return nil
	}

	// The cancellation is recorded against the run's own owner rather than the caller.
	// For a person those are the same account; for an operator cancelling somebody's
	// runaway run they are not, and the row to update is the run's.
	if err := r.canceller.RequestCancel(ctx, record.Run.OwnerUserID, record.Run.ID, r.clock()); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The UPDATE matches only an in-flight, not-yet-cancelled run, so losing this
			// race means somebody else asked first or the run finished on its own.
			// Either way what the caller wanted is now true.
			return nil
		}
		return err
	}
	r.log.Info().Str("runId", record.Run.ID).Str("by", actor.String()).Msg("cancellation requested")
	return nil
}

// Ledger is the caller's own token spend for today.
//
// Their own only, including for an operator: the ledger is per account, and an operator
// asking about "the ledger" without naming one would be asking a question with no answer.
// Instance-wide totals are the goal engine's, through the samples core pushes it.
func (r *Runs) Ledger(ctx context.Context, actor Actor) (domain.Ledger, error) {
	prepared, err := actor.prepare()
	if err != nil {
		return domain.Ledger{}, err
	}
	userID, err := prepared.Owner()
	if err != nil {
		return domain.Ledger{}, err
	}
	return r.spend.Ledger(ctx, userID, r.clock())
}

// read fetches a run under the caller's scope.
//
// The two paths are different queries rather than one query and a comparison: a run that
// belongs to somebody else is not returned to a user at all, so there is no moment where
// the wrong row is in a variable and a forgotten check would serve it.
func (r *Runs) read(ctx context.Context, sc scope, runID string) (repository.RunRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return repository.RunRecord{}, fmt.Errorf("%w: a run id is required", ErrValidation)
	}

	if sc.everything {
		record, err := r.auditor.Get(ctx, runID)
		if err != nil {
			return repository.RunRecord{}, mapAbsence(err)
		}
		return record, nil
	}
	record, err := r.runs.GetForUser(ctx, sc.userID, runID)
	if err != nil {
		return repository.RunRecord{}, mapAbsence(err)
	}
	return record, nil
}
