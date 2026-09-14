package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openAIClient points a real client at a test server.
//
// The base URL carries the /v1 prefix the vendor's own does, because the SDK joins
// the endpoint path onto it relatively — a base URL without it would send the call to
// /chat/completions and the test would pass against a path production never uses.
func openAIClient(t *testing.T, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewOpenAI(Options{APIKey: "sk-test", BaseURL: server.URL + "/v1/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	return client
}

// openAIReply answers with a Chat Completions response body.
func openAIReply(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write the reply: %v", err)
		}
	}
}

const openAIAnswer = `{
	"id": "chatcmpl-01",
	"object": "chat.completion",
	"created": 1757600000,
	"model": "gpt-5.2-2026-04-01",
	"choices": [{
		"index": 0,
		"finish_reason": "stop",
		"message": {"role": "assistant", "content": "Here is the list."}
	}],
	"usage": {"prompt_tokens": 812, "completion_tokens": 97, "total_tokens": 909}
}`

func openAIRequest() Request {
	req := validRequest()
	req.Model = "gpt-5.2"
	return req
}

func TestOpenAI_complete_sendsTheBriefTheSystemPromptAndTheToolSchema(t *testing.T) {
	// Arrange
	var path string
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})

	// Act
	_, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Chat Completions, not the Responses API: it is the endpoint every
	// OpenAI-compatible gateway implements.
	if path != "/v1/chat/completions" {
		t.Errorf("called %s; want /v1/chat/completions", path)
	}
	if got := digString(t, body, "model"); got != "gpt-5.2" {
		t.Errorf("model = %q", got)
	}
	// The system prompt is a message here, and it has to lead.
	if got := digString(t, body, "messages", 0, "role"); got != "system" {
		t.Errorf("first message role = %q; want system", got)
	}
	if got := digString(t, body, "messages", 0, "content"); got != "You are an operations assistant." {
		t.Errorf("system message = %q", got)
	}
	if got := digString(t, body, "messages", 1, "role"); got != "user" {
		t.Errorf("second message role = %q; want user", got)
	}
	// MaxCompletionTokens, not the deprecated max_tokens: the reasoning models reject
	// the older field outright.
	if got := digNumber(t, body, "max_completion_tokens"); got != 2_000 {
		t.Errorf("max_completion_tokens = %v; want 2000", got)
	}
	if _, present := body["max_tokens"]; present {
		t.Error("the deprecated max_tokens field was sent")
	}
	if got := digString(t, body, "tools", 0, "type"); got != "function" {
		t.Errorf("tool type = %q; want function", got)
	}
	if got := digString(t, body, "tools", 0, "function", "name"); got != "read_report" {
		t.Errorf("tool name = %q", got)
	}
	// The whole schema crosses as one object, unlike Anthropic's split parameter.
	if got := digString(t, body, "tools", 0, "function", "parameters", "properties", "period", "type"); got != "string" {
		t.Errorf("the property type did not survive: %q", got)
	}
	if got := digString(t, body, "tools", 0, "function", "parameters", "required", 0); got != "period" {
		t.Errorf("required = %q; want period", got)
	}
	// Strict mode is deliberately unset: it demands every property be required and
	// additionalProperties be false, which is a rewrite of a schema that arrived from
	// an MCP server or an OpenAPI document.
	function, ok := dig(t, body, "tools", 0, "function").(map[string]any)
	if !ok {
		t.Fatal("the function definition is not an object")
	}
	if _, present := function["strict"]; present {
		t.Error("strict mode was set, which rewrites a third party's contract to fit one vendor's flag")
	}
}

func TestOpenAI_complete_sendsTheSmallerOfTheRunAllowanceAndTheReplyCeiling(t *testing.T) {
	// Arrange
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	}))
	t.Cleanup(server.Close)

	client, err := NewOpenAI(Options{
		APIKey:          "sk-test",
		BaseURL:         server.URL + "/v1/",
		MaxRetries:      -1,
		MaxOutputTokens: 4_096,
	})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	req := openAIRequest()
	req.MaxTokens = 900_000

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// What the ladder has left is larger than any model will emit in one reply, and a
	// ceiling above the model's own limit is rejected outright.
	if got := digNumber(t, body, "max_completion_tokens"); got != 4_096 {
		t.Errorf("max_completion_tokens = %v; want the reply ceiling 4096", got)
	}
}

func TestOpenAI_complete_replaysAToolCallBeforeTheResultThatAnswersIt(t *testing.T) {
	// Arrange
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})

	req := openAIRequest()
	req.Messages = append(req.Messages,
		Message{
			Role:      RoleAssistant,
			Text:      "Reading the report first.",
			ToolCalls: []ToolCall{{ID: "call_01", Name: "read_report", Input: []byte(`{"period":"today"}`)}},
		},
		Message{
			Role:        RoleUser,
			Text:        "and the week before",
			ToolResults: []ToolResult{{CallID: "call_01", Content: "revenue 41.2m"}},
		},
	)

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// system, user, assistant, tool, user: one neutral turn became two messages,
	// because a tool result is its own message with its own role here.
	if got := digLen(t, body, "messages"); got != 5 {
		t.Fatalf("sent %d messages; want 5", got)
	}
	if got := digString(t, body, "messages", 2, "role"); got != "assistant" {
		t.Errorf("the replayed turn has role %q; want assistant", got)
	}
	if got := digString(t, body, "messages", 2, "content"); got != "Reading the report first." {
		t.Errorf("the assistant text did not survive: %q", got)
	}
	// The calls are replayed because they are what the tool message after them refers
	// to; an assistant turn stripped of them leaves results answering nothing.
	if got := digString(t, body, "messages", 2, "tool_calls", 0, "id"); got != "call_01" {
		t.Errorf("tool call id = %q", got)
	}
	// The arguments are a JSON string on this API, not an object.
	arguments := digString(t, body, "messages", 2, "tool_calls", 0, "function", "arguments")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatalf("the arguments were not sent as a JSON string: %q", arguments)
	}
	if decoded["period"] != "today" {
		t.Errorf("arguments = %q", arguments)
	}
	if got := digString(t, body, "messages", 3, "role"); got != "tool" {
		t.Errorf("the result was sent with role %q; want tool", got)
	}
	if got := digString(t, body, "messages", 3, "tool_call_id"); got != "call_01" {
		t.Errorf("the result answers %q; want call_01", got)
	}
	// The text that came with the results follows them, keeping the pairing the API
	// checks intact.
	if got := digString(t, body, "messages", 4, "role"); got != "user" {
		t.Errorf("the accompanying text was sent with role %q; want user", got)
	}
}

func TestOpenAI_complete_marksAFailedToolAndSubstitutesForOneThatPrintedNothing(t *testing.T) {
	// Arrange
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})

	req := openAIRequest()
	req.Messages = append(req.Messages,
		Message{
			Role:      RoleAssistant,
			ToolCalls: []ToolCall{{ID: "call_02", Name: "read_report"}},
		},
		Message{
			Role:        RoleUser,
			ToolResults: []ToolResult{{CallID: "call_02", Content: "permission denied", IsError: true}},
		},
	)

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// A tool message has no error flag here, unlike Anthropic's tool result block.
	// Without the prefix a stack trace arrives as ordinary output.
	content := digString(t, body, "messages", 3, "content")
	if !strings.HasPrefix(content, "error: ") {
		t.Errorf("a failed tool was reported as %q", content)
	}
	// A tool with no arguments still sends an object: the field is a required JSON
	// string and an empty one is not valid JSON.
	if got := digString(t, body, "messages", 2, "tool_calls", 0, "function", "arguments"); got != "{}" {
		t.Errorf("arguments-free call sent %q; want {}", got)
	}
}

func TestOpenAI_complete_substitutesAPlaceholderForOutputThatWasEmpty(t *testing.T) {
	// Arrange
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})

	req := openAIRequest()
	req.Messages = append(req.Messages,
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_03", Name: "read_report"}}},
		Message{Role: RoleUser, ToolResults: []ToolResult{{CallID: "call_03", Content: ""}}},
	)

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// A command that printed nothing succeeded, and the model has to be able to tell
	// that from one that failed.
	if got := digString(t, body, "messages", 3, "content"); got != "(no output)" {
		t.Errorf("empty tool output was sent as %q", got)
	}
}

func TestOpenAI_complete_readsTextAndToolCallsFromOneReply(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusOK, `{
		"id": "chatcmpl-02",
		"object": "chat.completion",
		"created": 1757600000,
		"model": "gpt-5.2-2026-04-01",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": "Checking today's figures.",
				"tool_calls": [{
					"id": "call_09",
					"type": "function",
					"function": {"name": "read_report", "arguments": "{\"period\":\"today\"}"}
				}]
			}
		}],
		"usage": {"prompt_tokens": 812, "completion_tokens": 97, "total_tokens": 909}
	}`))

	// Act
	response, err := client.Complete(context.Background(), openAIRequest())

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
	if len(response.ToolCalls) != 1 {
		t.Fatalf("read %d tool calls; want 1", len(response.ToolCalls))
	}
	call := response.ToolCalls[0]
	if call.ID != "call_09" || call.Name != "read_report" {
		t.Errorf("tool call = %s/%s", call.ID, call.Name)
	}
	// The arguments arrive as a JSON string and are carried as the raw JSON they
	// contain, so both providers hand the registry the same thing.
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
	if response.Model != "gpt-5.2-2026-04-01" {
		t.Errorf("model = %q; want what the vendor said answered", response.Model)
	}
}

func TestOpenAI_complete_aRefusalIsRecordedAsWhatTheModelSaid(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusOK, `{
		"id": "chatcmpl-03",
		"object": "chat.completion",
		"created": 1757600000,
		"model": "gpt-5.2-2026-04-01",
		"choices": [{
			"index": 0,
			"finish_reason": "stop",
			"message": {"role": "assistant", "content": "", "refusal": "I won't draft that."}
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
	}`))

	// Act
	response, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// A refusal is an outcome, not a failure: the identical prompt earns the identical
	// refusal, so retrying only spends tokens.
	if response.Finish != FinishRefused {
		t.Errorf("finish = %q; want %q", response.Finish, FinishRefused)
	}
	// Leaving Text empty would record a run that stopped with nothing said, when in
	// fact the model said why.
	if response.Text != "I won't draft that." {
		t.Errorf("text = %q; want the refusal", response.Text)
	}
}

func TestOpenAI_complete_aToolCallForATypeTheRegistryCannotRunIsNotPassedOn(t *testing.T) {
	// Arrange
	// A custom tool call. The registry only knows function tools, so passing this on
	// would produce a call for a tool no grant covers.
	client := openAIClient(t, openAIReply(t, http.StatusOK, `{
		"id": "chatcmpl-04",
		"object": "chat.completion",
		"created": 1757600000,
		"model": "gpt-5.2-2026-04-01",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [{
					"id": "call_10",
					"type": "custom",
					"custom": {"name": "shell", "input": "rm -rf ."}
				}]
			}
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
	}`))

	// Act
	response, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(response.ToolCalls) != 0 {
		t.Fatalf("read %d tool calls; want none, because none of them can be run", len(response.ToolCalls))
	}
	// Labelled as calling a tool with nothing runnable in the message: nothing was
	// answered either, so this is not success.
	if response.Finish != FinishUnusable {
		t.Errorf("finish = %q; want %q", response.Finish, FinishUnusable)
	}
}

func TestOpenAI_complete_aReplyWithNoChoicesIsAFailedCallNotAnEmptyAnswer(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusOK, `{
		"id": "chatcmpl-05",
		"object": "chat.completion",
		"created": 1757600000,
		"model": "gpt-5.2-2026-04-01",
		"choices": [],
		"usage": {"prompt_tokens": 12, "completion_tokens": 0, "total_tokens": 12}
	}`))

	// Act
	_, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	// A 200 with nothing in it. There is no reply to act on and no usage worth billing,
	// and the alternative reading is that a well-formed request produced a well-formed
	// nothing.
	if err == nil {
		t.Fatal("a reply with no choices was reported as an answer")
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("an empty reply was not classified as a provider failure: %v", err)
	}
	if providerErr.Kind != "no_choices" {
		t.Errorf("kind = %q; want no_choices", providerErr.Kind)
	}
	if !Retryable(err) {
		t.Error("an empty reply was not marked retryable")
	}
}

func TestOpenAI_complete_argumentsThatArrivedEmptyBecomeAnObject(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusOK, `{
		"id": "chatcmpl-06",
		"object": "chat.completion",
		"created": 1757600000,
		"model": "gpt-5.2-2026-04-01",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [{"id": "call_11", "type": "function", "function": {"name": "read_report", "arguments": ""}}]
			}
		}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
	}`))

	// Act
	response, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// The loop hands this to a tool that will try to decode it, and "" is not JSON.
	if got := string(response.ToolCalls[0].Input); got != "{}" {
		t.Errorf("empty arguments read as %q; want {}", got)
	}
}

func TestOpenAI_complete_sendsNoToolsFieldWhenNoneAreOffered(t *testing.T) {
	// Arrange
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})
	req := openAIRequest()
	req.Tools = nil

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// An empty list is not the same as no list, and a run whose grants allow nothing is
	// an ordinary run rather than a broken request.
	if _, present := body["tools"]; present {
		t.Errorf("an empty tools field was sent: %v", body["tools"])
	}
}

func TestOpenAI_complete_sendsNoSystemMessageWhenThereIsNoPrompt(t *testing.T) {
	// Arrange
	var body map[string]any
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		body = jsonBody(t, r)
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})
	req := openAIRequest()
	req.System = "   "

	// Act
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Assert
	// A blank system message is a turn the model has to account for and cannot read
	// anything from.
	if got := digString(t, body, "messages", 0, "role"); got != "user" {
		t.Errorf("first message role = %q; want user, with no empty system turn before it", got)
	}
	if got := digLen(t, body, "messages"); got != 1 {
		t.Errorf("sent %d messages; want 1", got)
	}
}

func TestOpenAI_complete_refusesATurnWithNothingInItWithoutSpendingACall(t *testing.T) {
	// Arrange
	// Validate lets this through, so it is the adapter that has to catch it, and before
	// the network.
	calls := 0
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})
	req := openAIRequest()
	req.Messages = append(req.Messages, Message{Role: RoleUser})

	// Act
	_, err := client.Complete(context.Background(), req)

	// Assert
	// A turn with neither text nor a tool result is a caller mistake, and the API
	// rejects it.
	if err == nil {
		t.Fatal("a turn with no content was sent")
	}
	if calls != 0 {
		t.Errorf("the API was called %d times for a request that could not be built", calls)
	}
}

func TestOpenAI_complete_aServerThatAnsweredNothingIsRetryable(t *testing.T) {
	// Arrange
	server := httptest.NewServer(openAIReply(t, http.StatusOK, openAIAnswer))
	address := server.URL
	server.Close()

	client, err := NewOpenAI(Options{APIKey: "sk-test", BaseURL: address + "/v1/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}

	// Act
	_, err = client.Complete(context.Background(), openAIRequest())

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

func TestOpenAIFinish_mapsEveryFinishReasonTheVendorDocuments(t *testing.T) {
	// Arrange
	// One case per branch, for the same reason the Anthropic table exists: the mapping
	// is what the loop's continuation decision reads.
	cases := []struct {
		reason       string
		hasToolCalls bool
		want         Finish
	}{
		{reason: "stop", want: FinishAnswered},
		{reason: "length", want: FinishTruncated},
		{reason: "content_filter", want: FinishRefused},
		// Labelled as calling a tool with no call in the message.
		{reason: "tool_calls", want: FinishUnusable},
		{reason: "function_call", want: FinishUnusable},
		// A value from a future model, or a gateway that answered something else.
		{reason: "something_new", want: FinishUnusable},
		{reason: "", want: FinishUnusable},
		// Tool calls win over every label above.
		{reason: "stop", hasToolCalls: true, want: FinishToolUse},
		{reason: "length", hasToolCalls: true, want: FinishToolUse},
	}

	// Act, Assert
	for _, testCase := range cases {
		got := openAIFinish(testCase.reason, testCase.hasToolCalls)
		if got != testCase.want {
			t.Errorf("finish reason %q (tool calls: %v) = %q; want %q",
				testCase.reason, testCase.hasToolCalls, got, testCase.want)
		}
	}
}

func TestOpenAI_complete_anExhaustedAccountIsNotRetriedAndTheBodyDoesNotLeak(t *testing.T) {
	// Arrange
	// A 429 that means the account has no credit rather than "slow down", with the
	// request quoted back in the message the way a rejection does.
	client := openAIClient(t, openAIReply(t, http.StatusTooManyRequests, `{
		"error": {
			"message": "You exceeded your current quota while asking: revenue is behind pace",
			"type": "insufficient_quota",
			"code": "insufficient_quota",
			"param": null
		}
	}`))

	// Act
	_, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err == nil {
		t.Fatal("a 429 was reported as success")
	}
	// Retrying it burns a run's whole retry budget against a bill nobody has paid, and
	// the answer never changes until somebody does.
	if Retryable(err) {
		t.Error("an exhausted account was treated as a transient rate limit")
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("the SDK error was not classified: %v", err)
	}
	if providerErr.Provider != NameOpenAI || providerErr.Status != http.StatusTooManyRequests {
		t.Errorf("classified as %s/%d", providerErr.Provider, providerErr.Status)
	}
	// Code is the specific reason and Type the family it belongs to; the specific one
	// is what changes what to do about it.
	if providerErr.Kind != "insufficient_quota" {
		t.Errorf("kind = %q; want insufficient_quota", providerErr.Kind)
	}
	if strings.Contains(err.Error(), "behind pace") {
		t.Errorf("the response body reached the error message: %v", err)
	}
}

func TestOpenAI_complete_aVendorOutageIsRetryable(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusServiceUnavailable, `{
		"error": {"message": "the engine is currently overloaded", "type": "server_error", "code": null}
	}`))

	// Act
	_, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err == nil {
		t.Fatal("a 503 was reported as success")
	}
	if !Retryable(err) {
		t.Error("a vendor outage was not marked retryable")
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("the SDK error was not classified: %v", err)
	}
	// With no code the family is what is left to record.
	if providerErr.Kind != "server_error" {
		t.Errorf("kind = %q; want server_error", providerErr.Kind)
	}
}

func TestOpenAI_complete_aWrongKeyIsNotRetried(t *testing.T) {
	// Arrange
	client := openAIClient(t, openAIReply(t, http.StatusUnauthorized, `{
		"error": {"message": "Incorrect API key provided", "type": "invalid_request_error", "code": "invalid_api_key"}
	}`))

	// Act
	_, err := client.Complete(context.Background(), openAIRequest())

	// Assert
	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	// A key that is wrong now is wrong in a minute, and retrying an authentication
	// failure is how a misconfigured deployment turns into a rate-limit ban.
	if Retryable(err) {
		t.Error("an authentication failure was queued up to be repeated")
	}
}

func TestOpenAI_complete_refusesAMalformedRequestWithoutSpendingACall(t *testing.T) {
	// Arrange
	calls := 0
	client := openAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		openAIReply(t, http.StatusOK, openAIAnswer)(w, r)
	})
	req := openAIRequest()
	req.Messages = nil

	// Act
	_, err := client.Complete(context.Background(), req)

	// Assert
	if err == nil {
		t.Fatal("a request with no messages was sent")
	}
	if calls != 0 {
		t.Errorf("the API was called %d times for a request that could not be sent", calls)
	}
}
