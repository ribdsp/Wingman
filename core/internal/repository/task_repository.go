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

// TaskRepository stores the work core was asked to do.
//
// Two kinds of read live here and they are scoped differently on purpose. A person
// reading their own tasks passes their user id and every query names it. A run worker
// claiming queued work is instance-wide, because it acts for the operator who runs the
// box, and the task it claims carries the owner it will be billed to.
type TaskRepository struct {
	db *sqlx.DB
}

// NewTaskRepository builds a repository over the given pool.
func NewTaskRepository(db *sqlx.DB) *TaskRepository {
	return &TaskRepository{db: db}
}

const taskColumns = `id, owner_user_id, source, chat_id, bot_id, channel_id, brief,
	idempotency_key, metadata::text AS metadata, status, created_at`

type taskRow struct {
	ID             string         `db:"id"`
	OwnerUserID    string         `db:"owner_user_id"`
	Source         string         `db:"source"`
	ChatID         sql.NullString `db:"chat_id"`
	BotID          string         `db:"bot_id"`
	ChannelID      string         `db:"channel_id"`
	Brief          string         `db:"brief"`
	IdempotencyKey sql.NullString `db:"idempotency_key"`
	Metadata       string         `db:"metadata"`
	Status         string         `db:"status"`
	CreatedAt      time.Time      `db:"created_at"`
}

func (r taskRow) toTask() (domain.Task, string, error) {
	metadata, err := unmarshalMetadata(r.Metadata)
	if err != nil {
		return domain.Task{}, "", fmt.Errorf("task %s: %w", r.ID, err)
	}
	return domain.Task{
		ID:             r.ID,
		OwnerUserID:    r.OwnerUserID,
		Source:         domain.TaskSource(r.Source),
		BotID:          r.BotID,
		ChannelID:      r.ChannelID,
		Brief:          r.Brief,
		IdempotencyKey: r.IdempotencyKey.String,
		Metadata:       metadata,
		Status:         domain.TaskStatus(r.Status),
		CreatedAt:      r.CreatedAt,
	}, r.ChatID.String, nil
}

// NewTask is what Create needs. The chat id is separate from domain.Task because it is
// a delivery detail — where the answer goes — and not part of what the agent decides.
type NewTask struct {
	Task   domain.Task
	ChatID string
}

// Create records a task.
//
// A duplicate idempotency key returns ErrConflict, and that is the healthy outcome of a
// retry rather than a failure: the row already exists, so no second run will be
// started. The caller then reads the original with GetByIdempotencyKey.
//
// It is deliberately two steps rather than ON CONFLICT DO NOTHING. The API has to
// distinguish "I made this" from "this already existed" — 201 against 200 — and a
// single upsert that quietly returns the old row makes a retry indistinguishable from a
// fresh dispatch in the logs of both services.
func (r *TaskRepository) Create(ctx context.Context, input NewTask) (domain.Task, error) {
	const query = `
		INSERT INTO tasks (
			owner_user_id, source, chat_id, bot_id, channel_id, brief,
			idempotency_key, metadata, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
		RETURNING ` + taskColumns

	task := input.Task.WithDefaults()
	if err := task.Validate(); err != nil {
		return domain.Task{}, fmt.Errorf("task is invalid: %w", err)
	}

	metadata, err := marshalMetadata(task.Metadata)
	if err != nil {
		return domain.Task{}, err
	}

	var row taskRow
	err = r.db.QueryRowxContext(ctx, query,
		task.OwnerUserID, string(task.Source), nullIfEmpty(input.ChatID),
		task.BotID, task.ChannelID, task.Brief,
		nullIfEmpty(task.IdempotencyKey), metadata, string(task.Status),
	).StructScan(&row)
	if err != nil {
		return domain.Task{}, fmt.Errorf("create task for user %s: %w", task.OwnerUserID, classify(err))
	}

	created, _, err := row.toTask()
	return created, err
}

// GetByIdempotencyKey returns the task a key already created, or ErrNotFound.
//
// There is no user id in this query, and that is not an oversight: the key comes from
// the goal engine's own bridge, which acts for the operator, and the caller has to be
// able to find the task it created on a previous attempt. Handlers reach it through an
// operator- or bot-only route.
func (r *TaskRepository) GetByIdempotencyKey(ctx context.Context, key string) (domain.Task, error) {
	const query = `SELECT ` + taskColumns + ` FROM tasks WHERE idempotency_key = $1`

	if key == "" {
		return domain.Task{}, ErrNotFound
	}

	var row taskRow
	if err := r.db.QueryRowxContext(ctx, query, key).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, ErrNotFound
		}
		return domain.Task{}, fmt.Errorf("get task by idempotency key: %w", classify(err))
	}
	task, _, err := row.toTask()
	return task, err
}

// GetForUser returns one of a user's tasks, or ErrNotFound.
func (r *TaskRepository) GetForUser(ctx context.Context, userID, taskID string) (domain.Task, error) {
	const query = `SELECT ` + taskColumns + ` FROM tasks WHERE id = $2 AND owner_user_id = $1`

	var row taskRow
	if err := r.db.QueryRowxContext(ctx, query, userID, taskID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, ErrNotFound
		}
		return domain.Task{}, fmt.Errorf("get task %s: %w", taskID, classify(err))
	}
	task, _, err := row.toTask()
	return task, err
}

// Get returns any task, for the run worker and for operator routes.
func (r *TaskRepository) Get(ctx context.Context, taskID string) (domain.Task, string, error) {
	const query = `SELECT ` + taskColumns + ` FROM tasks WHERE id = $1`

	var row taskRow
	if err := r.db.QueryRowxContext(ctx, query, taskID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, "", ErrNotFound
		}
		return domain.Task{}, "", fmt.Errorf("get task %s: %w", taskID, classify(err))
	}
	return row.toTask()
}

// ListForUser returns a user's tasks, newest first.
func (r *TaskRepository) ListForUser(ctx context.Context, userID string, limit, offset int) ([]domain.Task, int, error) {
	const countQuery = `SELECT count(*) FROM tasks WHERE owner_user_id = $1`
	const listQuery = `
		SELECT ` + taskColumns + `
		FROM tasks
		WHERE owner_user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tasks for user %s: %w", userID, classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []taskRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, userID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list tasks for user %s: %w", userID, classify(err))
	}

	tasks := make([]domain.Task, 0, len(rows))
	for _, row := range rows {
		task, _, err := row.toTask()
		if err != nil {
			return nil, 0, err
		}
		tasks = append(tasks, task)
	}
	return tasks, total, nil
}

// ClaimQueued takes the oldest queued task and marks it running, or returns ErrNotFound
// when the queue is empty.
//
// FOR UPDATE SKIP LOCKED is what makes more than one worker safe: two workers polling
// at the same moment take different rows instead of both taking the oldest and running
// somebody's task twice. "Run it twice" here means paying twice and, for a tool with
// side effects, doing the side effect twice.
func (r *TaskRepository) ClaimQueued(ctx context.Context) (domain.Task, string, error) {
	const query = `
		UPDATE tasks SET status = 'running'
		WHERE id = (
			SELECT id FROM tasks
			WHERE status = 'queued'
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + taskColumns

	var row taskRow
	if err := r.db.QueryRowxContext(ctx, query).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, "", ErrNotFound
		}
		return domain.Task{}, "", fmt.Errorf("claim queued task: %w", classify(err))
	}
	return row.toTask()
}

// SetStatus records a task's outcome.
//
// The mapping from a run's stop reason to a task status is domain.TaskStatusFor, not
// this method: there is one reason that means the work got done and nine that do not,
// and collapsing them at each call site is how "halted" eventually gets filed as a
// success.
func (r *TaskRepository) SetStatus(ctx context.Context, taskID string, status domain.TaskStatus) error {
	const query = `UPDATE tasks SET status = $2 WHERE id = $1`

	switch status {
	case domain.TaskStatusQueued, domain.TaskStatusRunning,
		domain.TaskStatusSucceeded, domain.TaskStatusFailed:
	default:
		return fmt.Errorf("task status %q is not one of the four", status)
	}

	result, err := r.db.ExecContext(ctx, query, taskID, string(status))
	if err != nil {
		return fmt.Errorf("set task %s status: %w", taskID, classify(err))
	}
	return requireOneRow(result, taskID)
}

// RequeueStale puts tasks left running by a worker that died back on the queue.
//
// A task stuck in `running` is invisible to every worker, so without this a crash
// during a run means the work is never picked up again and nothing says why. The cutoff
// is a parameter rather than a constant here because how long is "too long" depends on
// the run limits an operator configured.
func (r *TaskRepository) RequeueStale(ctx context.Context, startedBefore time.Time) (int, error) {
	const query = `
		UPDATE tasks SET status = 'queued'
		WHERE status = 'running'
			AND updated_at < $1
			AND NOT EXISTS (
				SELECT 1 FROM runs
				WHERE runs.task_id = tasks.id AND runs.stop IS NULL
			)`

	result, err := r.db.ExecContext(ctx, query, startedBefore)
	if err != nil {
		return 0, fmt.Errorf("requeue stale tasks: %w", classify(err))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count requeued tasks: %w", err)
	}
	return int(affected), nil
}
