package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// discardLog is the logger for tests that do not care what was logged.
func discardLog() zerolog.Logger { return zerolog.New(nil).Level(zerolog.Disabled) }

// serve runs one request through the given middleware and a handler that
// answers 200, returning the recorder.
func serve(t *testing.T, req *http.Request, mw ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	return serveWith(t, req, func(c *gin.Context) {
		utils.Success(c, http.StatusOK, "ok", gin.H{"principal": Principal(c)})
	}, mw...)
}

// serveWith runs one request through the middleware and the given handler.
func serveWith(t *testing.T, req *http.Request, handler gin.HandlerFunc, mw ...gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	router := gin.New()
	router.Use(mw...)
	router.GET("/goals", handler)
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

func newRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "203.0.113.7:54321"
	return req
}
