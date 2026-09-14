package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// discardLog is the logger for tests that do not care what was logged.
func discardLog() zerolog.Logger { return zerolog.New(nil).Level(zerolog.Disabled) }

// serve runs one request through the given middleware and a handler that answers 200
// with whatever authentication decided, returning the recorder.
func serve(t *testing.T, req *http.Request, mw ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, req, func(c *gin.Context) {
		caller, _ := CallerOf(c)
		utils.Success(c, http.StatusOK, "ok", gin.H{
			"principal": Principal(c),
			"role":      string(caller.Role),
			"userId":    caller.UserID,
		})
	}, mw...)
}

// serveWith runs one request through the middleware and the given handler.
func serveWith(t *testing.T, req *http.Request, handler gin.HandlerFunc, mw ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	router := gin.New()
	router.Use(mw...)
	router.GET("/v1/chats", handler)
	router.GET("/healthz", handler)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decode reads the standard envelope out of a response.
func decode(t *testing.T, rec *httptest.ResponseRecorder) utils.Response {
	t.Helper()
	var body utils.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the standard envelope: %v (%s)", err, rec.Body.String())
	}
	return body
}

// dataOf reads the success payload, which is where serve reports who authenticated.
func dataOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decode(t, rec)
	data, ok := body.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data: %v", body.Data)
	}
	return data
}

// entryFrom reads the single log line a request produced. It fails if the buffer holds
// anything but one JSON value, which is what makes the log-forging tests meaningful.
func entryFrom(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, buf.String())
	}
	return entry
}

func newRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "203.0.113.7:54321"
	return req
}

// withCaller puts a caller on the context without authenticating, for the states
// Authenticate itself refuses to produce — a user with no id, most of all. The
// guards downstream still have to hold if one ever appeared.
func withCaller(caller Caller) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ContextKeyCaller, caller)
		c.Next()
	}
}
