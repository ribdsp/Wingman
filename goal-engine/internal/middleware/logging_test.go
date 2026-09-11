package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

// entryFrom reads the single log line a request produced.
func entryFrom(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	return entry
}

func TestAccessLogRecordsWhoAskedForWhatAndHowItWent(t *testing.T) {
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/goals?product=acme")
	req.Header.Set(HeaderRequestID, "trace-9")
	req.Header.Set(HeaderAPIKey, testSecret)

	serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()), AccessLog(log))

	entry := entryFrom(t, &logged)
	for field, want := range map[string]any{
		"requestId": "trace-9",
		"principal": "ops",
		"method":    http.MethodGet,
		"path":      "/goals",
		"route":     "/goals",
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

func TestAccessLogRaisesTheLevelWithTheStatus(t *testing.T) {
	// A 500 buried at info level is a 500 nobody notices.
	cases := []struct {
		status int
		level  string
	}{
		{http.StatusOK, "info"},
		{http.StatusBadRequest, "warn"},
		{http.StatusUnauthorized, "warn"},
		{http.StatusInternalServerError, "error"},
		{http.StatusBadGateway, "error"},
	}

	for _, c := range cases {
		var logged bytes.Buffer
		log := zerolog.New(&logged)

		serveWith(t, newRequest(t, "/goals"), func(ctx *gin.Context) {
			utils.Error(ctx, c.status, "TEST", "test")
		}, RequestID(), AccessLog(log))

		if got := entryFrom(t, &logged)["level"]; got != c.level {
			t.Fatalf("status %d: expected %s, got %v", c.status, c.level, got)
		}
	}
}

func TestAccessLogKeepsHealthChecksOutOfTheWay(t *testing.T) {
	// A probe every few seconds would bury the requests somebody wants to read.
	var logged bytes.Buffer
	log := zerolog.New(&logged).Level(zerolog.InfoLevel)

	serve(t, newRequest(t, "/healthz"), RequestID(), AccessLog(log))

	if logged.Len() != 0 {
		t.Fatalf("expected the health check logged below info, got %s", logged.String())
	}
}

func TestAccessLogStillRecordsAFailingHealthCheck(t *testing.T) {
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

func TestAccessLogEscapesAHostilePath(t *testing.T) {
	// The path is caller-controlled. It has to land in the log as data, not as a
	// second log line.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/goals")
	req.URL.Path = "/goals\n{\"level\":\"info\",\"msg\":\"forged\"}"

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
