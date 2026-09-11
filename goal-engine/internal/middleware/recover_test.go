package middleware

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

func TestRecoverTurnsAPanicIntoAnEnvelopedFiveHundred(t *testing.T) {
	// One bad request must not take the process — and the monitor with it — down.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	rec := serveWith(t, newRequest(t, "/goals"), func(*gin.Context) {
		panic("target value was nil somewhere")
	}, RequestID(), Recover(log))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	body := decode(t, rec)
	if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeInternal {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	if body.Meta.RequestID == "" {
		t.Fatal("expected the request id on the response so the log line can be found")
	}
}

func TestRecoverKeepsThePanicOutOfTheResponse(t *testing.T) {
	// A panic message routinely carries a query fragment or a connection string.
	secret := "postgres://wingman:hunter2@db:5432/goals"
	rec := serveWith(t, newRequest(t, "/goals"), func(*gin.Context) {
		panic("dial failed: " + secret)
	}, RequestID(), Recover(discardLog()))

	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the response leaked the panic value: %s", rec.Body)
	}
}

func TestRecoverLogsEnoughToFindTheRequest(t *testing.T) {
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/goals")
	req.Header.Set(HeaderRequestID, "trace-77")
	serveWith(t, req, func(*gin.Context) {
		panic("boom")
	}, RequestID(), Recover(log))

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, logged.String())
	}
	if entry["requestId"] != "trace-77" {
		t.Fatalf("expected the request id logged, got %v", entry["requestId"])
	}
	if entry["level"] != "error" {
		t.Fatalf("expected error level, got %v", entry["level"])
	}
	if entry["panic"] != "boom" {
		t.Fatalf("expected the panic value logged, got %v", entry["panic"])
	}
	if stack, _ := entry["stack"].(string); !strings.Contains(stack, "middleware") {
		t.Fatal("expected a stack trace in the log line")
	}
}

func TestRecoverLetsANormalRequestThrough(t *testing.T) {
	rec := serve(t, newRequest(t, "/goals"), RequestID(), Recover(discardLog()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRecoverTreatsAClientDisconnectAsNoise(t *testing.T) {
	// The client is gone: there is nobody to send a 500 to, and a stack trace for
	// somebody closing a tab is noise in the log that matters.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	disconnect := &net.OpError{
		Op:  "write",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "write", Err: brokenPipeError{}},
	}

	serveWith(t, newRequest(t, "/goals"), func(*gin.Context) {
		panic(disconnect)
	}, RequestID(), Recover(log))

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, logged.String())
	}
	if entry["level"] != "warn" {
		t.Fatalf("expected warn level for a disconnect, got %v", entry["level"])
	}
	if _, hasStack := entry["stack"]; hasStack {
		t.Fatal("expected no stack trace for a disconnect")
	}
}

func TestIsDisconnectOnlyMatchesARealDisconnect(t *testing.T) {
	cases := []struct {
		name  string
		value any
		wants bool
	}{
		{"a broken pipe", &net.OpError{Err: &os.SyscallError{Err: brokenPipeError{}}}, true},
		{
			"a reset connection",
			&net.OpError{Err: &os.SyscallError{Err: resetError{}}},
			true,
		},
		{"a string panic", "boom", false},
		{"a plain error", os.ErrClosed, false},
		{"some other network error", &net.OpError{Err: os.ErrDeadlineExceeded}, false},
		{"nil", nil, false},
	}

	for _, c := range cases {
		if got := isDisconnect(c.value); got != c.wants {
			t.Fatalf("%s: expected %v, got %v", c.name, c.wants, got)
		}
	}
}

type brokenPipeError struct{}

func (brokenPipeError) Error() string { return "broken pipe" }

type resetError struct{}

func (resetError) Error() string { return "connection reset by peer" }
