package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// titleRequest names or renames a chat. Both routes take the same one field, so they take
// the same struct — a second identical type would be two places to add a field to.
type titleRequest struct {
	Title string `json:"title"`
}

// chatView is a conversation as a client lists it.
//
// It does not carry userId. Every chat a caller can see is their own — the service scopes
// on the account in the WHERE clause rather than filtering afterwards — so rendering the
// id would add a field that is either constant or a bug.
type chatView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ArchivedAt is null on a live chat. It is rendered as well as isArchived because the
	// answer to "when did this get put away?" is in the audit trail, not just the flag.
	ArchivedAt *time.Time `json:"archivedAt"`
	IsArchived bool       `json:"isArchived"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

func viewChat(chat repository.Chat) chatView {
	return chatView{
		ID:         chat.ID,
		Title:      chat.Title,
		ArchivedAt: chat.ArchivedAt,
		IsArchived: chat.ArchivedAt != nil,
		CreatedAt:  chat.CreatedAt,
		UpdatedAt:  chat.UpdatedAt,
	}
}

func viewChats(chats []repository.Chat) []chatView {
	views := make([]chatView, 0, len(chats))
	for _, chat := range chats {
		views = append(views, viewChat(chat))
	}
	return views
}

// messageView is one turn.
//
// runId is present so a client can put an assistant message next to the transcript that
// produced it — which is how somebody checks what their agent actually did rather than
// what it said it did. It is empty on anything a person typed.
type messageView struct {
	ID        string    `json:"id"`
	ChatID    string    `json:"chatId"`
	RunID     string    `json:"runId,omitempty"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TokensIn  int64     `json:"tokensIn"`
	TokensOut int64     `json:"tokensOut"`
	CreatedAt time.Time `json:"createdAt"`
}

func viewMessage(message repository.Message) messageView {
	return messageView{
		ID:        message.ID,
		ChatID:    message.ChatID,
		RunID:     message.RunID,
		Role:      string(message.Role),
		Content:   message.Content,
		TokensIn:  message.TokensIn,
		TokensOut: message.TokensOut,
		CreatedAt: message.CreatedAt,
	}
}

func viewMessages(messages []repository.Message) []messageView {
	views := make([]messageView, 0, len(messages))
	for _, message := range messages {
		views = append(views, viewMessage(message))
	}
	return views
}

// CreateChat starts an empty conversation.
func (h *Handler) CreateChat(c *gin.Context) {
	var req titleRequest
	if !bindJSON(c, &req) {
		return
	}

	chat, err := h.chats.Create(c.Request.Context(), req.Title, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Chat created.", viewChat(chat))
}

// GetChat reads one conversation.
func (h *Handler) GetChat(c *gin.Context) {
	chat, err := h.chats.Get(c.Request.Context(), c.Param("id"), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Chat found.", viewChat(chat))
}

// ListChats renders a page of the caller's conversations.
//
// Archived ones are left out unless ?includeArchived=true. boolQuery treats an
// unparseable value as false, which here means the quieter list — a client that sends
// nonsense gets the default view rather than an error or a surprise.
func (h *Handler) ListChats(c *gin.Context) {
	limit, offset := page(c)

	chats, total, err := h.chats.List(c.Request.Context(), boolQuery(c, "includeArchived"), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Chats listed.", viewChats(chats), limit, offset, total)
}

// RenameChat changes a chat's title and nothing else.
func (h *Handler) RenameChat(c *gin.Context) {
	var req titleRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.chats.Rename(c.Request.Context(), c.Param("id"), req.Title, actorFrom(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Chat renamed.", gin.H{"id": c.Param("id"), "title": req.Title})
}

// ArchiveChat puts a chat away. There is no delete route, deliberately: see the service,
// where the reasoning about append-only transcripts lives.
func (h *Handler) ArchiveChat(c *gin.Context) {
	if err := h.chats.Archive(c.Request.Context(), c.Param("id"), actorFrom(c)); err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Chat archived.", gin.H{"id": c.Param("id"), "isArchived": true})
}

// ChatMessages renders what was said in one chat, oldest first.
func (h *Handler) ChatMessages(c *gin.Context) {
	limit, offset := page(c)

	messages, total, err := h.chats.Messages(c.Request.Context(), c.Param("id"), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Messages listed.", viewMessages(messages), limit, offset, total)
}
