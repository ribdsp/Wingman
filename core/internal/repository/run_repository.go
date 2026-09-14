package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// RunRecord is a stored run and the one thing about it that is not a domain concern:
// whether somebody has asked it to stop.
type RunRecord struct {
	Run domain.Run
	// CancelRequestedAt is when a person asked for this run to end. The loop reads it
	// at the top of each iteration; it is a request rather than a state, so the run
	// stops at a step boundary with its transcript intact.
	CancelRequestedAt *time.Time
}

// Cancelled reports whether a cancellation has been requested.
func (r RunRecord) Cancelled() bool { return r.CancelRequestedAt != nil }

// RunRepository stores runs and their transcripts.
type RunRepository struct {
	db *sqlx.DB
}

// NewRunRepository builds a repository over the given pool.
func NewRunRepository(db *sqlx.DB) *RunRepository {
	return &RunRepository{db: db}
}

const runColumns = `id, task_id, owner_user_id, provider, model,
	max_iterations, max_tool_calls, max_tokens_per_run, max_tokens_per_user_day,
	step_timeout_seconds, sandbox_timeout_seconds,
	iterations, tool_calls, tokens_used,
	stop, reason, cancel_requested_at, started_at, finished_at`

type runRow struct {
	ID                  string         `db:"id"`
	TaskID              string         `db:"task_id"`
	OwnerUserID         string         `db:"owner_user_id"`
	Provider            string         `db:"provider"`
	Model               string         `db:"model"`
	MaxIterations       int            `db:"max_iterations"`
	MaxToolCalls        int            `db:"max_tool_calls"`
	MaxTokensPerRun     int64          `db:"max_tokens_per_run"`
	MaxTokensPerUserDay int64          `db:"max_tokens_per_user_day"`
	StepTimeoutSeconds  int            `db:"step_timeout_seconds"`
	SandboxTimeoutSecs  int            `db:"sandbox_timeout_seconds"`
	Iterations          int            `db:"iterations"`
	ToolCalls           int            `db:"tool_calls"`
	TokensUsed          int64          `db:"tokens_used"`
	Stop                sql.NullString `db:"stop"`
	Reason              string         `db:"reason"`
	CancelRequestedAt   sql.NullTime   `db:"cancel_requested_at"`
	StartedAt           time.Time      `db:"started_at"`
	FinishedAt          sql.NullTime   `db:"finished_at"`
}

func (r runRow) toRecord() RunRecord {
	return RunRecord{
		Run: domain.Run{
			ID:          r.ID,
			TaskID:      r.TaskID,
			OwnerUserID: r.OwnerUserID,
			Provider:    r.Provider,
			Model:       r.Model,
			Limits: domain.RunLimits{
				MaxIterations:       r.MaxIterations,
				MaxToolCalls:        r.MaxToolCalls,
				MaxTokensPerRun:     r.MaxTokensPerRun,
				MaxTokensPerUserDay: r.MaxTokensPerUserDay,
				StepTimeout:         time.Duration(r.StepTimeoutSeconds) * time.Second,
				SandboxTimeout:      time.Duration(r.SandboxTimeoutSecs) * time.Second,
			},
			State: domain.RunState{
				Iterations: r.Iterations,
				ToolCalls:  r.ToolCalls,
				TokensUsed: r.TokensUsed,
			},
			// An empty StopReason is how domain.Run spells "in flight"; the column is
			// NULL. Neither side gets to see the other's spelling.
			Stop:       domain.StopReason(r.Stop.String),
			Reason:     r.Reason,
			StartedAt:  r.StartedAt,
			FinishedAt: timePtr(r.FinishedAt),
		},
		CancelRequestedAt: timePtr(r.CancelRequestedAt),
	}
}

// Start records a run and the limits it is actually running under.
//
// The limits are copied into the row rather than read from configuration later. A run
// judged against today's caps would be a run judged against a bound it never had, and
// "why did this stop at 40 iterations when the limit is 60" has to be answerable a
// month afterwards.
func (r *RunRepository) Start(ctx context.Context, run domain.Run) (RunRecord, error) {
	const query = `
		INSERT INTO runs (
			task_id, owner_user_id, provider, model,
			max_iterations, max_tool_calls, max_tokens_per_run, max_tokens_per_user_day,
			step_timeout_seconds, sandbox_timeout_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING ` + runColumns

	if err := run.Limits.Validate(); err != nil {
		return RunRecord{}, fmt.Errorf("run limits are invalid: %w", err)
	}
	// Defaults are applied after validation and before the write, in that order.
	// Validate treats a zero as "not set"; the loop treats one as "no limit"; and the
	// schema refuses one outright. Filling them here is what keeps a caller that
	// forgot from being the caller that gets an unbounded run.
	limits := run.Limits.WithDefaults()

	var row runRow
	err := r.db.QueryRowxContext(ctx, query,
		run.TaskID, run.OwnerUserID, run.Provider, run.Model,
		limits.MaxIterations, limits.MaxToolCalls,
		limits.MaxTokensPerRun, limits.MaxTokensPerUserDay,
		int(limits.StepTimeout.Seconds()), int(limits.SandboxTimeout.Seconds()),
	).StructScan(&row)
	if err != nil {
		return RunRecord{}, fmt.Errorf("start run for task %s: %w", run.TaskID, classify(err))
	}
	return row.toRecord(), nil
}

// Get returns any run, for the worker that is executing it.
func (r *RunRepository) Get(ctx context.Context, runID string) (RunRecord, error) {
	const query = `SELECT ` + runColumns + ` FROM runs WHERE id = $1`

	var row runRow
	if err := r.db.QueryRowxContext(ctx, query, runID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunRecord{}, ErrNotFound
		}
		return RunRecord{}, fmt.Errorf("get run %s: %w", runID, classify(err))
	}
	return row.toRecord(), nil
}

// GetForUser returns one of a user's runs, or ErrNotFound.
func (r *RunRepository) GetForUser(ctx context.Context, userID, runID string) (RunRecord, error) {
	const query = `SELECT ` + runColumns + ` FROM runs WHERE id = $2 AND owner_user_id = $1`

	var row runRow
	if err := r.db.QueryRowxContext(ctx, query, userID, runID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunRecord{}, ErrNotFound
		}
		return RunRecord{}, fmt.Errorf("get run %s: %w", runID, classify(err))
	}
	return row.toRecord(), nil
}

// ListForTask returns a task's runs, newest first. A task can be run more than once, so
// this is what shows a retry after an outage as a second run rather than an edit of the
// first.
func (r *RunRepository) ListForTask(ctx context.Context, taskID string, limit, offset int) ([]RunRecord, error) {
	const query = `
		SELECT ` + runColumns + `
		FROM runs
		WHERE task_id = $1
		ORDER BY started_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	limit, offset = normalisePage(limit, offset)
	rows := []runRow{}
	if err := r.db.SelectContext(ctx, &rows, query, taskID, limit, offset); err != nil {
		return nil, fmt.Errorf("list runs for task %s: %w", taskID, classify(err))
	}

	records := make([]RunRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.toRecord())
	}
	return records, nil
}

// AppendStep records one step and advances the run's counters in a single transaction.
//
// The two writes are together because the counters are what the caps are checked
// against. If a step could be recorded without its cost, a crash between the two would
// leave a run whose transcript shows work its counters do not, and the next iteration
// would decide it had budget it had already spent.
func (r *RunRepository) AppendStep(ctx context.Context, step domain.Step) error {
	const insertStep = `
		INSERT INTO run_steps (run_id, idx, kind, tool_name, tokens_in, tokens_out, content, err, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	// The counters are incremented in SQL rather than written from a value read
	// earlier: two workers cannot both be stepping one run, but a retry can, and
	// += in the database cannot lose an increment the way read-modify-write can.
	const bumpCounters = `
		UPDATE runs
		SET iterations = iterations + $2,
			tool_calls = tool_calls + $3,
			tokens_used = tokens_used + $4
		WHERE id = $1`

	if step.Index < 1 {
		// The schema refuses this too. Failing here says which caller did it.
		return fmt.Errorf("step index must start at 1, got %d", step.Index)
	}

	var iterationDelta, toolCallDelta int
	switch step.Kind {
	case domain.StepKindModel:
		iterationDelta = 1
	case domain.StepKindTool:
		toolCallDelta = 1
		if step.Tokens() != 0 {
			// A sandbox does not bill tokens. Charging a tool step for the model call
			// that requested it would double-count against the run's allowance.
			return fmt.Errorf("tool step %d bills %d tokens; tool steps are free", step.Index, step.Tokens())
		}
	default:
		return fmt.Errorf("step kind %q is neither model nor tool", step.Kind)
	}

	at := step.At
	if at.IsZero() {
		return fmt.Errorf("step %d has no timestamp", step.Index)
	}

	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin step transaction for run %s: %w", step.RunID, classify(err))
	}
	// Rollback after a commit is a no-op, so this covers every early return without
	// the commit path needing to remember anything.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, insertStep,
		step.RunID, step.Index, string(step.Kind), step.ToolName,
		step.TokensIn, step.TokensOut, domain.TruncateContent(step.Content), step.Err, at,
	); err != nil {
		return fmt.Errorf("record step %d of run %s: %w", step.Index, step.RunID, classify(err))
	}

	result, err := tx.ExecContext(ctx, bumpCounters,
		step.RunID, iterationDelta, toolCallDelta, step.Tokens())
	if err != nil {
		return fmt.Errorf("advance counters for run %s: %w", step.RunID, classify(err))
	}
	if err := requireOneRow(result, step.RunID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit step %d of run %s: %w", step.Index, step.RunID, classify(err))
	}
	return nil
}

// Steps returns a run's transcript in order.
func (r *RunRepository) Steps(ctx context.Context, runID string, limit, offset int) ([]domain.Step, error) {
	const query = `
		SELECT run_id, idx, kind, tool_name, tokens_in, tokens_out, content, err, at
		FROM run_steps
		WHERE run_id = $1
		ORDER BY idx ASC
		LIMIT $2 OFFSET $3`

	limit, offset = normalisePage(limit, offset)
	rows := []stepRow{}
	if err := r.db.SelectContext(ctx, &rows, query, runID, limit, offset); err != nil {
		return nil, fmt.Errorf("read transcript of run %s: %w", runID, classify(err))
	}

	steps := make([]domain.Step, 0, len(rows))
	for _, row := range rows {
		steps = append(steps, row.toStep())
	}
	return steps, nil
}

// NextStepIndex returns the index the next step should carry.
//
// The transcript numbers itself from 1 and the schema keeps (run_id, idx) unique, so a
// worker that restarts mid-run continues the numbering instead of colliding with the
// step it had already written.
func (r *RunRepository) NextStepIndex(ctx context.Context, runID string) (int, error) {
	const query = `SELECT coalesce(max(idx), 0) + 1 FROM run_steps WHERE run_id = $1`

	var next int
	if err := r.db.QueryRowContext(ctx, query, runID).Scan(&next); err != nil {
		return 0, fmt.Errorf("read next step index for run %s: %w", runID, classify(err))
	}
	return next, nil
}

// Finish records why a run ended.
//
// The stop reason and the finish time are written together because the schema requires
// them to agree: a row with one and not the other reads as a run still going, which is
// exactly the state a crashed worker would otherwise leave behind.
func (r *RunRepository) Finish(ctx context.Context, runID string, stop domain.StopReason, reason string, at time.Time) error {
	const query = `
		UPDATE runs
		SET stop = $2, reason = $3, finished_at = $4
		WHERE id = $1 AND stop IS NULL`

	if stop == "" {
		return errors.New("run: a finished run needs a stop reason")
	}
	if at.IsZero() {
		return errors.New("run: a finished run needs a finish time")
	}

	result, err := r.db.ExecContext(ctx, query, runID, string(stop), reason, at)
	if err != nil {
		return fmt.Errorf("finish run %s: %w", runID, classify(err))
	}
	// No rows means the run is already finished, and overwriting the first reason with
	// a second would destroy the record of why it actually stopped.
	return requireOneRow(result, runID)
}

// RequestCancel asks one of a user's runs to stop.
//
// It records a request and returns; it does not kill anything. The loop stops at its
// next step boundary, which is what leaves a readable transcript and a run whose
// counters match what it actually spent.
func (r *RunRepository) RequestCancel(ctx context.Context, userID, runID string, at time.Time) error {
	const query = `
		UPDATE runs SET cancel_requested_at = $3
		WHERE id = $2 AND owner_user_id = $1
			AND stop IS NULL AND cancel_requested_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, userID, runID, at)
	if err != nil {
		return fmt.Errorf("request cancel of run %s: %w", runID, classify(err))
	}
	return requireOneRow(result, runID)
}

// FailAbandoned finishes runs left in flight by a worker that is no longer there.
//
// A run stuck with no stop reason is a run that reads as still working, forever: it
// holds a place in the in-flight index, and it tells a user their work is in progress
// when nothing is executing it. The reason is StopAbandoned rather than
// StopProviderError because a crash in this process and a failure at the vendor send an
// operator to different logs.
func (r *RunRepository) FailAbandoned(ctx context.Context, startedBefore, at time.Time) (int, error) {
	const query = `
		UPDATE runs
		SET stop = $1, reason = 'the worker running this run stopped before it finished',
			finished_at = $2
		WHERE stop IS NULL AND started_at < $3`

	result, err := r.db.ExecContext(ctx, query, string(domain.StopAbandoned), at, startedBefore)
	if err != nil {
		return 0, fmt.Errorf("fail abandoned runs: %w", classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count abandoned runs: %w", err)
	}
	return int(affected), nil
}

type stepRow struct {
	RunID     string    `db:"run_id"`
	Index     int       `db:"idx"`
	Kind      string    `db:"kind"`
	ToolName  string    `db:"tool_name"`
	TokensIn  int64     `db:"tokens_in"`
	TokensOut int64     `db:"tokens_out"`
	Content   string    `db:"content"`
	Err       string    `db:"err"`
	At        time.Time `db:"at"`
}

func (r stepRow) toStep() domain.Step {
	return domain.Step{
		RunID:     r.RunID,
		Index:     r.Index,
		Kind:      domain.StepKind(r.Kind),
		ToolName:  r.ToolName,
		TokensIn:  r.TokensIn,
		TokensOut: r.TokensOut,
		Content:   r.Content,
		Err:       r.Err,
		At:        r.At,
	}
}
