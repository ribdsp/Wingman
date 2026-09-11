package utils

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestContext returns a gin context writing into a recorder.
func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/goals", nil)
	return c, recorder
}

// decode unmarshals the recorded body into a generic map so the test asserts on
// the wire format, not on Go structs.
func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, recorder.Body.String())
	}
	return body
}

func TestSuccessUsesCamelCaseEnvelope(t *testing.T) {
	// Arrange
	c, recorder := newTestContext()
	c.Set(ContextKeyRequestID, "req-123")

	// Act
	Success(c, http.StatusOK, "Goal created", map[string]any{"goalId": "abc"})

	// Assert
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := decode(t, recorder)
	if body["success"] != true {
		t.Fatalf("expected success true, got %v", body["success"])
	}
	if body["message"] != "Goal created" {
		t.Fatalf("unexpected message %v", body["message"])
	}
	meta, ok := body["meta"].(map[string]any)
	if !ok {
		t.Fatalf("expected meta object, got %T", body["meta"])
	}
	if meta["requestId"] != "req-123" {
		t.Fatalf("expected the request id to be echoed, got %v", meta["requestId"])
	}
	if _, ok := meta["timestamp"].(string); !ok {
		t.Fatal("expected a timestamp in meta")
	}
	if _, exists := body["error"]; exists {
		t.Fatal("expected no error key on a success response")
	}
}

func TestSuccessWithPaginationComputesTotalPages(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		totalItems int
		wantPages  float64
	}{
		{"exact multiple", 10, 30, 3},
		{"partial last page", 10, 25, 3},
		{"single item", 10, 1, 1},
		{"empty", 10, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newTestContext()

			SuccessWithPagination(c, http.StatusOK, "ok", []string{}, 1, tc.limit, tc.totalItems)

			meta := decode(t, recorder)["meta"].(map[string]any)
			pagination, ok := meta["pagination"].(map[string]any)
			if !ok {
				t.Fatalf("expected pagination object, got %T", meta["pagination"])
			}
			if pagination["totalPages"] != tc.wantPages {
				t.Fatalf("expected %v pages, got %v", tc.wantPages, pagination["totalPages"])
			}
			if pagination["totalItems"] != float64(tc.totalItems) {
				t.Fatalf("expected %d items, got %v", tc.totalItems, pagination["totalItems"])
			}
		})
	}
}

func TestSuccessWithPaginationNormalisesBadInput(t *testing.T) {
	c, recorder := newTestContext()

	SuccessWithPagination(c, http.StatusOK, "ok", []string{}, 0, 0, 10)

	pagination := decode(t, recorder)["meta"].(map[string]any)["pagination"].(map[string]any)
	if pagination["page"] != float64(1) {
		t.Fatalf("expected page to fall back to 1, got %v", pagination["page"])
	}
	if pagination["limit"] != float64(50) {
		t.Fatalf("expected limit to fall back to 50, got %v", pagination["limit"])
	}
}

func TestErrorCarriesCodeAndMessage(t *testing.T) {
	c, recorder := newTestContext()

	Error(c, http.StatusNotFound, ErrCodeNotFound, "Goal not found")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", recorder.Code)
	}
	body := decode(t, recorder)
	if body["success"] != false {
		t.Fatal("expected success false")
	}
	errInfo, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", body["error"])
	}
	if errInfo["code"] != ErrCodeNotFound {
		t.Fatalf("unexpected error code %v", errInfo["code"])
	}
	if errInfo["message"] != "Goal not found" {
		t.Fatalf("unexpected error message %v", errInfo["message"])
	}
	if _, exists := body["data"]; exists {
		t.Fatal("expected no data key on an error response")
	}
}

func TestValidationErrorCarriesFields(t *testing.T) {
	c, recorder := newTestContext()

	ValidationError(c, "Invalid goal", map[string]string{"targetValue": "must be a number"})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
	errInfo := decode(t, recorder)["error"].(map[string]any)
	fields, ok := errInfo["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected fields object, got %T", errInfo["fields"])
	}
	if fields["targetValue"] != "must be a number" {
		t.Fatalf("unexpected field message %v", fields["targetValue"])
	}
}

func TestRequestIDFallsBackToAGeneratedValue(t *testing.T) {
	c, _ := newTestContext()

	// No middleware ran, so the helper must still produce something usable.
	if id := RequestID(c); id == "" {
		t.Fatal("expected a generated request id")
	}
	if id := RequestID(nil); id == "" {
		t.Fatal("expected a generated request id for a nil context")
	}
}

func TestSetLocationChangesRenderedTimezone(t *testing.T) {
	original := location
	defer func() { location = original }()

	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}

	SetLocation(jakarta)
	if got := NowISO(); !hasOffset(got, "+07:00") {
		t.Fatalf("expected a +07:00 offset, got %q", got)
	}

	SetLocation(nil) // must be ignored rather than resetting to UTC
	if got := NowISO(); !hasOffset(got, "+07:00") {
		t.Fatalf("expected the location to survive a nil call, got %q", got)
	}
}

func hasOffset(timestamp, offset string) bool {
	return len(timestamp) >= len(offset) && timestamp[len(timestamp)-len(offset):] == offset
}
