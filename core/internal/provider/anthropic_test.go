package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// anthropicClient points a real client at a test server.
//
// MaxRetries is negative on purpose: an error test that let the SDK retry would
// sleep through its backoff, and what is under test here is the classification of
// the answer, not the SDK's retrying.
func anthropicClient(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewAnthropic(Options{APIKey: "sk-ant-test", BaseURL: server.URL, MaxRetries: -1})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	return client
}

// anthropicReply answers with a Messages response body.
func anthropicReply(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write the reply: %v", err)
		}
	}
}

const anthropicAnswer = `{
	"id": "msg_01",
	"type": "message",
	"role": "assistant",
	"model": "claude-opus-5-20260101",
	"content": [{"type": "text", "text": "Here is the list."}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 812, "output_tokens": 97}
}`

func TestAnthropic_complete_sendsTheBriefTheSystemPromptAndTheToolSchema(t *testing.T) {
	// Arrange
	var path string
	var body map[string]any
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = jsonBody(t, r)
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})

	// Act
	_, err := client.Complete(context.Background(), validRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if path != "/v1/messages" {
		t.Errorf("called %s; want /v1/messages", path)
	}
	if got := digString(t, body, "model"); got != "claude-opus-5" {
		t.Errorf("model = %q; want the one the caller asked for", got)
	}
	// The system prompt is a top-level field here, not a message. A prompt sent as a
	// user turn is one the model may argue with.
	if got := digString(t, body, "system", 0, "text"); got != "You are an operations assistant." {
		t.Errorf("system = %q", got)
	}
	if got := digString(t, body, "messages", 0, "role"); got != "user" {
		t.Errorf("first message role = %q; want user", got)
	}
	if got := digString(t, body, "messages", 0, "content", 0, "text"); !strings.Contains(got, "behind pace") {
		t.Errorf("the brief did not survive: %q", got)
	}
	if got := digString(t, body, "tools", 0, "name"); got != "read_report" {
		t.Errorf("tool name = %q", got)
	}
	// The schema is split across Properties and Required by the parameter's shape, so
	// both halves have to arrive.
	if got := digString(t, body, "tools", 0, "input_schema", "properties", "period", "type"); got != "string" {
		t.Errorf("the property type did not survive: %q", got)
	}
	if got := digString(t, body, "tools", 0, "input_schema", "required", 0); got != "period" {
		t.Errorf("required = %q; want period", got)
	}
}

func TestAnthropicTools_carriesTheSchemaKeysTheParameterHasNoFieldFor(t *testing.T) {
	// Arrange
	// A schema as it arrives from an MCP server, decoded from JSON: the required list
	// is []any rather than []string, and there are keys the parameter type does not
	// name.
	tools := []ToolSchema{{
		Name:        "read_report",
		Description: "read a saved report",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"period": map[string]any{"type": "string"}},
			"required":             []any{"period"},
			"additionalProperties": false,
		},
	}}

	// Act
	converted := anthropicTools(tools)

	// Assert
	if len(converted) != 1 {
		t.Fatalf("converted %d tools; want 1", len(converted))
	}
	schema := converted[0].OfTool.InputSchema
	// A required list dropped because of which Go type it decoded into is a model free
	// to omit an argument the tool needs.
	if !reflect.DeepEqual(schema.Required, []string{"period"}) {
		t.Errorf("required = %#v; want [period]", schema.Required)
	}
	if schema.Properties == nil {
		t.Error("the properties were dropped")
	}
	// additionalProperties and enums are what stop a model inventing arguments, so an
	// unrecognised key is carried rather than discarded.
	if got, ok := schema.ExtraFields["additionalProperties"]; !ok || got != false {
		t.Errorf("extraFields = %#v; want additionalProperties false", schema.ExtraFields)
	}
	// "type" is a constant in the parameter and must not be duplicated into the extras.
	if _, present := schema.ExtraFields["type"]; present {
		t.Error("type was copied into extraFields, where it would be sent twice")
	}
}

func TestAnthropic_complete_sendsTheSmallerOfTheRunAllowanceAndTheReplyCeiling(t *testing.T) {
	// Arrange
	// What the ladder has left is routinely larger than any model will emit in one
	// reply, and a ceiling above the model's own limit is rejected outright — so the
	// clamp is what keeps a well-funded run from 400ing on every call.
	cases := []struct {
		name            string
		allowance       int64
		maxOutputTokens int64
		want            float64
	}{
		{name: "the allowance is the larger of the two", allowance: 900_000, maxOutputTokens: 4_096, want: 4_096},
		{name: "the run is nearly out of allowance", allowance: 120, maxOutputTokens: 4_096, want: 120},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body = jsonBody(t, r)
				anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
			}))
			t.Cleanup(server.Close)

			client, err := NewAnthropic(Options{
				APIKey:          "sk-ant-test",
				BaseURL:         server.URL,
				MaxRetries:      -1,
				MaxOutputTokens: testCase.maxOutputTokens,
			})
			if err != nil {
				t.Fatalf("build the client: %v", err)
			}
			req := validRequest()
			req.MaxTokens = testCase.allowance

			// Act
			if _, err := client.Complete(context.Background(), req); err != nil {
				t.Fatalf("complete: %v", err)
			}

			// Assert
			if got := digNumber(t, body, "max_tokens"); got != testCase.want {
				t.Errorf("max_tokens = %v; want %v", got, testCase.want)
			}
		})
	}
}

func TestAnthropic_complete_replaysAToolCallAndItsResultInTheOrderTheAPIRequires(t *testing.T) {
	// Arrange
	var body map[string]any
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})

	req := validRequest()
	req.Messages = append(req.Messages,
		Message{
			Role:      RoleAssistant,
			Text:      "Reading the report first.",
			ToolCalls: []ToolCall{{ID: "toolu_01", Name: "read_report", Input: []byte(`{"period":"today"}`)}},
		},
		Message{
			Role:        RoleUser,
			Text:        "and the week before",
			ToolResults: []ToolResult{{CallID: "toolu_01", Content: "revenue 41.2m"}},
		},
	)

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	if got := digString(t, body, "messages", 1, "role"); got != "assistant" {
		t.Errorf("the replayed turn has role %q; want assistant", got)
	}
	// Text before the call, because that is the order the model produced it in.
	if got := digString(t, body, "messages", 1, "content", 0, "type"); got != "text" {
		t.Errorf("first assistant block = %q; want text", got)
	}
	if got := digString(t, body, "messages", 1, "content", 1, "type"); got != "tool_use" {
		t.Errorf("second assistant block = %q; want tool_use", got)
	}
	// The arguments have to arrive as an object. Passing the raw bytes through would
	// encode them as a base64 string, which the model cannot read as arguments.
	if got := digString(t, body, "messages", 1, "content", 1, "input", "period"); got != "today" {
		t.Errorf("the tool arguments arrived as %q", got)
	}
	// The result comes first in the answering turn: the API requires a tool result to
	// open the turn that answers a tool call.
	if got := digString(t, body, "messages", 2, "content", 0, "type"); got != "tool_result" {
		t.Errorf("first block of the answering turn = %q; want tool_result", got)
	}
	if got := digString(t, body, "messages", 2, "content", 0, "tool_use_id"); got != "toolu_01" {
		t.Errorf("the result answers %q; want toolu_01", got)
	}
	if got := digString(t, body, "messages", 2, "content", 1, "type"); got != "text" {
		t.Errorf("the accompanying text was not sent after the result: %q", got)
	}
}

func TestAnthropic_complete_marksAFailedToolAndSubstitutesForOneThatPrintedNothing(t *testing.T) {
	// Arrange
	var body map[string]any
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})

	req := validRequest()
	req.Messages = append(req.Messages,
		Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: "toolu_01", Name: "read_report"}},
		},
		Message{
			Role:        RoleUser,
			ToolResults: []ToolResult{{CallID: "toolu_01", Content: "   ", IsError: true}},
		},
	)

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// An empty result is rejected by the API, and "" is also not what happened: a
	// command that printed nothing succeeded, and the model has to be able to tell
	// that from one that failed.
	if got := digString(t, body, "messages", 2, "content", 0, "content", 0, "text"); got != "(no output)" {
		t.Errorf("empty tool output was sent as %q", got)
	}
	if got := dig(t, body, "messages", 2, "content", 0, "is_error"); got != true {
		t.Errorf("is_error = %v; a failure passed back as ordinary output invites the model to read it as data", got)
	}
	// A tool call with no arguments still sends an object, because the API rejects null.
	if got := dig(t, body, "messages", 1, "content", 0, "input"); !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("arguments-free call sent input %#v; want an empty object", got)
	}
}

func TestAnthropic_complete_readsTextAndToolCallsFromOneReply(t *testing.T) {
	// Arrange
	// A model that explains itself and then acts, with a block type this package has
	// no use for in between.
	client := anthropicClient(t, anthropicReply(t, http.StatusOK, `{
		"id": "msg_02",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-5-20260101",
		"content": [
			{"type": "text", "text": "Checking today's figures."},
			{"type": "thinking", "thinking": "the operator asked about pace"},
			{"type": "tool_use", "id": "toolu_09", "name": "read_report", "input": {"period": "today"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 812, "output_tokens": 97}
	}`))

	// Act
	response, err := client.Complete(context.Background(), validRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Finish != FinishToolUse {
		t.Errorf("finish = %q; want %q", response.Finish, FinishToolUse)
	}
	if response.Text != "Checking today's figures." {
		t.Errorf("text = %q", response.Text)
	}
	// A block type this package does not read is ignored rather than refused — the
	// vendor adds them over time — but it must not leak into the reply either.
	if strings.Contains(response.Text, "pace") {
		t.Errorf("an unread block type reached the transcript: %q", response.Text)
	}
	if len(response.ToolCalls) != 1 {
		t.Fatalf("read %d tool calls; want 1", len(response.ToolCalls))
	}
	call := response.ToolCalls[0]
	if call.ID != "toolu_09" || call.Name != "read_report" {
		t.Errorf("tool call = %s/%s", call.ID, call.Name)
	}
	var arguments map[string]any
	if err := json.Unmarshal(call.Input, &arguments); err != nil {
		t.Fatalf("the arguments are not JSON: %v", err)
	}
	if arguments["period"] != "today" {
		t.Errorf("arguments = %#v", arguments)
	}
	if response.Usage.TokensIn != 812 || response.Usage.TokensOut != 97 {
		t.Errorf("usage = %+v; the ladder bills against this", response.Usage)
	}
	// The dated id, not the alias that was asked for: a run stays explainable after
	// the alias moves.
	if response.Model != "claude-opus-5-20260101" {
		t.Errorf("model = %q; want what the vendor said answered", response.Model)
	}
}

func TestAnthropic_complete_aReplyWithNoToolCallIsAnAnswer(t *testing.T) {
	// Arrange
	client := anthropicClient(t, anthropicReply(t, http.StatusOK, anthropicAnswer))

	// Act
	response, err := client.Complete(context.Background(), validRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Finish != FinishAnswered {
		t.Errorf("finish = %q; want %q", response.Finish, FinishAnswered)
	}
	if len(response.ToolCalls) != 0 {
		t.Errorf("read %d tool calls from a plain answer", len(response.ToolCalls))
	}
}

func TestAnthropic_complete_aToolCallOutranksTheStopReasonOnTheEnvelope(t *testing.T) {
	// Arrange
	// end_turn with a tool_use block in the content. Trusting the label would report
	// the run complete while the work it asked for was never done.
	client := anthropicClient(t, anthropicReply(t, http.StatusOK, `{
		"id": "msg_03",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-5-20260101",
		"content": [{"type": "tool_use", "id": "toolu_10", "name": "read_report"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`))

	// Act
	response, err := client.Complete(context.Background(), validRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Finish != FinishToolUse {
		t.Errorf("finish = %q; want %q", response.Finish, FinishToolUse)
	}
	// An empty object, not empty bytes: the loop hands this to a tool that will try to
	// decode it.
	if string(response.ToolCalls[0].Input) != "{}" {
		t.Errorf("empty arguments read as %q; want {}", response.ToolCalls[0].Input)
	}
}

func TestAnthropic_complete_sendsNoToolsFieldWhenNoneAreOffered(t *testing.T) {
	// Arrange
	var body map[string]any
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})
	req := validRequest()
	req.Tools = nil

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// An empty list is not the same as no list: the API rejects a tools array with
	// nothing in it, so a run whose grants allow nothing would fail on every call.
	if _, present := body["tools"]; present {
		t.Errorf("an empty tools field was sent: %v", body["tools"])
	}
}

func TestAnthropic_complete_refusesATurnWithNothingInItWithoutSpendingACall(t *testing.T) {
	// Arrange
	// Validate lets this through — a turn with no text and no results is well-formed as
	// a value — so it is the adapter that has to catch it, and before the network.
	calls := 0
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})
	req := validRequest()
	req.Messages = append(req.Messages, Message{Role: RoleUser})

	// Act
	_, err := client.Complete(context.Background(), req)

	// Assert
	// The API rejects an empty turn and there is nothing sensible to substitute — a
	// turn with no content is a caller mistake, unlike empty tool output, which is a
	// fact about the world.
	if err == nil {
		t.Fatal("a turn with no content was sent")
	}
	if calls != 0 {
		t.Errorf("the API was called %d times for a request that could not be built", calls)
	}
}

func TestStringSlice_ignoresARequiredListThatIsNotAList(t *testing.T) {
	// Arrange, Act, Assert
	// A malformed schema. Guessing that a bare string meant a one-item list would be
	// this package deciding what a third party's contract says.
	if got := stringSlice("period"); got != nil {
		t.Errorf("required = %#v; want nothing", got)
	}
	// A list from JSON with something that is not a name in it: the names survive, the
	// rest is dropped rather than stringified.
	if got := stringSlice([]any{"period", 3}); !reflect.DeepEqual(got, []string{"period"}) {
		t.Errorf("required = %#v; want [period]", got)
	}
}

func TestDecodeToolInput_refusesArgumentsThatAreNotJSON(t *testing.T) {
	// Arrange
	// Unreachable through Complete, because Validate refuses this first. It is checked
	// anyway: the SDK would encode the failure as a null input and the model would be
	// asked to run a tool with no arguments it recognises.
	call := ToolCall{ID: "toolu_01", Name: "read_report", Input: []byte(`{"period":`)}

	// Act
	_, err := decodeToolInput(call)

	// Assert
	if err == nil {
		t.Fatal("truncated arguments were decoded")
	}
	if !strings.Contains(err.Error(), "toolu_01") {
		t.Errorf("the error does not name the call: %v", err)
	}
}

func TestAnthropicFinish_mapsEveryStopReasonTheVendorDocuments(t *testing.T) {
	// Arrange
	// One case per branch. The mapping is what the loop's continuation decision reads,
	// so a stop reason silently folded into the wrong bucket is a run that stops when
	// it should continue, or continues when nothing can be done.
	cases := []struct {
		reason       anthropic.StopReason
		hasToolCalls bool
		want         Finish
	}{
		{reason: anthropic.StopReasonEndTurn, want: FinishAnswered},
		{reason: anthropic.StopReasonStopSequence, want: FinishAnswered},
		{reason: anthropic.StopReasonMaxTokens, want: FinishTruncated},
		{reason: anthropic.StopReasonPauseTurn, want: FinishPaused},
		{reason: anthropic.StopReasonRefusal, want: FinishRefused},
		// Labelled tool_use with nothing to run: not an answer, and not actionable.
		{reason: anthropic.StopReasonToolUse, want: FinishUnusable},
		// Sending the same conversation again produces the same answer.
		{reason: anthropic.StopReasonModelContextWindowExceeded, want: FinishUnusable},
		// A value from a future model. Read as success it would be a run reported
		// complete on a reply that never arrived.
		{reason: anthropic.StopReason("something_new"), want: FinishUnusable},
		{reason: anthropic.StopReason(""), want: FinishUnusable},
		// Tool calls win over every label above.
		{reason: anthropic.StopReasonEndTurn, hasToolCalls: true, want: FinishToolUse},
		{reason: anthropic.StopReasonMaxTokens, hasToolCalls: true, want: FinishToolUse},
	}

	// Act, Assert
	for _, testCase := range cases {
		got := anthropicFinish(testCase.reason, testCase.hasToolCalls)
		if got != testCase.want {
			t.Errorf("stop reason %q (tool calls: %v) = %q; want %q",
				testCase.reason, testCase.hasToolCalls, got, testCase.want)
		}
	}
}

func TestAnthropic_complete_aRateLimitIsRetryableAndDoesNotEchoTheResponseBody(t *testing.T) {
	// Arrange
	// A rejected request is quoted back in the error body, which here means somebody's
	// brief and whatever a tool read on their behalf.
	client := anthropicClient(t, anthropicReply(t, http.StatusTooManyRequests, `{
		"type": "error",
		"error": {"type": "rate_limit_error", "message": "rate limit exceeded for revenue is behind pace"}
	}`))

	// Act
	_, err := client.Complete(context.Background(), validRequest())

	// Assert
	if err == nil {
		t.Fatal("a 429 was reported as success")
	}
	if !Retryable(err) {
		t.Error("a rate limit was not marked retryable")
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("the SDK error was not classified: %v", err)
	}
	if providerErr.Provider != NameAnthropic || providerErr.Status != http.StatusTooManyRequests {
		t.Errorf("classified as %s/%d", providerErr.Provider, providerErr.Status)
	}
	// The vendor's type survives, because it is what separates "slow down" from "your
	// key is wrong" without needing the body.
	if providerErr.Kind != "rate_limit_error" {
		t.Errorf("kind = %q; want rate_limit_error", providerErr.Kind)
	}
	if strings.Contains(err.Error(), "behind pace") {
		t.Errorf("the response body reached the error message: %v", err)
	}
}

func TestAnthropic_complete_aRejectedRequestIsNotRetried(t *testing.T) {
	// Arrange
	client := anthropicClient(t, anthropicReply(t, http.StatusBadRequest, `{
		"type": "error",
		"error": {"type": "invalid_request_error", "message": "max_tokens exceeds the model limit"}
	}`))

	// Act
	_, err := client.Complete(context.Background(), validRequest())

	// Assert
	// Sending the identical request again earns the identical rejection; retrying it
	// only spends the run's retry budget.
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if Retryable(err) {
		t.Error("a rejected request was queued up to be rejected again")
	}
}

func TestAnthropic_complete_aServerThatAnsweredNothingIsRetryable(t *testing.T) {
	// Arrange
	// A closed listener stands in for the commonest transport failure: the network
	// briefly not being there.
	server := httptest.NewServer(anthropicReply(t, http.StatusOK, anthropicAnswer))
	address := server.URL
	server.Close()

	client, err := NewAnthropic(Options{APIKey: "sk-ant-test", BaseURL: address, MaxRetries: -1})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}

	// Act
	_, err = client.Complete(context.Background(), validRequest())

	// Assert
	if err == nil {
		t.Fatal("a call to a closed listener was reported as success")
	}
	if !Retryable(err) {
		t.Errorf("a transport failure was not marked retryable: %v", err)
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("the transport failure was not classified: %v", err)
	}
	if providerErr.Status != 0 {
		t.Errorf("status = %d; want 0, because no answer came back", providerErr.Status)
	}
}

func TestAnthropic_complete_stopsWhenTheStepHasNoTimeLeft(t *testing.T) {
	// Arrange
	client := anthropicClient(t, anthropicReply(t, http.StatusOK, anthropicAnswer))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, err := client.Complete(ctx, validRequest())

	// Assert
	if err == nil {
		t.Fatal("a cancelled call was reported as success")
	}
	// The next attempt has no more time than this one did, and the cancellation was
	// the caller's decision.
	if Retryable(err) {
		t.Error("a cancelled step was queued up to be cancelled again")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation was lost: %v", err)
	}
}

func TestAnthropic_complete_refusesAMalformedRequestWithoutSpendingACall(t *testing.T) {
	// Arrange
	calls := 0
	client := anthropicClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		anthropicReply(t, http.StatusOK, anthropicAnswer)(w, r)
	})
	req := validRequest()
	req.MaxTokens = 0

	// Act
	_, err := client.Complete(context.Background(), req)

	// Assert
	if err == nil {
		t.Fatal("a request with no token allowance was sent")
	}
	// Validating before the network is what makes a ladder mistake cost nothing and
	// name its own problem, instead of arriving as a vendor 400 to be decoded.
	if calls != 0 {
		t.Errorf("the API was called %d times for a request that could not be sent", calls)
	}
	if Retryable(err) {
		t.Error("a malformed request was marked retryable")
	}
}
