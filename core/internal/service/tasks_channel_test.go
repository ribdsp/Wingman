package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// FromChannel's tests are here rather than in tasks_test.go because that file is already
// at the length this project treats as a ceiling. They are about one entry point: the port
// Inbox reaches, which is the only way a task is filed by somebody who presented no
// credential at all.

// channelTasks builds the task service the way cmd wires it, and hands back the fake chat
// store as well: what chat an accepted message landed in is half of what these tests
// assert.
func channelTasks(t *testing.T) (*Tasks, *fakeChats) {
	t.Helper()

	chats := newFakeChats()
	tasks, err := NewTasks(TasksDeps{
		Tasks:           newFakeTasks(),
		Reader:          newFakeTasks(),
		Chats:           chats,
		Messages:        chats,
		ChannelChats:    chats,
		UnattendedOwner: "user-unattended",
		Clock:           fixedClock(testNow),
		Logger:          silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewTasks: %v", err)
	}
	return tasks, chats
}

// channelMessage is one message from a linked Telegram DM.
func channelMessage(userID, text string) ChannelMessage {
	return ChannelMessage{
		UserID:         userID,
		Kind:           domain.ChannelTelegram,
		ConversationID: "tg-chat-42",
		Text:           text,
	}
}

func TestTasksFromChannel_filesTheWorkAgainstTheSenderAndTheirChat(t *testing.T) {
	// Arrange
	tasks, _ := channelTasks(t)

	// Act
	sent, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "Check yesterday's revenue"))

	// Assert
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}
	// The owner is the one Inbox resolved from the channel identity. Nothing in the message
	// names an account, which is what stops this entry point from being a way to spend
	// somebody else's budget.
	if sent.Task.OwnerUserID != "user-1" {
		t.Errorf("ownerUserId = %q, want the sender's account", sent.Task.OwnerUserID)
	}
	if sent.Task.Source != domain.TaskSourceChannel {
		t.Errorf("source = %q, want %q", sent.Task.Source, domain.TaskSourceChannel)
	}
	// Queued, not running: the runner picks it up. A channel message that started a run
	// inline would hold the platform's connection open for the length of the run.
	if sent.Task.Status != domain.TaskStatusQueued {
		t.Errorf("status = %q, want queued", sent.Task.Status)
	}
	if sent.Task.Brief != "Check yesterday's revenue" {
		t.Errorf("brief = %q, want the message as it arrived", sent.Task.Brief)
	}

	// The chat and the message it holds belong to the same account, and the task points at
	// the chat, so a run's answer has somewhere to be delivered.
	if sent.Chat.UserID != "user-1" || sent.Message.UserID != "user-1" {
		t.Errorf("chat/message owner = %q/%q, want the sender's account", sent.Chat.UserID, sent.Message.UserID)
	}
	if sent.Message.Role != repository.MessageRoleUser {
		t.Errorf("role = %q, want the message recorded as the person's", sent.Message.Role)
	}
	if sent.Chat.Title != "Check yesterday's revenue" {
		t.Errorf("title = %q, want the chat named after the first message", sent.Chat.Title)
	}
}

func TestTasksFromChannel_titlesALongFirstMessageWithoutLettingItRun(t *testing.T) {
	// Arrange — a title is a label in a list, so it is cut. The brief is not.
	tasks, _ := channelTasks(t)
	long := strings.TrimSpace(strings.Repeat("revenue ", 40))

	// Act
	sent, err := tasks.FromChannel(context.Background(), channelMessage("user-1", long))

	// Assert
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}
	if len([]rune(sent.Chat.Title)) > maxChatTitleLength {
		t.Errorf("title is %d runes, want at most %d", len([]rune(sent.Chat.Title)), maxChatTitleLength)
	}
	if sent.Task.Brief != long {
		t.Error("the brief was cut down to the title's length")
	}
}

func TestTasksFromChannel_asecondMessageContinuesTheSameChatAndCarriesTheHistory(t *testing.T) {
	// Arrange — one conversation on a platform is one chat, so the agent answering the
	// second message can see what the first one said.
	tasks, _ := channelTasks(t)
	first, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "Check yesterday's revenue"))
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}

	// Act
	second, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "And the day before"))

	// Assert
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}
	if second.Chat.ID != first.Chat.ID {
		t.Fatalf("chatId = %q, want the first message's chat %q", second.Chat.ID, first.Chat.ID)
	}
	if !strings.Contains(second.Task.Brief, "Check yesterday's revenue") {
		t.Error("the brief carries no history, so the follow-up has no antecedent")
	}
	if !strings.Contains(second.Task.Brief, "And the day before") {
		t.Error("the brief does not contain the message that caused it")
	}
	// The title stays whatever the first message made it: a chat that renamed itself on
	// every message is a list nobody can scan.
	if second.Chat.Title != first.Chat.Title {
		t.Errorf("title = %q, want it left at %q", second.Chat.Title, first.Chat.Title)
	}
}

func TestTasksFromChannel_adifferentConversationIsADifferentChat(t *testing.T) {
	// Arrange — the same person's DM and a room they called the bot in are two threads, and
	// merging them would put a private brief into a shared room's history.
	tasks, _ := channelTasks(t)
	first, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "Check yesterday's revenue"))
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}

	elsewhere := channelMessage("user-1", "Check it here instead")
	elsewhere.ConversationID = "tg-chat-77"

	// Act
	second, err := tasks.FromChannel(context.Background(), elsewhere)

	// Assert
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}
	if second.Chat.ID == first.Chat.ID {
		t.Fatal("two conversations landed in one chat")
	}
	if strings.Contains(second.Task.Brief, "Check yesterday's revenue") {
		t.Error("one conversation's history leaked into another's brief")
	}
}

func TestTasksFromChannel_thesameConversationOnTwoAccountsIsTwoChats(t *testing.T) {
	// Arrange — two people in the same room, both linked. A conversation id is the
	// platform's, so it is only unique within an account.
	tasks, _ := channelTasks(t)
	mine, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "Check my revenue"))
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}

	// Act
	theirs, err := tasks.FromChannel(context.Background(), channelMessage("user-2", "Check mine"))

	// Assert
	if err != nil {
		t.Fatalf("FromChannel: %v", err)
	}
	if theirs.Chat.ID == mine.Chat.ID {
		t.Fatal("two accounts share one chat")
	}
	if strings.Contains(theirs.Task.Brief, "Check my revenue") {
		t.Error("one account's history reached another account's brief")
	}
}

func TestTasksFromChannel_refusesAMessageThatCannotBeFiledAgainstAnybody(t *testing.T) {
	// Arrange — an owner is not something a sender supplies, so an empty one is a bug in the
	// caller rather than bad input. Refused here as well as in Inbox: a task with no owner is
	// a task with no budget to charge and nobody to show it to.
	tasks, _ := channelTasks(t)

	// Act
	_, err := tasks.FromChannel(context.Background(), channelMessage("  ", "Check yesterday's revenue"))

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestTasksFromChannel_refusesAnEmptyOrOversizedMessage(t *testing.T) {
	// Arrange — both are checked in the ladder before this port is reached, and both are
	// checked again here, because this port is public to the rest of core and the next caller
	// may not be Inbox.
	cases := map[string]string{
		"nothing to act on":          "   \n\t ",
		"longer than a brief may be": strings.Repeat("a", domain.MaxBriefLength+1),
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			tasks, chats := channelTasks(t)

			// Act
			_, err := tasks.FromChannel(context.Background(), channelMessage("user-1", text))

			// Assert
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			// And nothing was written on the way to refusing.
			if len(chats.chats) != 0 {
				t.Errorf("chats = %d, want none created for a refused message", len(chats.chats))
			}
		})
	}
}

func TestTasksFromChannel_reportsAStoreFailureAsItself(t *testing.T) {
	// Arrange
	tasks, chats := channelTasks(t)
	chats.failEnsure = errBoom

	// Act
	_, err := tasks.FromChannel(context.Background(), channelMessage("user-1", "Check yesterday's revenue"))

	// Assert — not a validation error: nothing about the message was wrong. Inbox turns this
	// into an apology, and it can only do that if it is not told the sender is at fault.
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Error("a store failure was reported as a bad message")
	}
}
