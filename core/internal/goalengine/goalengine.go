// Package goalengine talks to the Wingman goal engine.
//
// Core is the half that does the work; the goal engine is the half that decides
// whether work should happen at all. Three of its endpoints matter here:
//
//	GET  /v1/flags/kill-switch        may anything run
//	POST /v1/approvals                may this money be spent
//	POST /v1/metrics/<key>/samples    what this run cost
//
// This is deliberately the only place in core that knows those paths, so when the
// contract moves there is one package to change — the mirror image of
// goal-engine/internal/core, which is the only place over there that knows core's.
//
// Every method fails closed by construction: it answers with an error rather than a
// permissive value, and its callers read an error as "stop" rather than "go ahead".
// That is why Engaged returns (bool, error) instead of a bool — a switch nobody can
// read is not a switch that is off — and why a spending call that cannot be filed
// refuses the call instead of making it.
package goalengine

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
	// KillSwitchPath is where the engine reports whether everything is halted.
	KillSwitchPath = "/v1/flags/kill-switch"

	// ApprovalsPath is where a spending call is filed. The engine always answers
	// 201 with a decision inside, including when the decision is no.
	ApprovalsPath = "/v1/approvals"

	// DefaultTimeout bounds one request. It is short because the kill switch is
	// read before every iteration of every run: a goal engine that has stopped
	// answering must make runs halt quickly, not hold workers open.
	DefaultTimeout = 15 * time.Second

	// maxErrorBody caps how much of a failed response is kept. An error page can be
	// megabytes of HTML and none of it belongs in a log line.
	maxErrorBody = 512

	// maxResponseBody caps a successful body too. Every response this package reads
	// is a small envelope, and the ceiling is what stops a wrong endpoint from
	// being read into memory.
	maxResponseBody = 1 << 20
)

// Error is a failed call, carrying whether trying again could plausibly help.
//
// Body is the engine's own message, truncated. It is included because that message
// is how an operator learns which policy refused a spend; it is the engine's
// envelope, not a driver error, and it carries no credential — the API key travels
// in a header and is never echoed. TestClient_noErrorEverQuotesTheAPIKey holds that
// line.
type Error struct {
	StatusCode int
	Retryable  bool
	Body       string
	Err        error
}

func (e *Error) Error() string {
	switch {
	case e.StatusCode > 0 && e.Body != "":
		return fmt.Sprintf("goal engine returned %d: %s", e.StatusCode, e.Body)
	case e.StatusCode > 0 && e.Err != nil:
		return fmt.Sprintf("goal engine returned %d but the body was unusable: %v", e.StatusCode, e.Err)
	case e.StatusCode > 0:
		return fmt.Sprintf("goal engine returned %d", e.StatusCode)
	default:
		return fmt.Sprintf("goal engine unreachable: %v", e.Err)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether err describes a condition that might clear on its
// own. A rejected request is not one: retrying a 400 forever only fills the log.
func IsRetryable(err error) bool {
	var engineErr *Error
	if errors.As(err, &engineErr) {
		return engineErr.Retryable
	}
	return false
}

// retryableStatus mirrors goal-engine/internal/core.retryableStatus, so both ends of
// the same wire agree on what is worth another attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// Client is core's connection to the goal engine.
type Client struct {
	baseURL string
	apiKey  string
	metric  string
	http    *http.Client
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

// WithHTTPClient replaces the underlying client, for tests and for callers that need
// their own transport.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

// WithSpendMetric names the push metric that token spend is reported to.
//
// Without it ReportTokens refuses rather than guessing a metric name: a sample filed
// under a key the operator did not declare is rejected by the engine anyway, and a
// default here would hide which of the two ends was misconfigured.
func WithSpendMetric(metric string) Option {
	return func(c *Client) { c.metric = strings.TrimSpace(metric) }
}

// New builds a client for the goal engine at baseURL.
//
// An instance with no goal engine configured does not build one of these at all —
// see Absent for the kill switch it uses instead, and note that its spending gate is
// nil, which the agent loop reads as "nothing may spend money".
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("goalengine: base url is required")
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return nil, fmt.Errorf("goalengine: base url %q must start with http:// or https://", baseURL)
	}

	client := &Client{
		baseURL: trimmed,
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

// do sends one request and decodes the envelope into out, which may be nil when the
// response body says nothing the caller needs.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure may be a restart or a blip, so it is worth retrying.
		// What it is never worth is reading as an answer.
		return &Error{Retryable: true, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{
			StatusCode: resp.StatusCode,
			Retryable:  retryableStatus(resp.StatusCode),
			Body:       truncate(string(raw), maxErrorBody),
		}
	}
	if readErr != nil {
		return &Error{StatusCode: resp.StatusCode, Retryable: true, Err: fmt.Errorf("read response: %w", readErr)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// Not retryable. The same request gets the same unreadable answer, and the
		// likely cause is something other than the goal engine on that address.
		return &Error{StatusCode: resp.StatusCode, Err: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}

// truncate shortens s to at most limit bytes, marking that it was cut.
func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
