package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ribdsp/wingman/core/internal/domain"
)

func chatRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "user_id", "title", "archived_at", "created_at", "updated_at"})
}

func messageRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "chat_id", "user_id", "run_id", "role", "content",
		"tokens_in", "tokens_out", "created_at",
	})
}

func TestChatRepository_createChat_trimsTheTitle(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`INSERT INTO chats`).
		WithArgs("usr_01", "Quarterly numbers").
		WillReturnRows(chatRows().
			AddRow("cht_01", "usr_01", "Quarterly numbers", nil, fixedNow, fixedNow))

	// Act
	chat, err := repo.CreateChat(context.Background(), "usr_01", "  Quarterly numbers\n")

	// Assert
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if chat.ArchivedAt != nil {
		t.Error("a new chat came back archived")
	}
	if chat.Title != "Quarterly numbers" {
		t.Errorf("title = %q; want it trimmed", chat.Title)
	}
}

// The user id is the first parameter and appears in the WHERE clause. A test that only
// checked the returned chat would still pass if the scoping were dropped, so this one
// matches on the SQL.
func TestChatRepository_getChat_scopesTheReadToTheOwner(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`FROM chats WHERE id = \$2 AND user_id = \$1`).
		WithArgs("usr_01", "cht_01").
		WillReturnRows(chatRows().AddRow("cht_01", "usr_01", "Quarterly numbers", nil, fixedNow, fixedNow))

	// Act
	chat, err := repo.GetChat(context.Background(), "usr_01", "cht_01")

	// Assert
	if err != nil {
		t.Fatalf("get chat: %v", err)
	}
	if chat.UserID != "usr_01" {
		t.Errorf("chat belongs to %q; want usr_01", chat.UserID)
	}
}

func TestChatRepository_getChat_anotherAccountsChatIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`FROM chats WHERE id = \$2 AND user_id = \$1`).
		WithArgs("usr_02", "cht_01").
		WillReturnRows(chatRows())

	// Act
	_, err := repo.GetChat(context.Background(), "usr_02", "cht_01")

	// Assert
	// A chat id is a uuid somebody could hold from a shared link or an old response.
	// "Not yours" and "no such chat" answer the same so holding one teaches nothing.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get another account's chat = %v; want ErrNotFound", err)
	}
}

func TestChatRepository_getChat_malformedIDIsNotFoundRatherThanAnError(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`FROM chats WHERE id = \$2 AND user_id = \$1`).
		WithArgs("usr_01", "not-a-uuid").
		WillReturnError(pgError(pgInvalidTextRepr))

	// Act
	_, err := repo.GetChat(context.Background(), "usr_01", "not-a-uuid")

	// Assert
	// A uuid that is not a uuid can never match a row, so it is the same answer as an
	// id that does not exist rather than a 500 that says the input was inspected.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get chat with a malformed id = %v; want ErrNotFound", err)
	}
}

func TestChatRepository_listChats_excludesArchivedByDefault(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`count\(\*\) FROM chats\s+WHERE user_id = \$1 AND \(\$2 OR archived_at IS NULL\)`).
		WithArgs("usr_01", false).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM chats\s+WHERE user_id = \$1 AND \(\$2 OR archived_at IS NULL\)`).
		WithArgs("usr_01", false, 20, 0).
		WillReturnRows(chatRows().AddRow("cht_01", "usr_01", "Live", nil, fixedNow, fixedNow))

	// Act
	chats, total, err := repo.ListChats(context.Background(), "usr_01", false, 20, 0)

	// Assert
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if total != 1 || len(chats) != 1 {
		t.Fatalf("got %d of %d chats; want 1 of 1", len(chats), total)
	}
}

func TestChatRepository_listChats_includeArchivedPassesTheFlagToBothQueries(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	archived := fixedNow
	mock.ExpectQuery(`count\(\*\) FROM chats`).
		WithArgs("usr_01", true).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`FROM chats\s+WHERE user_id = \$1`).
		WithArgs("usr_01", true, defaultPageLimit, 0).
		WillReturnRows(chatRows().
			AddRow("cht_02", "usr_01", "Live", nil, fixedNow, fixedNow).
			AddRow("cht_01", "usr_01", "Old", archived, fixedNow, fixedNow))

	// Act
	chats, total, err := repo.ListChats(context.Background(), "usr_01", true, 0, 0)

	// Assert
	// The count and the page have to agree about what is being counted, or a UI shows
	// "2 chats" above a list of one.
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if total != 2 || len(chats) != 2 {
		t.Fatalf("got %d of %d chats; want 2 of 2", len(chats), total)
	}
	if chats[1].ArchivedAt == nil {
		t.Error("the archived chat came back unarchived")
	}
}

func TestChatRepository_archiveChat_alreadyArchivedIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectExec(`UPDATE chats SET archived_at = \$3\s+WHERE id = \$2 AND user_id = \$1 AND archived_at IS NULL`).
		WithArgs("usr_01", "cht_01", fixedNow).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.ArchiveChat(context.Background(), "usr_01", "cht_01", fixedNow)

	// Assert
	// `archived_at IS NULL` is what keeps a second archive from moving the timestamp,
	// which would misdate when the conversation actually stopped being used.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive an archived chat = %v; want ErrNotFound", err)
	}
}

func TestChatRepository_renameChat_anotherAccountsChatIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectExec(`UPDATE chats SET title = \$3 WHERE id = \$2 AND user_id = \$1`).
		WithArgs("usr_02", "cht_01", "Renamed").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Act
	err := repo.RenameChat(context.Background(), "usr_02", "cht_01", " Renamed ")

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename another account's chat = %v; want ErrNotFound", err)
	}
}

// This is the test that pins AppendMessage's shape. The ownership check is inside the
// INSERT — `INSERT … SELECT … FROM chats WHERE c.id = $1 AND c.user_id = $2` — rather
// than a SELECT before it, because a check-then-write leaves a window in which a message
// lands in a conversation the caller does not own. Matching the SQL is the only way to
// notice if somebody replaces it with the two-step version.
func TestChatRepository_appendMessage_reChecksOwnershipInsideTheInsert(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`INSERT INTO messages .*\s+SELECT c.id, \$2, \$3, \$4, \$5, \$6, \$7\s+FROM chats c\s+WHERE c.id = \$1 AND c.user_id = \$2`).
		WithArgs("cht_01", "usr_01", nil, "user", "how are the numbers?", int64(0), int64(0)).
		WillReturnRows(messageRows().
			AddRow("msg_01", "cht_01", "usr_01", nil, "user", "how are the numbers?", 0, 0, fixedNow))

	// Act
	message, err := repo.AppendMessage(context.Background(), NewMessage{
		ChatID: "cht_01", UserID: "usr_01",
		Role: MessageRoleUser, Content: "how are the numbers?",
	})

	// Assert
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	if message.RunID != "" {
		t.Errorf("runId = %q; want empty on a message a person typed", message.RunID)
	}
}

func TestChatRepository_appendMessage_unownedChatMatchesNothingAndIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`INSERT INTO messages`).
		WithArgs("cht_01", "usr_02", nil, "user", "hello", int64(0), int64(0)).
		WillReturnRows(messageRows())

	// Act
	_, err := repo.AppendMessage(context.Background(), NewMessage{
		ChatID: "cht_01", UserID: "usr_02",
		Role: MessageRoleUser, Content: "hello",
	})

	// Assert
	// The SELECT matched nothing, so no row was inserted at all. That is the point of
	// putting the check there: the write cannot happen against somebody else's chat.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to another account's chat = %v; want ErrNotFound", err)
	}
}

func TestChatRepository_appendMessage_boundsTheContentBeforeTheWrite(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	oversized := strings.Repeat("x", domain.MaxStepContentLength+2_000)
	bounded := domain.TruncateContent(oversized)
	mock.ExpectQuery(`INSERT INTO messages`).
		WithArgs("cht_01", "usr_01", "run_01", "assistant", bounded, int64(120), int64(340)).
		WillReturnRows(messageRows().
			AddRow("msg_01", "cht_01", "usr_01", "run_01", "assistant", bounded, 120, 340, fixedNow))

	// Act
	message, err := repo.AppendMessage(context.Background(), NewMessage{
		ChatID: "cht_01", UserID: "usr_01", RunID: "run_01",
		Role: MessageRoleAssistant, Content: oversized,
		TokensIn: 120, TokensOut: 340,
	})

	// Assert
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	// A tool that returns a whole log file must not be able to write it into a row a
	// prompt is later rebuilt from — the history is re-sent on every iteration, so an
	// unbounded message is paid for repeatedly.
	if len([]rune(message.Content)) > domain.MaxStepContentLength+len([]rune("\n… truncated")) {
		t.Errorf("stored %d characters; want it bounded", len([]rune(message.Content)))
	}
	if message.RunID != "run_01" {
		t.Errorf("runId = %q; want the run that produced it", message.RunID)
	}
}

func TestChatRepository_listMessages_readsOldestFirst(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`count\(\*\) FROM messages WHERE chat_id = \$2 AND user_id = \$1`).
		WithArgs("usr_01", "cht_01").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`FROM messages\s+WHERE chat_id = \$2 AND user_id = \$1\s+ORDER BY created_at ASC`).
		WithArgs("usr_01", "cht_01", defaultPageLimit, 0).
		WillReturnRows(messageRows().
			AddRow("msg_01", "cht_01", "usr_01", nil, "user", "question", 0, 0, fixedNow).
			AddRow("msg_02", "cht_01", "usr_01", "run_01", "assistant", "answer", 40, 90, fixedNow))

	// Act
	messages, total, err := repo.ListMessages(context.Background(), "usr_01", "cht_01", 0, 0)

	// Assert
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if total != 2 || len(messages) != 2 {
		t.Fatalf("got %d of %d messages; want 2 of 2", len(messages), total)
	}
	// A conversation read newest-first is a conversation nobody can follow.
	if messages[0].Role != MessageRoleUser || messages[1].Role != MessageRoleAssistant {
		t.Errorf("messages came back out of order: %+v", messages)
	}
}

func TestChatRepository_listMessages_countFailureStopsTheRead(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`count\(\*\) FROM messages`).
		WithArgs("usr_01", "cht_01").
		WillReturnError(errors.New("connection reset"))

	// Act
	messages, total, err := repo.ListMessages(context.Background(), "usr_01", "cht_01", 0, 0)

	// Assert
	if err == nil {
		t.Fatal("a failed count was reported as a successful list")
	}
	if messages != nil || total != 0 {
		t.Errorf("got %v / %d alongside the error; want nothing", messages, total)
	}
}

// RecentMessages is what the agent loop rebuilds a prompt from, so the bound on it is a
// spending control rather than a pagination detail: the history is re-sent on every
// iteration, and an unbounded read would be paid for once per iteration.
func TestChatRepository_recentMessages_boundsTheHistoryAndReturnsItOldestFirst(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`FROM messages\s+WHERE chat_id = \$2 AND user_id = \$1\s+ORDER BY created_at DESC, id DESC\s+LIMIT \$3`).
		WithArgs("usr_01", "cht_01", maxPageLimit).
		WillReturnRows(messageRows().
			AddRow("msg_01", "cht_01", "usr_01", nil, "user", "question", 0, 0, fixedNow).
			AddRow("msg_02", "cht_01", "usr_01", "run_01", "assistant", "answer", 40, 90, fixedNow))

	// Act
	messages, err := repo.RecentMessages(context.Background(), "usr_01", "cht_01", 100_000)

	// Assert
	if err != nil {
		t.Fatalf("read recent messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages; want 2", len(messages))
	}
	// The inner query takes the newest rows; the outer one puts them back in reading
	// order. A prompt assembled in reverse is a prompt that describes the conversation
	// backwards.
	if messages[0].ID != "msg_01" {
		t.Errorf("history starts at %q; want the oldest of the window", messages[0].ID)
	}
}

func TestChatRepository_recentMessages_zeroLimitTakesTheDefaultNotEverything(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`FROM messages`).
		WithArgs("usr_01", "cht_01", defaultPageLimit).
		WillReturnRows(messageRows())

	// Act
	messages, err := repo.RecentMessages(context.Background(), "usr_01", "cht_01", 0)

	// Assert
	// Zero reads as "no limit" in SQL if it is passed through, which is the mistake the
	// whole limits design exists to prevent.
	if err != nil {
		t.Fatalf("read recent messages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("got %d messages from an empty chat; want 0", len(messages))
	}
}

func TestChatRepository_ensureChannelChat_createsTheThreadOnFirstMessage(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`INSERT INTO chats`).
		WithArgs("usr_01", "Telegram", "telegram", "884422").
		WillReturnRows(chatRows().AddRow("cht_01", "usr_01", "Telegram", nil, fixedNow, fixedNow))

	// Act
	chat, err := repo.EnsureChannelChat(context.Background(), ChannelChat{
		UserID: "usr_01", Kind: ChannelTelegram, ConversationID: " 884422 ", Title: " Telegram ",
	})

	// Assert
	if err != nil {
		t.Fatalf("ensure channel chat: %v", err)
	}
	if chat.ID != "cht_01" {
		t.Fatalf("chat id = %q, want cht_01", chat.ID)
	}
}

func TestChatRepository_ensureChannelChat_refusesAKindTheEnumDoesNotHave(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)

	// Act
	_, err := repo.EnsureChannelChat(context.Background(), ChannelChat{
		UserID: "usr_01", Kind: "whatsapp", ConversationID: "6281",
	})

	// Assert
	if err == nil {
		t.Fatal("an unknown channel kind was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestChatRepository_ensureChannelChat_refusesAnEmptyConversationID(t *testing.T) {
	// Arrange
	db, _ := newMock(t)
	repo := NewChatRepository(db)

	// Act
	_, err := repo.EnsureChannelChat(context.Background(), ChannelChat{
		UserID: "usr_01", Kind: ChannelDiscord, ConversationID: "   ",
	})

	// Assert
	// A chat with a kind and no conversation id has nowhere to send an answer, which
	// is why the schema refuses it too.
	if err == nil {
		t.Fatal("a channel chat with no conversation was accepted")
	}
}

func TestChatRepository_channelTargetFor_saysWhereToAnswer(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`SELECT channel_kind, channel_conversation_id`).
		WithArgs("usr_01", "cht_01").
		WillReturnRows(sqlmock.NewRows([]string{"channel_kind", "channel_conversation_id"}).
			AddRow("slack", "C024BE91L"))

	// Act
	target, err := repo.ChannelTargetFor(context.Background(), "usr_01", "cht_01")

	// Assert
	if err != nil {
		t.Fatalf("channel target: %v", err)
	}
	if !target.Deliverable() {
		t.Fatal("a channel chat reported nowhere to deliver")
	}
	if target.Kind != ChannelSlack || target.ConversationID != "C024BE91L" {
		t.Fatalf("target = %+v, want the slack conversation", target)
	}
}

func TestChatRepository_channelTargetFor_aWebChatIsNotDeliverableAndNotAnError(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`SELECT channel_kind, channel_conversation_id`).
		WithArgs("usr_01", "cht_01").
		WillReturnRows(sqlmock.NewRows([]string{"channel_kind", "channel_conversation_id"}).
			AddRow(nil, ""))

	// Act
	target, err := repo.ChannelTargetFor(context.Background(), "usr_01", "cht_01")

	// Assert
	// Most chats are not channel conversations. The runner asks about every one it
	// answers, so "nowhere to deliver" has to be an ordinary answer.
	if err != nil {
		t.Fatalf("channel target: %v", err)
	}
	if target.Deliverable() {
		t.Fatalf("target = %+v, want nothing to deliver to", target)
	}
}

func TestChatRepository_channelTargetFor_anotherAccountsChatIsNotFound(t *testing.T) {
	// Arrange
	db, mock := newMock(t)
	repo := NewChatRepository(db)
	mock.ExpectQuery(`SELECT channel_kind, channel_conversation_id`).
		WithArgs("usr_02", "cht_01").
		WillReturnError(sql.ErrNoRows)

	// Act
	_, err := repo.ChannelTargetFor(context.Background(), "usr_02", "cht_01")

	// Assert
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
