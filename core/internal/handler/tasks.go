package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/utils"
)

// HeaderIdempotencyKey is where the goal engine sends the key.
//
// It sends it in the header as well as the body deliberately — see
// goal-engine/internal/core/client.go, which keeps it out of the query string so it does
// not end up in an access log. Core accepts it either way.
const HeaderIdempotencyKey = "Idempotency-Key"

// dispatchRequest is the goal engine's payload, field for field.
//
// This struct is a contract with another service, not a convenience: the names are the
// ones goal-engine/internal/core/client.go writes. Renaming one here breaks the trigger
// bridge silently, because an omitted field decodes as empty rather than as an error.
type dispatchRequest struct {
	BotID          string            `json:"botId"`
	ChannelID      string            `json:"channelId"`
	Brief          string            `json:"brief"`
	IdempotencyKey string            `json:"idempotencyKey"`
	Metadata       map[string]string `json:"metadata"`
}

// sendRequest is a person saying something to their agent.
type sendRequest struct {
	// ChatID may be omitted, in which case a chat is made for this message. That keeps
	// the first message of a conversation to one round trip.
	ChatID string `json:"chatId"`
	Text   string `json:"text"`
}

// taskView is a unit of work as a client sees it.
//
// The id is at data.id in the envelope, which is what the goal engine's client reads
// first. That is a contract: see extractTaskID in goal-engine/internal/core/client.go.
type taskView struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Status string `json:"status"`
	// BotID and ChannelID are which persona ran and where to report. They are empty on a
	// task a person started from a chat, which has both answers already.
	BotID          string            `json:"botId,omitempty"`
	ChannelID      string            `json:"channelId,omitempty"`
	Brief          string            `json:"brief"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
}

func viewTask(task domain.Task) taskView {
	return taskView{
		ID:             task.ID,
		Source:         string(task.Source),
		Status:         string(task.Status),
		BotID:          task.BotID,
		ChannelID:      task.ChannelID,
		Brief:          task.Brief,
		IdempotencyKey: task.IdempotencyKey,
		Metadata:       task.Metadata,
		CreatedAt:      task.CreatedAt,
	}
}

func viewTasks(tasks []domain.Task) []taskView {
	views := make([]taskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, viewTask(task))
	}
	return views
}

// sentView is what a client needs after saying something: where it went, what was stored,
// and the task that will answer it.
type sentView struct {
	Chat    chatView    `json:"chat"`
	Message messageView `json:"message"`
	Task    taskView    `json:"task"`
}

// DispatchTask queues work nobody is watching. It serves both POST /api/v1/tasks — the
// path the goal engine is hardcoded to call — and POST /v1/tasks.
//
// A retry answers 200 with the original task; a new task answers 201. The difference is
// what keeps a timed-out trigger that was received from looking like a fresh one in either
// service's logs.
func (h *Handler) DispatchTask(c *gin.Context) {
	var req dispatchRequest
	if !bindJSON(c, &req) {
		return
	}

	key, ok := idempotencyKey(c, req.IdempotencyKey)
	if !ok {
		// Answered rather than resolved. Picking one of the two would mean the caller's
		// retry logic and core's deduplication are keyed on different strings, which is
		// exactly the state in which a retry becomes a second run.
		badRequest(c, "The Idempotency-Key header and idempotencyKey in the body disagree. Send one, or send the same value in both.")
		return
	}

	dispatched, err := h.tasks.Dispatch(c.Request.Context(), service.Dispatch{
		BotID:          req.BotID,
		ChannelID:      req.ChannelID,
		Brief:          req.Brief,
		IdempotencyKey: key,
		Metadata:       req.Metadata,
	}, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}

	if !dispatched.Created {
		utils.Success(c, http.StatusOK, "That task was already queued under this idempotency key.",
			viewTask(dispatched.Task))
		return
	}
	utils.Success(c, http.StatusCreated, "Task queued.", viewTask(dispatched.Task))
}

// SendMessage records what a person said and queues the work of answering it.
func (h *Handler) SendMessage(c *gin.Context) {
	var req sendRequest
	if !bindJSON(c, &req) {
		return
	}

	sent, err := h.tasks.Send(c.Request.Context(), service.Send{
		ChatID: req.ChatID,
		Text:   req.Text,
	}, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusCreated, "Message sent.", sentView{
		Chat:    viewChat(sent.Chat),
		Message: viewMessage(sent.Message),
		Task:    viewTask(sent.Task),
	})
}

// GetTask reads one task.
func (h *Handler) GetTask(c *gin.Context) {
	task, err := h.tasks.Get(c.Request.Context(), c.Param("id"), actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	utils.Success(c, http.StatusOK, "Task found.", viewTask(task))
}

// ListTasks renders a page of the caller's tasks.
//
// There is no route guard on it: the service scopes by actor, giving a person their own
// tasks and a dispatcher the unattended account's. A guard here would have to duplicate
// that decision, and two copies of a scoping rule is how one of them ends up wrong.
func (h *Handler) ListTasks(c *gin.Context) {
	limit, offset := page(c)

	tasks, total, err := h.tasks.List(c.Request.Context(), limit, offset, actorFrom(c))
	if err != nil {
		h.respondError(c, err)
		return
	}
	paged(c, "Tasks listed.", viewTasks(tasks), limit, offset, total)
}

// idempotencyKey reads the key from the header or the body, and reports false when both
// are present and disagree.
//
// Trimmed before comparing, because a proxy that adds a trailing space to a header would
// otherwise turn every retry into a 400.
func idempotencyKey(c *gin.Context, fromBody string) (string, bool) {
	header := strings.TrimSpace(c.GetHeader(HeaderIdempotencyKey))
	body := strings.TrimSpace(fromBody)

	switch {
	case header == "":
		return body, true
	case body == "":
		return header, true
	case header != body:
		return "", false
	default:
		return header, true
	}
}
