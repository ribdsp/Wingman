package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// RunnerDeps is everything the runner needs. Reporter and the two janitors are optional;
// everything else is required.
type RunnerDeps struct {
	Queue      TaskQueue
	Runs       RunStore
	Agent      AgentRunner
	Tools      ToolSource
	Workspaces Workspaces
	Messages   MessageStore
	Spend      SpendReader

	// Reporter files a finished run's cost with the goal engine. Nil when no goal engine
	// is configured, which is a supported way to run core.
	Reporter SpendReporter

	// Replier delivers a run's answer to the platform it was asked on. Nil when no channel
	// is configured, which is the ordinary way to run an instance reached only over HTTP.
	Replier ChannelReplier

	// TaskJanitor and RunJanitor are the crash sweeps. Nil disables them, which is what
	// a test wants and what a single-worker instance can live without.
	TaskJanitor TaskJanitor
	RunJanitor  RunJanitor

	// Provider and Model are what a run is started with. They are recorded on the run
	// row, so a run that used a different model from today's default stays readable.
	Provider string
	Model    string
	// Limits bounds every run this instance starts, already defaulted by config.
	Limits domain.RunLimits

	// StaleAfter is how long a run may be in flight before the sweep treats its worker
	// as gone. It has to be longer than a legitimate run: the cost of getting it wrong
	// is requeueing work that was still being done.
	StaleAfter time.Duration

	Clock  Clock
	Logger zerolog.Logger
}

// Minimum bound on the crash sweep. A short StaleAfter turns the sweep into a way to
// requeue live work, so a value below this is raised rather than honoured.
const minStaleAfter = 5 * time.Minute

// Runner is the worker end: it takes one queued task at a time and drives it to a stop
// reason.
//
// It is the only place a run is started, and it does the six things around the loop that
// the loop itself must not do — claim the work, resolve the workspace, take the tool
// snapshot, file the task's outcome, put the answer back in the chat, and report what it
// cost. The loop's job is to end with one stop reason; everything about what happens
// either side of it is here.
//
// No method takes an Actor. Nothing here is reachable from a request: cmd runs RunNext in
// a loop and Sweep on a ticker, so there is no caller whose privilege could be checked and
// none that could ask for somebody else's task.
type Runner struct {
	queue      TaskQueue
	runs       RunStore
	agent      AgentRunner
	tools      ToolSource
	workspaces Workspaces
	messages   MessageStore
	spend      SpendReader

	reporter    SpendReporter
	replier     ChannelReplier
	taskJanitor TaskJanitor
	runJanitor  RunJanitor

	provider   string
	model      string
	limits     domain.RunLimits
	staleAfter time.Duration

	clock Clock
	log   zerolog.Logger
}

// NewRunner validates its wiring and returns a ready worker.
func NewRunner(deps RunnerDeps) (*Runner, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Queue != nil, "Queue")
	require(deps.Runs != nil, "Runs")
	require(deps.Agent != nil, "Agent")
	require(deps.Tools != nil, "Tools")
	require(deps.Workspaces != nil, "Workspaces")
	require(deps.Messages != nil, "Messages")
	require(deps.Spend != nil, "Spend")
	require(strings.TrimSpace(deps.Provider) != "", "Provider")
	require(strings.TrimSpace(deps.Model) != "", "Model")
	if len(missing) > 0 {
		return nil, fmt.Errorf("runner: missing dependencies: %v", missing)
	}
	// Validated here rather than trusted, even though config already did it: a zero
	// limit reads as "no limit" to the loop, and this is the last place before a run row
	// is written with one.
	if err := deps.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("runner: limits are invalid: %w", err)
	}

	r := &Runner{
		queue:       deps.Queue,
		runs:        deps.Runs,
		agent:       deps.Agent,
		tools:       deps.Tools,
		workspaces:  deps.Workspaces,
		messages:    deps.Messages,
		spend:       deps.Spend,
		reporter:    deps.Reporter,
		replier:     deps.Replier,
		taskJanitor: deps.TaskJanitor,
		runJanitor:  deps.RunJanitor,
		provider:    strings.TrimSpace(deps.Provider),
		model:       strings.TrimSpace(deps.Model),
		limits:      deps.Limits.WithDefaults(),
		staleAfter:  deps.StaleAfter,
		clock:       deps.Clock,
		log:         deps.Logger,
	}
	if r.clock == nil {
		r.clock = time.Now
	}
	if r.staleAfter < minStaleAfter {
		r.staleAfter = minStaleAfter
	}
	return r, nil
}

// RunNext takes one queued task and runs it, reporting whether there was one.
//
// A false with no error means the queue was empty, which is the normal case and not
// something to log. The caller sleeps and asks again.
//
// The error result is about this process failing, not about the run failing. A run that
// hit a cap, was denied a tool or was halted returns true and nil: it ended correctly, and
// its stop reason is on the run row.
func (r *Runner) RunNext(ctx context.Context) (bool, error) {
	task, chatID, err := r.queue.ClaimQueued(ctx)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("claim a queued task: %w", err)
	}

	log := r.log.With().Str("taskId", task.ID).Str("userId", task.OwnerUserID).Logger()

	// Everything that can fail before a run exists is done before one is started. A run
	// row written and then abandoned because a workspace could not be made would sit in
	// flight until the sweep closed it, and would tell the person their work was in
	// progress the whole time.
	workspace, err := r.workspaces.For(task.OwnerUserID)
	if err != nil {
		return true, r.abandon(ctx, task, chatID, log, "the workspace for this account could not be prepared", err)
	}
	offering, err := r.tools.Offer(ctx)
	if err != nil {
		return true, r.abandon(ctx, task, chatID, log, "the tool set for this run could not be assembled", err)
	}
	r.reportToolGaps(offering, log)

	record, err := r.runs.Start(ctx, domain.Run{
		TaskID:      task.ID,
		OwnerUserID: task.OwnerUserID,
		Provider:    r.provider,
		Model:       r.model,
		Limits:      r.limits,
	})
	if err != nil {
		return true, r.abandon(ctx, task, chatID, log, "the run could not be started", err)
	}
	log = log.With().Str("runId", record.Run.ID).Logger()

	outcome, err := r.agent.Run(ctx, agent.Input{
		Run:       record.Run,
		Task:      task,
		Workspace: workspace,
		Tools:     offering,
	})
	if err != nil {
		// The loop returns an error only when it could not write the transcript or the
		// run row — the one failure it cannot record itself. The run is left in flight on
		// purpose: the sweep will close it as abandoned, which is what it is, and
		// inventing a stop reason from out here would put a second author on that row.
		r.setStatus(ctx, task.ID, domain.TaskStatusFailed, log)
		return true, fmt.Errorf("run task %s: %w", task.ID, err)
	}

	r.setStatus(ctx, task.ID, domain.TaskStatusFor(outcome.Stop), log)
	r.deliver(ctx, task, chatID, record.Run.ID, outcome, log)
	r.report(ctx, record.Run.ID, outcome, log)
	return true, nil
}

// Sweep closes out work whose worker is gone, and reports how much it moved.
//
// The order matters. Runs are failed first and tasks requeued second, so a task picked up
// by another worker in between starts with no run of its own still reading as in flight.
// The reverse order can leave one task with two live runs, and a transcript that cannot be
// read as one story.
func (r *Runner) Sweep(ctx context.Context) (runsFailed, tasksRequeued int, err error) {
	now := r.clock()
	before := now.Add(-r.staleAfter)

	if r.runJanitor != nil {
		runsFailed, err = r.runJanitor.FailAbandoned(ctx, before, now)
		if err != nil {
			return 0, 0, fmt.Errorf("fail abandoned runs: %w", err)
		}
	}
	if r.taskJanitor != nil {
		tasksRequeued, err = r.taskJanitor.RequeueStale(ctx, before)
		if err != nil {
			// The runs are already closed. Reporting the whole sweep as failed would say
			// nothing was done, so the count that succeeded is returned with the error.
			return runsFailed, 0, fmt.Errorf("requeue stale tasks: %w", err)
		}
	}
	if runsFailed > 0 || tasksRequeued > 0 {
		r.log.Warn().Int("runsFailed", runsFailed).Int("tasksRequeued", tasksRequeued).
			Msg("swept work left behind by a worker that stopped")
	}
	return runsFailed, tasksRequeued, nil
}

// abandon files a task as failed when it could not be started at all.
//
// There is no run row, so there is no transcript: the task status and this log line are
// the whole record. The person waiting in the chat is told in the plainest terms available,
// because from their side a task that never started and one that failed look the same.
func (r *Runner) abandon(ctx context.Context, task domain.Task, chatID string, log zerolog.Logger, what string, cause error) error {
	log.Error().Err(cause).Msg(what)
	r.setStatus(ctx, task.ID, domain.TaskStatusFailed, log)

	if chatID != "" {
		notice := "This task could not be started: " + what + "."
		r.note(ctx, task, chatID, notice, log)
		r.replyOnChannel(ctx, task.OwnerUserID, chatID, notice, log)
	}
	return fmt.Errorf("%s: %w", what, cause)
}

// deliver puts the run's result back where somebody is waiting for it.
//
// Only when a chat is waiting: an unattended task dispatched by the goal engine has no
// conversation, and its record is the transcript. What lands is either the model's answer
// or, when there is none, a system note saying the run stopped — never an empty assistant
// message, which reads as the assistant having nothing to say.
//
// Then the same text goes to the platform the chat belongs to, if it belongs to one. Both
// happen: the chat is the record, and the channel is where the person actually is.
func (r *Runner) deliver(ctx context.Context, task domain.Task, chatID, runID string, outcome agent.Outcome, log zerolog.Logger) {
	if chatID == "" {
		return
	}

	text := strings.TrimSpace(outcome.Answer)
	if text != "" {
		if _, err := r.messages.AppendMessage(ctx, repository.NewMessage{
			ChatID:  chatID,
			UserID:  task.OwnerUserID,
			RunID:   runID,
			Role:    repository.MessageRoleAssistant,
			Content: text,
		}); err != nil {
			// The run happened and its transcript is written. Failing here loses the reply
			// from the conversation, which is bad enough to log loudly and not bad enough
			// to report the run as failed.
			log.Error().Err(err).Msg("the run's answer could not be added to the chat")
		}
	} else {
		text = stoppedNotice(outcome)
		r.note(ctx, task, chatID, text, log)
	}

	// Attempted even if the write above failed. Somebody asked on Telegram and is waiting
	// there; losing the answer in both places is worse than losing it in one.
	r.replyOnChannel(ctx, task.OwnerUserID, chatID, text, log)
}

// replyOnChannel sends the same text to the platform the chat came from, when it came from
// one.
//
// Logged and dropped on failure, for the same reason report is: the run has happened and its
// answer is in the chat, so a platform being unreachable must not turn a finished run into a
// failed one. Most chats are somebody using the web client, and for those the replier finds
// no target and asks no platform anything.
func (r *Runner) replyOnChannel(ctx context.Context, userID, chatID, text string, log zerolog.Logger) {
	if r.replier == nil {
		return
	}
	if err := r.replier.Reply(ctx, userID, chatID, text); err != nil {
		log.Error().Err(err).Msg("the run's answer could not be delivered to the channel it was asked on")
	}
}

// note appends a system message: something a person needs to read that the assistant did
// not say.
//
// It carries no run id, and cannot: messages_run_is_assistants in migration 000003 allows
// one only on an assistant row, because attributing anything else to a run would let a
// chat read as though the agent had said it. Nothing is lost — the chat's task already
// names the run, so an operator can get from this note to that run without it.
func (r *Runner) note(ctx context.Context, task domain.Task, chatID, content string, log zerolog.Logger) {
	if _, err := r.messages.AppendMessage(ctx, repository.NewMessage{
		ChatID:  chatID,
		UserID:  task.OwnerUserID,
		Role:    repository.MessageRoleSystem,
		Content: content,
	}); err != nil {
		log.Error().Err(err).Msg("a system note could not be added to the chat")
	}
}

// stoppedNotice is what a chat shows when a run ended without the model saying anything.
//
// It quotes the run's own reason, which can name a tool or a cap. That is the owner's own
// run, and a person told only "it stopped" has to ask an operator what happened — which is
// how a bounded, working system comes across as a broken one.
func stoppedNotice(outcome agent.Outcome) string {
	if outcome.Reason != "" {
		return "This run stopped before answering: " + outcome.Reason + "."
	}
	return "This run stopped before answering (" + string(outcome.Stop) + ")."
}

// report files what the run cost with the goal engine, so cost is a metric a goal can be
// written against.
//
// Read back from the ledger rather than taken from the outcome, because the ledger is what
// was actually charged. A failure is logged and dropped: the run has already happened, and
// losing the bookkeeping must not turn into losing the work.
func (r *Runner) report(ctx context.Context, runID string, outcome agent.Outcome, log zerolog.Logger) {
	if r.reporter == nil {
		return
	}

	charges, err := r.spend.ForRun(ctx, runID)
	if err != nil {
		log.Error().Err(err).Msg("the run's cost could not be read, so it was not reported")
		return
	}
	var tokens int64
	for _, charge := range charges {
		tokens += charge.Total()
	}
	if tokens == 0 {
		// A run that spent nothing — halted, cancelled before its first turn — is not a
		// reading. Pushing a zero would put a real data point of "no spend" into a metric
		// an operator writes a goal against.
		return
	}

	if err := r.reporter.ReportTokens(ctx, goalengine.TokenSample{
		Tokens:     tokens,
		ObservedAt: r.clock(),
		// The run id and the stop reason, and nothing else. The note is stored and
		// rendered back by the engine, so it must never carry a brief, a model's words or
		// anything from a tool.
		Note: fmt.Sprintf("core run %s (%s)", runID, outcome.Stop),
	}); err != nil {
		log.Error().Err(err).Msg("the run's cost could not be reported to the goal engine")
	}
}

// setStatus files a task's outcome, logging rather than returning a failure.
//
// The work is done by the time this is called. A task stuck at running when its run
// finished is what the sweep exists for, so the recovery is already in place and the
// caller has nothing useful to do with the error.
func (r *Runner) setStatus(ctx context.Context, taskID string, status domain.TaskStatus, log zerolog.Logger) {
	if err := r.queue.SetStatus(ctx, taskID, status); err != nil {
		log.Error().Err(err).Str("status", string(status)).Msg("a task's status could not be recorded")
	}
}

// reportToolGaps says what this run will be missing before it starts.
//
// The snapshot is taken once and the run is judged against it, so a server that was down
// at this moment is a server this run does not have — and the only place that is visible
// is here. A withdrawn name is logged at warn because two sources claiming one name is a
// configuration mistake somebody has to fix.
func (r *Runner) reportToolGaps(offering *tool.Offering, log zerolog.Logger) {
	if offering == nil {
		return
	}
	for _, failure := range offering.Unavailable() {
		log.Warn().Err(failure.Err).Str("source", failure.Label).
			Msg("a tool source could not be listed; this run will not be offered its tools")
	}
	for _, conflict := range offering.Conflicting() {
		log.Warn().Str("tool", conflict.Name).Strs("sources", conflict.Labels).
			Msg("a tool name was claimed by more than one source and withdrawn from this run")
	}
}
