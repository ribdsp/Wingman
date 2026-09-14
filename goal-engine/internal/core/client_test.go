package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capturedRequest is what a stub core server saw. Task and notification bodies
// share no field names, so decoding into both is harmless and keeps one stub.
type capturedRequest struct {
	method  string
	path    string
	headers http.Header
	body    TaskRequest
	notify  NotifyRequest
	raw     string
}

// stubCore starts a server that records one request and answers with the given
// status and body.
func stubCore(t *testing.T, status int, responseBody string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.headers = r.Header.Clone()
		captured.raw = string(raw)
		_ = json.Unmarshal(raw, &captured.body)
		_ = json.Unmarshal(raw, &captured.notify)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	return server, captured
}

func testTask() TaskRequest {
	return TaskRequest{
		BotID:          "bot-1",
		ChannelID:      "chan-1",
		Brief:          "MRR is behind pace; propose and run a recovery plan.",
		IdempotencyKey: "goal-1:2026-09-11T08",
		Metadata:       map[string]string{"goalId": "goal-1", "product": "acme"},
	}
}

func TestCreateTaskSendsTheBriefAndCredentials(t *testing.T) {
	// Arrange
	server, captured := stubCore(t, http.StatusCreated, `{"id":"task-42"}`)
	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("expected a client, got %v", err)
	}

	// Act
	resp, err := client.CreateTask(context.Background(), testTask())

	// Assert
	if err != nil {
		t.Fatalf("expected the task to be created, got %v", err)
	}
	if resp.TaskID != "task-42" || resp.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected response %+v", resp)
	}
	if captured.method != http.MethodPost || captured.path != DefaultTaskPath {
		t.Fatalf("expected POST %s, got %s %s", DefaultTaskPath, captured.method, captured.path)
	}
	if got := captured.headers.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("unexpected authorization header %q", got)
	}
	if got := captured.headers.Get("Idempotency-Key"); got != "goal-1:2026-09-11T08" {
		t.Fatalf("unexpected idempotency header %q", got)
	}
	if captured.body.Brief != testTask().Brief || captured.body.Metadata["goalId"] != "goal-1" {
		t.Fatalf("unexpected body %+v", captured.body)
	}
}

func TestCreateTaskSendsCamelCaseJSON(t *testing.T) {
	// The rest of the stack consumes camelCase, and a snake_case field here would
	// be silently dropped by core rather than rejected.
	server, captured := stubCore(t, http.StatusOK, `{}`)
	client, _ := New(server.URL, "")

	if _, err := client.CreateTask(context.Background(), testTask()); err != nil {
		t.Fatalf("expected the task to be created, got %v", err)
	}
	for _, want := range []string{`"botId"`, `"channelId"`, `"idempotencyKey"`} {
		if !strings.Contains(captured.raw, want) {
			t.Fatalf("expected %s in %s", want, captured.raw)
		}
	}
}

func TestCreateTaskOmitsAuthorizationWhenNoKeyIsConfigured(t *testing.T) {
	// Sending "Bearer " with nothing after it looks like a credential to a log
	// scanner and is not one.
	server, captured := stubCore(t, http.StatusOK, `{}`)
	client, _ := New(server.URL, "   ")

	if _, err := client.CreateTask(context.Background(), testTask()); err != nil {
		t.Fatalf("expected the task to be created, got %v", err)
	}
	if _, ok := captured.headers["Authorization"]; ok {
		t.Fatal("expected no authorization header when no key is configured")
	}
}

func TestCreateTaskRefusesAMissingIdempotencyKey(t *testing.T) {
	// Without a key a retry creates a second task, which is the one failure mode
	// that turns a monitoring blip into duplicated spending.
	client, _ := New("http://localhost:1", "")
	task := testTask()
	task.IdempotencyKey = "  "

	if _, err := client.CreateTask(context.Background(), task); err == nil {
		t.Fatal("expected a dispatch without an idempotency key to be refused")
	}
}

func TestCreateTaskReadsTheTaskIDFromAlternativeEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"flat id", `{"id":"a"}`, "a"},
		{"flat taskId", `{"taskId":"b"}`, "b"},
		{"wrapped id", `{"data":{"id":"c"}}`, "c"},
		{"wrapped taskId", `{"data":{"taskId":"d"}}`, "d"},
		{"no identifier", `{"ok":true}`, ""},
		{"not json", `accepted`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := stubCore(t, http.StatusOK, tc.body)
			client, _ := New(server.URL, "")

			resp, err := client.CreateTask(context.Background(), testTask())
			if err != nil {
				t.Fatalf("expected the task to be created, got %v", err)
			}
			if resp.TaskID != tc.want {
				t.Fatalf("expected task id %q, got %q", tc.want, resp.TaskID)
			}
		})
	}
}

func TestCreateTaskTreatsAnUnreadableIDAsAcceptedAnyway(t *testing.T) {
	// A missing task id costs an audit-trail link. Reporting it as a failure would
	// cost a duplicate task, which is worse.
	server, _ := stubCore(t, http.StatusAccepted, `{"unexpected":"shape"}`)
	client, _ := New(server.URL, "")

	resp, err := client.CreateTask(context.Background(), testTask())
	if err != nil {
		t.Fatalf("expected the task to be accepted, got %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
}

func TestCreateTaskClassifiesFailuresByWhetherRetryingCouldHelp(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusConflict, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			server, _ := stubCore(t, tc.status, `{"error":"nope"}`)
			client, _ := New(server.URL, "")

			_, err := client.CreateTask(context.Background(), testTask())
			if err == nil {
				t.Fatalf("expected status %d to be an error", tc.status)
			}
			if got := IsRetryable(err); got != tc.wantRetryable {
				t.Fatalf("expected retryable=%v for %d, got %v (%v)", tc.wantRetryable, tc.status, got, err)
			}

			var coreErr *Error
			if !errors.As(err, &coreErr) || coreErr.StatusCode != tc.status {
				t.Fatalf("expected a *core.Error carrying the status, got %#v", err)
			}
		})
	}
}

func TestCreateTaskTreatsAnUnreachableCoreAsRetryable(t *testing.T) {
	// Core restarting during a deploy must not permanently drop the trigger.
	client, _ := New("http://127.0.0.1:1", "", WithTimeout(200*time.Millisecond))

	_, err := client.CreateTask(context.Background(), testTask())
	if err == nil {
		t.Fatal("expected an unreachable core to be an error")
	}
	if !IsRetryable(err) {
		t.Fatalf("expected a transport failure to be retryable, got %v", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("expected the error to name the cause, got %v", err)
	}
}

func TestCreateTaskTruncatesAHugeErrorBody(t *testing.T) {
	// An HTML error page must not end up whole in a log line or an audit row.
	server, _ := stubCore(t, http.StatusInternalServerError, strings.Repeat("x", 4096))
	client, _ := New(server.URL, "")

	_, err := client.CreateTask(context.Background(), testTask())
	if err == nil {
		t.Fatal("expected an error")
	}
	var coreErr *Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("expected a *core.Error, got %#v", err)
	}
	if len(coreErr.Body) > maxErrorBody+len("…") {
		t.Fatalf("expected the body to be truncated, got %d bytes", len(coreErr.Body))
	}
}

func TestCreateTaskHonoursAContextDeadline(t *testing.T) {
	// The handler outlives the caller's deadline but returns on its own, so the
	// test never depends on cancellation reaching the server: it only asserts that
	// the caller stops waiting.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	client, _ := New(server.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := client.CreateTask(ctx, testTask()); err == nil {
		t.Fatal("expected the deadline to abort the dispatch")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("expected the dispatch to give up at the deadline, waited %s", elapsed)
	}
}

func TestDryRunNeverReachesTheNetwork(t *testing.T) {
	// The handler failing the test is the assertion: dry run exists so the whole
	// autonomous loop can be exercised against live metrics without any agent
	// actually being woken.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("dry run must not send a request")
	}))
	t.Cleanup(server.Close)

	client, _ := New(server.URL, "secret", WithDryRun(true))
	if !client.DryRun() {
		t.Fatal("expected dry run to be reported")
	}

	resp, err := client.CreateTask(context.Background(), testTask())
	if err != nil {
		t.Fatalf("expected a dry run to succeed, got %v", err)
	}
	if !resp.DryRun || resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected dry-run response %+v", resp)
	}
}

func TestDryRunStillRequiresAnIdempotencyKey(t *testing.T) {
	// Otherwise a dispatch that works in dry run fails the moment it goes live.
	client, _ := New("http://localhost:1", "", WithDryRun(true))
	task := testTask()
	task.IdempotencyKey = ""

	if _, err := client.CreateTask(context.Background(), task); err == nil {
		t.Fatal("expected the missing key to be refused in dry run too")
	}
}

func TestWithTaskPathNormalisesALeadingSlash(t *testing.T) {
	server, captured := stubCore(t, http.StatusOK, `{}`)
	client, _ := New(server.URL+"/", "", WithTaskPath("v2/tasks"))

	if _, err := client.CreateTask(context.Background(), testTask()); err != nil {
		t.Fatalf("expected the task to be created, got %v", err)
	}
	if captured.path != "/v2/tasks" {
		t.Fatalf("expected /v2/tasks, got %s", captured.path)
	}
}

func TestNewRejectsAnUnusableBaseURL(t *testing.T) {
	for _, baseURL := range []string{"", "   ", "localhost:3000", "ftp://core"} {
		if _, err := New(baseURL, "key"); err == nil {
			t.Fatalf("expected %q to be rejected", baseURL)
		}
	}
}

func TestOptionsIgnoreMeaninglessValues(t *testing.T) {
	// A zero timeout would mean "wait forever", and a blank path would produce a
	// request to the bare host. Both are worse than the default.
	client, err := New("http://core", "key",
		WithTimeout(0),
		WithTaskPath("   "),
		WithNotifyPath(""),
		WithHTTPClient(nil),
	)
	if err != nil {
		t.Fatalf("expected a client, got %v", err)
	}
	if client.taskPath != DefaultTaskPath {
		t.Fatalf("expected the default path, got %q", client.taskPath)
	}
	if client.notifyPath != DefaultNotifyPath {
		t.Fatalf("expected the default notify path, got %q", client.notifyPath)
	}
	if client.http == nil || client.http.Timeout != DefaultTimeout {
		t.Fatalf("expected the default timeout, got %+v", client.http)
	}
}

func TestWithHTTPClientReplacesTheTransport(t *testing.T) {
	custom := &http.Client{Timeout: time.Second}
	client, _ := New("http://core", "key", WithHTTPClient(custom))
	if client.http != custom {
		t.Fatal("expected the supplied client to be used")
	}
}

func TestErrorMessageNeverCarriesTheAPIKey(t *testing.T) {
	server, _ := stubCore(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	client, _ := New(server.URL, "super-secret-key")

	_, err := client.CreateTask(context.Background(), testTask())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("expected the key to stay out of the error, got %v", err)
	}
}

func TestIsRetryableIgnoresUnrelatedErrors(t *testing.T) {
	if IsRetryable(errors.New("something else")) {
		t.Fatal("expected an unrelated error not to be retryable")
	}
	if IsRetryable(nil) {
		t.Fatal("expected nil not to be retryable")
	}
}

func TestErrorUnwrapsTheTransportFailure(t *testing.T) {
	sentinel := errors.New("dial failed")
	err := &Error{Retryable: true, Err: sentinel}
	if !errors.Is(err, sentinel) {
		t.Fatal("expected the transport failure to be unwrappable")
	}
	if !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("expected the cause in the message, got %v", err)
	}
}

func TestErrorMessageWithoutABody(t *testing.T) {
	err := &Error{StatusCode: http.StatusBadGateway}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected the status in the message, got %v", err)
	}
}

// --- Notify -----------------------------------------------------------------

func testNotification() NotifyRequest {
	return NotifyRequest{
		Kind:      NotifyApprovalPending,
		SubjectID: "req-7",
		Headline:  "A spend needs a decision.",
		Link:      "https://console.example.com/goals",
	}
}

func TestNotifySendsTheSubjectAndCredentials(t *testing.T) {
	// Arrange
	server, captured := stubCore(t, http.StatusAccepted, `{"delivered":1}`)
	client, err := New(server.URL, "secret-key")
	if err != nil {
		t.Fatalf("expected a client, got %v", err)
	}

	// Act
	err = client.Notify(context.Background(), testNotification())

	// Assert
	if err != nil {
		t.Fatalf("expected the notification to be accepted, got %v", err)
	}
	if captured.method != http.MethodPost || captured.path != DefaultNotifyPath {
		t.Fatalf("expected POST %s, got %s %s", DefaultNotifyPath, captured.method, captured.path)
	}
	if got := captured.headers.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("unexpected authorization header %q", got)
	}
	if captured.notify.Kind != NotifyApprovalPending || captured.notify.SubjectID != "req-7" {
		t.Fatalf("unexpected body %+v", captured.notify)
	}
	for _, want := range []string{`"subjectId"`, `"headline"`, `"link"`} {
		if !strings.Contains(captured.raw, want) {
			t.Fatalf("expected %s in %s", want, captured.raw)
		}
	}
}

func TestNotifyCarriesNoIdempotencyKey(t *testing.T) {
	// A duplicate ping is a nuisance; a suppressed one is a decision nobody hears
	// about. Core is deliberately not asked to deduplicate these.
	server, captured := stubCore(t, http.StatusAccepted, `{}`)
	client, _ := New(server.URL, "")

	if err := client.Notify(context.Background(), testNotification()); err != nil {
		t.Fatalf("expected the notification to be accepted, got %v", err)
	}
	if _, ok := captured.headers["Idempotency-Key"]; ok {
		t.Fatal("expected no idempotency header on a notification")
	}
}

func TestNotifyOmitsAnEmptyLink(t *testing.T) {
	// WEB_BASE_URL is optional, and "link":"" in the body would render as a dead
	// one rather than as no link at all.
	server, captured := stubCore(t, http.StatusAccepted, `{}`)
	client, _ := New(server.URL, "")
	notification := testNotification()
	notification.Link = ""

	if err := client.Notify(context.Background(), notification); err != nil {
		t.Fatalf("expected the notification to be accepted, got %v", err)
	}
	if strings.Contains(captured.raw, `"link"`) {
		t.Fatalf("expected no link field, got %s", captured.raw)
	}
}

func TestNotifyRefusesAnIncompleteNotification(t *testing.T) {
	// Each of these would reach core as a message with nothing in it, and core
	// would reject it — better to fail here, where the log line names the caller.
	cases := map[string]NotifyRequest{
		"no kind":     {SubjectID: "req-7", Headline: "something happened"},
		"no subject":  {Kind: NotifyTrigger, Headline: "something happened"},
		"no headline": {Kind: NotifyTrigger, SubjectID: "req-7", Headline: "   "},
		"wrong kind":  {Kind: "shout", SubjectID: "req-7", Headline: "something happened"},
	}
	for name, notification := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := New("http://localhost:1", "")
			if err := client.Notify(context.Background(), notification); err == nil {
				t.Fatal("expected the notification to be refused")
			}
		})
	}
}

func TestNotifyClassifiesFailuresByWhetherRetryingCouldHelp(t *testing.T) {
	// Nothing retries a notification today, but the caller logs whether it could
	// have — a permanent 401 and a transient 503 are different operator problems.
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			server, _ := stubCore(t, tc.status, `{"error":"nope"}`)
			client, _ := New(server.URL, "")

			err := client.Notify(context.Background(), testNotification())
			if err == nil {
				t.Fatalf("expected status %d to be an error", tc.status)
			}
			if got := IsRetryable(err); got != tc.wantRetryable {
				t.Fatalf("expected retryable=%v for %d, got %v (%v)", tc.wantRetryable, tc.status, got, err)
			}
			var coreErr *Error
			if !errors.As(err, &coreErr) || coreErr.StatusCode != tc.status {
				t.Fatalf("expected a *core.Error carrying the status, got %#v", err)
			}
		})
	}
}

func TestNotifyTreatsAnUnreachableCoreAsRetryable(t *testing.T) {
	client, _ := New("http://127.0.0.1:1", "", WithTimeout(200*time.Millisecond))

	err := client.Notify(context.Background(), testNotification())
	if err == nil {
		t.Fatal("expected an unreachable core to be an error")
	}
	if !IsRetryable(err) {
		t.Fatalf("expected a transport failure to be retryable, got %v", err)
	}
}

func TestNotifyTruncatesAHugeErrorBody(t *testing.T) {
	server, _ := stubCore(t, http.StatusInternalServerError, strings.Repeat("x", 4096))
	client, _ := New(server.URL, "")

	err := client.Notify(context.Background(), testNotification())
	var coreErr *Error
	if !errors.As(err, &coreErr) {
		t.Fatalf("expected a *core.Error, got %#v", err)
	}
	if len(coreErr.Body) > maxErrorBody+len("…") {
		t.Fatalf("expected the body to be truncated, got %d bytes", len(coreErr.Body))
	}
}

func TestNotifyErrorNeverCarriesTheAPIKey(t *testing.T) {
	server, _ := stubCore(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	client, _ := New(server.URL, "super-secret-key")

	err := client.Notify(context.Background(), testNotification())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Fatalf("expected the key to stay out of the error, got %v", err)
	}
}

func TestNotifyDryRunNeverReachesTheNetwork(t *testing.T) {
	// A dry-run engine wakes nobody, and that has to include the operator's phone:
	// the whole point of dry run is that the loop can be exercised without anyone
	// being told to act.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("dry run must not send a notification")
	}))
	t.Cleanup(server.Close)

	client, _ := New(server.URL, "secret", WithDryRun(true))
	if err := client.Notify(context.Background(), testNotification()); err != nil {
		t.Fatalf("expected a dry run to succeed, got %v", err)
	}
}

func TestNotifyHonoursAContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	client, _ := New(server.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	if err := client.Notify(ctx, testNotification()); err == nil {
		t.Fatal("expected the deadline to abort the notification")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("expected the notification to give up at the deadline, waited %s", elapsed)
	}
}

func TestWithNotifyPathNormalisesALeadingSlash(t *testing.T) {
	server, captured := stubCore(t, http.StatusAccepted, `{}`)
	client, _ := New(server.URL+"/", "", WithNotifyPath("v2/notifications"))

	if err := client.Notify(context.Background(), testNotification()); err != nil {
		t.Fatalf("expected the notification to be accepted, got %v", err)
	}
	if captured.path != "/v2/notifications" {
		t.Fatalf("expected /v2/notifications, got %s", captured.path)
	}
}
