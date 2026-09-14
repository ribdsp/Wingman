package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ribdsp/wingman/core/internal/repository"
)

// chatsFixture wires the service over one fake standing in for both stores.
type chatsFixture struct {
	chats *Chats
	store *fakeChats
}

func newChatsFixture(t *testing.T) chatsFixture {
	t.Helper()

	store := newFakeChats()
	chats, err := NewChats(ChatsDeps{
		Chats:    store,
		Messages: store,
		Clock:    fixedClock(testNow),
		Logger:   silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewChats: %v", err)
	}
	return chatsFixture{chats: chats, store: store}
}

func TestNewChats_namesEveryMissingDependencyAtOnce(t *testing.T) {
	// Act
	_, err := NewChats(ChatsDeps{})

	// Assert
	if err == nil {
		t.Fatal("err = nil, want a complaint about the wiring")
	}
	for _, name := range []string{"Chats", "Messages"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err = %v, want it to name %s", err, name)
		}
	}
}

func TestChatsCreate_acceptsNoTitleAndNamesItForNow(t *testing.T) {
	// Arrange — a client that opens a chat before the person has typed anything has nothing
	// to name it after yet.
	f := newChatsFixture(t)

	// Act
	chat, err := f.chats.Create(context.Background(), "   ", userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if chat.Title != "New chat" {
		t.Errorf("title = %q, want the placeholder", chat.Title)
	}
	if chat.UserID != "user-1" {
		t.Errorf("userId = %q, want the caller's", chat.UserID)
	}
}

func TestChatsCreate_flattensATitleThatWouldBreakAList(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)

	// Act
	chat, err := f.chats.Create(context.Background(), "  first line\n\tsecond   line  ", userActor("user-1"))

	// Assert — a newline in a title is a display problem, not something worth refusing.
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if chat.Title != "first line second line" {
		t.Errorf("title = %q, want it flattened to one line", chat.Title)
	}
}

func TestChatsCreate_refusesATitleTooLongToRender(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)

	// Act
	_, err := f.chats.Create(context.Background(), strings.Repeat("é", maxChatTitleLength+1), userActor("user-1"))

	// Assert — counted in runes, so a title of accented characters is not half the length
	// of the same title in ASCII.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("err = %v, want it to name the field", err)
	}
}

func TestChatsCreate_atTheLimitIsAccepted(t *testing.T) {
	// Arrange — the bound is inclusive, and an off-by-one here refuses a title a client's
	// own character counter said was fine.
	f := newChatsFixture(t)

	// Act
	chat, err := f.chats.Create(context.Background(), strings.Repeat("a", maxChatTitleLength), userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len([]rune(chat.Title)) != maxChatTitleLength {
		t.Errorf("title length = %d, want %d", len([]rune(chat.Title)), maxChatTitleLength)
	}
}

func TestChatsGet_readsTheCallersOwnChatOnly(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	mine, err := f.chats.Create(context.Background(), "Mine", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	theirs, err := f.chats.Create(context.Background(), "Theirs", userActor("user-2"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	got, mineErr := f.chats.Get(context.Background(), mine.ID, userActor("user-1"))
	_, theirsErr := f.chats.Get(context.Background(), theirs.ID, userActor("user-1"))

	// Assert
	if mineErr != nil {
		t.Fatalf("Get own: %v", mineErr)
	}
	if got.ID != mine.ID {
		t.Errorf("id = %q, want %q", got.ID, mine.ID)
	}
	// Somebody else's chat and a chat that does not exist are one answer, so the id itself
	// leaks nothing.
	if !errors.Is(theirsErr, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", theirsErr)
	}
	if errors.Is(theirsErr, repository.ErrNotFound) {
		t.Error("the repository's sentinel leaked out of the service")
	}
}

func TestChatsGet_requiresAnId(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)

	// Act
	_, err := f.chats.Get(context.Background(), "  ", userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestChatsList_leavesArchivedChatsOutUnlessAsked(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	kept, err := f.chats.Create(context.Background(), "Kept", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	away, err := f.chats.Create(context.Background(), "Away", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.chats.Archive(context.Background(), away.ID, userActor("user-1")); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Act
	visible, visibleTotal, err := f.chats.List(context.Background(), false, 10, 0, userActor("user-1"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	all, allTotal, err := f.chats.List(context.Background(), true, 10, 0, userActor("user-1"))
	if err != nil {
		t.Fatalf("List including archived: %v", err)
	}

	// Assert — archiving is how somebody clears a list they have to read, so honouring it
	// by default is the whole point of the field.
	if len(visible) != 1 || visible[0].ID != kept.ID {
		t.Fatalf("visible = %v, want only %q", visible, kept.ID)
	}
	if visibleTotal != 1 {
		t.Errorf("total = %d, want 1", visibleTotal)
	}
	if len(all) != 2 || allTotal != 2 {
		t.Errorf("all = %d (total %d), want 2", len(all), allTotal)
	}
}

func TestChatsRename_requiresATitleWhereCreateDoesNot(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Before", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	err = f.chats.Rename(context.Background(), chat.ID, "   ", userActor("user-1"))

	// Assert — an unnamed new chat is a normal state; a rename to nothing is a mistake.
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("err = %v, want it to say the title is required", err)
	}
	if f.store.chats[chat.ID].Title != "Before" {
		t.Errorf("title = %q, want it unchanged", f.store.chats[chat.ID].Title)
	}
}

func TestChatsRename_writesTheNewTitle(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Before", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	err = f.chats.Rename(context.Background(), chat.ID, "  After  ", userActor("user-1"))

	// Assert
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := f.store.chats[chat.ID].Title; got != "After" {
		t.Errorf("title = %q, want %q", got, "After")
	}
}

func TestChatsRename_cannotRetitleSomebodyElsesChat(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Theirs", userActor("user-2"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	err = f.chats.Rename(context.Background(), chat.ID, "Mine now", userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if f.store.chats[chat.ID].Title != "Theirs" {
		t.Errorf("title = %q, want it unchanged", f.store.chats[chat.ID].Title)
	}
}

func TestChatsArchive_stampsTheClockAndIsSafeToRepeat(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Away", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	first := f.chats.Archive(context.Background(), chat.ID, userActor("user-1"))
	second := f.chats.Archive(context.Background(), chat.ID, userActor("user-1"))

	// Assert — the caller wanted it gone, and it is gone. A conflict on the second call
	// would make a retried request look like a failure.
	if first != nil {
		t.Fatalf("Archive: %v", first)
	}
	if second != nil {
		t.Fatalf("Archive again: %v", second)
	}
	archivedAt := f.store.chats[chat.ID].ArchivedAt
	if archivedAt == nil {
		t.Fatal("archivedAt is nil, want the clock's time")
	}
	if !archivedAt.Equal(testNow) {
		t.Errorf("archivedAt = %s, want %s", archivedAt, testNow)
	}
}

func TestChatsMessages_readTheCallersOwnConversationIncludingToolRows(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Mine", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, role := range []repository.MessageRole{
		repository.MessageRoleUser,
		repository.MessageRoleAssistant,
		repository.MessageRoleTool,
		repository.MessageRoleSystem,
	} {
		if _, err := f.store.AppendMessage(context.Background(), repository.NewMessage{
			ChatID: chat.ID, UserID: "user-1", Role: role, Content: string(role) + " said something",
		}); err != nil {
			t.Fatalf("seed a %s message: %v", role, err)
		}
	}

	// Act
	messages, total, err := f.chats.Messages(context.Background(), chat.ID, 10, 0, userActor("user-1"))

	// Assert — somebody reading their own conversation should be able to see that the agent
	// went and looked something up.
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 4 || total != 4 {
		t.Fatalf("messages = %d (total %d), want all four roles", len(messages), total)
	}
	if messages[0].Role != repository.MessageRoleUser {
		t.Errorf("first role = %q, want the oldest message first", messages[0].Role)
	}
}

func TestChatsMessages_cannotReadSomebodyElsesConversation(t *testing.T) {
	// Arrange
	f := newChatsFixture(t)
	chat, err := f.chats.Create(context.Background(), "Theirs", userActor("user-2"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	_, _, err = f.chats.Messages(context.Background(), chat.ID, 10, 0, userActor("user-1"))

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestChats_everyMethodRefusesAMachineKey(t *testing.T) {
	// There is deliberately no operator view of somebody else's chats: an operator can read
	// a run's transcript, which is what auditing the agent needs, without reading the
	// conversation around it.
	for _, actor := range []Actor{operatorActor(), botActor()} {
		t.Run(string(actor.Type), func(t *testing.T) {
			// Arrange
			f := newChatsFixture(t)

			// Act
			errs := map[string]error{}
			_, errs["Create"] = f.chats.Create(context.Background(), "Title", actor)
			_, errs["Get"] = f.chats.Get(context.Background(), "chat-1", actor)
			_, _, errs["List"] = f.chats.List(context.Background(), false, 10, 0, actor)
			errs["Rename"] = f.chats.Rename(context.Background(), "chat-1", "Title", actor)
			errs["Archive"] = f.chats.Archive(context.Background(), "chat-1", actor)
			_, _, errs["Messages"] = f.chats.Messages(context.Background(), "chat-1", 10, 0, actor)

			// Assert
			for name, err := range errs {
				if !errors.Is(err, ErrForbidden) {
					t.Errorf("%s err = %v, want ErrForbidden", name, err)
				}
			}
			if len(f.store.chats) != 0 {
				t.Errorf("chats = %d, want none created", len(f.store.chats))
			}
		})
	}
}

func TestChats_defaultsTheClockWhenNoneIsGiven(t *testing.T) {
	// Arrange — cmd passes one; a caller that forgets must not archive at the zero time,
	// which would read as 1 January year 1 in every list.
	store := newFakeChats()
	chats, err := NewChats(ChatsDeps{Chats: store, Messages: store})
	if err != nil {
		t.Fatalf("NewChats: %v", err)
	}
	chat, err := chats.Create(context.Background(), "Away", userActor("user-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Act
	before := time.Now()
	if err := chats.Archive(context.Background(), chat.ID, userActor("user-1")); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Assert
	archivedAt := store.chats[chat.ID].ArchivedAt
	if archivedAt == nil {
		t.Fatal("archivedAt is nil, want a real time")
	}
	if archivedAt.Before(before) {
		t.Errorf("archivedAt = %s, want a time at or after %s", archivedAt, before)
	}
}
