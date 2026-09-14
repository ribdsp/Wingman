// Package tool is what an agent run can actually do.
//
// A tool has three parts, kept apart on purpose:
//
//   - the operator's declaration of it — its class and whether it is on — which
//     lives in YAML and is loaded by LoadGrants;
//   - a Runner that knows how to execute it, one per source (the built-in shell,
//     an MCP server, a declared HTTP API);
//   - the Registry, which offers the model only the tools that have a grant and a
//     runner, and routes calls back to the runner that declared them.
//
// The split exists so that the list of what an autonomous agent may do stays in a
// file an operator owns, while the list of what is technically reachable comes from
// servers that may add capabilities overnight. Neither is allowed to be the whole
// answer: a tool needs both a grant and a runner, and a name that has only one of
// them is not callable.
//
// domain.ClassifyTool is still the gate on every individual call. This package
// decides what the model is shown; that function decides what may run, because
// whether a write needs a human depends on the run, not on the tool list.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxOutputBytes bounds what one tool call may hand back.
//
// Tool output goes straight into the next model call, so an unbounded one is a run
// spending its whole token allowance on a single `cat` of a log file — and then
// stopping with run_budget_exhausted having done nothing. 64 KiB is roughly 16k
// tokens: enough for a stack trace or a page of JSON, not enough to end a run.
const MaxOutputBytes = 64 * 1024

// maxNameLength and namePattern are the intersection of what the two providers
// accept for a tool name. Anthropic and OpenAI both require
// `^[a-zA-Z0-9_-]{1,64}$`, and a name outside it is rejected for the whole request
// — so one badly named tool from one MCP server would otherwise break every call
// the run makes, not just the one that uses it.
const maxNameLength = 64

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidName reports whether both providers would accept this as a tool name.
//
// It is checked when grants are loaded rather than when a call is made, so an
// operator hears about a name that cannot be sent at startup instead of finding out
// from a 400 in the middle of a run.
func ValidName(name string) bool {
	return name != "" && len(name) <= maxNameLength && namePattern.MatchString(name)
}

// Source says where a tool came from. It is recorded on the run step: "the model
// called read_report" and "the model called read_report on the MCP server that was
// swapped out last night" are different facts.
type Source string

const (
	// SourceBuiltin is a tool this service implements itself — see shell.go.
	SourceBuiltin Source = "builtin"
	// SourceMCP is a tool an MCP server offers.
	SourceMCP Source = "mcp"
	// SourceHTTP is an endpoint of an HTTP API the operator declared.
	SourceHTTP Source = "http"
)

// Definition is one callable tool, in the shape the provider adapters want.
type Definition struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object describing the arguments. It is passed to
	// the vendor unchanged: an MCP server's schema is somebody else's contract, and
	// rewriting a third party's contract to suit a vendor flag is how a tool starts
	// being called with arguments it never declared.
	InputSchema map[string]any
	Source      Source
	// Origin names the specific source — an MCP server's name, an API's name — so
	// two servers offering similar tools are distinguishable in the audit trail.
	Origin string
}

// Invocation is one call the model asked for.
type Invocation struct {
	Name string
	// Input is the model's own JSON. It is not decoded here; a runner that needs
	// the arguments calls Arguments.
	Input json.RawMessage
	// Workspace is the directory this run's tools may work in. It is per-user and
	// passed per call rather than held on a runner, because a runner is shared
	// between every run on the instance and a workspace baked into a shared runner
	// is one user's files inside another user's tool call.
	Workspace string
}

// Arguments decodes the model's arguments into a map.
//
// Absent, empty and literal null arguments all decode to an empty map: a tool with no
// required arguments is called with `{}`, and some models send nothing at all rather
// than an empty object. Anything that is valid JSON but not an object is refused — a
// runner indexing into a string would otherwise panic on a well-formed reply.
func (i Invocation) Arguments() (map[string]any, error) {
	trimmed := strings.TrimSpace(string(i.Input))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}

	var arguments map[string]any
	if err := json.Unmarshal([]byte(trimmed), &arguments); err != nil {
		return nil, fmt.Errorf("tool %s: arguments are not a JSON object: %w", i.Name, err)
	}
	return arguments, nil
}

// StringArgument reads a required string argument.
func StringArgument(arguments map[string]any, key string) (string, error) {
	raw, present := arguments[key]
	if !present {
		return "", fmt.Errorf("argument %q is required", key)
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string, got %T", key, raw)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("argument %q is empty", key)
	}
	return text, nil
}

// Result is what a tool produced.
//
// A failed tool call is a Result with IsError set, not a Go error: the model is
// supposed to see that its command exited 1 and try something else. A Go error from
// a runner means the tool could not be attempted at all — the server is gone, the
// arguments were unusable — which is the loop's problem rather than the model's.
type Result struct {
	Content string
	IsError bool
	// Truncated reports that Content is not everything the tool produced. It is
	// surfaced separately from the marker inside Content so a run step can record
	// the fact without parsing text.
	Truncated bool
}

// Runner is one source of tools.
//
// Two methods and a label, so an MCP server, a declared HTTP API and the built-in
// shell are interchangeable to the registry, and a fake in a test is a dozen lines.
type Runner interface {
	// Label identifies this runner in errors and in the audit trail. It has to be
	// stable and specific: "mcp:reports", not "mcp".
	Label() string
	// Tools lists what this runner currently offers. It is called once per run, not
	// once per call — see Registry.Offer.
	Tools(ctx context.Context) ([]Definition, error)
	// Call executes one invocation. The context carries the sandbox timeout.
	Call(ctx context.Context, invocation Invocation) (Result, error)
}

// truncate bounds text to limit bytes, keeping the beginning and the end.
//
// Both ends, because the two halves answer different questions: the head says what
// the command was doing, the tail holds the error it died of. Cutting to a rune
// boundary matters because the pieces are JSON-encoded on the way to the model, and
// half a rune arrives there as a replacement character.
func truncate(text string, limit int) (string, bool) {
	if limit <= 0 || len(text) <= limit {
		return text, false
	}

	head := trimToRuneBoundary(text[:limit/2])
	tailStart := len(text) - (limit - limit/2)
	tail := advanceToRuneBoundary(text[tailStart:])
	dropped := len(text) - len(head) - len(tail)

	return fmt.Sprintf("%s\n... %d bytes dropped ...\n%s", head, dropped, tail), true
}

// trimToRuneBoundary drops a partial rune from the end of s.
func trimToRuneBoundary(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// advanceToRuneBoundary drops a partial rune from the start of s.
func advanceToRuneBoundary(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[1:]
	}
	return s
}
