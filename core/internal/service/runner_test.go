package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

const (
	testProvider = "anthropic"
	testModel    = "claude-opus-5"
)

// agentOutcome is what the fake loop ends with. A helper rather than a literal per test,
// because the three fields are read together: whether there is an answer decides what lands
// in the chat, and the other two are what a note says when there is none.
func agentOutcome(stop domain.StopReason, reason, answer string) agent.Outcome {
	return agent.Outcome{Stop: stop, Reason: reason, Answer: answer}
}

// runnerFixture holds every fake the worker is wired to, so a test can seed the queue and
// then read what was written on the way out.
type runnerFixture struct {
	runner     *Runner
	queue      *fakeTasks
	runs       *fakeRuns
	agent      *fakeAgent
	tools      *fakeTools
	workspaces *fakeWorkspaces
	chats      *fakeChats
	spend      *fakeSpend
	reporter   *fakeReporter
}

// newRunnerFakes builds the fakes without the worker, for the tests that wire it up
// themselves.
func newRunnerFakes() runnerFixture {
	return runnerFixture{
		queue:      newFakeTasks(),
		runs:       newFakeRuns(),
		agent:      &fakeAgent{outcome: agentOutcome(domain.StopCompleted, "", "Done.")},
		tools:      &fakeTools{},
		workspaces: &fakeWorkspaces{root: "/var/lib/wingman/workspaces"},
		chats:      newFakeChats(),
		spend:      newFakeSpend(),
		reporter:   &fakeReporter{},
	}
}

// requiredDeps is the wiring every runner test starts from: the seven required ports, a
// fixed clock, and nothing optional. Zero limits on purpose — WithDefaults is what fills
// them, and a test that passes real numbers cannot see that happen.
func (f runnerFixture) requiredDeps() RunnerDeps {
	return RunnerDeps{
		Queue:      f.queue,
		Runs:       f.runs,
		Agent:      f.agent,
		Tools:      f.tools,
		Workspaces: f.workspaces,
		Messages:   f.chats,
		Spend:      f.spend,
		Provider:   testProvider,
		Model:      testModel,
		Clock:      fixedClock(testNow),
		Logger:     silentLogger(),
	}
}

// newRunnerFixture wires a worker over the fakes. withReporter is explicit because "no
// goal engine configured" is a supported way to run core, not an edge case.
func newRunnerFixture(t *testing.T, withReporter bool) runnerFixture {
	t.Helper()

	f := newRunnerFakes()
	deps := f.requiredDeps()
	if withReporter {
		deps.Reporter = f.reporter
	}
	runner, err := NewRunner(deps)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	f.runner = runner
	return f
}

// seedTask queues one task. withChat decides whether somebody is waiting for the answer:
// an unattended task dispatched by the goal engine has no conversation at all.
func (f runnerFixture) seedTask(t *testing.T, owner, brief string, withChat bool) (domain.Task, string) {
	t.Helper()

	chatID := ""
	if withChat {
		chat, err := f.chats.CreateChat(context.Background(), owner, "Conversation")
		if err != nil {
			t.Fatalf("seed a chat: %v", err)
		}
		chatID = chat.ID
	}
	task, err := f.queue.Create(context.Background(), repository.NewTask{
		Task: domain.Task{
			OwnerUserID: owner,
			Source:      domain.TaskSourceUser,
			Brief:       brief,
			Status:      domain.TaskStatusQueued,
		},
		ChatID: chatID,
	})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	return task, chatID
}

// ---------------------------------------------------------------------------
// the sweep's janitors, recorded

// sweepTrace is the order the janitors were called in, which is the part of Sweep that
// carries the safety argument. It is local to this file rather than a field on the shared
// fakes, because no other test cares.
type sweepTrace struct{ calls []string }

// recordingRunJanitor is RunJanitor, keeping the window it was handed.
type recordingRunJanitor struct {
	trace  *sweepTrace
	failed int
	err    error

	before time.Time
	now    time.Time
}

func (j *recordingRunJanitor) FailAbandoned(_ context.Context, before, now time.Time) (int, error) {
	j.trace.calls = append(j.trace.calls, "runs")
	j.before, j.now = before, now
	if j.err != nil {
		return 0, j.err
	}
	return j.failed, nil
}

// recordingTaskJanitor is TaskJanitor, keeping the cutoff it was handed.
type recordingTaskJanitor struct {
	trace    *sweepTrace
	requeued int
	err      error

	before time.Time
}

func (j *recordingTaskJanitor) RequeueStale(_ context.Context, before time.Time) (int, error) {
	j.trace.calls = append(j.trace.calls, "tasks")
	j.before = before
	if j.err != nil {
		return 0, j.err
	}
	return j.requeued, nil
}

var (
	_ RunJanitor  = (*recordingRunJanitor)(nil)
	_ TaskJanitor = (*recordingTaskJanitor)(nil)
)

// newSweepFixture wires a worker whose only interesting parts are its janitors.
func newSweepFixture(t *testing.T, staleAfter time.Duration, runs RunJanitor, tasks TaskJanitor) *Runner {
	t.Helper()

	deps := newRunnerFakes().requiredDeps()
	deps.StaleAfter = staleAfter
	deps.RunJanitor = runs
	deps.TaskJanitor = tasks
	runner, err := NewRunner(deps)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}

// ---------------------------------------------------------------------------
// wiring

func TestNewRunner_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewRunner(RunnerDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Queue", "Runs", "Agent", "Tools", "Workspaces", "Messages", "Spend", "Provider", "Model"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestNewRunner_refusesLimitsThatWouldNotBoundARun(t *testing.T) {
	// Arrange — config validates these too. This is the last check before a run row is
	// written with them, and a run started against a broken bound is the one thing the
	// whole stop-reason ladder cannot recover from.
	deps := newRunnerFakes().requiredDeps()
	deps.Limits = domain.RunLimits{MaxIterations: -1}

	// Act
	_, err := NewRunner(deps)

	// Assert
	if err == nil {
		t.Fatal("err = nil, want the limits refused")
	}
	if !strings.Contains(err.Error(), "limits are invalid") {
		t.Errorf("err = %v, want it to say the limits were the problem", err)
	}
}

func TestNewRunner_startsARunWithDefaultedLimitsWhenNoneAreConfigured(t *testing.T) {
	// Arrange — a zero limit reads as "no limit" to the loop, so the run row must never
	// carry one.
	f := newRunnerFixture(t, false)
	f.seedTask(t, "user-1", "Reconcile yesterday's payouts", true)

	// Act
	if _, err := f.runner.RunNext(context.Background()); err != nil {
		t.Fatalf("RunNext: %v", err)
	}

	// Assert
	if len(f.runs.started) != 1 {
		t.Fatalf("runs started = %d, want 1", len(f.runs.started))
	}
	started := f.runs.started[0]
	if started.Limits.MaxIterations != domain.DefaultMaxIterations {
		t.Errorf("maxIterations = %d, want the default %d", started.Limits.MaxIterations, domain.DefaultMaxIterations)
	}
	if started.Limits.MaxTokensPerRun != domain.DefaultMaxTokensPerRun {
		t.Errorf("maxTokensPerRun = %d, want the default %d", started.Limits.MaxTokensPerRun, domain.DefaultMaxTokensPerRun)
	}
	// Recorded on the run, not read from config at display time: a run that used a
	// different model from today's default has to stay readable.
	if started.Provider != testProvider || started.Model != testModel {
		t.Errorf("provider/model = %q/%q, want %q/%q", started.Provider, started.Model, testProvider, testModel)
	}
}

// ---------------------------------------------------------------------------
// claiming

func TestRunnerRunNext_anEmptyQueueIsNotAnError(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, false)

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert — the normal state of an idle instance. An error here would fill the log with
	// the worker's own heartbeat.
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if worked {
		t.Error("worked = true, want false for an empty queue")
	}
}

func TestRunnerRunNext_reportsAClaimFailureAsItself(t *testing.T) {
	// Arrange — the database is unreachable, which is not the same as having nothing to do.
	f := newRunnerFixture(t, false)
	f.queue.failClaim = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if !strings.Contains(err.Error(), "claim a queued task") {
		t.Errorf("err = %v, want it to name what failed", err)
	}
	if worked {
		t.Error("worked = true, want false when nothing was claimed")
	}
}

// ---------------------------------------------------------------------------
// the happy path

func TestRunnerRunNext_drivesAQueuedTaskToItsAnswer(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, true)
	task, chatID := f.seedTask(t, "user-1", "Summarise this week's refunds", true)
	f.agent.outcome = agentOutcome(domain.StopCompleted, "", "  Seven refunds, all under the cap.  ")

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Fatal("worked = false, want true when a task was claimed")
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusSucceeded {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusSucceeded)
	}

	// The loop was given the run, the task, the workspace and a tool snapshot — the four
	// things it may not fetch for itself.
	if len(f.agent.inputs) != 1 {
		t.Fatalf("agent runs = %d, want 1", len(f.agent.inputs))
	}
	in := f.agent.inputs[0]
	if in.Run.ID == "" || in.Run.ID != f.runs.order[0] {
		t.Errorf("run id = %q, want the started run's %q", in.Run.ID, f.runs.order[0])
	}
	if in.Task.ID != task.ID || in.Task.Brief != task.Brief {
		t.Errorf("task = %+v, want the claimed one", in.Task)
	}
	if in.Workspace != "/var/lib/wingman/workspaces/user-1" {
		t.Errorf("workspace = %q, want the owner's own", in.Workspace)
	}
	if in.Tools == nil {
		t.Error("tools = nil, want the snapshot taken before the run started")
	}

	// And the answer is back in the conversation, attributed to the run that produced it.
	messages := f.chats.messages[chatID]
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the answer", len(messages))
	}
	if messages[0].Role != repository.MessageRoleAssistant {
		t.Errorf("role = %q, want an assistant message", messages[0].Role)
	}
	if messages[0].Content != "Seven refunds, all under the cap." {
		t.Errorf("content = %q, want the answer trimmed", messages[0].Content)
	}
	if messages[0].RunID != in.Run.ID {
		t.Errorf("runId = %q, want %q", messages[0].RunID, in.Run.ID)
	}
}

func TestRunnerRunNext_anUnattendedTaskLeavesNothingInAnyChat(t *testing.T) {
	// Arrange — the goal engine dispatched this. There is no conversation, and the run's
	// transcript is its record.
	f := newRunnerFixture(t, true)
	task, _ := f.seedTask(t, unattendedOwner, "Chase the overdue invoices", false)

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Fatal("worked = false, want true")
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusSucceeded {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusSucceeded)
	}
	if len(f.chats.messages) != 0 {
		t.Errorf("messages = %v, want none written for an unattended task", f.chats.messages)
	}
}

// ---------------------------------------------------------------------------
// a run that answered nothing

func TestRunnerRunNext_aRunThatSaidNothingLeavesASystemNoteCarryingNoRunId(t *testing.T) {
	// Arrange — a denied tool, which is a bounded system working, not a broken one.
	f := newRunnerFixture(t, true)
	_, chatID := f.seedTask(t, "user-1", "Wire the money", true)
	f.agent.outcome = agentOutcome(domain.StopToolDenied, `the tool "payments.transfer" is not permitted for this run`, "")

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Fatal("worked = false, want true: the run ended correctly, it just ended early")
	}

	messages := f.chats.messages[chatID]
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the note", len(messages))
	}
	note := messages[0]
	// The load-bearing assertion. messages_run_is_assistants allows a run id only on an
	// assistant row, so a note carrying one would be refused by a real Postgres — and
	// attributing it to the run would let the chat read as though the agent had said it.
	if note.Role != repository.MessageRoleSystem {
		t.Errorf("role = %q, want a system note rather than an empty assistant turn", note.Role)
	}
	if note.RunID != "" {
		t.Errorf("runId = %q, want none on a non-assistant row", note.RunID)
	}
	// Quoting the run's own reason: this is the owner's run, and "it stopped" alone sends
	// them to an operator to find out what a working limit did.
	if !strings.Contains(note.Content, "payments.transfer") {
		t.Errorf("content = %q, want it to say what stopped the run", note.Content)
	}
}

func TestRunnerRunNext_aStopWithNoReasonStillNamesTheStop(t *testing.T) {
	// Arrange — nothing worth quoting, so the enum is the next best thing.
	f := newRunnerFixture(t, true)
	_, chatID := f.seedTask(t, "user-1", "Keep going", true)
	f.agent.outcome = agentOutcome(domain.StopIterationCap, "", "")

	// Act
	if _, err := f.runner.RunNext(context.Background()); err != nil {
		t.Fatalf("RunNext: %v", err)
	}

	// Assert
	messages := f.chats.messages[chatID]
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the note", len(messages))
	}
	if !strings.Contains(messages[0].Content, string(domain.StopIterationCap)) {
		t.Errorf("content = %q, want it to name the stop reason", messages[0].Content)
	}
}

// ---------------------------------------------------------------------------
// failures before the run exists

func TestRunnerRunNext_aWorkspaceFailureFilesTheTaskWithoutStartingARun(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, true)
	task, chatID := f.seedTask(t, "user-1", "Read the export", true)
	f.workspaces.err = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert — a run row written and then abandoned would tell the person their work was
	// in progress until the sweep closed it hours later.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the workspace failure", err)
	}
	if !worked {
		t.Error("worked = false, want true: a task was claimed and has to be reported on")
	}
	if len(f.runs.started) != 0 {
		t.Errorf("runs started = %d, want none", len(f.runs.started))
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusFailed {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusFailed)
	}
	if len(f.chats.messages[chatID]) != 1 {
		t.Fatalf("messages = %d, want somebody waiting to be told", len(f.chats.messages[chatID]))
	}
	if role := f.chats.messages[chatID][0].Role; role != repository.MessageRoleSystem {
		t.Errorf("role = %q, want a system note", role)
	}
}

func TestRunnerRunNext_aToolSnapshotFailureFilesTheTaskWithoutStartingARun(t *testing.T) {
	// Arrange — an unlistable source is one thing; not being able to assemble the offering
	// at all means this run would not know what it is allowed to do.
	f := newRunnerFixture(t, true)
	task, chatID := f.seedTask(t, "user-1", "Look it up", true)
	f.tools.err = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the tool source's failure", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if len(f.runs.started) != 0 {
		t.Errorf("runs started = %d, want none", len(f.runs.started))
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusFailed {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusFailed)
	}
	if len(f.chats.messages[chatID]) != 1 {
		t.Errorf("messages = %d, want the note", len(f.chats.messages[chatID]))
	}
}

func TestRunnerRunNext_aRunThatCannotBeStartedNeverReachesTheLoop(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, true)
	task, chatID := f.seedTask(t, "user-1", "Do the thing", true)
	f.runs.failStart = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert — no run row means no transcript, so there is nowhere for the loop to record
	// what it did. It must not be started.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if len(f.agent.inputs) != 0 {
		t.Errorf("agent runs = %d, want none", len(f.agent.inputs))
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusFailed {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusFailed)
	}
	if len(f.chats.messages[chatID]) != 1 {
		t.Errorf("messages = %d, want the note", len(f.chats.messages[chatID]))
	}
}

func TestRunnerRunNext_anUnattendedTaskThatCannotStartIsFiledWithNoNote(t *testing.T) {
	// Arrange — nobody is waiting, so there is nowhere to put a note. The task status and
	// the log line are the whole record.
	f := newRunnerFixture(t, true)
	task, _ := f.seedTask(t, unattendedOwner, "Nightly reconciliation", false)
	f.workspaces.err = errBoom

	// Act
	_, err := f.runner.RunNext(context.Background())

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the workspace failure", err)
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusFailed {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusFailed)
	}
	if len(f.chats.messages) != 0 {
		t.Errorf("messages = %v, want none", f.chats.messages)
	}
}

// ---------------------------------------------------------------------------
// the loop's own failure

func TestRunnerRunNext_theLoopsOwnFailureLeavesTheRunInFlightForTheSweep(t *testing.T) {
	// Arrange — the loop returns an error only when it could not write the transcript or
	// the run row, which is the one failure it cannot record itself.
	f := newRunnerFixture(t, true)
	task, chatID := f.seedTask(t, "user-1", "Do the thing", true)
	f.agent.err = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the loop's failure", err)
	}
	if !strings.Contains(err.Error(), task.ID) {
		t.Errorf("err = %v, want it to name the task", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusFailed {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusFailed)
	}
	// Left in flight on purpose: the sweep closes it as abandoned, which is what it is.
	// Stamping a stop reason from out here would put a second author on that row.
	runID := f.runs.order[0]
	if !f.runs.records[runID].Run.InFlight() {
		t.Error("the run was stopped from outside the loop, want it left for the sweep")
	}
	// And nothing is delivered or reported: there is no outcome to deliver.
	if len(f.chats.messages[chatID]) != 0 {
		t.Errorf("messages = %d, want none", len(f.chats.messages[chatID]))
	}
	if len(f.reporter.samples) != 0 {
		t.Errorf("samples = %d, want none", len(f.reporter.samples))
	}
}

func TestRunnerRunNext_anAnswerThatCannotBeDeliveredDoesNotFailTheRun(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, true)
	task, _ := f.seedTask(t, "user-1", "Summarise it", true)
	f.chats.failAppend = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert — the run happened and its transcript is written. Losing the reply from the
	// conversation is bad enough to log loudly and not bad enough to rewrite history with.
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if got := f.queue.tasks[task.ID].Status; got != domain.TaskStatusSucceeded {
		t.Errorf("status = %q, want %q", got, domain.TaskStatusSucceeded)
	}
}

func TestRunnerRunNext_aStatusThatCannotBeRecordedDoesNotFailTheRun(t *testing.T) {
	// Arrange — a task stuck at running while its run finished is exactly what the sweep
	// exists for, so the recovery is already in place.
	f := newRunnerFixture(t, true)
	f.seedTask(t, "user-1", "Summarise it", true)
	f.queue.failSetStatus = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if len(f.queue.statuses) != 1 {
		t.Errorf("status writes = %v, want one attempt", f.queue.statuses)
	}
}

// ---------------------------------------------------------------------------
// reporting what it cost

func TestRunnerRunNext_reportsTheLedgersTotalWithNothingButTheRunAndItsStop(t *testing.T) {
	// Arrange — read back from the ledger rather than taken from the outcome, because the
	// ledger is what was actually charged.
	f := newRunnerFixture(t, true)
	f.seedTask(t, "user-1", "Summarise this week's refunds", true)
	f.spend.charges["run-1"] = []repository.Spend{
		{UserID: "user-1", RunID: "run-1", TokensIn: 900, TokensOut: 100},
		{UserID: "user-1", RunID: "run-1", TokensIn: 40, TokensOut: 60},
	}

	// Act
	if _, err := f.runner.RunNext(context.Background()); err != nil {
		t.Fatalf("RunNext: %v", err)
	}

	// Assert
	if len(f.reporter.samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(f.reporter.samples))
	}
	sample := f.reporter.samples[0]
	if sample.Tokens != 1100 {
		t.Errorf("tokens = %d, want every charge on the run", sample.Tokens)
	}
	if !sample.ObservedAt.Equal(testNow) {
		t.Errorf("observedAt = %s, want the clock's %s", sample.ObservedAt, testNow)
	}
	if sample.Note != "core run run-1 ("+string(domain.StopCompleted)+")" {
		t.Errorf("note = %q, want the run id and the stop reason", sample.Note)
	}
	// The note is stored by the engine and rendered back to an operator, so it must never
	// carry a brief, a model's words or anything a tool returned.
	for _, leaked := range []string{"refunds", "Done."} {
		if strings.Contains(sample.Note, leaked) {
			t.Errorf("note = %q, want nothing from the run's content in it", sample.Note)
		}
	}
}

func TestRunnerRunNext_aRunThatSpentNothingIsNotAReading(t *testing.T) {
	// Arrange — halted, or cancelled before its first turn.
	f := newRunnerFixture(t, true)
	f.seedTask(t, "user-1", "Do the thing", true)
	f.agent.outcome = agentOutcome(domain.StopHalted, "the kill switch is engaged", "")

	// Act
	if _, err := f.runner.RunNext(context.Background()); err != nil {
		t.Fatalf("RunNext: %v", err)
	}

	// Assert — pushing a zero would put a real data point of "no spend" into a metric an
	// operator writes a goal against.
	if len(f.reporter.samples) != 0 {
		t.Errorf("samples = %+v, want none for a run that spent nothing", f.reporter.samples)
	}
}

func TestRunnerRunNext_withoutAReporterTheLedgerIsNotEvenRead(t *testing.T) {
	// Arrange — no goal engine configured. The failing ledger proves the early return
	// rather than a read whose result is thrown away.
	f := newRunnerFixture(t, false)
	f.seedTask(t, "user-1", "Do the thing", true)
	f.spend.failForRun = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
}

func TestRunnerRunNext_anUnreadableLedgerDoesNotFailTheRun(t *testing.T) {
	// Arrange
	f := newRunnerFixture(t, true)
	f.seedTask(t, "user-1", "Do the thing", true)
	f.spend.failForRun = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert — the run has already happened. Losing the bookkeeping must not turn into
	// losing the work.
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
	if len(f.reporter.samples) != 0 {
		t.Errorf("samples = %d, want none: a total that could not be read is not a reading", len(f.reporter.samples))
	}
}

func TestRunnerRunNext_aReporterThatRefusesDoesNotFailTheRun(t *testing.T) {
	// Arrange — the goal engine is down, or its bot key was rotated.
	f := newRunnerFixture(t, true)
	f.seedTask(t, "user-1", "Do the thing", true)
	f.spend.charges["run-1"] = []repository.Spend{{UserID: "user-1", RunID: "run-1", TokensIn: 10, TokensOut: 5}}
	f.reporter.err = errBoom

	// Act
	worked, err := f.runner.RunNext(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("RunNext: %v", err)
	}
	if !worked {
		t.Error("worked = false, want true")
	}
}

// ---------------------------------------------------------------------------
// the crash sweep

func TestRunnerSweep_failsRunsBeforeRequeueingTasks(t *testing.T) {
	// Arrange
	trace := &sweepTrace{}
	runs := &recordingRunJanitor{trace: trace, failed: 2}
	tasks := &recordingTaskJanitor{trace: trace, requeued: 3}
	runner := newSweepFixture(t, time.Hour, runs, tasks)

	// Act
	runsFailed, tasksRequeued, err := runner.Sweep(context.Background())

	// Assert — the reverse order can leave one task with two live runs, and a transcript
	// that cannot be read as one story.
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if strings.Join(trace.calls, ",") != "runs,tasks" {
		t.Errorf("order = %v, want the runs closed first", trace.calls)
	}
	if runsFailed != 2 || tasksRequeued != 3 {
		t.Errorf("swept %d runs and %d tasks, want 2 and 3", runsFailed, tasksRequeued)
	}
	// The window is closed at both ends: the run janitor needs a "now" to stamp the
	// abandoned rows with, not just a cutoff to find them by.
	if !runs.now.Equal(testNow) {
		t.Errorf("now = %s, want the clock's %s", runs.now, testNow)
	}
	if !tasks.before.Equal(runs.before) {
		t.Errorf("cutoffs differ: tasks %s, runs %s — want one window", tasks.before, runs.before)
	}
}

func TestRunnerSweep_neverTreatsWorkAsStaleSoonerThanTheFloor(t *testing.T) {
	// A short window turns the sweep into a way to requeue live work, so a value under the
	// floor is raised rather than honoured.
	for name, tc := range map[string]struct{ configured, want time.Duration }{
		"under the floor": {time.Second, minStaleAfter},
		"unset":           {0, minStaleAfter},
		"over the floor":  {2 * time.Hour, 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			janitor := &recordingRunJanitor{trace: &sweepTrace{}}
			runner := newSweepFixture(t, tc.configured, janitor, nil)

			// Act
			if _, _, err := runner.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}

			// Assert
			if want := testNow.Add(-tc.want); !janitor.before.Equal(want) {
				t.Errorf("cutoff = %s, want %s", janitor.before, want)
			}
		})
	}
}

func TestRunnerSweep_withoutJanitorsDoesNothingAndSaysSo(t *testing.T) {
	// Arrange — a single-worker instance can live without the sweep, and a test wants it
	// off.
	runner := newSweepFixture(t, time.Hour, nil, nil)

	// Act
	runsFailed, tasksRequeued, err := runner.Sweep(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if runsFailed != 0 || tasksRequeued != 0 {
		t.Errorf("swept %d runs and %d tasks, want nothing", runsFailed, tasksRequeued)
	}
}

func TestRunnerSweep_aRunJanitorFailureStopsBeforeAnyTaskIsRequeued(t *testing.T) {
	// Arrange
	trace := &sweepTrace{}
	runs := &recordingRunJanitor{trace: trace, err: errBoom}
	tasks := &recordingTaskJanitor{trace: trace, requeued: 3}
	runner := newSweepFixture(t, time.Hour, runs, tasks)

	// Act
	runsFailed, tasksRequeued, err := runner.Sweep(context.Background())

	// Assert — requeueing a task whose run is still reading as in flight is the state the
	// order exists to avoid, so it must not happen on the way out of a failure either.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the janitor's failure", err)
	}
	if !strings.Contains(err.Error(), "fail abandoned runs") {
		t.Errorf("err = %v, want it to name what failed", err)
	}
	if strings.Join(trace.calls, ",") != "runs" {
		t.Errorf("calls = %v, want the task janitor left alone", trace.calls)
	}
	if runsFailed != 0 || tasksRequeued != 0 {
		t.Errorf("swept %d runs and %d tasks, want nothing counted", runsFailed, tasksRequeued)
	}
}

func TestRunnerSweep_aTaskFailureStillReportsTheRunsItClosed(t *testing.T) {
	// Arrange
	trace := &sweepTrace{}
	runs := &recordingRunJanitor{trace: trace, failed: 2}
	tasks := &recordingTaskJanitor{trace: trace, err: errBoom}
	runner := newSweepFixture(t, time.Hour, runs, tasks)

	// Act
	runsFailed, tasksRequeued, err := runner.Sweep(context.Background())

	// Assert — the runs are already closed. Reporting the whole sweep as failed would say
	// nothing was done.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the janitor's failure", err)
	}
	if !strings.Contains(err.Error(), "requeue stale tasks") {
		t.Errorf("err = %v, want it to name what failed", err)
	}
	if runsFailed != 2 {
		t.Errorf("runsFailed = %d, want the 2 that were closed before the failure", runsFailed)
	}
	if tasksRequeued != 0 {
		t.Errorf("tasksRequeued = %d, want 0", tasksRequeued)
	}
}

func TestRunner_defaultsTheClockWhenNoneIsGiven(t *testing.T) {
	// Arrange — a sweep window computed from the zero time would find every run stale and
	// requeue the whole queue.
	deps := newRunnerFakes().requiredDeps()
	deps.Clock = nil
	deps.StaleAfter = time.Hour
	janitor := &recordingRunJanitor{trace: &sweepTrace{}}
	deps.RunJanitor = janitor
	runner, err := NewRunner(deps)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// Act
	before := time.Now()
	if _, _, err := runner.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Assert
	if janitor.now.Before(before) {
		t.Errorf("now = %s, want a time at or after %s", janitor.now, before)
	}
}
