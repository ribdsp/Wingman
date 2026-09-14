package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ribdsp/wingman/core/internal/domain"
)

func runRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "task_id", "owner_user_id", "provider", "model",
		"max_iterations", "max_tool_calls", "max_tokens_per_run", "max_tokens_per_user_day",
		"step_timeout_seconds", "sandbox_timeout_seconds",
		"iterations", "tool_calls", "tokens_used",
		"stop", "reason", "cancel_requested_at", "started_at", "finished_at",
	})
}

// inFlightRun is a row as it looks while the loop is still working: no stop reason, no
// finish time, no cancellation.
func inFlightRun() *sqlmock.Rows {
	return runRows().AddRow(
		"run_01", "tsk_01", "usr_01", "anthropic", "claude-opus-5",
		15, 40, int64(250_000), int64(2_000_000),
		180, 90,
		0, 0, int64(0),
		nil, "", nil, fixedNow, nil,
	)
}

func stepRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"run_id", "idx", "kind", "tool_name", "tokens_in", "tokens_out", "content", "err", "at",
	})
}

// This is the test that pins the write ordering in Start. Three layers disagree about
// what a zero limit means: Validate treats it as "not set" and passes it, the loop reads
// it as "no limit", and the schema's runs_limits_positive refuses it outright. So the
// zeros have to be filled between validating and writing, and the assertion here is on
// the arguments that reach the INSERT.
func TestRunRepository_start_fillsUnsetLimitsBeforeTheInsert(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`INSERT INTO runs`).
		WithArgs("tsk_01", "usr_01", "anthropic", "claude-opus-5",
			domain.DefaultMaxIterations, domain.DefaultMaxToolCalls,
			domain.DefaultMaxTokensPerRun, domain.DefaultMaxTokensPerUserDay,
			int(domain.DefaultStepTimeout.Seconds()),
			int(domain.DefaultSandboxTimeout.Seconds())).
		WillReturnRows(inFlightRun())

	// Act
	record, err := repo.Start(context.Background(), domain.Run{
		TaskID: "tsk_01", OwnerUserID: "usr_01",
		Provider: "anthropic", Model: "claude-opus-5",
		// Limits left entirely unset — the caller forgot.
	})

	// Assert
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	// A caller that forgot must not be the caller that gets an unbounded run.
	if !record.Run.InFlight() {
		t.Error("a new run came back already finished")
	}
	if record.Cancelled() {
		t.Error("a new run came back cancelled")
	}
}

func TestRunRepository_start_refusesLimitsOutsideTheirBoundsWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewRunRepository(db)

	// Act
	_, err := repo.Start(context.Background(), domain.Run{
		TaskID: "tsk_01", OwnerUserID: "usr_01",
		Provider: "anthropic", Model: "claude-opus-5",
		Limits: domain.RunLimits{MaxIterations: domain.MaxMaxIterations + 1},
	})

	// Assert
	// The bound is a spending control, so an over-large value is refused rather than
	// clamped: silently lowering somebody's stated limit hides that they asked for
	// something the instance will not do.
	if err == nil {
		t.Fatal("a run with an out-of-range iteration limit was accepted")
	}
}

// The limits are copied into the row rather than read from configuration later, so a run
// is judged against the bounds it actually had. "Why did this stop at 40 iterations when
// the limit is 60" has to be answerable a month afterwards.
func TestRunRepository_get_readsBackTheLimitsTheRunActuallyHad(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs WHERE id = \$1`).
		WithArgs("run_01").
		WillReturnRows(runRows().AddRow(
			"run_01", "tsk_01", "usr_01", "openai", "gpt-5",
			8, 12, int64(50_000), int64(400_000),
			120, 45,
			8, 3, int64(49_120),
			"iteration_cap", "reached 8 iterations", nil, fixedNow, fixedNow.Add(time.Minute),
		))

	// Act
	record, err := repo.Get(context.Background(), "run_01")

	// Assert
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if record.Run.Limits.MaxIterations != 8 {
		t.Errorf("maxIterations = %d; want the 8 this run ran under", record.Run.Limits.MaxIterations)
	}
	// Seconds in the column, a Duration in the domain. A minute stored as 60 and read
	// back as 60 nanoseconds would make every timeout effectively instant.
	if record.Run.Limits.StepTimeout != 2*time.Minute {
		t.Errorf("stepTimeout = %s; want 2m", record.Run.Limits.StepTimeout)
	}
	if record.Run.Limits.SandboxTimeout != 45*time.Second {
		t.Errorf("sandboxTimeout = %s; want 45s", record.Run.Limits.SandboxTimeout)
	}
	if record.Run.Stop != domain.StopIterationCap || record.Run.InFlight() {
		t.Errorf("run came back in flight with a stop reason: %+v", record.Run)
	}
}

func TestRunRepository_get_inFlightRunHasNoStopReasonAndNoFinishTime(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs WHERE id = \$1`).
		WithArgs("run_01").
		WillReturnRows(inFlightRun())

	// Act
	record, err := repo.Get(context.Background(), "run_01")

	// Assert
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	// A NULL stop column and an empty StopReason are the same fact spelled two ways.
	// Neither side gets to see the other's spelling.
	if record.Run.Stop != "" {
		t.Errorf("stop = %q; want empty while in flight", record.Run.Stop)
	}
	if record.Run.FinishedAt != nil {
		t.Error("an in-flight run came back with a finish time")
	}
}

func TestRunRepository_getForUser_scopesTheReadToTheOwner(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs WHERE id = \$2 AND owner_user_id = \$1`).
		WithArgs("usr_02", "run_01").
		WillReturnRows(runRows())

	// Act
	_, err := repo.GetForUser(context.Background(), "usr_02", "run_01")

	// Assert
	// A transcript is the agent's reasoning about somebody's business, and it is not
	// readable by an account that guessed a run id.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get another account's run = %v; want ErrNotFound", err)
	}
}

func TestRunRepository_listForTask_showsARetryAsASecondRun(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs\s+WHERE task_id = \$1\s+ORDER BY started_at DESC`).
		WithArgs("tsk_01", defaultPageLimit, 0).
		WillReturnRows(runRows().
			AddRow("run_02", "tsk_01", "usr_01", "anthropic", "claude-opus-5",
				15, 40, int64(250_000), int64(2_000_000), 180, 90,
				2, 1, int64(4_200), nil, "", nil, fixedNow, nil).
			AddRow("run_01", "tsk_01", "usr_01", "anthropic", "claude-opus-5",
				15, 40, int64(250_000), int64(2_000_000), 180, 90,
				1, 0, int64(900), "provider_error", "upstream 503", nil,
				fixedNow.Add(-time.Hour), fixedNow.Add(-59*time.Minute)))

	// Act
	records, err := repo.ListForTask(context.Background(), "tsk_01", 0, 0)

	// Assert
	// A retry after an outage is a second run, not an edit of the first: the failed
	// attempt keeps its own transcript and its own spend.
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d runs; want 2", len(records))
	}
	if !records[0].Run.InFlight() || records[1].Run.Stop != domain.StopProviderError {
		t.Errorf("runs came back in the wrong order or state: %+v", records)
	}
}

// AppendStep is the load-bearing method in this file. The step and the counter increment
// are one transaction because the counters are what the caps are checked against: if a
// step could be recorded without its cost, a crash between the two writes would leave a
// run whose transcript shows work its counters do not, and the next iteration would
// decide it had budget it had already spent.
func TestRunRepository_appendStep_writesTheStepAndItsCostInOneTransaction(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO run_steps`).
		WithArgs("run_01", 1, "model", "", int64(1_200), int64(340), "thinking", "", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// A model step advances iterations by one and tool calls by none.
	mock.ExpectExec(`UPDATE runs\s+SET iterations = iterations \+ \$2,\s+tool_calls = tool_calls \+ \$3,\s+tokens_used = tokens_used \+ \$4\s+WHERE id = \$1`).
		WithArgs("run_01", 1, 0, int64(1_540)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Act
	err := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 1, Kind: domain.StepKindModel,
		TokensIn: 1_200, TokensOut: 340, Content: "thinking", At: fixedNow,
	})

	// Assert
	if err != nil {
		t.Fatalf("append step: %v", err)
	}
}

// The increment is `tokens_used = tokens_used + $4` rather than a value read earlier and
// written back. Two workers cannot both be stepping one run, but a retry can, and += in
// the database cannot lose an increment the way read-modify-write can.
func TestRunRepository_appendStep_toolStepAdvancesToolCallsAndCostsNothing(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO run_steps`).
		WithArgs("run_01", 4, "tool", "shell", int64(0), int64(0), "ok", "", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE runs`).
		WithArgs("run_01", 0, 1, int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Act
	err := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 4, Kind: domain.StepKindTool,
		ToolName: "shell", Content: "ok", At: fixedNow,
	})

	// Assert
	// Separating the two counters is what makes "reached the iteration cap having done
	// nothing" a different story from "reached the tool-call cap".
	if err != nil {
		t.Fatalf("append tool step: %v", err)
	}
}

func TestRunRepository_appendStep_rollsBackWhenTheCounterUpdateFails(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO run_steps`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE runs`).
		WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	// Act
	err := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 2, Kind: domain.StepKindModel,
		TokensIn: 500, TokensOut: 100, Content: "thinking", At: fixedNow,
	})

	// Assert
	// The step must not survive without its cost. A transcript that shows spend the
	// counters do not is a run that gets to spend it twice.
	if err == nil {
		t.Fatal("a failed counter update was reported as a recorded step")
	}
}

func TestRunRepository_appendStep_missingRunRollsBackTheStep(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO run_steps`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The counter update matched no run.
	mock.ExpectExec(`UPDATE runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	// Act
	err := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_missing", Index: 1, Kind: domain.StepKindModel,
		TokensIn: 10, TokensOut: 10, Content: "thinking", At: fixedNow,
	})

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("append step to a missing run = %v; want ErrNotFound", err)
	}
}

func TestRunRepository_appendStep_refusesAToolStepThatBillsTokens(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewRunRepository(db)

	// Act
	err := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 3, Kind: domain.StepKindTool,
		ToolName: "shell", TokensOut: 200, At: fixedNow,
	})

	// Assert
	// A sandbox does not bill tokens. Charging a tool step for the model call that
	// requested it would double-count against the run's allowance, and no transaction
	// is opened to find that out.
	if err == nil {
		t.Fatal("a tool step billing tokens was accepted")
	}
}

func TestRunRepository_appendStep_refusesAnUnnumberedStepAndAnUnknownKind(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewRunRepository(db)

	// Act
	zeroIndex := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 0, Kind: domain.StepKindModel, At: fixedNow,
	})
	unknownKind := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 1, Kind: domain.StepKind("guess"), At: fixedNow,
	})
	noTimestamp := repo.AppendStep(context.Background(), domain.Step{
		RunID: "run_01", Index: 1, Kind: domain.StepKindModel,
	})

	// Assert
	// The transcript numbers itself from 1 so a gap is visible rather than being read as
	// the beginning. An unknown kind would advance neither counter, which is how a run
	// spends without any cap noticing.
	if zeroIndex == nil {
		t.Error("a step numbered 0 was accepted")
	}
	if unknownKind == nil {
		t.Error("a step that is neither model nor tool was accepted")
	}
	if noTimestamp == nil {
		t.Error("a step with no timestamp was accepted")
	}
}

func TestRunRepository_nextStepIndex_continuesTheNumberingAfterARestart(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`SELECT coalesce\(max\(idx\), 0\) \+ 1 FROM run_steps WHERE run_id = \$1`).
		WithArgs("run_01").
		WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(5))

	// Act
	next, err := repo.NextStepIndex(context.Background(), "run_01")

	// Assert
	// (run_id, idx) is unique, so a restarted worker that began again at 1 would collide
	// with the step it had already written and stop the run it was trying to resume.
	if err != nil {
		t.Fatalf("read next step index: %v", err)
	}
	if next != 5 {
		t.Errorf("next index = %d; want 5", next)
	}
}

func TestRunRepository_nextStepIndex_anEmptyTranscriptStartsAtOne(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`coalesce\(max\(idx\), 0\) \+ 1`).
		WithArgs("run_01").
		WillReturnRows(sqlmock.NewRows([]string{"next"}).AddRow(1))

	// Act
	next, err := repo.NextStepIndex(context.Background(), "run_01")

	// Assert
	if err != nil {
		t.Fatalf("read next step index: %v", err)
	}
	if next != 1 {
		t.Errorf("next index = %d; want 1 on an empty transcript", next)
	}
}

func TestRunRepository_steps_returnsTheTranscriptInOrder(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM run_steps\s+WHERE run_id = \$1\s+ORDER BY idx ASC`).
		WithArgs("run_01", defaultPageLimit, 0).
		WillReturnRows(stepRows().
			AddRow("run_01", 1, "model", "", 900, 120, "I should look at the file", "", fixedNow).
			AddRow("run_01", 2, "tool", "shell", 0, 0, "", "exit status 1", fixedNow))

	// Act
	steps, err := repo.Steps(context.Background(), "run_01", 0, 0)

	// Assert
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps; want 2", len(steps))
	}
	// The error is written for a person reading the transcript, so it is a field of its
	// own rather than being folded into the content.
	if steps[1].Err != "exit status 1" {
		t.Errorf("step 2 error = %q; want the failure a reader needs", steps[1].Err)
	}
	if steps[0].Tokens() != 1_020 {
		t.Errorf("step 1 cost %d tokens; want 1020", steps[0].Tokens())
	}
}

// Overwriting the first stop reason with a second would destroy the record of why the run
// actually stopped, so `stop IS NULL` is in the WHERE clause and no rows is ErrNotFound.
func TestRunRepository_finish_refusesToOverwriteAnEarlierStopReason(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectExec(`UPDATE runs\s+SET stop = \$2, reason = \$3, finished_at = \$4\s+WHERE id = \$1 AND stop IS NULL`).
		WithArgs("run_01", "completed", "the model said it was done", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.Finish(context.Background(), "run_01",
		domain.StopCompleted, "the model said it was done", fixedNow)

	// Assert
	// A run halted by the kill switch that later reported "completed" would be a run
	// whose record says the opposite of what happened.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish an already-finished run = %v; want ErrNotFound", err)
	}
}

func TestRunRepository_finish_refusesAFinishWithNoReasonOrNoTime(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewRunRepository(db)

	// Act
	noReason := repo.Finish(context.Background(), "run_01", "", "", fixedNow)
	noTime := repo.Finish(context.Background(), "run_01", domain.StopCompleted, "done", time.Time{})

	// Assert
	// The schema requires the two to agree: a row with one and not the other reads as a
	// run still going, which is exactly the state a crashed worker leaves behind and
	// exactly what FailAbandoned exists to clean up.
	if noReason == nil {
		t.Error("a run was finished with no stop reason")
	}
	if noTime == nil {
		t.Error("a run was finished with no finish time")
	}
}

func TestRunRepository_finish_recordsTheReasonItWasGiven(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectExec(`UPDATE runs`).
		WithArgs("run_01", "halted", "the kill switch is engaged", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.Finish(context.Background(), "run_01",
		domain.StopHalted, "the kill switch is engaged", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// RequestCancel records a request and returns; it does not kill anything. The loop stops
// at its next step boundary, which is what leaves a readable transcript and a run whose
// counters match what it actually spent.
func TestRunRepository_requestCancel_scopesTheRequestToTheOwner(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectExec(`UPDATE runs SET cancel_requested_at = \$3\s+WHERE id = \$2 AND owner_user_id = \$1\s+AND stop IS NULL AND cancel_requested_at IS NULL`).
		WithArgs("usr_01", "run_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	err := repo.RequestCancel(context.Background(), "usr_01", "run_01", fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("request cancel: %v", err)
	}
}

func TestRunRepository_requestCancel_aFinishedOrAlreadyCancelledRunIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectExec(`UPDATE runs SET cancel_requested_at`).
		WithArgs("usr_01", "run_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.RequestCancel(context.Background(), "usr_01", "run_01", fixedNow)

	// Assert
	// `cancel_requested_at IS NULL` keeps a second request from moving the timestamp,
	// which would misdate when somebody actually asked the run to stop.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel a finished run = %v; want ErrNotFound", err)
	}
}

func TestRunRepository_get_readsBackACancellationRequest(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	requested := fixedNow.Add(-time.Minute)
	mock.ExpectQuery(`FROM runs WHERE id = \$1`).
		WithArgs("run_01").
		WillReturnRows(runRows().AddRow(
			"run_01", "tsk_01", "usr_01", "anthropic", "claude-opus-5",
			15, 40, int64(250_000), int64(2_000_000), 180, 90,
			3, 1, int64(9_000),
			nil, "", requested, fixedNow.Add(-2*time.Minute), nil,
		))

	// Act
	record, err := repo.Get(context.Background(), "run_01")

	// Assert
	// The loop reads this at the top of each iteration. A run that is cancelled but
	// still in flight is the normal state between the request and the next boundary.
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !record.Cancelled() {
		t.Error("a cancellation request was not read back")
	}
	if !record.Run.InFlight() {
		t.Error("a cancellation was read as a finished run; the loop stops at a step boundary, not here")
	}
}

// FailAbandoned is the other half of the crash-recovery story from
// TaskRepository.RequeueStale. A run stuck with no stop reason reads as still working,
// forever: it holds a place in the in-flight index and tells a user their work is in
// progress when nothing is executing it.
func TestRunRepository_failAbandoned_finishesRunsNoWorkerIsExecuting(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	cutoff := fixedNow.Add(-time.Hour)
	mock.ExpectExec(`UPDATE runs\s+SET stop = \$1, reason = 'the worker running this run stopped before it finished',\s+finished_at = \$2\s+WHERE stop IS NULL AND started_at < \$3`).
		WithArgs(string(domain.StopAbandoned), fixedNow, cutoff).
		WillReturnResult(sqlmock.NewResult(0, 3))

	// Act
	failed, err := repo.FailAbandoned(context.Background(), cutoff, fixedNow)

	// Assert
	if err != nil {
		t.Fatalf("fail abandoned runs: %v", err)
	}
	if failed != 3 {
		t.Errorf("failed %d runs; want 3", failed)
	}
}

func TestRunRepository_failAbandoned_usesItsOwnStopReasonNotProviderError(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectExec(`UPDATE runs`).
		WithArgs("abandoned", fixedNow, fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Act
	_, err := repo.FailAbandoned(context.Background(), fixedNow, fixedNow)

	// Assert
	// A crash in this process and a failure at the vendor send an operator to different
	// logs, so they are different reasons. The argument matcher above is the assertion:
	// it fails if the written reason changes.
	if err != nil {
		t.Fatalf("fail abandoned runs: %v", err)
	}
	// The reason has to be in the closed set the stop column's enum mirrors, or this
	// sweep would fail on every row it tried to clean up — the one write that must not
	// fail is the one that unsticks a run nothing is executing.
	var known bool
	for _, reason := range domain.AllStopReasons() {
		if reason == domain.StopAbandoned {
			known = true
			break
		}
	}
	if !known {
		t.Error("abandoned is not in AllStopReasons, so the stop column's enum will refuse it")
	}
	if domain.StopAbandoned.Succeeded() {
		t.Error("abandoned counted as success, so a crashed run would be filed as work done")
	}
}

func TestRunRepository_get_aRunThatDoesNotExistIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs WHERE id = \$1`).
		WithArgs("run_missing").
		WillReturnRows(runRows())

	// Act
	_, err := repo.Get(context.Background(), "run_missing")

	// Assert
	// A zero RunRecord returned instead would be a run with no limits at all, and the
	// loop reads a zero limit as "no limit" — so this is the difference between an
	// unknown id and an unbounded run.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get a missing run = %v; want ErrNotFound", err)
	}
}

func TestRunRepository_getForUser_returnsTheOwnersOwnRun(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewRunRepository(db)
	mock.ExpectQuery(`FROM runs WHERE id = \$2 AND owner_user_id = \$1`).
		WithArgs("usr_01", "run_01").
		WillReturnRows(inFlightRun())

	// Act
	run, err := repo.GetForUser(context.Background(), "usr_01", "run_01")

	// Assert
	// The scoped read has to work for the owner as well as fail for everybody else. A
	// clause that matched nothing at all would pass the not-found test on its own.
	if err != nil {
		t.Fatalf("get own run: %v", err)
	}
	if run.Run.ID != "run_01" || run.Run.OwnerUserID != "usr_01" {
		t.Errorf("run = %+v; want run_01 owned by usr_01", run)
	}
}
