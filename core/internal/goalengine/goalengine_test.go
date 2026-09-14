package goalengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fake goal engine. It records what arrived and answers what the test set, which
// is all these tests need: every property worth asserting here is either "what did we
// send" or "what did we do with what came back".

// testAPIKey is an obvious non-credential. It exists so a test can prove no error
// path ever quotes the key it authenticated with.
const testAPIKey = "test-key-not-a-real-credential"

// errUnusable stands in for whatever went wrong underneath. It is deliberately not an
// *Error: an error this package did not produce is the case its callers have to be
// careful with.
var errUnusable = errors.New("something underneath failed")

// The envelope bodies below are the goal engine's own, field for field, so a change
// on its side that these tests do not survive is a contract change somebody has to
// look at.

const killSwitchReleasedBody = `{
  "success": true,
  "code": 200,
  "message": "Kill switch read.",
  "data": {"key": "kill_switch", "enabled": false, "updatedAt": null},
  "meta": {"requestId": "req_1", "timestamp": "2026-09-12T10:30:00+07:00"}
}`

const killSwitchEngagedBody = `{
  "success": true,
  "code": 200,
  "message": "Kill switch read.",
  "data": {
    "key": "kill_switch",
    "enabled": true,
    "reason": "supplier portal is returning nonsense",
    "updatedBy": "operator:ops",
    "updatedAt": "2026-09-12T09:00:00+07:00"
  },
  "meta": {"requestId": "req_2", "timestamp": "2026-09-12T10:30:00+07:00"}
}`

// approvalBodyFor is one decision from the spending gate, in the shape its
// approvalView renders.
func approvalBodyFor(outcome, reason string) string {
	return `{
  "success": true,
  "code": 201,
  "message": "Auto-approved under policy.",
  "data": {
    "id": "apr_7",
    "actionType": "supplier.invoice",
    "amount": 1500,
    "currency": "IDR",
    "requestedBy": "bot:wingman-core",
    "goalId": null,
    "idempotencyKey": "run_1:call_1",
    "outcome": "` + outcome + `",
    "policyReason": "` + reason + `",
    "resolution": null,
    "resolvedBy": null,
    "resolvedAt": null,
    "isOpen": false,
    "createdAt": "2026-09-12T10:30:00+07:00",
    "expiresAt": null
  },
  "meta": {"requestId": "req_3", "timestamp": "2026-09-12T10:30:00+07:00"}
}`
}

const sampleRecordedBody = `{
  "success": true,
  "code": 201,
  "message": "Sample recorded.",
  "data": {
    "id": 42,
    "metricKey": "ops.tokens_spent",
    "value": 1234,
    "observedAt": "2026-09-12T10:30:00+07:00",
    "source": "push"
  },
  "meta": {"requestId": "req_4", "timestamp": "2026-09-12T10:30:00+07:00"}
}`

// recorded is one request as the engine saw it.
type recorded struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// decode reads the request body as JSON, which is how the field-level assertions are
// written: the body's shape is the contract, not the Go type that produced it.
func (r recorded) decode(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("request body %q is not JSON: %v", r.body, err)
	}
	return out
}

type engine struct {
	status   int
	body     string
	requests []recorded
}

func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxResponseBody))
	e.requests = append(e.requests, recorded{
		method: r.Method,
		// Escaped, so a test can tell what was actually on the wire rather than
		// what the server decoded it to.
		path:   r.URL.EscapedPath(),
		header: r.Header.Clone(),
		body:   raw,
	})

	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, e.body)
}

// only is the single request the engine received, or a failure saying how many it did.
func (e *engine) only(t *testing.T) recorded {
	t.Helper()
	if len(e.requests) != 1 {
		t.Fatalf("the engine received %d requests; want exactly one", len(e.requests))
	}
	return e.requests[0]
}

// newEngine starts a fake goal engine answering status and body, and returns a client
// pointed at it.
func newEngine(t *testing.T, status int, body string, opts ...Option) (*engine, *Client) {
	t.Helper()

	fake := &engine{status: status, body: body}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)

	client, err := New(server.URL, testAPIKey, append([]Option{WithSpendMetric("ops.tokens_spent")}, opts...)...)
	if err != nil {
		t.Fatalf("New() = %v; want a client", err)
	}
	return fake, client
}

// newUnreachableEngine returns a client pointed at an address nothing is listening on,
// for the transport-failure branch.
func newUnreachableEngine(t *testing.T) *Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	client, err := New(url, testAPIKey, WithSpendMetric("ops.tokens_spent"))
	if err != nil {
		t.Fatalf("New() = %v; want a client", err)
	}
	return client
}

func TestNew_refusesABaseURLItCannotCall(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "empty", baseURL: "   ", want: "required"},
		// A bare host is the likeliest thing an operator writes, and it would
		// otherwise fail at the first request as an unreadable kill switch — which
		// halts every run and says nothing about the typo that caused it.
		{name: "no scheme", baseURL: "goal-engine:8080", want: "http://"},
		{name: "the wrong kind of url", baseURL: "postgres://goal-engine:5432", want: "http://"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act
			client, err := New(test.baseURL, testAPIKey)

			// Assert
			if err == nil {
				t.Fatalf("New(%q) = %+v, nil; want a refusal", test.baseURL, client)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to mention %q", err, test.want)
			}
		})
	}
}

func TestNew_takesTheAddressAsGivenAndItsOptions(t *testing.T) {
	// Arrange, Act
	client, err := New("  http://goal-engine:8080/  ", "  "+testAPIKey+"  ",
		WithTimeout(3*time.Second),
		WithSpendMetric("  ops.tokens_spent  "),
		// Ignored rather than fatal: an option that says "no opinion" should leave
		// the default alone, and nil is what a caller passing a config field gets.
		WithTimeout(0),
		WithHTTPClient(nil),
	)

	// Assert
	if err != nil {
		t.Fatalf("New() = %v; want a client", err)
	}
	// Trailing slash removed, because every path in this package starts with one and
	// //v1/flags/kill-switch is a different URL to some proxies.
	if client.baseURL != "http://goal-engine:8080" {
		t.Errorf("baseURL = %q; want it trimmed", client.baseURL)
	}
	if client.apiKey != testAPIKey {
		t.Errorf("apiKey = %q; want it trimmed", client.apiKey)
	}
	if client.metric != "ops.tokens_spent" {
		t.Errorf("metric = %q; want it trimmed", client.metric)
	}
	if client.http.Timeout != 3*time.Second {
		t.Errorf("timeout = %v; want the option's 3s and not the zero that followed it", client.http.Timeout)
	}
}

func TestClient_everyRequestCarriesTheBotCredential(t *testing.T) {
	// Arrange
	fake, client := newEngine(t, http.StatusOK, killSwitchReleasedBody)

	// Act
	if _, err := client.Engaged(context.Background()); err != nil {
		t.Fatalf("Engaged() = %v; want an answer", err)
	}

	// Assert
	got := fake.only(t)
	if want := "Bearer " + testAPIKey; got.header.Get("Authorization") != want {
		t.Errorf("Authorization = %q; want %q", got.header.Get("Authorization"), want)
	}
	if got.header.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q; want application/json", got.header.Get("Accept"))
	}
	// No body, no content type. A GET with a Content-Type is the kind of detail a
	// strict gateway rejects.
	if got.header.Get("Content-Type") != "" {
		t.Errorf("Content-Type = %q; want none on a GET", got.header.Get("Content-Type"))
	}
}

func TestClient_noCredentialConfigured_sendsNoAuthorizationHeaderRatherThanAnEmptyOne(t *testing.T) {
	// Arrange
	// A keyless client is a misconfiguration, and it surfaces as a 401 — which reads
	// as an unreadable kill switch, which halts. What it must not do is send
	// "Bearer " and have a lenient engine treat that as a caller.
	fake := &engine{status: http.StatusOK, body: killSwitchReleasedBody}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)

	client, err := New(server.URL, "   ")
	if err != nil {
		t.Fatalf("New() = %v; want a client", err)
	}

	// Act
	if _, err := client.Engaged(context.Background()); err != nil {
		t.Fatalf("Engaged() = %v; want an answer", err)
	}

	// Assert
	if _, ok := fake.only(t).header["Authorization"]; ok {
		t.Error("an Authorization header was sent with no credential in it")
	}
}

func TestError_saysWhatHappenedInEachOfItsThreeShapes(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "the engine refused and said why",
			err:  &Error{StatusCode: 403, Body: "operator credential required"},
			want: "goal engine returned 403: operator credential required",
		},
		{
			name: "the engine refused and said nothing",
			err:  &Error{StatusCode: 502},
			want: "goal engine returned 502",
		},
		{
			name: "the answer could not be read",
			err:  &Error{StatusCode: 200, Err: errUnusable},
			want: "goal engine returned 200 but the body was unusable",
		},
		{
			name: "nothing answered at all",
			err:  &Error{Err: errUnusable},
			want: "goal engine unreachable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act, Assert
			if got := test.err.Error(); !strings.Contains(got, test.want) {
				t.Errorf("Error() = %q; want it to contain %q", got, test.want)
			}
		})
	}
}

func TestIsRetryable_agreesWithTheEngineAboutWhatMightClearOnItsOwn(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// The three the goal engine's own client retries, so both ends of one wire
		// hold the same opinion.
		{name: "timeout", err: &Error{StatusCode: 408, Retryable: retryableStatus(408)}, want: true},
		{name: "rate limited", err: &Error{StatusCode: 429, Retryable: retryableStatus(429)}, want: true},
		{name: "server error", err: &Error{StatusCode: 503, Retryable: retryableStatus(503)}, want: true},
		// A transport failure is a restart or a blip, so another attempt is worth
		// making — for a caller that has something to retry. The kill switch does not:
		// it halts, and the halt is what the retry would have been for.
		{name: "unreachable", err: &Error{Retryable: true, Err: errUnusable}, want: true},
		// A rejected request is the caller's fault and will be rejected again.
		{name: "bad request", err: &Error{StatusCode: 400, Retryable: retryableStatus(400)}, want: false},
		{name: "unauthorised", err: &Error{StatusCode: 401, Retryable: retryableStatus(401)}, want: false},
		{name: "not found", err: &Error{StatusCode: 404, Retryable: retryableStatus(404)}, want: false},
		// Anything that is not one of ours. Guessing "retryable" for an unknown
		// error is how a caller ends up retrying a programming mistake.
		{name: "not an engine error", err: errUnusable, want: false},
		{name: "no error", err: nil, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act, Assert
			if got := IsRetryable(test.err); got != test.want {
				t.Errorf("IsRetryable(%v) = %v; want %v", test.err, got, test.want)
			}
		})
	}
}

func TestError_unwrapsToWhatActuallyFailed(t *testing.T) {
	// Arrange
	// Wrapped rather than described. A caller that needs to tell a cancelled context
	// from a refusal asks errors.Is, and it can only ask if the cause survives.
	err := &Error{Retryable: true, Err: errUnusable}

	// Act, Assert
	if !errors.Is(err, errUnusable) {
		t.Errorf("errors.Is() = false; want the cause reachable through %v", err)
	}
}

func TestWithHTTPClient_replacesTheTransportForCallersThatHaveTheirOwn(t *testing.T) {
	// Arrange
	// The option exists for tests and for a caller with its own transport — a proxy, a
	// pinned certificate. Its timeout is theirs too, which is why WithTimeout is a
	// separate option rather than something this one silently overwrites.
	own := &http.Client{Timeout: 42 * time.Second}

	// Act
	client, err := New("http://goal-engine:8080", testAPIKey, WithHTTPClient(own))

	// Assert
	if err != nil {
		t.Fatalf("New() = %v; want a client", err)
	}
	if client.http != own {
		t.Error("the client kept its own transport; want the caller's")
	}
	if client.http.Timeout != 42*time.Second {
		t.Errorf("timeout = %v; want the caller's own", client.http.Timeout)
	}
}

func TestClient_noErrorEverQuotesTheAPIKey(t *testing.T) {
	// The one assertion in this package that is a secrets test. Every error here is
	// destined for a log line or a run's reason field, and this client is the only
	// thing in core holding the goal engine's bot key.
	tests := []struct {
		name string
		call func(t *testing.T) error
	}{
		{
			name: "the engine refused",
			call: func(t *testing.T) error {
				_, client := newEngine(t, http.StatusForbidden, `{"success":false,"error":{"code":"forbidden"}}`)
				_, err := client.Engaged(context.Background())
				return err
			},
		},
		{
			name: "the engine answered with something else",
			call: func(t *testing.T) error {
				_, client := newEngine(t, http.StatusOK, "<html>a proxy login page</html>")
				_, err := client.Engaged(context.Background())
				return err
			},
		},
		{
			name: "nothing answered",
			call: func(t *testing.T) error {
				_, err := newUnreachableEngine(t).Engaged(context.Background())
				return err
			},
		},
		{
			name: "a spend could not be filed",
			call: func(t *testing.T) error {
				_, client := newEngine(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
				_, err := client.RequestSpend(context.Background(), testSpend())
				return err
			},
		},
		{
			name: "a sample could not be filed",
			call: func(t *testing.T) error {
				_, client := newEngine(t, http.StatusBadRequest, `{"error":{"message":"unknown metric"}}`)
				return client.ReportTokens(context.Background(), TokenSample{Tokens: 10})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act
			err := test.call(t)

			// Assert
			if err == nil {
				t.Fatal("the call succeeded; this test needs the failure path")
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Errorf("the error quotes the API key: %v", err)
			}
		})
	}
}

func TestTruncate_boundsWhatAnEnginesErrorPageCanPutInALogLine(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "short enough", text: "  policy not found  ", want: "policy not found"},
		{name: "exactly the limit", text: strings.Repeat("a", maxErrorBody), want: strings.Repeat("a", maxErrorBody)},
		{name: "too long", text: strings.Repeat("a", maxErrorBody+1), want: strings.Repeat("a", maxErrorBody) + "…"},
		{name: "empty", text: "\n\t ", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act, Assert
			if got := truncate(test.text, maxErrorBody); got != test.want {
				t.Errorf("truncate() returned %d bytes (%.20q…); want %d", len(got), got, len(test.want))
			}
		})
	}
}
