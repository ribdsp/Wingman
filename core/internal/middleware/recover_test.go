package middleware

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestRecover_turnsAPanicIntoAnEnvelopedFiveHundred(t *testing.T) {
	// One bad request must not take the process down, and with it every run worker
	// mid-conversation.
	rec := serveWith(t, newRequest(t, "/v1/chats"), func(*gin.Context) {
		panic("tool result was nil somewhere")
	}, RequestID(), Recover(discardLog()))

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

func TestRecover_keepsThePanicOutOfTheResponse(t *testing.T) {
	// A panic here routinely carries a prompt fragment, a tool argument, or a
	// provider's error body — which is to say somebody's data and possibly somebody's
	// credential.
	secret := "sk-ant-api03-hunter2-not-a-real-key"
	rec := serveWith(t, newRequest(t, "/v1/chats"), func(*gin.Context) {
		panic("provider call failed: " + secret)
	}, RequestID(), Recover(discardLog()))

	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the response leaked the panic value: %s", rec.Body)
	}
}

func TestRecover_logsEnoughToFindTheRequest(t *testing.T) {
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderRequestID, "trace-77")
	req.Header.Set(HeaderAPIKey, operatorSecret)

	serveWith(t, req, func(*gin.Context) {
		panic("boom")
	}, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()), Recover(log))

	entry := entryFrom(t, &logged)
	for field, want := range map[string]any{
		"requestId": "trace-77",
		"principal": "ops",
		"level":     "error",
		"panic":     "boom",
	} {
		if entry[field] != want {
			t.Fatalf("%s: expected %v, got %v", field, want, entry[field])
		}
	}
	if stack, _ := entry["stack"].(string); !strings.Contains(stack, "middleware") {
		t.Fatal("expected a stack trace in the log line")
	}
}

func TestRecover_letsANormalRequestThrough(t *testing.T) {
	rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), Recover(discardLog()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRecover_treatsAClientDisconnectAsNoise(t *testing.T) {
	// The client is gone: there is nobody to send a 500 to, and a stack trace for
	// somebody closing a tab mid-answer is noise in the log that matters. It happens
	// more here than elsewhere, because a long streamed reply is a connection people
	// abandon.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	disconnect := &net.OpError{
		Op:  "write",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "write", Err: brokenPipeError{}},
	}

	serveWith(t, newRequest(t, "/v1/chats"), func(*gin.Context) {
		panic(disconnect)
	}, RequestID(), Recover(log))

	entry := entryFrom(t, &logged)
	if entry["level"] != "warn" {
		t.Fatalf("expected warn level for a disconnect, got %v", entry["level"])
	}
	if _, hasStack := entry["stack"]; hasStack {
		t.Fatal("expected no stack trace for a disconnect")
	}
}

func TestIsDisconnect_onlyMatchesARealDisconnect(t *testing.T) {
	cases := []struct {
		name  string
		value any
		wants bool
	}{
		{"a broken pipe", &net.OpError{Err: &os.SyscallError{Err: brokenPipeError{}}}, true},
		{"a reset connection", &net.OpError{Err: &os.SyscallError{Err: resetError{}}}, true},
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
