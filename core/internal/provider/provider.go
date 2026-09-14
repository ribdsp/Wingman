// Package provider is Wingman core's model port and its two implementations.
//
// The port exists so the run loop never sees a vendor SDK. Both vendors describe the
// same conversation — turns, tool calls, tool results, a token count — in types that
// share no shape, and a loop written against either one would have that vendor's
// assumptions built into the decisions it makes about budgets and tools. So the
// neutral types here are the ones the loop and the transcript are written against,
// and each adapter translates in one file.
//
// Two things are deliberately absent. There is no streaming: nothing in stage one
// reads a reply as it arrives, and a streaming port would double the surface every
// adapter has to get right for a feature no caller has. And there is no retry loop —
// both SDKs already retry inside a single call, and the decision to try a *step*
// again belongs to the loop that knows what the run has spent.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Names of the providers New can build.
//
// These are the values DEFAULT_PROVIDER accepts. internal/config validates against
// its own copy of the same two strings — it cannot import this package without
// pulling both vendor SDKs into configuration loading — so a test here asserts the
// two agree. A name config accepts but New rejects is a service that boots cleanly
// and then fails at the first model call.
const (
	NameAnthropic = "anthropic"
	NameOpenAI    = "openai"
)

// defaultMaxRetries is how many times a single call is retried inside the SDK
// before Complete returns. It matches both SDKs' own default; it is set explicitly
// rather than inherited so an SDK upgrade cannot change how long a step can take.
const defaultMaxRetries = 2

// defaultMaxOutputTokens caps one response.
//
// The ceiling the continuation ladder passes in is what is left of a run's whole
// allowance, which is routinely larger than any model will emit in one reply — and
// both vendors reject a ceiling above the model's own limit outright. 4096 is the
// lowest ceiling the shipping models accept, so a deployment that never sets
// Options.MaxOutputTokens gets truncated replies rather than a 400 on every call.
const defaultMaxOutputTokens int64 = 4096

// Role is who produced a turn.
//
// There is no system role. A system prompt is Request.System, because one vendor
// takes it as a top-level field and the other as a message in the list, and hiding
// that difference is precisely what the adapters are for.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	// ID is the vendor's handle for this call. The result has to quote it back, so
	// it is carried verbatim rather than renumbered.
	ID   string
	Name string
	// Input is the arguments as JSON. It stays raw here: this package has no idea
	// what any tool's arguments mean, and decoding them into a map only to encode
	// them again would be a chance to lose a field.
	Input json.RawMessage
}

// ToolResult is what running a tool produced, on its way back to the model.
type ToolResult struct {
	// CallID is the ToolCall.ID this answers.
	CallID  string
	Content string
	// IsError marks a tool that failed. The model is told so it can try something
	// else; a failure passed back as ordinary output invites it to treat an error
	// message as data.
	IsError bool
}

// Message is one turn of the conversation.
//
// An assistant turn may carry text and tool calls together — that is how both
// vendors report a model that explains itself before acting. The user turn that
// follows carries the results.
type Message struct {
	Role Role
	Text string
	// ToolCalls is set on assistant turns only.
	ToolCalls []ToolCall
	// ToolResults is set on user turns only, answering the calls in the assistant
	// turn immediately before.
	ToolResults []ToolResult
}

// ToolSchema is one tool offered to the model.
type ToolSchema struct {
	Name string
	// Description is what the model reads to decide whether this is the right tool.
	Description string
	// InputSchema is a JSON Schema object describing the arguments. It is passed
	// through as the operator wrote it: a tool's schema arrives from an MCP server
	// or an OpenAPI document, and rewriting it here would mean this package
	// deciding what a third party's contract means.
	InputSchema map[string]any
}

// Request is one model call.
type Request struct {
	// Model is the vendor's model id. It is never defaulted in this package —
	// vendors retire ids on their own schedule, and a constant compiled into a
	// release fails with a message about the vendor rather than about the config.
	Model string
	// System is the system prompt, or empty.
	System   string
	Messages []Message
	Tools    []ToolSchema
	// MaxTokens is the ceiling this call may spend, as the continuation ladder
	// computed it. The adapters clamp it to Options.MaxOutputTokens and send the
	// smaller of the two.
	//
	// It bounds output tokens only, because that is the only ceiling either vendor
	// accepts, while the ladder's allowance covers input as well. That makes it a
	// conservative bound rather than an exact one, which is the right direction: a
	// run can stop with allowance unspent, but it cannot overshoot by one
	// expensive call.
	MaxTokens int64
}

// Validate reports whether a request can be sent at all.
//
// This runs before the network, so a malformed call costs nothing and names its own
// problem instead of arriving as a vendor 400 that has to be decoded.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("provider: request has no model")
	}
	if r.MaxTokens < 1 {
		// Zero would be sent as a max_tokens of zero, which both vendors reject —
		// but the reason to stop here is that a run continuing with no allowance
		// left is a ladder mistake, and it should surface as one.
		return fmt.Errorf("provider: request allows %d output tokens; a call needs at least 1", r.MaxTokens)
	}
	if len(r.Messages) == 0 {
		return errors.New("provider: request has no messages")
	}

	seen := make(map[string]struct{}, len(r.Tools))
	for i, tool := range r.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return fmt.Errorf("provider: tool %d has no name", i)
		}
		if _, duplicate := seen[name]; duplicate {
			// Both vendors accept a duplicate and then answer with a call the
			// registry cannot resolve to one grant, which would mean deciding a
			// tool's class by whichever entry a map iteration reached first.
			return fmt.Errorf("provider: tool %q is offered twice", name)
		}
		seen[name] = struct{}{}
	}

	for i, message := range r.Messages {
		switch message.Role {
		case RoleUser, RoleAssistant:
		default:
			return fmt.Errorf("provider: message %d has role %q, which is neither user nor assistant", i, message.Role)
		}
		for _, result := range message.ToolResults {
			if strings.TrimSpace(result.CallID) == "" {
				// A result with no call id is a result the model cannot match to
				// what it asked for, and both vendors reject the turn.
				return fmt.Errorf("provider: message %d carries a tool result with no call id", i)
			}
		}
		for _, call := range message.ToolCalls {
			if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
				return fmt.Errorf("provider: message %d carries a tool call with no id or name", i)
			}
			if len(call.Input) > 0 && !json.Valid(call.Input) {
				// Checked here so both adapters can decode without guessing. The
				// arguments were the model's own JSON on the way in, so bytes that
				// are not JSON mean something rewrote them between the two calls.
				return fmt.Errorf("provider: arguments of tool call %s are not JSON", call.ID)
			}
		}
	}
	return nil
}

// Usage is what a call cost.
type Usage struct {
	TokensIn  int64
	TokensOut int64
}

// Total is what the call is billed at for budget purposes.
func (u Usage) Total() int64 { return u.TokensIn + u.TokensOut }

// Finish is why one model response ended.
//
// It is deliberately not a domain.StopReason. A vendor's reason for ending a
// response and a run's reason for stopping are different facts — "the model asked
// for a tool" is not an outcome — and translating one into the other inside an
// adapter would put a safety decision in the least examined layer. The loop reads
// this and asks the domain ladder what it means.
type Finish string

const (
	// FinishAnswered means the model replied and wants nothing further.
	FinishAnswered Finish = "answered"
	// FinishToolUse means the response carries at least one tool call.
	FinishToolUse Finish = "tool_use"
	// FinishTruncated means the reply hit the output ceiling and is partial.
	FinishTruncated Finish = "truncated"
	// FinishPaused means the vendor stopped a long turn and expects to be asked to
	// continue. The instruction is the same as truncation's; the cause is not, and
	// the cause is what an operator needs when the two happen at different rates.
	FinishPaused Finish = "paused"
	// FinishRefused means the model would not answer. That is an outcome rather
	// than a failure: the identical prompt earns the identical refusal, so retrying
	// only spends tokens.
	FinishRefused Finish = "refused"
	// FinishUnusable means the response ended for a reason the loop cannot act on:
	// input too large for the context window, or a value this package does not
	// recognise.
	//
	// Unrecognised lands here rather than under FinishAnswered because a new vendor
	// value read as success would be a run reported complete on a reply that never
	// arrived — the same reason the goal engine spells skipped_invalid_sample
	// instead of folding it into noop.
	FinishUnusable Finish = "unusable"
)

// Response is one model reply.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Finish    Finish
	Usage     Usage
	// Model is what the vendor says answered, which is not always what was asked
	// for — an alias resolves to a dated id. The transcript records this one, so a
	// run remains explainable after the alias moves.
	Model string
}

// Client is the port. Both adapters implement it and nothing else does.
type Client interface {
	// Name is the provider name, for the transcript and the spend ledger.
	Name() string
	// Complete makes one model call. The context bounds it: the loop passes the
	// run's step timeout, and that also bounds the SDK's internal retries.
	Complete(ctx context.Context, req Request) (Response, error)
}

// Options configures a client. It carries a credential, so it is never logged.
type Options struct {
	APIKey string
	// BaseURL overrides the vendor endpoint. Empty means the vendor's own. It
	// exists for the test suite, which points it at an httptest server, and for a
	// deployment that routes model calls through a gateway of its own.
	BaseURL string
	// MaxRetries is how many times the SDK retries one call. Zero means
	// defaultMaxRetries; negative disables retrying.
	MaxRetries int
	// MaxOutputTokens caps a single response. Zero means defaultMaxOutputTokens.
	// Raise it for a model that allows more; the cost of it being too low is a
	// truncated reply, and of it being too high is a rejected call.
	MaxOutputTokens int64
}

func (o Options) withDefaults() Options {
	if o.MaxRetries == 0 {
		o.MaxRetries = defaultMaxRetries
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.MaxOutputTokens < 1 {
		o.MaxOutputTokens = defaultMaxOutputTokens
	}
	return o
}

func (o Options) validate(name string) error {
	if strings.TrimSpace(o.APIKey) == "" {
		// Failing at construction rather than at the first call is the same rule
		// internal/config follows: a service that runs autonomous agents should
		// refuse to start half-configured instead of discovering a missing
		// credential four tool calls into a run.
		return fmt.Errorf("provider %s: no API key was configured", name)
	}
	if o.BaseURL != "" && !strings.HasPrefix(o.BaseURL, "http://") && !strings.HasPrefix(o.BaseURL, "https://") {
		// The URL is not echoed: an operator who pasted a credentialled endpoint
		// would otherwise have it in a startup log.
		return fmt.Errorf("provider %s: base URL must start with http:// or https://", name)
	}
	return nil
}

// New builds the named provider.
//
// The switch is the only place a name becomes a client, so an unrecognised one is
// refused here rather than defaulting to whichever provider happens to be first.
func New(name string, opts Options) (Client, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case NameAnthropic:
		return NewAnthropic(opts)
	case NameOpenAI:
		return NewOpenAI(opts)
	default:
		return nil, fmt.Errorf("provider %q is not %s or %s", name, NameAnthropic, NameOpenAI)
	}
}

// Error is a model call that failed.
type Error struct {
	Provider string
	// Status is the HTTP status the vendor answered with, or 0 when the request
	// never got a response.
	Status int
	// Kind is the vendor's machine-readable error type when it gave one —
	// "rate_limit_error", "invalid_request_error", "insufficient_quota". It is kept
	// because it separates "you are going too fast" from "your key is wrong"
	// without needing the response body.
	Kind string
	// Retryable is whether trying the same step again could work. It is set by
	// failure, which is the only place the question is answered.
	Retryable bool
	// Cause is the transport failure, when there was one.
	//
	// It is nil for an error the vendor answered with, and that is deliberate: the
	// SDK's own error message appends the raw response body, and the body of a
	// rejected request quotes the request back — which here means somebody's brief
	// and whatever a tool read on their behalf. Status and Kind are enough to act
	// on without putting a prompt in a log file.
	Cause error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Provider)
	b.WriteString(": model call failed")
	if e.Status != 0 {
		fmt.Fprintf(&b, " with status %d", e.Status)
	}
	if e.Kind != "" {
		fmt.Fprintf(&b, " (%s)", e.Kind)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes the transport failure so errors.Is can find a context deadline.
func (e *Error) Unwrap() error { return e.Cause }

// failure classifies a failed call. It is the one place retryability is decided.
func failure(provider string, status int, kind string, cause error) *Error {
	err := &Error{
		Provider:  provider,
		Status:    status,
		Kind:      kind,
		Retryable: retryableStatus(status),
		Cause:     cause,
	}

	switch {
	case cause != nil && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)):
		// The call ran out of the time the run gave it. Another attempt has no more
		// time than the last one did, and the deadline is the caller's decision.
		err.Retryable = false
	case kind == "insufficient_quota":
		// A 429 that means "the account has no credit" rather than "slow down".
		// Retrying it burns a run's whole retry budget against a bill nobody has
		// paid, and the answer never changes until somebody does.
		err.Retryable = false
	}
	return err
}

// retryableStatus mirrors the list both SDKs retry on: no response at all, 408, 409,
// 429 and anything from 500 up.
//
// By the time an error reaches this function the SDK has already exhausted its own
// attempts, so what is being answered is whether the *loop* should run the step
// again later. The two lists agree on purpose: a status the SDK judged transient
// within one call is transient across two.
func retryableStatus(status int) bool {
	switch {
	case status == 0:
		// No status means the request never reached a server, or the answer never
		// came back. A network that is briefly gone is the commonest cause.
		return true
	case status == http.StatusRequestTimeout,
		status == http.StatusConflict,
		status == http.StatusTooManyRequests:
		return true
	case status >= http.StatusInternalServerError:
		return true
	default:
		// Including 401 and 403. A key that is wrong now is wrong in a minute, and
		// retrying an authentication failure is how a misconfigured deployment
		// turns into a rate-limit ban.
		return false
	}
}

// Retryable reports whether a failed model call is worth trying again.
//
// An error this package did not produce answers false. That is the fail-closed
// reading: an unrecognised failure retried in a loop is an agent spending money on
// something that is not going to start working, and the run stopping with
// provider_error sends somebody to look at it.
func Retryable(err error) bool {
	var providerErr *Error
	if errors.As(err, &providerErr) {
		return providerErr.Retryable
	}
	return false
}
