package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// runsFixture wires the service over the fakes, kept to hand so a test can read what was
// stored or inject a failure.
type runsFixture struct {
	runs  *Runs
	store *fakeRuns
	tasks *fakeTasks
	spend *fakeSpend
}

func newRunsFixture(t *testing.T, owner string) runsFixture {
	t.Helper()

	store := newFakeRuns()
	tasks := newFakeTasks()
	spend := newFakeSpend()
	runs, err := NewRuns(RunsDeps{
		Runs:            store,
		Auditor:         store,
		Tasks:           tasks,
		Canceller:       store,
		Spend:           spend,
		UnattendedOwner: owner,
		Clock:           fixedClock(testNow),
		Logger:          silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewRuns: %v", err)
	}
	return runsFixture{runs: runs, store: store, tasks: tasks, spend: spend}
}

// seedRun starts a run for the given owner and task, in flight.
func seedRun(t *testing.T, store *fakeRuns, ownerUserID, taskID string) repository.RunRecord {
	t.Helper()

	record, err := store.Start(context.Background(), domain.Run{
		TaskID:      taskID,
		OwnerUserID: ownerUserID,
		Provider:    "anthropic",
		Model:       "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("seed a run: %v", err)
	}
	return record
}

// finish stamps a run with a stop reason, which is what takes it out of flight.
func finish(store *fakeRuns, runID string, stop domain.StopReason) {
	finishedAt := testNow
	store.records[runID].Run.Stop = stop
	store.records[runID].Run.FinishedAt = &finishedAt
}

func TestNewRuns_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewRuns(RunsDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Runs", "Auditor", "Tasks", "Canceller", "Spend"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

// ---------------------------------------------------------------------------
// the three scopes

func TestRunsGet_aPersonReadsTheirOwnRunAndNobodyElses(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	theirs := seedRun(t, f.store, "user-2", "task-2")

	// Act
	got, mineErr := f.runs.Get(context.Background(), mine.Run.ID, userActor("user-1"))
	_, theirsErr := f.runs.Get(context.Background(), theirs.Run.ID, userActor("user-1"))

	// Assert
	if mineErr != nil {
		t.Fatalf("Get own: %v", mineErr)
	}
	if got.Run.ID != mine.Run.ID {
		t.Errorf("id = %q, want %q", got.Run.ID, mine.Run.ID)
	}
	// A different query rather than one query and a comparison, so there is no moment where
	// the wrong row is in a variable.
	if !errors.Is(theirsErr, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", theirsErr)
	}
}

func TestRunsGet_anOperatorReadsEveryRunOnTheInstance(t *testing.T) {
	// Arrange — auditing what an agent did is what an operator is for, so this scope is not
	// a privilege escalation but the point of the role.
	f := newRunsFixture(t, unattendedOwner)
	somebodys := seedRun(t, f.store, "user-1", "task-1")

	// Act
	got, err := f.runs.Get(context.Background(), somebodys.Run.ID, operatorActor())

	// Assert
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Run.ID != somebodys.Run.ID {
		t.Errorf("id = %q, want %q", got.Run.ID, somebodys.Run.ID)
	}
}

func TestRunsGet_aDispatcherReadsOnlyTheUnattendedAccountsRuns(t *testing.T) {
	// Arrange — the goal engine follows up on the run its own dispatch started, and gets
	// nothing else.
	f := newRunsFixture(t, unattendedOwner)
	unattended := seedRun(t, f.store, unattendedOwner, "task-1")
	personal := seedRun(t, f.store, "user-1", "task-2")

	// Act
	got, err := f.runs.Get(context.Background(), unattended.Run.ID, botActor())
	_, personalErr := f.runs.Get(context.Background(), personal.Run.ID, botActor())

	// Assert
	if err != nil {
		t.Fatalf("Get the unattended run: %v", err)
	}
	if got.Run.ID != unattended.Run.ID {
		t.Errorf("id = %q, want %q", got.Run.ID, unattended.Run.ID)
	}
	if !errors.Is(personalErr, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a person's run", personalErr)
	}
}

func TestRunsGet_aDispatcherWithNoConfiguredOwnerFindsNothing(t *testing.T) {
	// Arrange — nothing was ever filed against an unattended account, so there is nothing
	// for this caller to find.
	f := newRunsFixture(t, "")
	run := seedRun(t, f.store, "user-1", "task-1")

	// Act
	_, err := f.runs.Get(context.Background(), run.Run.ID, botActor())

	// Assert — ErrNotFound rather than a configuration complaint: the dispatcher cannot fix
	// it and does not need to know about it.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRuns_everyReadRequiresARunId(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)

	// Act
	errs := map[string]error{}
	_, errs["Get"] = f.runs.Get(context.Background(), "  ", userActor("user-1"))
	_, errs["Steps"] = f.runs.Steps(context.Background(), "  ", 10, 0, userActor("user-1"))
	_, errs["Cost"] = f.runs.Cost(context.Background(), "  ", userActor("user-1"))
	errs["Cancel"] = f.runs.Cancel(context.Background(), "  ", userActor("user-1"))
	_, errs["ListForTask"] = f.runs.ListForTask(context.Background(), "  ", 10, 0, userActor("user-1"))

	// Assert
	for name, err := range errs {
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s err = %v, want ErrValidation", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// transcript, task runs and cost

func TestRunsSteps_authorisesThroughTheRunFirst(t *testing.T) {
	// Arrange — the step query is not user-scoped, because a step has no owner of its own.
	f := newRunsFixture(t, unattendedOwner)
	theirs := seedRun(t, f.store, "user-2", "task-1")
	f.store.steps[theirs.Run.ID] = []domain.Step{
		{Kind: domain.StepKindModel, Content: "somebody else's thinking"},
	}

	// Act
	_, err := f.runs.Steps(context.Background(), theirs.Run.ID, 10, 0, userActor("user-1"))

	// Assert — the transcript is never reached, so nothing to filter afterwards.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRunsSteps_returnsTheTranscriptForARunTheCallerMayRead(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	f.store.steps[mine.Run.ID] = []domain.Step{
		{Kind: domain.StepKindModel, Content: "first"},
		{Kind: domain.StepKindTool, Content: "second"},
	}

	// Act
	steps, err := f.runs.Steps(context.Background(), mine.Run.ID, 10, 0, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if steps[0].Content != "first" {
		t.Errorf("first step = %q, want the oldest first", steps[0].Content)
	}
}

func TestRunsListForTask_requiresTheTaskToBeTheCallersOwn(t *testing.T) {
	// Arrange — ListForTask is not user-scoped either, so the ownership check happens
	// against the task, which is.
	f := newRunsFixture(t, unattendedOwner)
	theirTask, err := f.tasks.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-2", Source: domain.TaskSourceUser, Brief: "theirs", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	seedRun(t, f.store, "user-2", theirTask.ID)

	// Act
	_, err = f.runs.ListForTask(context.Background(), theirTask.ID, 10, 0, userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRunsListForTask_listsEveryAttemptAtOneTask(t *testing.T) {
	// Arrange — a run abandoned by a crashed worker is requeued, and the second attempt is
	// a second run against the same task.
	f := newRunsFixture(t, unattendedOwner)
	task, err := f.tasks.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-1", Source: domain.TaskSourceUser, Brief: "mine", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	first := seedRun(t, f.store, "user-1", task.ID)
	finish(f.store, first.Run.ID, domain.StopAbandoned)
	seedRun(t, f.store, "user-1", task.ID)

	// Act
	records, err := f.runs.ListForTask(context.Background(), task.ID, 10, 0, userActor("user-1"))

	// Assert — "it was tried twice" is part of what happened.
	if err != nil {
		t.Fatalf("ListForTask: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("runs = %d, want both attempts", len(records))
	}
}

func TestRunsListForTask_anOperatorNeedsNoTaskOwnership(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	task, err := f.tasks.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-1", Source: domain.TaskSourceUser, Brief: "somebody's", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	seedRun(t, f.store, "user-1", task.ID)
	// The task read is skipped entirely for an operator, so a failure here would show up as
	// an error if it were being called.
	f.tasks.failGet = errBoom

	// Act
	records, err := f.runs.ListForTask(context.Background(), task.ID, 10, 0, operatorActor())

	// Assert
	if err != nil {
		t.Fatalf("ListForTask: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("runs = %d, want 1", len(records))
	}
}

func TestRunsCost_isChargeByChargeForARunTheCallerMayRead(t *testing.T) {
	// Arrange — several model calls, because which one was expensive is the useful part.
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	f.spend.charges[mine.Run.ID] = []repository.Spend{
		{UserID: "user-1", RunID: mine.Run.ID, TokensIn: 900, TokensOut: 100},
		{UserID: "user-1", RunID: mine.Run.ID, TokensIn: 40, TokensOut: 60},
	}

	// Act
	charges, err := f.runs.Cost(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if len(charges) != 2 {
		t.Fatalf("charges = %d, want 2", len(charges))
	}
	if charges[0].Total() != 1000 {
		t.Errorf("first charge = %d, want 1000", charges[0].Total())
	}
}

func TestRunsCost_authorisesThroughTheRunFirst(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	theirs := seedRun(t, f.store, "user-2", "task-1")
	f.spend.charges[theirs.Run.ID] = []repository.Spend{{UserID: "user-2", TokensIn: 10}}

	// Act
	_, err := f.runs.Cost(context.Background(), theirs.Run.ID, userActor("user-1"))

	// Assert — what somebody else's run cost is somebody else's business.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// cancellation

func TestRunsCancel_stampsTheRequestAtTheClock(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")

	// Act
	err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert — a request, not a kill: the loop reads the flag before each iteration, so the
	// run ends with its transcript intact and its counters matching what it spent.
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	requested := f.store.records[mine.Run.ID].CancelRequestedAt
	if requested == nil {
		t.Fatal("cancelRequestedAt is nil, want the clock's time")
	}
	if !requested.Equal(testNow) {
		t.Errorf("cancelRequestedAt = %s, want %s", requested, testNow)
	}
	// Still in flight: the loop is what ends it, and inventing a stop reason from here would
	// put a second author on that row.
	if !f.store.records[mine.Run.ID].Run.InFlight() {
		t.Error("the run was stopped here, want it left for the loop to end")
	}
}

func TestRunsCancel_askingTwiceSucceeds(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	if err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1")); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	firstRequest := *f.store.records[mine.Run.ID].CancelRequestedAt

	// Act
	err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert — somebody clicking cancel again because nothing has visibly happened yet is
	// not making a mistake.
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if got := *f.store.records[mine.Run.ID].CancelRequestedAt; !got.Equal(firstRequest) {
		t.Errorf("cancelRequestedAt = %s, want the first request's %s", got, firstRequest)
	}
}

func TestRunsCancel_aFinishedRunIsAConflict(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	finish(f.store, mine.Run.ID, domain.StopCompleted)

	// Act
	err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert — a conflict rather than a silent success, because the caller's picture of the
	// run is out of date and telling them so is more use than pretending they stopped it.
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if f.store.records[mine.Run.ID].CancelRequestedAt != nil {
		t.Error("a finished run was stamped with a cancellation")
	}
}

func TestRunsCancel_anOperatorCancelsAgainstTheRunsOwnAccount(t *testing.T) {
	// Arrange — for a person the caller and the owner are the same account; for an operator
	// cancelling somebody's runaway run they are not, and the row to update is the run's.
	f := newRunsFixture(t, unattendedOwner)
	somebodys := seedRun(t, f.store, "user-1", "task-1")

	// Act
	err := f.runs.Cancel(context.Background(), somebodys.Run.ID, operatorActor())

	// Assert — the fake's RequestCancel matches on the owner exactly as the real UPDATE
	// does, so a cancellation filed under the operator would simply not be found.
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if f.store.records[somebodys.Run.ID].CancelRequestedAt == nil {
		t.Error("the run was not stamped, want the request recorded against its owner")
	}
}

func TestRunsCancel_losingTheRaceIsNotAnError(t *testing.T) {
	// Arrange — the read said in flight and not cancelled; by the time the UPDATE ran,
	// somebody else had asked or the run had finished on its own.
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	f.store.failCancel = repository.ErrNotFound

	// Act
	err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert — either way what the caller wanted is now true.
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestRunsCancel_reportsAStoreFailureAsItself(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	mine := seedRun(t, f.store, "user-1", "task-1")
	f.store.failCancel = errBoom

	// Act
	err := f.runs.Cancel(context.Background(), mine.Run.ID, userActor("user-1"))

	// Assert — only absence means the race was lost. Anything else must not be reported as
	// a cancellation that happened.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
}

// ---------------------------------------------------------------------------
// ledger

func TestRunsLedger_isTheCallersOwnAccountOnly(t *testing.T) {
	// Arrange
	f := newRunsFixture(t, unattendedOwner)
	f.spend.ledger = domain.Ledger{Readable: true, TokensToday: 4200}

	// Act
	ledger, err := f.runs.Ledger(context.Background(), userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if !ledger.Readable || ledger.TokensToday != 4200 {
		t.Errorf("ledger = %+v, want the account's spend for today", ledger)
	}
}

func TestRunsLedger_isRefusedForAMachineKeyIncludingAnOperator(t *testing.T) {
	// The ledger is per account. An operator asking about "the ledger" without naming one
	// would be asking a question with no answer; instance-wide totals are the goal engine's,
	// through the samples core pushes it.
	f := newRunsFixture(t, unattendedOwner)

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act
			_, err := f.runs.Ledger(context.Background(), actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestRunsLedger_anUnreadableLedgerIsAnErrorNotAZero(t *testing.T) {
	// Arrange — a false Readable is "no answer", not "nothing spent". Reporting zero here
	// would show somebody a clean budget while the counter is broken.
	f := newRunsFixture(t, unattendedOwner)
	f.spend.failLedger = errBoom

	// Act
	_, err := f.runs.Ledger(context.Background(), userActor("user-1"))

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
}

func TestRuns_defaultsTheClockWhenNoneIsGiven(t *testing.T) {
	// Arrange — a cancellation stamped at the zero time would read as a request made in the
	// year 1, which is worse than no timestamp at all.
	store := newFakeRuns()
	runs, err := NewRuns(RunsDeps{
		Runs: store, Auditor: store, Tasks: newFakeTasks(), Canceller: store, Spend: newFakeSpend(),
	})
	if err != nil {
		t.Fatalf("NewRuns: %v", err)
	}
	record := seedRun(t, store, "user-1", "task-1")

	// Act
	before := time.Now()
	if err := runs.Cancel(context.Background(), record.Run.ID, userActor("user-1")); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Assert
	requested := store.records[record.Run.ID].CancelRequestedAt
	if requested == nil {
		t.Fatal("cancelRequestedAt is nil, want a real time")
	}
	if requested.Before(before) {
		t.Errorf("cancelRequestedAt = %s, want a time at or after %s", requested, before)
	}
}
