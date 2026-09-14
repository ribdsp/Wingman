package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ribdsp/wingman/core/internal/domain"
)

func taskRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "owner_user_id", "source", "chat_id", "bot_id", "channel_id", "brief",
		"idempotency_key", "metadata", "status", "created_at",
	})
}

// dispatchedTask is the shape the goal engine's bridge sends: unattended work, with the
// goal that caused it in the metadata.
func dispatchedTask() domain.Task {
	return domain.Task{
		OwnerUserID:    "usr_01",
		Source:         domain.TaskSourceGoalEngine,
		BotID:          "ops",
		ChannelID:      "chn_ops",
		Brief:          "revenue is behind pace; draft the follow-up list",
		IdempotencyKey: "goal_7:2026-09-11T14:00:00Z",
		Metadata:       map[string]string{"goalId": "goal_7"},
	}
}

func TestTaskRepository_create_defaultsAnUnsetStatusToQueued(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	task := dispatchedTask()
	mock.ExpectQuery(`INSERT INTO tasks`).
		WithArgs("usr_01", "goal_engine", nil, "ops", "chn_ops",
			task.Brief, task.IdempotencyKey, `{"goalId":"goal_7"}`, "queued").
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "goal_engine", nil,
			"ops", "chn_ops", task.Brief, task.IdempotencyKey,
			`{"goalId": "goal_7"}`, "queued", fixedNow))

	// Act
	created, err := repo.Create(context.Background(), NewTask{Task: task})

	// Assert
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// A task with no status would be a task no worker claims, so the work would sit
	// there looking accepted and never run.
	if created.Status != domain.TaskStatusQueued {
		t.Errorf("status = %q; want queued", created.Status)
	}
	if created.Metadata["goalId"] != "goal_7" {
		t.Errorf("metadata = %v; want the goal that caused the task", created.Metadata)
	}
}

func TestTaskRepository_create_refusesAnInvalidTaskWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewTaskRepository(db)
	ownerless := dispatchedTask()
	ownerless.OwnerUserID = "  "

	// Act
	_, err := repo.Create(context.Background(), NewTask{Task: ownerless})

	// Assert
	// A run whose tokens belong to nobody cannot be capped, so a task with no owner is
	// refused before it can reach a queue. No query is expected; the mock's expectation
	// check fails the test if one were made.
	if err == nil {
		t.Fatal("a task with no owner was accepted")
	}
}

// The two-step design is the point of this test. A retry is a healthy outcome, but the
// API has to answer 200-with-the-original rather than 201, and an upsert that quietly
// returned the old row would make a retry indistinguishable from a fresh dispatch in the
// logs of both services.
func TestTaskRepository_create_duplicateIdempotencyKeyIsAConflict(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	task := dispatchedTask()
	mock.ExpectQuery(`INSERT INTO tasks`).
		WillReturnError(pgError(pgUniqueViolation))

	// Act
	_, err := repo.Create(context.Background(), NewTask{Task: task})

	// Assert
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create task with a used key = %v; want ErrConflict", err)
	}
}

func TestTaskRepository_getByIdempotencyKey_returnsTheOriginalTask(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	task := dispatchedTask()
	mock.ExpectQuery(`FROM tasks WHERE idempotency_key = \$1`).
		WithArgs(task.IdempotencyKey).
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "goal_engine", "cht_01",
			"ops", "chn_ops", task.Brief, task.IdempotencyKey, `{}`, "running", fixedNow))

	// Act
	found, err := repo.GetByIdempotencyKey(context.Background(), task.IdempotencyKey)

	// Assert
	// This is what closes the retry loop: the monitor that retried gets the same task
	// id back, so it does not start a second run of work already in flight.
	if err != nil {
		t.Fatalf("get task by key: %v", err)
	}
	if found.ID != "tsk_01" {
		t.Errorf("task id = %q; want the original", found.ID)
	}
}

func TestTaskRepository_getByIdempotencyKey_emptyKeyIsNotFoundWithoutQuerying(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewTaskRepository(db)

	// Act
	_, err := repo.GetByIdempotencyKey(context.Background(), "")

	// Assert
	// An empty key would match every task stored without one, and returning somebody
	// else's task to a caller that sent no key is the worst possible answer here.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get task by empty key = %v; want ErrNotFound", err)
	}
}

func TestTaskRepository_getForUser_scopesTheReadToTheOwner(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE id = \$2 AND owner_user_id = \$1`).
		WithArgs("usr_02", "tsk_01").
		WillReturnRows(taskRows())

	// Act
	_, err := repo.GetForUser(context.Background(), "usr_02", "tsk_01")

	// Assert
	// The brief is the instruction somebody gave an agent about their business. It is
	// not readable by an account that guessed a task id.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get another account's task = %v; want ErrNotFound", err)
	}
}

func TestTaskRepository_get_returnsTheDeliveryChatAlongsideTheTask(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE id = \$1`).
		WithArgs("tsk_01").
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "user", "cht_01",
			"", "", "summarise this thread", nil, `{}`, "queued", fixedNow))

	// Act
	task, chatID, err := repo.Get(context.Background(), "tsk_01")

	// Assert
	// The chat id is where the answer goes, and it is returned separately because it is
	// a delivery detail rather than part of what the agent decides.
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if chatID != "cht_01" {
		t.Errorf("chatId = %q; want the chat the answer goes to", chatID)
	}
	if task.IdempotencyKey != "" {
		t.Errorf("idempotencyKey = %q; want empty on a task a person typed", task.IdempotencyKey)
	}
}

func TestTaskRepository_get_undecodableMetadataIsAnErrorNotAnEmptyMap(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE id = \$1`).
		WithArgs("tsk_01").
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "goal_engine", nil,
			"ops", "chn_ops", "brief", nil, `{"goalId":`, "queued", fixedNow))

	// Act
	_, _, err := repo.Get(context.Background(), "tsk_01")

	// Assert
	// The metadata is how unattended work is traced back to the goal that caused it.
	// Silently dropping it would leave a run nobody can explain.
	if err == nil {
		t.Fatal("a task with undecodable metadata was returned as if it were fine")
	}
}

func TestTaskRepository_listForUser_boundsThePageAndReportsTheTotal(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`count\(\*\) FROM tasks WHERE owner_user_id = \$1`).
		WithArgs("usr_01").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`FROM tasks\s+WHERE owner_user_id = \$1\s+ORDER BY created_at DESC`).
		WithArgs("usr_01", maxPageLimit, 0).
		WillReturnRows(taskRows().
			AddRow("tsk_02", "usr_01", "user", nil, "", "", "newer", nil, `{}`, "queued", fixedNow).
			AddRow("tsk_01", "usr_01", "goal_engine", nil, "ops", "chn_ops", "older", nil, `{}`, "succeeded", fixedNow))

	// Act
	tasks, total, err := repo.ListForUser(context.Background(), "usr_01", 9_999, 0)

	// Assert
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if total != 2 || len(tasks) != 2 {
		t.Fatalf("got %d of %d tasks; want 2 of 2", len(tasks), total)
	}
	if tasks[0].ID != "tsk_02" {
		t.Errorf("tasks came back oldest first: %+v", tasks)
	}
}

// FOR UPDATE SKIP LOCKED is the whole reason more than one worker is safe. Two workers
// polling at the same moment must take different rows; without it both take the oldest
// and the same task runs twice, which means paying twice and, for a tool with side
// effects, doing the side effect twice.
func TestTaskRepository_claimQueued_locksTheRowItTakes(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`UPDATE tasks SET status = 'running'.*FOR UPDATE SKIP LOCKED`).
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "goal_engine", nil,
			"ops", "chn_ops", "brief", "key", `{}`, "running", fixedNow))

	// Act
	task, _, err := repo.ClaimQueued(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("claim queued task: %v", err)
	}
	// The claim and the status change are one statement, so a worker that dies between
	// them cannot exist.
	if task.Status != domain.TaskStatusRunning {
		t.Errorf("status = %q; want running once claimed", task.Status)
	}
	// The task carries the owner it will be billed to. The claim itself is
	// instance-wide, so this field is the only thing tying the spend to an account.
	if task.OwnerUserID != "usr_01" {
		t.Errorf("ownerUserId = %q; want the account the run is billed to", task.OwnerUserID)
	}
}

func TestTaskRepository_claimQueued_emptyQueueIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`UPDATE tasks SET status = 'running'`).
		WillReturnRows(taskRows())

	// Act
	_, _, err := repo.ClaimQueued(context.Background())

	// Assert
	// An idle queue is the normal state of this service, so it has to be a cheap,
	// recognisable answer and not something a worker logs as a failure every second.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim from an empty queue = %v; want ErrNotFound", err)
	}
}

func TestTaskRepository_setStatus_refusesAStatusOutsideTheFour(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewTaskRepository(db)

	// Act
	err := repo.SetStatus(context.Background(), "tsk_01", domain.TaskStatus("halted"))

	// Assert
	// A stop reason is not a task status. Writing one here would be the exact mistake
	// domain.TaskStatusFor exists to prevent: nine reasons that are not success getting
	// filed as something a poller reads as an outcome.
	if err == nil {
		t.Fatal("a stop reason was accepted as a task status")
	}
}

func TestTaskRepository_setStatus_missingTaskIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectExec(`UPDATE tasks SET status = \$2 WHERE id = \$1`).
		WithArgs("tsk_missing", "failed").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.SetStatus(context.Background(), "tsk_missing", domain.TaskStatusFailed)

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("set status on a missing task = %v; want ErrNotFound", err)
	}
}

// RequeueStale is one half of the crash-recovery story; RunRepository.FailAbandoned is
// the other. Without it a task stuck in `running` is invisible to every worker, so a
// crash mid-run means the work is never picked up again and nothing says why.
func TestTaskRepository_requeueStale_skipsTasksWithARunStillInFlight(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	cutoff := fixedNow.Add(-30 * time.Minute)
	mock.ExpectExec(`UPDATE tasks SET status = 'queued'\s+WHERE status = 'running'\s+AND updated_at < \$1\s+AND NOT EXISTS \(\s+SELECT 1 FROM runs\s+WHERE runs.task_id = tasks.id AND runs.stop IS NULL`).
		WithArgs(cutoff).
		WillReturnResult(sqlmock.NewResult(0, 2))

	// Act
	requeued, err := repo.RequeueStale(context.Background(), cutoff)

	// Assert
	// The NOT EXISTS clause is what stops this from requeueing a long but healthy run:
	// a live run has a row with no stop reason, and requeueing its task would run the
	// same work twice concurrently.
	if err != nil {
		t.Fatalf("requeue stale tasks: %v", err)
	}
	if requeued != 2 {
		t.Errorf("requeued %d tasks; want 2", requeued)
	}
}

func TestTaskRepository_requeueStale_unreadableCountIsAnError(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectExec(`UPDATE tasks SET status = 'queued'`).
		WithArgs(fixedNow).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver lost the count")))

	// Act
	requeued, err := repo.RequeueStale(context.Background(), fixedNow)

	// Assert
	// Reporting zero would read as "nothing was stale", which is the one thing an
	// operator watching a stuck queue must not be told incorrectly.
	if err == nil {
		t.Fatal("an unreadable row count was reported as a successful sweep")
	}
	if requeued != 0 {
		t.Errorf("requeued = %d alongside the error; want 0", requeued)
	}
}

func TestTaskRepository_get_aTaskThatDoesNotExistIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE id = \$1`).
		WithArgs("tsk_missing").
		WillReturnRows(taskRows())

	// Act
	_, _, err := repo.Get(context.Background(), "tsk_missing")

	// Assert
	// A zero Task returned instead would be a brief of "" with no owner, and a worker
	// handed that would start a run nobody asked for and nobody is billed for.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get a missing task = %v; want ErrNotFound", err)
	}
}

func TestTaskRepository_getByIdempotencyKey_anUnusedKeyIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE idempotency_key = \$1`).
		WithArgs("goal_7:2026-09-11T15:00:00Z").
		WillReturnRows(taskRows())

	// Act
	_, err := repo.GetByIdempotencyKey(context.Background(), "goal_7:2026-09-11T15:00:00Z")

	// Assert
	// This is the answer that means "go ahead and create it", so it has to be
	// distinguishable from a read that failed — see the outage tests elsewhere.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get task by an unused key = %v; want ErrNotFound", err)
	}
}

func TestTaskRepository_getForUser_returnsTheOwnersOwnTask(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewTaskRepository(db)
	mock.ExpectQuery(`FROM tasks WHERE id = \$2 AND owner_user_id = \$1`).
		WithArgs("usr_01", "tsk_01").
		WillReturnRows(taskRows().AddRow("tsk_01", "usr_01", "goal_engine", nil,
			"ops", "chn_ops", "revenue is behind pace", "goal_7:2026-09-11T14:00:00Z",
			`{"goalId": "goal_7"}`, "running", fixedNow))

	// Act
	task, err := repo.GetForUser(context.Background(), "usr_01", "tsk_01")

	// Assert
	// The scoped read has to work for the owner as well as fail for everybody else, and
	// the metadata has to survive it: this is the route by which a person sees which goal
	// caused work to be done on their behalf.
	if err != nil {
		t.Fatalf("get own task: %v", err)
	}
	if task.ID != "tsk_01" || task.Metadata["goalId"] != "goal_7" {
		t.Errorf("task = %+v; want tsk_01 carrying its goal", task)
	}
}
