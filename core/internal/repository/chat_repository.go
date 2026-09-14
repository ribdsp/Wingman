package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ribdsp/wingman/core/internal/domain"
)

// Chat is one conversation.
type Chat struct {
	ID         string
	UserID     string
	Title      string
	ArchivedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Message is one turn in a conversation.
type Message struct {
	ID     string
	ChatID string
	UserID string
	// RunID is set on an assistant message produced by an agent run, and empty on
	// anything a person typed.
	RunID     string
	Role      MessageRole
	Content   string
	TokensIn  int64
	TokensOut int64
	CreatedAt time.Time
}

// MessageRole mirrors the message_role enum.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleSystem    MessageRole = "system"
	MessageRoleTool      MessageRole = "tool"
)

// ChatRepository stores conversations and their messages.
//
// Every method takes a user id, and every query names it in the WHERE clause. A chat id
// is a uuid somebody could hold from a shared link or an old response, so ownership is
// enforced by the query rather than checked after the row comes back.
type ChatRepository struct {
	db *sqlx.DB
}

// NewChatRepository builds a repository over the given pool.
func NewChatRepository(db *sqlx.DB) *ChatRepository {
	return &ChatRepository{db: db}
}

const chatColumns = `id, user_id, title, archived_at, created_at, updated_at`

type chatRow struct {
	ID         string       `db:"id"`
	UserID     string       `db:"user_id"`
	Title      string       `db:"title"`
	ArchivedAt sql.NullTime `db:"archived_at"`
	CreatedAt  time.Time    `db:"created_at"`
	UpdatedAt  time.Time    `db:"updated_at"`
}

func (r chatRow) toChat() Chat {
	return Chat{
		ID:         r.ID,
		UserID:     r.UserID,
		Title:      r.Title,
		ArchivedAt: timePtr(r.ArchivedAt),
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
}

// CreateChat starts a conversation.
func (r *ChatRepository) CreateChat(ctx context.Context, userID, title string) (Chat, error) {
	const query = `
		INSERT INTO chats (user_id, title)
		VALUES ($1, $2)
		RETURNING ` + chatColumns

	var row chatRow
	err := r.db.QueryRowxContext(ctx, query, userID, strings.TrimSpace(title)).StructScan(&row)
	if err != nil {
		return Chat{}, fmt.Errorf("create chat for user %s: %w", userID, classify(err))
	}
	return row.toChat(), nil
}

// ChannelChat identifies the conversation behind an inbound channel message.
type ChannelChat struct {
	UserID         string
	Kind           ChannelKind
	ConversationID string
	// Title is used only if the chat is being created. A person who renamed the
	// thread keeps their name for it.
	Title string
}

// ChannelTarget is where a chat's answers go. A zero value means the chat is not a
// channel conversation — the web client's chats are not, and that is not an error.
type ChannelTarget struct {
	Kind           ChannelKind
	ConversationID string
}

// Deliverable reports whether there is a channel to answer on.
func (t ChannelTarget) Deliverable() bool {
	return t.Kind != "" && t.ConversationID != ""
}

// EnsureChannelChat returns the chat for a channel conversation, creating it the first
// time somebody speaks there.
//
// Upsert rather than select-then-insert, because two messages arriving together would
// otherwise both find nothing and both create a chat, and the second insert would be
// the one that fails. The conflict target names the partial index's predicate because
// Postgres needs it to infer a partial index.
//
// DO UPDATE sets the title to what it already is. That looks like a no-op and is one,
// deliberately: DO NOTHING returns no row, so there would have to be a second query,
// and overwriting the title with a freshly generated one would rename a thread somebody
// had named themselves. The row's updated_at still moves, because the trigger fires on
// any update — which is what floats an active conversation to the top of the list.
func (r *ChatRepository) EnsureChannelChat(ctx context.Context, input ChannelChat) (Chat, error) {
	const query = `
		INSERT INTO chats (user_id, title, channel_kind, channel_conversation_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, channel_kind, channel_conversation_id)
			WHERE channel_kind IS NOT NULL
			DO UPDATE SET title = chats.title
		RETURNING ` + chatColumns

	if err := validChannelKind(input.Kind); err != nil {
		return Chat{}, err
	}
	conversationID := strings.TrimSpace(input.ConversationID)
	if conversationID == "" {
		// The check constraint would refuse this too. Refusing here names the field.
		return Chat{}, errors.New("channel chat: conversation id is required")
	}

	var row chatRow
	err := r.db.QueryRowxContext(ctx, query,
		input.UserID, strings.TrimSpace(input.Title), string(input.Kind), conversationID,
	).StructScan(&row)
	if err != nil {
		return Chat{}, fmt.Errorf("ensure %s chat for user %s: %w", input.Kind, input.UserID, classify(err))
	}
	return row.toChat(), nil
}

// ChannelTargetFor says where an answer to this chat should be sent.
//
// It exists so the agent runner never learns what a channel is: it knows the chat it
// answered in, asks where that chat lives, and hands the text to whoever delivers
// there. The user id is in the WHERE clause like everywhere else — a chat id from
// somewhere else must not reveal which Telegram conversation it belongs to.
func (r *ChatRepository) ChannelTargetFor(ctx context.Context, userID, chatID string) (ChannelTarget, error) {
	const query = `
		SELECT channel_kind, channel_conversation_id
		FROM chats WHERE id = $2 AND user_id = $1`

	var row struct {
		Kind           sql.NullString `db:"channel_kind"`
		ConversationID string         `db:"channel_conversation_id"`
	}
	if err := r.db.QueryRowxContext(ctx, query, userID, chatID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelTarget{}, ErrNotFound
		}
		return ChannelTarget{}, fmt.Errorf("channel target for chat %s: %w", chatID, classify(err))
	}
	return ChannelTarget{
		Kind:           ChannelKind(row.Kind.String),
		ConversationID: row.ConversationID,
	}, nil
}

// GetChat returns one of the user's chats, or ErrNotFound. A chat belonging to
// somebody else answers ErrNotFound too — the caller learns nothing about whether the
// id exists.
func (r *ChatRepository) GetChat(ctx context.Context, userID, chatID string) (Chat, error) {
	const query = `SELECT ` + chatColumns + ` FROM chats WHERE id = $2 AND user_id = $1`

	var row chatRow
	if err := r.db.QueryRowxContext(ctx, query, userID, chatID).StructScan(&row); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Chat{}, ErrNotFound
		}
		return Chat{}, fmt.Errorf("get chat %s: %w", chatID, classify(err))
	}
	return row.toChat(), nil
}

// ListChats returns a user's chats, most recently updated first. Archived chats are
// excluded unless asked for.
func (r *ChatRepository) ListChats(ctx context.Context, userID string, includeArchived bool, limit, offset int) ([]Chat, int, error) {
	const countQuery = `
		SELECT count(*) FROM chats
		WHERE user_id = $1 AND ($2 OR archived_at IS NULL)`
	const listQuery = `
		SELECT ` + chatColumns + `
		FROM chats
		WHERE user_id = $1 AND ($2 OR archived_at IS NULL)
		ORDER BY updated_at DESC, id DESC
		LIMIT $3 OFFSET $4`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery, userID, includeArchived).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count chats for user %s: %w", userID, classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []chatRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, userID, includeArchived, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list chats for user %s: %w", userID, classify(err))
	}

	chats := make([]Chat, 0, len(rows))
	for _, row := range rows {
		chats = append(chats, row.toChat())
	}
	return chats, total, nil
}

// RenameChat sets a chat's title.
func (r *ChatRepository) RenameChat(ctx context.Context, userID, chatID, title string) error {
	const query = `UPDATE chats SET title = $3 WHERE id = $2 AND user_id = $1`

	result, err := r.db.ExecContext(ctx, query, userID, chatID, strings.TrimSpace(title))
	if err != nil {
		return fmt.Errorf("rename chat %s: %w", chatID, classify(err))
	}
	return requireOneRow(result, chatID)
}

// ArchiveChat hides a chat from the list without destroying it.
//
// Archiving rather than deleting: a run that cited this conversation is auditable only
// while the conversation still exists, and a person clearing their sidebar is not
// asking to make an agent's past reasoning unreadable.
func (r *ChatRepository) ArchiveChat(ctx context.Context, userID, chatID string, at time.Time) error {
	const query = `
		UPDATE chats SET archived_at = $3
		WHERE id = $2 AND user_id = $1 AND archived_at IS NULL`

	result, err := r.db.ExecContext(ctx, query, userID, chatID, at)
	if err != nil {
		return fmt.Errorf("archive chat %s: %w", chatID, classify(err))
	}
	return requireOneRow(result, chatID)
}

// NewMessage is what AppendMessage needs.
type NewMessage struct {
	ChatID string
	UserID string
	// RunID attributes an assistant message to the run that produced it. Empty for
	// anything a person typed; the schema refuses a run id on any other role.
	RunID     string
	Role      MessageRole
	Content   string
	TokensIn  int64
	TokensOut int64
}

// AppendMessage adds a turn to a conversation.
//
// The chat's ownership is re-checked in the INSERT itself rather than by a SELECT
// first: a check-then-write leaves a window, and the window here would let a message
// land in a conversation the caller does not own.
func (r *ChatRepository) AppendMessage(ctx context.Context, input NewMessage) (Message, error) {
	const query = `
		INSERT INTO messages (chat_id, user_id, run_id, role, content, tokens_in, tokens_out)
		SELECT c.id, $2, $3, $4, $5, $6, $7
		FROM chats c
		WHERE c.id = $1 AND c.user_id = $2
		RETURNING id, chat_id, user_id, run_id, role, content, tokens_in, tokens_out, created_at`

	var row messageRow
	err := r.db.QueryRowxContext(ctx, query,
		input.ChatID, input.UserID, nullIfEmpty(input.RunID), string(input.Role),
		domain.TruncateContent(input.Content), input.TokensIn, input.TokensOut,
	).StructScan(&row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The SELECT matched nothing, so the chat does not exist or is not this
			// user's. Both are ErrNotFound.
			return Message{}, ErrNotFound
		}
		return Message{}, fmt.Errorf("append message to chat %s: %w", input.ChatID, classify(err))
	}
	return row.toMessage(), nil
}

// ListMessages returns a conversation in order, oldest first, which is how it is read.
func (r *ChatRepository) ListMessages(ctx context.Context, userID, chatID string, limit, offset int) ([]Message, int, error) {
	const countQuery = `SELECT count(*) FROM messages WHERE chat_id = $2 AND user_id = $1`
	const listQuery = `
		SELECT id, chat_id, user_id, run_id, role, content, tokens_in, tokens_out, created_at
		FROM messages
		WHERE chat_id = $2 AND user_id = $1
		ORDER BY created_at ASC, id ASC
		LIMIT $3 OFFSET $4`

	var total int
	if err := r.db.QueryRowContext(ctx, countQuery, userID, chatID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count messages in chat %s: %w", chatID, classify(err))
	}

	limit, offset = normalisePage(limit, offset)
	rows := []messageRow{}
	if err := r.db.SelectContext(ctx, &rows, listQuery, userID, chatID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list messages in chat %s: %w", chatID, classify(err))
	}

	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, row.toMessage())
	}
	return messages, total, nil
}

// RecentMessages returns the tail of a conversation, oldest first, for building a
// model prompt.
//
// The limit is what stops a long conversation from becoming an expensive one: the
// prompt is rebuilt on every iteration of the agent loop, so an unbounded history is
// paid for repeatedly.
func (r *ChatRepository) RecentMessages(ctx context.Context, userID, chatID string, limit int) ([]Message, error) {
	const query = `
		SELECT id, chat_id, user_id, run_id, role, content, tokens_in, tokens_out, created_at
		FROM (
			SELECT * FROM messages
			WHERE chat_id = $2 AND user_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $3
		) recent
		ORDER BY created_at ASC, id ASC`

	limit, _ = normalisePage(limit, 0)
	rows := []messageRow{}
	if err := r.db.SelectContext(ctx, &rows, query, userID, chatID, limit); err != nil {
		return nil, fmt.Errorf("read recent messages in chat %s: %w", chatID, classify(err))
	}

	messages := make([]Message, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, row.toMessage())
	}
	return messages, nil
}

type messageRow struct {
	ID        string         `db:"id"`
	ChatID    string         `db:"chat_id"`
	UserID    string         `db:"user_id"`
	RunID     sql.NullString `db:"run_id"`
	Role      string         `db:"role"`
	Content   string         `db:"content"`
	TokensIn  int64          `db:"tokens_in"`
	TokensOut int64          `db:"tokens_out"`
	CreatedAt time.Time      `db:"created_at"`
}

func (r messageRow) toMessage() Message {
	return Message{
		ID:        r.ID,
		ChatID:    r.ChatID,
		UserID:    r.UserID,
		RunID:     r.RunID.String,
		Role:      MessageRole(r.Role),
		Content:   r.Content,
		TokensIn:  r.TokensIn,
		TokensOut: r.TokensOut,
		CreatedAt: r.CreatedAt,
	}
}
