// Package agent runs the loop: ask the model, run what it asked for, ask again.
//
// Everything that bounds a run lives outside this package. domain.Decide owns the
// order the stop conditions are checked in, domain.ClassifyTool owns whether a tool
// call is permitted, and the goal engine owns whether money may move. What is here is
// the sequencing — when each of those is asked, and what is written down afterwards.
//
// Three properties are worth stating because a later change could quietly drop any of
// them:
//
//   - The ladder is consulted before every iteration, not once. A kill switch engaged
//     while a run is on its fourth tool call has to stop that run, and the only way to
//     notice is to look again.
//   - The tool set is taken once, at the start. See tool.Registry.Offer: a run is
//     judged against the set it was planned with, so a server that gains a capability
//     mid-run does not hand it to a run already under way.
//   - Exactly one stop reason is recorded per run, by the single Finish write that
//     ends it. A run with two reasons is a run whose transcript cannot be read.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

const (
	// maxStepAttempts bounds how often one model turn is retried after a retryable
	// provider error. The provider package's own doc explains why the retry lives
	// here: both SDKs already retry inside a single call, and the decision to try a
	// step again belongs to the loop that knows what the run has spent. A retried
	// attempt costs no iteration, so this bound is what stops a flapping provider
	// consuming a run's wall clock without consuming its allowance.
	maxStepAttempts = 3

	// defaultRetryPause is how long the loop waits before trying a failed turn
	// again. Short, because the SDK has already backed off underneath us; the pause
	// exists so a provider returning 429 in microseconds cannot spin.
	defaultRetryPause = 2 * time.Second
)

// Config is what the loop needs. Every field is required except Gate, Logger, Now and
// RetryPause; New says what a missing one means.
type Config struct {
	Provider   Provider
	Model      string
	Transcript Transcript
	Runs       Runs
	Budget     Budget
	Halt       Halt
	// Gate is the goal engine's spending ladder. A nil Gate means no spending call
	// can be made — refused, never waved through. An instance with no goal engine
	// configured is an instance where nothing may spend money, which is the correct
	// reading of "the thing that decides is not there".
	Gate SpendGate
	// Grants is the operator's declared tool list. The offering already excludes
	// anything ungranted, but the class is read per call: it decides whether a write
	// needs a human, and that answer depends on the run, not on the tool.
	Grants *tool.Grants
	Logger zerolog.Logger
	// Now and RetryPause exist for tests. The loop has a clock — only domain is
	// forbidden one — and this is where it comes from.
	Now        func() time.Time
	RetryPause time.Duration
}

// Loop runs tasks. It holds no per-run state; everything a run needs is in Input.
type Loop struct {
	cfg Config
}

// New checks the wiring at construction rather than on the first task.
//
// A worker that starts and then fails every run is harder to diagnose than a process
// that refuses to start, and the wiring cannot change afterwards.
func New(cfg Config) (*Loop, error) {
	var missing []string
	if cfg.Provider == nil {
		missing = append(missing, "provider")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		missing = append(missing, "model")
	}
	if cfg.Transcript == nil {
		missing = append(missing, "transcript")
	}
	if cfg.Runs == nil {
		missing = append(missing, "runs")
	}
	if cfg.Budget == nil {
		missing = append(missing, "budget")
	}
	if cfg.Halt == nil {
		// Not optional, and not defaulted to "nothing is halted". The kill switch is
		// the one control that stops everything at once; a loop built without it
		// would look like it had one.
		missing = append(missing, "kill switch")
	}
	if cfg.Grants == nil {
		missing = append(missing, "tool grants")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("agent loop: not configured: %s", strings.Join(missing, ", "))
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryPause <= 0 {
		cfg.RetryPause = defaultRetryPause
	}
	return &Loop{cfg: cfg}, nil
}

// Input is one unit of work: a run the caller has already started, and the task that
// caused it.
type Input struct {
	// Run must be in flight and carry validated limits. repository.RunRepository.Start
	// fills the defaults in; a run that reached here without passing through it would
	// have zero limits, and domain.Decide reads a zero as a stop.
	Run  domain.Run
	Task domain.Task
	// Workspace is this user's sandbox directory. Tools that touch the filesystem
	// receive it; nothing here creates or checks it, because internal/sandbox does
	// and does it against its own root.
	Workspace string
	// Tools is the run's snapshot, from tool.Registry.Offer.
	Tools Tools
}

// Outcome is how the run ended. It is what was written to the run row, returned so a
// caller can log or report it without reading the row back.
type Outcome struct {
	Stop   domain.StopReason
	Reason string
	// Answer is the last thing the model said, so a caller can put it in the chat the
	// task came from without reading the transcript back.
	//
	// It is empty when the run stopped before the model said anything — halted on the
	// kill switch, refused by the ladder, cancelled before the first turn. That
	// emptiness is meaningful: a caller must not post it as an answer, because a blank
	// reply reads as the assistant having nothing to say rather than as work that never
	// started.
	Answer string
}

// Run drives one task to a stop reason.
//
// The error result is about the loop failing, not about the run failing: a run that
// hits a cap, is denied a tool or is halted has ended correctly and returns a nil
// error with the reason in Outcome. An error here means the transcript or the run row
// could not be written, which is the one failure the loop cannot record.
func (l *Loop) Run(ctx context.Context, in Input) (Outcome, error) {
	if err := validateInput(in); err != nil {
		return Outcome{}, err
	}

	log := l.cfg.Logger.With().
		Str("runId", in.Run.ID).
		Str("taskId", in.Run.TaskID).
		Str("source", string(in.Task.Source)).
		Logger()

	messages := openingMessages(in.Task)
	state := in.Run.State
	// After the first turn these carry what that turn concluded into the next
	// consultation of the ladder. They are the only mutable signals between
	// iterations; everything else is read fresh.
	var finished, providerFailed bool
	// note is what the turn knows and the ladder does not: which provider failed and
	// how, or that the model declined and in what words.
	var note string

	for {
		engaged, cancelled, ledger := l.gather(ctx, in, log)

		decision := domain.Decide(domain.ContinuationInput{
			Limits:            in.Run.Limits,
			State:             state,
			Ledger:            ledger,
			KillSwitchEngaged: engaged,
			Cancelled:         cancelled,
			ProviderFailed:    providerFailed,
			ModelFinished:     finished,
		}, l.cfg.Now())

		if !decision.Continue {
			reason := decision.Reason
			if note != "" {
				// The ladder names the branch; only the loop knows the particulars.
				// "provider_error" on its own tells an operator nothing about which
				// provider or why, and "completed" does not say the model declined.
				reason = decision.Reason + ": " + note
			}
			return l.finish(ctx, in, state, decision.Stop, reason, messages, log)
		}

		turn, err := l.iterate(ctx, in, &messages, &state, decision.RemainingTokens, log)
		if err != nil {
			return Outcome{}, err
		}
		if turn.stop != "" {
			// A tool the run may not call. Not a ladder branch — the ladder bounds what
			// a run consumes, and this is about what it is permitted to do.
			return l.finish(ctx, in, state, turn.stop, turn.reason, messages, log)
		}
		finished, providerFailed, note = turn.finished, turn.providerFailed, turn.reason
	}
}

// gather reads the three facts that come from outside the run.
//
// None of them can fail into a permissive answer, so none of them returns an error:
// an unreadable kill switch is engaged, an unreadable cancellation flag is a
// cancellation, and an unreadable ledger is domain.Ledger{Readable: false}, which the
// ladder stops on. Every one is logged, because a run that stopped for a reason the
// operator has to fix should say which one.
func (l *Loop) gather(ctx context.Context, in Input, log zerolog.Logger) (engaged, cancelled bool, ledger domain.Ledger) {
	engaged, err := l.cfg.Halt.Engaged(ctx)
	if err != nil {
		// The same rule the goal engine applies to its own flag: unreadable means
		// halt. A switch nobody can read is not a switch that is off.
		engaged = true
		log.Error().Err(err).Msg("the kill switch could not be read: treating it as engaged")
	}

	cancelled, err = l.cfg.Runs.CancelRequested(ctx, in.Run.ID)
	if err != nil {
		// Stopping a run whose cancellation flag is unreadable costs the work done so
		// far. Continuing one somebody asked to stop costs whatever it does next.
		cancelled = true
		log.Error().Err(err).Msg("the cancellation flag could not be read: stopping the run")
	}

	ledger, err = l.cfg.Budget.Ledger(ctx, in.Run.OwnerUserID, l.cfg.Now())
	if err != nil {
		log.Error().Err(err).Msg("the spend ledger could not be read: the run will stop")
	}
	return engaged, cancelled, ledger
}

// finish writes the one row that ends a run.
func (l *Loop) finish(ctx context.Context, in Input, state domain.RunState, stop domain.StopReason, reason string, messages []provider.Message, log zerolog.Logger) (Outcome, error) {
	at := l.cfg.Now()
	if err := l.cfg.Runs.Finish(ctx, in.Run.ID, stop, reason, at); err != nil {
		return Outcome{}, fmt.Errorf("finish run %s as %s: %w", in.Run.ID, stop, err)
	}

	event := log.Info()
	if !stop.Succeeded() {
		event = log.Warn()
	}
	// The reason is not logged. It is on the run row, and it can quote a tool name, a
	// gate's sentence or a provider's message; the row is read by somebody who already
	// has the run, while a log line goes wherever logs go.
	event.Str("stop", string(stop)).
		Int("iterations", state.Iterations).
		Int("toolCalls", state.ToolCalls).
		Int64("tokens", state.TokensUsed).
		Msg("run finished")

	return Outcome{Stop: stop, Reason: reason, Answer: lastAssistantText(messages)}, nil
}

// lastAssistantText is the final thing the model said, or "" if it never said anything.
//
// Taken from the conversation rather than from the transcript because the conversation is
// already in hand: a run that stopped on the ladder has no turn to report, and one that
// answered has its answer as the last assistant message. Scanned backwards for a
// non-empty one, so a closing turn that only asked for a tool call does not blank out the
// sentence before it.
func lastAssistantText(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != provider.RoleAssistant {
			continue
		}
		if text := strings.TrimSpace(messages[i].Text); text != "" {
			return text
		}
	}
	return ""
}

func validateInput(in Input) error {
	var problems []string
	if strings.TrimSpace(in.Run.ID) == "" {
		problems = append(problems, "the run has no id")
	}
	if strings.TrimSpace(in.Run.OwnerUserID) == "" {
		// Every budget question and every workspace is per user. A run with no owner
		// would be measured against nobody's allowance.
		problems = append(problems, "the run has no owner")
	}
	if !in.Run.InFlight() {
		problems = append(problems, "the run has already stopped as "+string(in.Run.Stop))
	}
	if strings.TrimSpace(in.Task.Brief) == "" {
		problems = append(problems, "the task has no brief")
	}
	if in.Tools == nil {
		// An offering with nothing in it is fine and common. A nil one is a wiring
		// mistake, and it would surface as a nil dereference on the first tool call.
		problems = append(problems, "no tool offering was given")
	}
	if len(problems) > 0 {
		return errors.New("agent loop: " + strings.Join(problems, "; "))
	}
	return nil
}

// record appends one step and mirrors the counters the ladder reads.
//
// AppendStep bumps those counters in SQL. They are mirrored here rather than read
// back because the in-memory copy is what domain.Decide is given, and a round trip
// per step to learn a number we just added would be a query for nothing.
func (l *Loop) record(ctx context.Context, in Input, state *domain.RunState, step domain.Step) error {
	index, err := l.cfg.Transcript.NextStepIndex(ctx, in.Run.ID)
	if err != nil {
		return fmt.Errorf("next step index for run %s: %w", in.Run.ID, err)
	}
	step.RunID = in.Run.ID
	step.Index = index
	if step.At.IsZero() {
		step.At = l.cfg.Now()
	}

	if err := l.cfg.Transcript.AppendStep(ctx, step); err != nil {
		return fmt.Errorf("record step %d of run %s: %w", index, in.Run.ID, err)
	}

	switch step.Kind {
	case domain.StepKindModel:
		state.Iterations++
	case domain.StepKindTool:
		state.ToolCalls++
	}
	state.TokensUsed += step.Tokens()
	return nil
}
