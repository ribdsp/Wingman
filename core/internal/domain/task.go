package domain

import (
	"fmt"
	"strings"
	"time"
)

// Bounds on the free text and metadata a task carries.
const (
	// MaxBriefLength bounds the instruction a run is given. A brief is an
	// instruction, not a document — anything longer belongs in a file the sandbox
	// can read, where it costs one tool call instead of every prompt.
	MaxBriefLength = 16000
	// MaxIdempotencyKeyLength bounds the key a caller retries under.
	MaxIdempotencyKeyLength = 200
	// MaxMetadataEntries and MaxMetadataValueLength bound the goal engine's
	// metadata map, which exists to correlate a task with the goal that caused it
	// and not to carry state between services.
	MaxMetadataEntries     = 32
	MaxMetadataValueLength = 512
	// MaxStepContentLength bounds one recorded step. A tool that prints a
	// megabyte of log gets truncated rather than turning the transcript into the
	// largest table in the database.
	MaxStepContentLength = 32000
)

// TaskSource says what asked for the work. It decides nothing on its own, but it
// is the field an operator reads first when a run surprises them: work nobody
// requested and work somebody typed deserve different suspicion.
type TaskSource string

const (
	// TaskSourceGoalEngine is a goal that fell behind pace. Nobody was watching.
	TaskSourceGoalEngine TaskSource = "goal_engine"
	// TaskSourceUser is a person asking directly, in a chat.
	TaskSourceUser TaskSource = "user"
	// TaskSourceChannel is a message arriving on a connected channel.
	TaskSourceChannel TaskSource = "channel"
)

// TaskStatus is the lifecycle of one requested unit of work.
type TaskStatus string

const (
	TaskStatusQueued    TaskStatus = "queued"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
)

// TaskStatusFor maps a finished run's stop reason onto its task's status.
//
// It exists so the mapping lives in one place. There is one reason that means the
// work got done and eight that mean it did not, and a caller polling for an answer
// needs that collapsed into a single field — but collapsing it at each call site
// is how "halted" eventually gets filed as a success somewhere.
func TaskStatusFor(reason StopReason) TaskStatus {
	if reason.Succeeded() {
		return TaskStatusSucceeded
	}
	return TaskStatusFailed
}

// Task is one unit of work Wingman was asked to do.
type Task struct {
	ID string
	// OwnerUserID is the account the work is done for and billed to. Every task
	// has one, including a task the goal engine dispatched — unattended work is
	// still somebody's, and a run whose tokens belong to nobody cannot be capped.
	OwnerUserID string
	Source      TaskSource
	// BotID and ChannelID come straight from the goal engine's dispatch payload:
	// which agent persona to run as, and where to report.
	BotID     string
	ChannelID string
	Brief     string
	// IdempotencyKey makes a retry safe. The same key returns the original task
	// rather than starting a second one, which matters because the caller
	// retrying is a monitor that already believes it may have failed.
	IdempotencyKey string
	Metadata       map[string]string
	Status         TaskStatus
	CreatedAt      time.Time
}

// Validate reports every structural problem with a task.
func (t Task) Validate() error {
	var errs ValidationErrors
	add := func(field, message string) {
		errs = append(errs, ValidationError{Field: field, Message: message})
	}

	if strings.TrimSpace(t.OwnerUserID) == "" {
		add("ownerUserId", "is required")
	}

	switch brief := strings.TrimSpace(t.Brief); {
	case brief == "":
		add("brief", "is required")
	case len([]rune(brief)) > MaxBriefLength:
		add("brief", fmt.Sprintf("must be at most %d characters", MaxBriefLength))
	}

	switch t.Source {
	case TaskSourceGoalEngine, TaskSourceUser, TaskSourceChannel:
	case "":
		add("source", "is required")
	default:
		add("source", fmt.Sprintf("%q is not a task source", t.Source))
	}

	switch t.Status {
	case TaskStatusQueued, TaskStatusRunning, TaskStatusSucceeded, TaskStatusFailed:
	case "":
		add("status", "is required")
	default:
		add("status", fmt.Sprintf("%q is not a task status", t.Status))
	}

	if len([]rune(t.IdempotencyKey)) > MaxIdempotencyKeyLength {
		add("idempotencyKey", fmt.Sprintf("must be at most %d characters", MaxIdempotencyKeyLength))
	}

	if len(t.Metadata) > MaxMetadataEntries {
		add("metadata", fmt.Sprintf("must have at most %d entries", MaxMetadataEntries))
	}
	for key, value := range t.Metadata {
		if len([]rune(value)) > MaxMetadataValueLength {
			add("metadata", fmt.Sprintf("value for %q must be at most %d characters", key, MaxMetadataValueLength))
			break
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// WithDefaults returns a copy with its text trimmed and its status filled in.
func (t Task) WithDefaults() Task {
	out := t
	out.OwnerUserID = strings.TrimSpace(out.OwnerUserID)
	out.BotID = strings.TrimSpace(out.BotID)
	out.ChannelID = strings.TrimSpace(out.ChannelID)
	out.Brief = strings.TrimSpace(out.Brief)
	out.IdempotencyKey = strings.TrimSpace(out.IdempotencyKey)
	if out.Status == "" {
		out.Status = TaskStatusQueued
	}
	return out
}

// Run is one execution of a task. A task can be run more than once — a retry
// after a provider outage is a second run, not an edit of the first — so the
// transcript, the spend and the stop reason all belong here rather than on Task.
type Run struct {
	ID          string
	TaskID      string
	OwnerUserID string
	// Provider and Model record what actually answered, not what was asked for.
	// A run that silently fell back to a cheaper model is a run whose output has
	// to be read differently.
	Provider string
	Model    string
	Limits   RunLimits
	State    RunState
	// Stop is empty while the run is in flight.
	Stop      StopReason
	Reason    string
	StartedAt time.Time
	// FinishedAt is nil while the run is in flight.
	FinishedAt *time.Time
}

// InFlight reports whether the run is still going.
func (r Run) InFlight() bool { return r.Stop == "" }

// StepKind separates thinking from acting. The distinction is what makes
// "reached the iteration cap having done nothing" a different story from
// "reached the tool-call cap".
type StepKind string

const (
	StepKindModel StepKind = "model"
	StepKindTool  StepKind = "tool"
)

// Step is one recorded entry in a run's transcript.
type Step struct {
	RunID string
	// Index orders the transcript. It is assigned by the caller and starts at 1,
	// so a gap is visible rather than being read as the beginning.
	Index    int
	Kind     StepKind
	ToolName string
	// TokensIn and TokensOut are what this step cost. Both are zero on a tool
	// step: a sandbox does not bill tokens.
	TokensIn  int64
	TokensOut int64
	// Content is the recorded body — the model's text, or the tool's output,
	// truncated to MaxStepContentLength.
	Content string
	// Err carries a short message when the step failed. It is written for a human
	// reading the transcript, so it must never contain a provider payload, an API
	// key, a DSN or a header.
	Err string
	At  time.Time
}

// Tokens is what the step cost in total.
func (s Step) Tokens() int64 { return s.TokensIn + s.TokensOut }

// TruncateContent returns content bounded to MaxStepContentLength, marked so a
// reader knows the transcript is not the whole output.
//
// Truncation is counted in runes rather than bytes: cutting a UTF-8 sequence in
// half produces a column of replacement characters in every dashboard downstream.
func TruncateContent(content string) string {
	runes := []rune(content)
	if len(runes) <= MaxStepContentLength {
		return content
	}
	return string(runes[:MaxStepContentLength]) + "\n… truncated"
}
