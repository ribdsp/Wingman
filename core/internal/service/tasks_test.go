package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// The account unattended work is filed against in these tests. In a real instance it is
// CORE_UNATTENDED_OWNER, resolved to an id by cmd at boot.
const unattendedOwner = "user-unattended"

// tasksFixture wires the service over the two fakes it needs.
type tasksFixture struct {
	tasks *Tasks
	store *fakeTasks
	chats *fakeChats
}

func newTasksFixture(t *testing.T, owner string) tasksFixture {
	t.Helper()

	store := newFakeTasks()
	chats := newFakeChats()
	tasks, err := NewTasks(TasksDeps{
		Tasks:           store,
		Reader:          store,
		Chats:           chats,
		Messages:        chats,
		ChannelChats:    chats,
		UnattendedOwner: owner,
		Clock:           fixedClock(testNow),
		Logger:          silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewTasks: %v", err)
	}
	return tasksFixture{tasks: tasks, store: store, chats: chats}
}

func TestNewTasks_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewTasks(TasksDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Tasks", "Reader", "Chats", "Messages", "ChannelChats"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

// ---------------------------------------------------------------------------
// Dispatch — the goal engine's way in

func TestTasksDispatch_isRefusedForAPerson(t *testing.T) {
	// Arrange — a person asks in a chat, where somebody is waiting for the answer. Letting
	// a session dispatch would put work in the queue with no conversation and no budget
	// holder anybody chose.
	f := newTasksFixture(t, unattendedOwner)

	// Act
	_, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "Check yesterday's revenue", IdempotencyKey: "goal-1:2026-03-14",
	}, userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestTasksDispatch_withoutAConfiguredOwnerIsAMisconfigurationNotABadRequest(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, "")

	// Act
	_, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "Check yesterday's revenue", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor())

	// Assert — deliberately not one of this package's sentinels: the caller did nothing
	// wrong, and a 400 would send them looking at their own request.
	if err == nil {
		t.Fatal("err = nil, want a complaint about the configuration")
	}
	for _, sentinel := range []error{ErrValidation, ErrForbidden, ErrNotFound} {
		if errors.Is(err, sentinel) {
			t.Errorf("err = %v, want it not to be %v", err, sentinel)
		}
	}
	if !strings.Contains(err.Error(), "CORE_UNATTENDED_OWNER") {
		t.Errorf("err = %v, want it to name the env var an operator has to set", err)
	}
}

func TestTasksDispatch_withoutAnIdempotencyKeyIsRefused(t *testing.T) {
	// Arrange — without one a retry is a second run, and the engine always sends it.
	f := newTasksFixture(t, unattendedOwner)

	// Act
	_, err := f.tasks.Dispatch(context.Background(), Dispatch{Brief: "Do the thing"}, botActor())

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "idempotencyKey") {
		t.Errorf("err = %v, want it to name the field", err)
	}
	if len(f.store.tasks) != 0 {
		t.Errorf("tasks = %d, want none queued", len(f.store.tasks))
	}
}

func TestTasksDispatch_withoutABriefIsRefusedWithTheFieldNamed(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)

	// Act
	_, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "   ", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor())

	// Assert — validated here rather than left to the repository, so the answer is
	// something the caller can act on instead of a wrapped database error.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "brief") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestTasksDispatch_queuesWorkAgainstTheUnattendedAccount(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)

	// Act
	dispatched, err := f.tasks.Dispatch(context.Background(), Dispatch{
		BotID:          "revenue-watcher",
		ChannelID:      "ops",
		Brief:          "  Revenue is behind pace; find out why  ",
		IdempotencyKey: "  goal-1:2026-03-14  ",
		Metadata:       map[string]string{"goalId": "goal-1"},
	}, botActor())

	// Assert
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !dispatched.Created {
		t.Error("created = false, want true for a task that was just made")
	}
	task := dispatched.Task
	// The owner comes from configuration, never from the request: a dispatcher that could
	// name its own owner could file work against anybody's budget.
	if task.OwnerUserID != unattendedOwner {
		t.Errorf("owner = %q, want the configured %q", task.OwnerUserID, unattendedOwner)
	}
	if task.Source != domain.TaskSourceGoalEngine {
		t.Errorf("source = %q, want %q", task.Source, domain.TaskSourceGoalEngine)
	}
	if task.Status != domain.TaskStatusQueued {
		t.Errorf("status = %q, want %q", task.Status, domain.TaskStatusQueued)
	}
	if task.Brief != "Revenue is behind pace; find out why" {
		t.Errorf("brief = %q, want it trimmed", task.Brief)
	}
	if task.IdempotencyKey != "goal-1:2026-03-14" {
		t.Errorf("idempotencyKey = %q, want it trimmed", task.IdempotencyKey)
	}
	if task.BotID != "revenue-watcher" || task.ChannelID != "ops" {
		t.Errorf("botId/channelId = %q/%q, want them carried through", task.BotID, task.ChannelID)
	}
}

func TestTasksDispatch_anOperatorMayDispatchToo(t *testing.T) {
	// Arrange — whoever owns the instance owns the unattended account, so an operator
	// driving core by hand is the same authority the bridge has.
	f := newTasksFixture(t, unattendedOwner)

	// Act
	dispatched, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "Do the thing", IdempotencyKey: "manual-1",
	}, operatorActor())

	// Assert
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !dispatched.Created {
		t.Error("created = false, want true")
	}
}

func TestTasksDispatch_theSameKeyTwiceIsOneTask(t *testing.T) {
	// Arrange — a goal engine that timed out waiting for a response and tried again must
	// not cause a second run.
	f := newTasksFixture(t, unattendedOwner)
	in := Dispatch{Brief: "Revenue is behind pace", IdempotencyKey: "goal-1:2026-03-14"}
	first, err := f.tasks.Dispatch(context.Background(), in, botActor())
	if err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	// Act
	second, err := f.tasks.Dispatch(context.Background(), in, botActor())

	// Assert
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if second.Created {
		// Created is what tells 201 from 200. Collapsing the two would make a retried
		// trigger indistinguishable from a fresh one in both services' logs.
		t.Error("created = true, want false for a retry")
	}
	if second.Task.ID != first.Task.ID {
		t.Errorf("id = %q, want the original %q", second.Task.ID, first.Task.ID)
	}
	if len(f.store.tasks) != 1 {
		t.Errorf("tasks = %d, want 1", len(f.store.tasks))
	}
}

func TestTasksDispatch_aRetryWithADifferentBriefStillAnswersWithTheOriginal(t *testing.T) {
	// Arrange — the engine derives its keys from a goal and a period, so two briefs under
	// one key means the goal was edited between attempts.
	f := newTasksFixture(t, unattendedOwner)
	first, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "The original brief", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor())
	if err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	// Act
	second, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "A rewritten brief", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor())

	// Assert — a conflict here would lose the trigger entirely, which is worse than
	// running the brief the key was first seen with.
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if second.Task.Brief != first.Task.Brief {
		t.Errorf("brief = %q, want the original %q", second.Task.Brief, first.Task.Brief)
	}
	if second.Created {
		t.Error("created = true, want false")
	}
}

func TestTasksDispatch_aTakenKeyThatCannotBeReadBackIsNotAnInventedTaskId(t *testing.T) {
	// Arrange — the unique index says the row exists, and reading it fails.
	f := newTasksFixture(t, unattendedOwner)
	if _, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "The original brief", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor()); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	f.store.failByKey = errBoom

	// Act
	_, err := f.tasks.Dispatch(context.Background(), Dispatch{
		Brief: "The original brief", IdempotencyKey: "goal-1:2026-03-14",
	}, botActor())

	// Assert — a 5xx, which is the caller's cue to retry. Answering ErrConflict would be a
	// terminal answer to a problem that is not the caller's.
	if err == nil {
		t.Fatal("err = nil, want the read failure")
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("err = %v, want the store's failure", err)
	}
	for _, sentinel := range []error{ErrValidation, ErrConflict, ErrNotFound} {
		if errors.Is(err, sentinel) {
			t.Errorf("err = %v, want it not to be %v", err, sentinel)
		}
	}
}

// ---------------------------------------------------------------------------
// Send — a person's way in

func TestTasksSend_refusesAMachineKey(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)

	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Act — a machine key has no account, so there is no chat of its own to send in.
			_, err := f.tasks.Send(context.Background(), Send{Text: "hello"}, actor)

			// Assert
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestTasksSend_refusesTextItWouldNotStore(t *testing.T) {
	cases := map[string]string{
		"empty":                 "   ",
		"past the cap":          strings.Repeat("a", domain.MaxBriefLength+1),
		"past the cap in kanji": strings.Repeat("あ", domain.MaxBriefLength+1),
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			f := newTasksFixture(t, unattendedOwner)

			// Act
			_, err := f.tasks.Send(context.Background(), Send{Text: text}, userActor("user-1"))

			// Assert — the bound is in runes, so a message in a non-Latin script is not
			// refused at a quarter of the documented length.
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), "text") {
				t.Errorf("err = %v, want it to name the field", err)
			}
			if len(f.chats.chats) != 0 {
				t.Errorf("chats = %d, want none created for a refused message", len(f.chats.chats))
			}
		})
	}
}

func TestTasksSend_firstMessageMakesAChatNamedAfterIt(t *testing.T) {
	// Arrange — making the client create a chat first would mean two round trips to say one
	// thing.
	f := newTasksFixture(t, unattendedOwner)

	// Act
	sent, err := f.tasks.Send(context.Background(), Send{
		Text: "Why did signups drop yesterday?\nI checked the dashboard and it looks flat.",
	}, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent.Chat.ID == "" {
		t.Fatal("no chat was created")
	}
	// The first line only: a title is a thing a person scans a list by.
	if sent.Chat.Title != "Why did signups drop yesterday?" {
		t.Errorf("title = %q, want the first line", sent.Chat.Title)
	}
	if sent.Message.Role != repository.MessageRoleUser {
		t.Errorf("role = %q, want %q", sent.Message.Role, repository.MessageRoleUser)
	}
	if sent.Task.Source != domain.TaskSourceUser {
		t.Errorf("source = %q, want %q", sent.Task.Source, domain.TaskSourceUser)
	}
	if sent.Task.Status != domain.TaskStatusQueued {
		t.Errorf("status = %q, want %q", sent.Task.Status, domain.TaskStatusQueued)
	}
	if sent.Task.OwnerUserID != "user-1" {
		t.Errorf("owner = %q, want the sender", sent.Task.OwnerUserID)
	}
	// Nothing was said before, so the brief is the message and nothing else — no empty
	// history block for the model to read as a conversation that did not happen.
	if sent.Task.Brief != "Why did signups drop yesterday?\nI checked the dashboard and it looks flat." {
		t.Errorf("brief = %q, want just the message", sent.Task.Brief)
	}
}

func TestTasksSend_titleOfAVeryLongFirstLineIsBounded(t *testing.T) {
	// Arrange — a pasted stack trace makes a bad title.
	f := newTasksFixture(t, unattendedOwner)

	// Act
	sent, err := f.tasks.Send(context.Background(), Send{Text: strings.Repeat("a", 400)}, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := len([]rune(sent.Chat.Title)); got != maxChatTitleLength+1 {
		// The cap plus the ellipsis that says it was cut.
		t.Errorf("title length = %d, want %d", got, maxChatTitleLength+1)
	}
	if !strings.HasSuffix(sent.Chat.Title, "…") {
		t.Errorf("title = %q, want it to end with an ellipsis", sent.Chat.Title)
	}
}

func TestTasksSend_intoSomebodyElsesChatIsNotFound(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)
	theirs, err := f.chats.CreateChat(context.Background(), "user-2", "Theirs")
	if err != nil {
		t.Fatalf("seed a chat: %v", err)
	}

	// Act
	_, err = f.tasks.Send(context.Background(), Send{ChatID: theirs.ID, Text: "hello"}, userActor("user-1"))

	// Assert — fetched rather than assumed, so this is a readable 404 here rather than a
	// foreign-key error two statements later.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(f.chats.messages[theirs.ID]) != 0 {
		t.Error("a message was written into somebody else's chat")
	}
}

func TestTasksSend_intoAnArchivedChatIsAConflict(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)
	chat, err := f.chats.CreateChat(context.Background(), "user-1", "Away")
	if err != nil {
		t.Fatalf("seed a chat: %v", err)
	}
	if err := f.chats.ArchiveChat(context.Background(), "user-1", chat.ID, testNow); err != nil {
		t.Fatalf("archive it: %v", err)
	}

	// Act
	_, err = f.tasks.Send(context.Background(), Send{ChatID: chat.ID, Text: "hello"}, userActor("user-1"))

	// Assert — the chat is theirs and it exists, so this is a state problem rather than a
	// missing row: unarchiving is something the caller can do.
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestTasksSend_briefCarriesTheConversationAsItStoodBeforeTheMessage(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)
	chat, err := f.chats.CreateChat(context.Background(), "user-1", "Ongoing")
	if err != nil {
		t.Fatalf("seed a chat: %v", err)
	}
	for _, seed := range []struct {
		role    repository.MessageRole
		content string
	}{
		{repository.MessageRoleUser, "What was revenue yesterday?"},
		{repository.MessageRoleAssistant, "It was 4,200."},
	} {
		if _, err := f.chats.AppendMessage(context.Background(), repository.NewMessage{
			ChatID: chat.ID, UserID: "user-1", Role: seed.role, Content: seed.content,
		}); err != nil {
			t.Fatalf("seed a message: %v", err)
		}
	}

	// Act
	sent, err := f.tasks.Send(context.Background(), Send{ChatID: chat.ID, Text: "And the day before?"}, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	brief := sent.Task.Brief
	for _, want := range []string{"What was revenue yesterday?", "It was 4,200.", "And the day before?"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
	// Labelled as a record, for the same reason internal/agent labels dispatch metadata as
	// facts: text somebody else wrote is still text, and a line in it that reads like an
	// instruction is not one.
	if !strings.Contains(brief, "not instructions") {
		t.Errorf("brief does not label the history as a record:\n%s", brief)
	}
	// Read before the write, so the history is the conversation as it stood — the new
	// message must not appear twice.
	if got := strings.Count(brief, "And the day before?"); got != 1 {
		t.Errorf("the new message appears %d times, want 1:\n%s", got, brief)
	}
}

func TestComposeBrief_leavesTheNewMessageWholeAndDropsTheOldestHistory(t *testing.T) {
	// Arrange — a large new message, and far more history than can fit beside it.
	text := strings.Repeat("n", domain.MaxBriefLength-2000)
	history := []repository.Message{{
		Role: repository.MessageRoleUser, Content: "the-oldest-row " + strings.Repeat("o", 700),
	}}
	for range 8 {
		history = append(history, repository.Message{
			Role: repository.MessageRoleAssistant, Content: strings.Repeat("m", 700),
		})
	}
	history = append(history, repository.Message{
		Role: repository.MessageRoleUser, Content: "the-newest-row " + strings.Repeat("w", 700),
	})

	// Act
	brief := composeBrief(history, text)

	// Assert — somebody's actual question surviving intact matters more than context, so
	// the new message is never trimmed to make room.
	if !strings.Contains(brief, text) {
		t.Error("the new message was trimmed, want it whole")
	}
	if len([]rune(brief)) > domain.MaxBriefLength {
		t.Errorf("brief = %d runes, want at most %d", len([]rune(brief)), domain.MaxBriefLength)
	}
	// What is dropped is the oldest, not the nearest: the last thing said is the most likely
	// to be what the new message refers to.
	if strings.Contains(brief, "the-oldest-row") {
		t.Error("the oldest history survived, want it dropped first")
	}
	if !strings.Contains(brief, "the-newest-row") {
		t.Errorf("the newest history was dropped, want it kept:\n%s", brief)
	}
}

func TestComposeBrief_skipsRowsThatArePreviousRunsInternals(t *testing.T) {
	// Arrange
	history := []repository.Message{
		{Role: repository.MessageRoleUser, Content: "a person asked something"},
		{Role: repository.MessageRoleTool, Content: "a tool returned some JSON"},
		{Role: repository.MessageRoleSystem, Content: "a run stopped before answering"},
		{Role: repository.MessageRoleAssistant, Content: "the assistant replied"},
		{Role: repository.MessageRoleUser, Content: "   "},
	}

	// Act
	brief := composeBrief(history, "the new message")

	// Assert — replaying tool output would put a previous run's internals in the next run's
	// prompt, and an empty row adds a label with nothing after it.
	for _, unwanted := range []string{"a tool returned some JSON", "a run stopped before answering"} {
		if strings.Contains(brief, unwanted) {
			t.Errorf("brief contains %q, want it skipped:\n%s", unwanted, brief)
		}
	}
	for _, want := range []string{"a person asked something", "the assistant replied"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief is missing %q:\n%s", want, brief)
		}
	}
	if got := strings.Count(brief, "user:"); got != 1 {
		t.Errorf("user rows in the brief = %d, want the one with content", got)
	}
}

func TestComposeBrief_capsOneVeryLongHistoryRowRatherThanDroppingIt(t *testing.T) {
	// Arrange
	history := []repository.Message{
		{Role: repository.MessageRoleAssistant, Content: strings.Repeat("h", maxHistoryMessageLen+500)},
	}

	// Act
	brief := composeBrief(history, "the new message")

	// Assert — a long answer still carries what it was about, which a dropped row does not.
	want := "assistant: " + strings.Repeat("h", maxHistoryMessageLen) + " […]"
	if !strings.Contains(brief, want) {
		t.Errorf("brief does not carry the row cut to %d characters:\n%s", maxHistoryMessageLen, brief)
	}
	if strings.Contains(brief, strings.Repeat("h", maxHistoryMessageLen+1)) {
		t.Error("the row was not cut, want it capped")
	}
}

func TestComposeBrief_withNothingToReplayIsJustTheMessage(t *testing.T) {
	// Act
	brief := composeBrief(nil, "the only message")

	// Assert — no preamble, so a first message does not arrive describing a conversation
	// that has not happened.
	if brief != "the only message" {
		t.Errorf("brief = %q, want just the message", brief)
	}
}

// ---------------------------------------------------------------------------
// reads

func TestTasksGet_scopesToTheCallersOwnTasks(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)
	mine, err := f.store.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-1", Source: domain.TaskSourceUser, Brief: "mine", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	theirs, err := f.store.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-2", Source: domain.TaskSourceUser, Brief: "theirs", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}

	// Act
	got, mineErr := f.tasks.Get(context.Background(), mine.ID, userActor("user-1"))
	_, theirsErr := f.tasks.Get(context.Background(), theirs.ID, userActor("user-1"))

	// Assert
	if mineErr != nil {
		t.Fatalf("Get own: %v", mineErr)
	}
	if got.ID != mine.ID {
		t.Errorf("id = %q, want %q", got.ID, mine.ID)
	}
	if !errors.Is(theirsErr, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", theirsErr)
	}
}

func TestTasksGet_requiresAnId(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)

	// Act
	_, err := f.tasks.Get(context.Background(), "  ", userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestTasksGet_aDispatcherSeesTheUnattendedAccountsWorkAndNothingElse(t *testing.T) {
	// Arrange — this is what lets the goal engine follow up on the task it queued without
	// being able to read anybody's chats: nobody signs in as the unattended account, so it
	// owns the dispatched work and nothing else.
	f := newTasksFixture(t, unattendedOwner)
	unattended, err := f.store.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: unattendedOwner, Source: domain.TaskSourceGoalEngine, Brief: "dispatched",
		IdempotencyKey: "goal-1", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}
	personal, err := f.store.Create(context.Background(), repository.NewTask{Task: domain.Task{
		OwnerUserID: "user-1", Source: domain.TaskSourceUser, Brief: "a person's", Status: domain.TaskStatusQueued,
	}})
	if err != nil {
		t.Fatalf("seed a task: %v", err)
	}

	// Act
	got, err := f.tasks.Get(context.Background(), unattended.ID, botActor())
	_, personalErr := f.tasks.Get(context.Background(), personal.ID, botActor())

	// Assert
	if err != nil {
		t.Fatalf("Get the dispatched task: %v", err)
	}
	if got.ID != unattended.ID {
		t.Errorf("id = %q, want %q", got.ID, unattended.ID)
	}
	if !errors.Is(personalErr, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a person's task", personalErr)
	}
}

func TestTasksGet_aDispatcherWithNoConfiguredOwnerFindsNothing(t *testing.T) {
	// Arrange — a read, unlike a dispatch, has nothing to tell the caller to fix: there is
	// simply no account whose work this could be.
	f := newTasksFixture(t, "")

	// Act
	_, getErr := f.tasks.Get(context.Background(), "task-1", botActor())
	_, _, listErr := f.tasks.List(context.Background(), 10, 0, botActor())

	// Assert
	if !errors.Is(getErr, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", getErr)
	}
	if !errors.Is(listErr, ErrNotFound) {
		t.Errorf("List err = %v, want ErrNotFound", listErr)
	}
}

func TestTasksList_isTheCallersOwnTasksWithTheTotal(t *testing.T) {
	// Arrange
	f := newTasksFixture(t, unattendedOwner)
	for _, owner := range []string{"user-1", "user-1", "user-2"} {
		if _, err := f.store.Create(context.Background(), repository.NewTask{Task: domain.Task{
			OwnerUserID: owner, Source: domain.TaskSourceUser, Brief: "a task", Status: domain.TaskStatusQueued,
		}}); err != nil {
			t.Fatalf("seed a task: %v", err)
		}
	}

	// Act
	got, total, err := f.tasks.List(context.Background(), 1, 0, userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("page = %d, want the limit of 1", len(got))
	}
	// The total is what the caller has, not what fits on the page, or a client cannot
	// render a pager.
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
}
