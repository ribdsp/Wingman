package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/config"
)

// A request the loop would actually make: a brief, a tool on offer, and a ceiling
// that came from the continuation ladder.
func validRequest() Request {
	return Request{
		Model:     "claude-opus-5",
		System:    "You are an operations assistant.",
		MaxTokens: 2_000,
		Messages: []Message{{
			Role: RoleUser,
			Text: "revenue is behind pace; draft the follow-up list",
		}},
		Tools: []ToolSchema{{
			Name:        "read_report",
			Description: "read a saved report",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"period": map[string]any{"type": "string"}},
				"required":   []string{"period"},
			},
		}},
	}
}

func TestRequest_validate_acceptsACallTheLoopWouldMake(t *testing.T) {
	// Arrange
	req := validRequest()

	// Act
	err := req.Validate()

	// Assert
	if err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
}

func TestRequest_validate_refusesACallWithNoTokenAllowanceLeft(t *testing.T) {
	// Arrange
	req := validRequest()
	req.MaxTokens = 0

	// Act
	err := req.Validate()

	// Assert
	// Both vendors reject a zero ceiling, but that is not the reason to stop here: a
	// run still calling the model with nothing left to spend is a ladder mistake, and
	// it has to surface as one rather than as a vendor 400.
	if err == nil {
		t.Fatal("a call with a zero token ceiling was accepted")
	}
}

func TestRequest_validate_refusesTheSameToolOfferedTwice(t *testing.T) {
	// Arrange
	req := validRequest()
	req.Tools = append(req.Tools, ToolSchema{Name: "read_report", InputSchema: map[string]any{}})

	// Act
	err := req.Validate()

	// Assert
	// A duplicate is accepted by both vendors and then answered with a call the
	// registry cannot resolve to one grant — so which class the tool has, and
	// therefore whether it needs approval, would depend on map iteration order.
	if err == nil {
		t.Fatal("a tool offered under the same name twice was accepted")
	}
	if !strings.Contains(err.Error(), "read_report") {
		t.Errorf("the error does not name the duplicated tool: %v", err)
	}
}

func TestRequest_validate_refusesAToolResultThatAnswersNothing(t *testing.T) {
	// Arrange
	req := validRequest()
	req.Messages = append(req.Messages, Message{
		Role:        RoleUser,
		ToolResults: []ToolResult{{Content: "42"}},
	})

	// Act
	err := req.Validate()

	// Assert
	// A result with no call id cannot be matched to what the model asked for.
	if err == nil {
		t.Fatal("a tool result with no call id was accepted")
	}
}

func TestRequest_validate_refusesToolArgumentsThatAreNotJSON(t *testing.T) {
	// Arrange
	req := validRequest()
	req.Messages = append(req.Messages, Message{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "toolu_01", Name: "read_report", Input: []byte(`{"period":`)}},
	})

	// Act
	err := req.Validate()

	// Assert
	// The arguments were the model's own JSON on the way in, so bytes that are not
	// JSON mean something rewrote them in between. Checking once here is what lets
	// both adapters decode without guessing.
	if err == nil {
		t.Fatal("a tool call with truncated arguments was accepted")
	}
}

func TestRequest_validate_refusesARoleThatIsNeitherUserNorAssistant(t *testing.T) {
	// Arrange
	req := validRequest()
	req.Messages[0].Role = Role("system")

	// Act
	err := req.Validate()

	// Assert
	// There is no system role in these types on purpose — a system prompt is
	// Request.System — and accepting one here would silently send it as a user turn.
	if err == nil {
		t.Fatal("a system-role message was accepted")
	}
}

func TestOptions_withDefaults_fillsTheCeilingsThatWereLeftUnset(t *testing.T) {
	// Arrange
	opts := Options{APIKey: "k"}

	// Act
	filled := opts.withDefaults()

	// Assert
	if filled.MaxRetries != defaultMaxRetries {
		t.Errorf("maxRetries = %d; want the default %d", filled.MaxRetries, defaultMaxRetries)
	}
	// A zero output ceiling would be sent as max_tokens=0 and rejected on every call.
	if filled.MaxOutputTokens != defaultMaxOutputTokens {
		t.Errorf("maxOutputTokens = %d; want the default %d", filled.MaxOutputTokens, defaultMaxOutputTokens)
	}
}

func TestOptions_withDefaults_negativeRetriesDisableRetryingRatherThanDefaulting(t *testing.T) {
	// Arrange
	opts := Options{APIKey: "k", MaxRetries: -1}

	// Act
	filled := opts.withDefaults()

	// Assert
	// Zero has to mean "unset" so a caller that never thought about retries gets the
	// SDK's behaviour; that leaves no way to ask for none, which is what negative is
	// for. The test suite is the caller that needs it.
	if filled.MaxRetries != 0 {
		t.Errorf("maxRetries = %d; want 0", filled.MaxRetries)
	}
}

func TestNew_refusesAProviderWithNoAPIKeyAtWiringTime(t *testing.T) {
	// Arrange, Act
	_, err := New(NameAnthropic, Options{})

	// Assert
	// The same rule internal/config follows: refuse to start half-configured rather
	// than discover a missing credential four tool calls into a run.
	if err == nil {
		t.Fatal("a provider with no API key was built")
	}
}

func TestNew_refusesABaseURLThatIsNotHTTPAndDoesNotEchoIt(t *testing.T) {
	// Arrange
	secret := "gateway.internal/v1?token=s3cr3t"

	// Act
	_, err := New(NameOpenAI, Options{APIKey: "k", BaseURL: secret})

	// Assert
	if err == nil {
		t.Fatal("a base URL with no scheme was accepted")
	}
	// An operator who pasted a credentialled endpoint would otherwise have it in a
	// startup log, which is where the one place it was not supposed to be written is.
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the error echoed the base URL: %v", err)
	}
}

func TestNew_refusesANameThatIsNeitherProvider(t *testing.T) {
	// Arrange, Act
	_, err := New("ollama", Options{APIKey: "k"})

	// Assert
	// Falling back to whichever provider is first in the switch would send a run to
	// a vendor nobody configured, on a key nobody meant to spend.
	if err == nil {
		t.Fatal("an unknown provider name was accepted")
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Errorf("the error does not name the value: %v", err)
	}
}

func TestNew_buildsBothProvidersTheConfigWillAccept(t *testing.T) {
	// Arrange
	// internal/config validates DEFAULT_PROVIDER against its own two constants,
	// because importing this package would pull both vendor SDKs into configuration
	// loading. A name config accepts but New rejects is a service that boots cleanly
	// and then fails at the first model call, so the two lists are pinned together.
	names := map[string]string{
		config.ProviderAnthropic: NameAnthropic,
		config.ProviderOpenAI:    NameOpenAI,
	}

	// Act, Assert
	for configured, expected := range names {
		if configured != expected {
			t.Fatalf("config accepts %q; this package builds %q", configured, expected)
		}
		client, err := New(configured, Options{APIKey: "k"})
		if err != nil {
			t.Fatalf("build %s: %v", configured, err)
		}
		if client.Name() != configured {
			t.Errorf("provider %q reports itself as %q", configured, client.Name())
		}
	}
}

func TestFailure_statusesTheSDKRetriesAreRetriedByTheLoopToo(t *testing.T) {
	// Arrange
	// The list is the SDKs' own: no response, 408, 409, 429, and 500 up. By the time
	// an error reaches the loop the SDK has already exhausted its attempts, so what
	// is being answered is whether the *step* should run again later.
	cases := map[int]bool{
		0:                                true,
		http.StatusRequestTimeout:        true,
		http.StatusConflict:              true,
		http.StatusTooManyRequests:       true,
		http.StatusInternalServerError:   true,
		http.StatusNotImplemented:        true,
		http.StatusServiceUnavailable:    true,
		http.StatusGatewayTimeout:        true,
		http.StatusBadRequest:            false,
		http.StatusUnauthorized:          false,
		http.StatusForbidden:             false,
		http.StatusNotFound:              false,
		http.StatusUnprocessableEntity:   false,
		http.StatusRequestEntityTooLarge: false,
		http.StatusUnsupportedMediaType:  false,
		http.StatusUpgradeRequired:       false,
		http.StatusPreconditionFailed:    false,
		http.StatusMisdirectedRequest:    false,
	}

	// Act, Assert
	for status, want := range cases {
		err := failure(NameAnthropic, status, "", nil)
		if got := Retryable(err); got != want {
			t.Errorf("status %d retryable = %v; want %v", status, got, want)
		}
	}
}

func TestFailure_anExhaustedAccountIsNotRetriedDespiteBeingA429(t *testing.T) {
	// Arrange, Act
	err := failure(NameOpenAI, http.StatusTooManyRequests, "insufficient_quota", nil)

	// Assert
	// "Slow down" and "this account has no credit" arrive with the same status. Only
	// the first one clears on its own; retrying the second spends a run's whole retry
	// budget against a bill nobody has paid.
	if Retryable(err) {
		t.Error("an exhausted account was treated as a transient rate limit")
	}
}

func TestFailure_aCallThatRanOutOfTheStepTimeoutIsNotRetried(t *testing.T) {
	// Arrange, Act
	err := failure(NameAnthropic, 0, "", context.DeadlineExceeded)

	// Assert
	// A transport failure with no status is normally worth another attempt, but the
	// deadline was the caller's decision and the next attempt has no more time than
	// this one did.
	if Retryable(err) {
		t.Error("a step that ran out of time was queued up to run out of time again")
	}
	// The loop matches on the cause, so it has to survive the wrapping.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("the deadline was lost inside the provider error")
	}
}

func TestRetryable_anErrorFromSomewhereElseIsNotRetried(t *testing.T) {
	// Arrange, Act, Assert
	// Fail closed: an unrecognised failure retried in a loop is an agent spending
	// money on something that is not going to start working, and a run that stops
	// with provider_error sends somebody to look at it.
	if Retryable(errors.New("something else entirely")) {
		t.Error("an unrecognised error was treated as retryable")
	}
	if Retryable(nil) {
		t.Error("a nil error was treated as retryable")
	}
}

func TestError_message_carriesTheStatusAndKindButNotTheResponseBody(t *testing.T) {
	// Arrange
	err := failure(NameAnthropic, http.StatusBadRequest, "invalid_request_error", nil)

	// Act
	message := err.Error()

	// Assert
	for _, want := range []string{NameAnthropic, "400", "invalid_request_error"} {
		if !strings.Contains(message, want) {
			t.Errorf("the error message is missing %q: %s", want, message)
		}
	}
	// The SDK's own message appends the raw response body, and the body of a rejected
	// request quotes the request back — which here means somebody's brief and
	// whatever a tool read on their behalf. Cause stays nil for a vendor answer.
	if err.Cause != nil {
		t.Errorf("a vendor error carried a cause, and with it the response body: %v", err.Cause)
	}
}

func TestUsage_total_billsInputAndOutputTogether(t *testing.T) {
	// Arrange
	usage := Usage{TokensIn: 812, TokensOut: 97}

	// Act, Assert
	// The ladder's allowance covers both directions, so a run that only counted its
	// replies would spend several times its cap on long conversations.
	if usage.Total() != 909 {
		t.Errorf("total = %d; want 909", usage.Total())
	}
}

func TestRequest_validate_refusesTheOtherWaysACallCannotBeSent(t *testing.T) {
	// Arrange
	// Each of these is rejected by both vendors with a message about the wire format.
	// Refusing here instead means the run's stop reason names what the loop got wrong.
	cases := map[string]func(*Request){
		"no model": func(r *Request) {
			r.Model = " "
		},
		"no messages": func(r *Request) {
			r.Messages = nil
		},
		"a tool with no name": func(r *Request) {
			r.Tools = append(r.Tools, ToolSchema{Description: "nameless"})
		},
		"a tool call with no id": func(r *Request) {
			r.Messages = append(r.Messages, Message{
				Role:      RoleAssistant,
				ToolCalls: []ToolCall{{Name: "read_report"}},
			})
		},
		"a tool call with no name": func(r *Request) {
			r.Messages = append(r.Messages, Message{
				Role:      RoleAssistant,
				ToolCalls: []ToolCall{{ID: "toolu_01"}},
			})
		},
	}

	// Act, Assert
	for name, breakIt := range cases {
		req := validRequest()
		breakIt(&req)
		if err := req.Validate(); err == nil {
			t.Errorf("a request with %s was accepted", name)
		}
	}
}

func TestError_message_carriesTheTransportFailureWhenThereWasOne(t *testing.T) {
	// Arrange
	// No status and no vendor type: nothing answered. The cause is all there is to
	// report, and unlike a vendor body it is ours — it holds no prompt.
	err := failure(NameOpenAI, 0, "", errors.New("dial tcp: connection refused"))

	// Act
	message := err.Error()

	// Assert
	if !strings.Contains(message, "connection refused") {
		t.Errorf("the transport failure was not reported: %s", message)
	}
	if strings.Contains(message, "status") {
		t.Errorf("a status was reported for a call that never got one: %s", message)
	}
}

// jsonBody decodes what a provider actually put on the wire.
func jsonBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

// dig walks a decoded JSON body: a string step indexes an object, an int step
// indexes an array. Asserting on the body the vendor would have received is the only
// way to know an adapter built what it claims to build.
func dig(t *testing.T, value any, path ...any) any {
	t.Helper()
	for i, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%v: step %d expected an object, got %T", path, i, value)
			}
			value, ok = object[key]
			if !ok {
				t.Fatalf("%v: step %d has no key %q", path, i, key)
			}
		case int:
			array, ok := value.([]any)
			if !ok {
				t.Fatalf("%v: step %d expected an array, got %T", path, i, value)
			}
			if key >= len(array) {
				t.Fatalf("%v: step %d wants index %d of %d", path, i, key, len(array))
			}
			value = array[key]
		default:
			t.Fatalf("%v: step %d is neither a key nor an index", path, i)
		}
	}
	return value
}

func digString(t *testing.T, value any, path ...any) string {
	t.Helper()
	found := dig(t, value, path...)
	text, ok := found.(string)
	if !ok {
		t.Fatalf("%v: expected a string, got %T", path, found)
	}
	return text
}

func digNumber(t *testing.T, value any, path ...any) float64 {
	t.Helper()
	found := dig(t, value, path...)
	number, ok := found.(float64)
	if !ok {
		t.Fatalf("%v: expected a number, got %T", path, found)
	}
	return number
}

// digLen is how many entries an array has, and 0 when the key is absent — which is
// what an adapter that sent nothing looks like.
func digLen(t *testing.T, value any, key string) int {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", value)
	}
	found, present := object[key]
	if !present || found == nil {
		return 0
	}
	array, ok := found.([]any)
	if !ok {
		t.Fatalf("%q: expected an array, got %T", key, found)
	}
	return len(array)
}
