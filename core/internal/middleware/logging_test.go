package middleware

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestAccessLog_recordsWhoAskedForWhatAndHowItWent(t *testing.T) {
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/v1/chats?limit=20")
	req.Header.Set(HeaderRequestID, "trace-9")
	req.Header.Set(HeaderAPIKey, operatorSecret)

	serve(t, req, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()), AccessLog(log))

	entry := entryFrom(t, &logged)
	for field, want := range map[string]any{
		"requestId": "trace-9",
		"principal": "ops",
		"role":      string(RoleOperator),
		"method":    http.MethodGet,
		"path":      "/v1/chats",
		"route":     "/v1/chats",
		"status":    float64(http.StatusOK),
		"level":     "info",
	} {
		if entry[field] != want {
			t.Fatalf("%s: expected %v, got %v", field, want, entry[field])
		}
	}
	if _, ok := entry["latency"]; !ok {
		t.Fatal("expected a latency field")
	}
}

func TestAccessLog_saysWhichKindOfCallerItWas(t *testing.T) {
	// The name says which credential acted; the role says whether it should have been
	// able to. A bot appearing where only people belong is the line worth finding.
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_2", Name: "rani"})

	cases := []struct {
		name      string
		header    string
		value     string
		principal string
		role      Role
	}{
		{"an operator key", HeaderAPIKey, operatorSecret, "ops", RoleOperator},
		{"a bot key", HeaderAPIKey, botSecret, "bridge", RoleBot},
		{"a session", HeaderAuthorization, "Bearer " + token, "rani", RoleUser},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			log := zerolog.New(&logged)

			req := newRequest(t, "/v1/chats")
			req.Header.Set(c.header, c.value)

			serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()), AccessLog(log))

			entry := entryFrom(t, &logged)
			if entry["principal"] != c.principal || entry["role"] != string(c.role) {
				t.Fatalf("expected %s/%s, got %v/%v", c.principal, c.role, entry["principal"], entry["role"])
			}
		})
	}
}

func TestAccessLog_neverWritesTheCredentialItself(t *testing.T) {
	// The line identifies the caller by name. Whatever it was authenticated with stays
	// out of the log, because a log is copied, shipped and kept.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_8", Name: "adi"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	serve(t, req, RequestID(), Authenticate(testCredentials(), sessions, discardLog()), AccessLog(log))

	if strings.Contains(logged.String(), token) {
		t.Fatalf("the access log carried a live session token: %s", logged.String())
	}
}

func TestAccessLog_raisesTheLevelWithTheStatus(t *testing.T) {
	// A 500 buried at info level is a 500 nobody notices.
	cases := []struct {
		status int
		level  string
	}{
		{http.StatusOK, "info"},
		{http.StatusBadRequest, "warn"},
		{http.StatusUnauthorized, "warn"},
		{http.StatusTooManyRequests, "warn"},
		{http.StatusInternalServerError, "error"},
		{http.StatusServiceUnavailable, "error"},
	}

	for _, c := range cases {
		var logged bytes.Buffer
		log := zerolog.New(&logged)

		serveWith(t, newRequest(t, "/v1/chats"), func(ctx *gin.Context) {
			utils.Error(ctx, c.status, "TEST", "test")
		}, RequestID(), AccessLog(log))

		if got := entryFrom(t, &logged)["level"]; got != c.level {
			t.Fatalf("status %d: expected %s, got %v", c.status, c.level, got)
		}
	}
}

func TestAccessLog_keepsHealthChecksOutOfTheWay(t *testing.T) {
	// A probe every few seconds would bury the requests somebody wants to read.
	var logged bytes.Buffer
	log := zerolog.New(&logged).Level(zerolog.InfoLevel)

	serve(t, newRequest(t, "/healthz"), RequestID(), AccessLog(log))

	if logged.Len() != 0 {
		t.Fatalf("expected the health check logged below info, got %s", logged.String())
	}
}

func TestAccessLog_stillRecordsAFailingHealthCheck(t *testing.T) {
	// Quiet means quiet while it is passing, not silent when it starts failing.
	var logged bytes.Buffer
	log := zerolog.New(&logged).Level(zerolog.InfoLevel)

	serveWith(t, newRequest(t, "/healthz"), func(c *gin.Context) {
		utils.Error(c, http.StatusServiceUnavailable, utils.ErrCodeUnavailable, "database is unreachable")
	}, RequestID(), AccessLog(log))

	if got := entryFrom(t, &logged)["level"]; got != "error" {
		t.Fatalf("expected a failing probe logged at error, got %v", got)
	}
}

func TestAccessLog_escapesAHostilePath(t *testing.T) {
	// The path is caller-controlled. It has to land in the log as data, not as a
	// second log line.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/v1/chats")
	req.URL.Path = "/v1/chats\n{\"level\":\"info\",\"msg\":\"forged\"}"

	serveWith(t, req, func(c *gin.Context) {
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), AccessLog(log))

	// One JSON object, whatever the caller put in the path: entryFrom fails if the
	// buffer holds anything but a single value.
	entry := entryFrom(t, &logged)
	if entry[zerolog.MessageFieldName] != "request" {
		t.Fatalf("expected one log line for the request, got %v", entry)
	}
	if path, _ := entry["path"].(string); !strings.Contains(path, "forged") {
		t.Fatalf("expected the hostile path kept as data, got %q", path)
	}
}
