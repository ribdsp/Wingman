// Package core talks to Wingman core — the service in core/ that actually runs the
// agents.
//
// This is the trigger bridge's outbound edge: the goal engine decides that a goal
// is off track, and this package is what turns that decision into an agent task.
// It is deliberately the only place that knows Wingman core's HTTP contract, so
// when that contract moves there is exactly one file to change.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTaskPath is where Wingman core accepts a new task. It is overridable
	// so that core moving its route is a config edit here, not a redeploy of this
	// service — the two deploy independently and will not always be in step.
	DefaultTaskPath = "/api/v1/tasks"

	// DefaultNotifyPath is where Wingman core accepts a notification. There is no
	// /api/v1 alias for it: that alias exists for tasks alone, because this service
	// hardcoded the task path before core had a router.
	DefaultNotifyPath = "/v1/notifications"

	// DefaultTimeout bounds a single dispatch attempt. Waking an agent is not
	// urgent, but hanging on it would stall the whole monitor tick.
	DefaultTimeout = 20 * time.Second

	// maxErrorBody caps how much of a failed response is kept. An error page can
	// be megabytes of HTML, and none of it belongs in a log line or a database
	// column.
	maxErrorBody = 512
)

// TaskRequest is one instruction for an agent.
type TaskRequest struct {
	// BotID is the persistent bot that should act.
	BotID string `json:"botId"`
	// ChannelID is where the conversation happens, so a human can read along.
	ChannelID string `json:"channelId"`
	// Brief is the instruction, in the operator's own terms.
	Brief string `json:"brief"`
	// IdempotencyKey lets core reject a duplicate even if this service retries
	// after a response was lost in transit.
	IdempotencyKey string `json:"idempotencyKey"`
	// Metadata carries the goal context. It is informational: policy decisions are
	// made here, never delegated to the agent.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// TaskResponse is what core reported back.
type TaskResponse struct {
	// TaskID is core's identifier, when it gave one.
	TaskID string
	// StatusCode is the HTTP status, recorded for the audit trail.
	StatusCode int
	// DryRun is true when no request was actually sent.
	DryRun bool
}

// NotifyKind names what happened. Core routes on it, so it is a closed set rather
// than free text.
type NotifyKind string

const (
	// NotifyTrigger says an agent was woken for a goal that fell behind.
	NotifyTrigger NotifyKind = "trigger"
	// NotifyApprovalPending says a spend is waiting for a human.
	NotifyApprovalPending NotifyKind = "approval_pending"
)

// Valid reports whether core will recognise this kind.
func (k NotifyKind) Valid() bool {
	switch k {
	case NotifyTrigger, NotifyApprovalPending:
		return true
	}
	return false
}

// NotifyRequest tells core that something happened which a human should hear
// about. Core decides who to tell and on which chat platform; this service only
// says what happened.
//
// There is deliberately no amount, currency, metric value or pace figure in this
// struct, and adding one is a change of posture rather than a field. Two reasons,
// both worth the constraint: the message ends up retained on a third party's
// servers, whereas the business numbers are supposed to stay on the box; and a
// message complete enough to act on from the notification itself invites approving
// a spend at a glance, which is the failure this project is most exposed to. The
// link is what makes somebody open the console, where the numbers are.
type NotifyRequest struct {
	// Kind is what happened.
	Kind NotifyKind `json:"kind"`
	// SubjectID identifies the thing that happened — a goal id or a spend request
	// id — so the recipient can find it in the console.
	SubjectID string `json:"subjectId"`
	// Headline is one short sentence, already fit to send.
	Headline string `json:"headline"`
	// Link points at the console. Empty when WEB_BASE_URL is not configured, and
	// omitted rather than sent blank.
	Link string `json:"link,omitempty"`
}

// Error is a failed dispatch, carrying whether retrying could plausibly help.
type Error struct {
	StatusCode int
	Retryable  bool
	Body       string
	Err        error
}

func (e *Error) Error() string {
	switch {
	case e.StatusCode > 0 && e.Body != "":
		return fmt.Sprintf("wingman core returned %d: %s", e.StatusCode, e.Body)
	case e.StatusCode > 0:
		return fmt.Sprintf("wingman core returned %d", e.StatusCode)
	default:
		return fmt.Sprintf("wingman core unreachable: %v", e.Err)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether err is a dispatch failure worth attempting again.
// A rejected request is not: retrying a 400 forever just fills the log.
func IsRetryable(err error) bool {
	var coreErr *Error
	if errors.As(err, &coreErr) {
		return coreErr.Retryable
	}
	return false
}

// Client dispatches tasks to Wingman core.
type Client struct {
	baseURL    string
	apiKey     string
	taskPath   string
	notifyPath string
	dryRun     bool
	http       *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout bounds a single request.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.http.Timeout = timeout
		}
	}
}

// WithTaskPath overrides the task-creation path.
func WithTaskPath(path string) Option {
	return func(c *Client) {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			c.taskPath = "/" + strings.TrimLeft(trimmed, "/")
		}
	}
}

// WithNotifyPath overrides the notification path.
func WithNotifyPath(path string) Option {
	return func(c *Client) {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			c.notifyPath = "/" + strings.TrimLeft(trimmed, "/")
		}
	}
}

// WithDryRun makes every dispatch a no-op that reports success.
//
// This is the safety valve for a system whose whole point is acting without being
// asked: the full loop — monitor, evaluate, decide, record — can be exercised
// against production metrics without a single agent waking up. It suppresses
// notifications too, because a dry run that still messages the operator's phone is
// not a dry run.
func WithDryRun(dryRun bool) Option {
	return func(c *Client) { c.dryRun = dryRun }
}

// WithHTTPClient replaces the underlying client, for tests and for callers that
// need their own transport.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// New builds a client for the core service at baseURL.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("core: base url is required")
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return nil, fmt.Errorf("core: base url %q must start with http:// or https://", baseURL)
	}

	client := &Client{
		baseURL:    trimmed,
		apiKey:     strings.TrimSpace(apiKey),
		taskPath:   DefaultTaskPath,
		notifyPath: DefaultNotifyPath,
		http:       &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

// DryRun reports whether dispatches are being suppressed.
func (c *Client) DryRun() bool { return c.dryRun }

// CreateTask asks Wingman core to wake an agent.
func (c *Client) CreateTask(ctx context.Context, req TaskRequest) (TaskResponse, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return TaskResponse{}, errors.New("core: idempotency key is required")
	}
	if c.dryRun {
		return TaskResponse{StatusCode: http.StatusOK, DryRun: true}, nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return TaskResponse{}, fmt.Errorf("core: encode task: %w", err)
	}

	// Sent as a header rather than a query parameter so the credential does not
	// end up in core's access log.
	resp, err := c.post(ctx, c.taskPath, body, map[string]string{"Idempotency-Key": req.IdempotencyKey})
	if err != nil {
		return TaskResponse{}, err
	}
	if resp.bodyErr != nil {
		// Core accepted the task; only the body was lost. Reporting this as
		// retryable would create a second task for work already under way.
		return TaskResponse{StatusCode: resp.statusCode}, nil
	}

	return TaskResponse{TaskID: extractTaskID(resp.body), StatusCode: resp.statusCode}, nil
}

// Notify tells core that something happened which a human should hear about.
//
// It returns an error and no response value on purpose: there is nothing here
// worth recording. A notification is not a decision, so the caller's contract is
// to fire this after the decision is already durable and to log a failure rather
// than act on it — the approval is still in the queue, the console still shows it,
// and its TTL still expires it, all of which is a better safety net than a retry.
func (c *Client) Notify(ctx context.Context, req NotifyRequest) error {
	if !req.Kind.Valid() {
		return fmt.Errorf("core: notification kind %q is not one core accepts", req.Kind)
	}
	if strings.TrimSpace(req.SubjectID) == "" {
		return errors.New("core: notification subject id is required")
	}
	if strings.TrimSpace(req.Headline) == "" {
		return errors.New("core: notification headline is required")
	}
	if c.dryRun {
		return nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("core: encode notification: %w", err)
	}

	// The response body is not read for anything: how many chat platforms core
	// reached is core's log line, not this service's business.
	_, err = c.post(ctx, c.notifyPath, body, nil)
	return err
}

// response is one answer from core. bodyErr is set when the request itself
// succeeded but the body could not be read, which is a different thing from the
// request failing and the two callers treat it differently.
type response struct {
	statusCode int
	body       []byte
	bodyErr    error
}

// post sends body to path with the configured credential. A transport failure and
// a non-2xx status both come back as *Error carrying whether a retry could help.
func (c *Client) post(ctx context.Context, path string, body []byte, headers map[string]string) (response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return response{}, fmt.Errorf("core: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for name, value := range headers {
		httpReq.Header.Set(name, value)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure may be a restart or a blip, so it is worth retrying.
		return response{}, &Error{Retryable: true, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return response{}, &Error{
			StatusCode: resp.StatusCode,
			Retryable:  retryableStatus(resp.StatusCode),
			Body:       truncate(string(raw), maxErrorBody),
		}
	}

	return response{statusCode: resp.StatusCode, body: raw, bodyErr: readErr}, nil
}

// retryableStatus reports whether a status code describes a condition that might
// clear on its own.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// taskEnvelope covers the shapes core may use for a created task. Core deploys
// separately from this service, so a moved field must not fail a dispatch that
// actually succeeded — the task id is for the audit trail, not for correctness.
type taskEnvelope struct {
	ID     string `json:"id"`
	TaskID string `json:"taskId"`
	Data   struct {
		ID     string `json:"id"`
		TaskID string `json:"taskId"`
	} `json:"data"`
}

// extractTaskID returns the first identifier the response offers, or "" when it
// offers none.
func extractTaskID(raw []byte) string {
	var envelope taskEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	for _, candidate := range []string{envelope.ID, envelope.TaskID, envelope.Data.ID, envelope.Data.TaskID} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// truncate shortens s to at most limit bytes, marking that it was cut.
func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
