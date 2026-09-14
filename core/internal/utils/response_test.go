package utils

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// record runs one handler and decodes the envelope it wrote.
func record(t *testing.T, handler gin.HandlerFunc) (*httptest.ResponseRecorder, Response) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	handler(c)

	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, rec.Body.String())
	}
	return rec, body
}

func TestSuccess_writesTheEnvelope(t *testing.T) {
	// Arrange
	payload := map[string]string{"id": "run_01"}

	// Act
	rec, body := record(t, func(c *gin.Context) {
		c.Set(ContextKeyRequestID, "req_01")
		Success(c, http.StatusOK, "Run started", payload)
	})

	// Assert
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if !body.Success {
		t.Error("success = false on a success response")
	}
	// The envelope repeats the status inside the body because a client reading a
	// logged payload has no headers to consult.
	if body.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", body.Code)
	}
	if body.Message != "Run started" {
		t.Errorf("message = %q; want %q", body.Message, "Run started")
	}
	if body.Error != nil {
		t.Errorf("a success response carries an error block: %+v", body.Error)
	}
	if body.Meta.RequestID != "req_01" {
		t.Errorf("meta.requestId = %q; want the id the middleware set", body.Meta.RequestID)
	}
}

// The goal engine's client reads a task id from data.id, so the data block must
// serialise as itself and not be wrapped a second time.
func TestSuccess_dataIsNotDoubleWrapped(t *testing.T) {
	// Arrange
	type created struct {
		ID string `json:"id"`
	}

	// Act
	rec, _ := record(t, func(c *gin.Context) {
		Success(c, http.StatusCreated, "Created", created{ID: "task_01"})
	})

	// Assert
	var raw struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("could not decode: %v", err)
	}
	if raw.Data.ID != "task_01" {
		t.Errorf("data.id = %q; want %q — this is the path the goal engine's client parses", raw.Data.ID, "task_01")
	}
}

func TestError_reportsFailedWithTheCodeAndNoData(t *testing.T) {
	// Act
	rec, body := record(t, func(c *gin.Context) {
		Error(c, http.StatusForbidden, ErrCodeForbidden, "operator only")
	})

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
	if body.Success {
		t.Error("success = true on an error response")
	}
	// "Failed" is fixed so a client can branch on success rather than parsing prose.
	if body.Message != "Failed" {
		t.Errorf("message = %q; want %q", body.Message, "Failed")
	}
	if body.Error == nil {
		t.Fatal("error response has no error block")
	}
	if body.Error.Code != ErrCodeForbidden {
		t.Errorf("error.code = %q; want %q", body.Error.Code, ErrCodeForbidden)
	}
	if body.Data != nil {
		t.Errorf("an error response carries data: %+v", body.Data)
	}
}

func TestValidationError_carriesPerFieldMessages(t *testing.T) {
	// Arrange
	fields := map[string]string{"brief": "is required", "source": "is required"}

	// Act
	rec, body := record(t, func(c *gin.Context) {
		ValidationError(c, "the request is not valid", fields)
	})

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if body.Error == nil {
		t.Fatal("no error block")
	}
	if body.Error.Code != ErrCodeValidation {
		t.Errorf("error.code = %q; want %q", body.Error.Code, ErrCodeValidation)
	}
	// One round trip should be enough to learn every problem.
	if len(body.Error.Fields) != 2 {
		t.Errorf("error.fields = %v; want both fields", body.Error.Fields)
	}
}

func TestSuccessWithPagination_computesTotalPages(t *testing.T) {
	// Arrange
	// 7 items at 3 per page is three pages, the last one short.
	const items, limit = 7, 3

	// Act
	_, body := record(t, func(c *gin.Context) {
		SuccessWithPagination(c, http.StatusOK, "OK", []string{}, 1, limit, items)
	})

	// Assert
	if body.Meta.Pagination == nil {
		t.Fatal("no pagination block")
	}
	if got := body.Meta.Pagination.TotalPages; got != 3 {
		t.Errorf("totalPages = %d; want 3", got)
	}
}

func TestSuccessWithPagination_nonsensePageAndLimitFallBackToTheFirstPage(t *testing.T) {
	// Act
	_, body := record(t, func(c *gin.Context) {
		SuccessWithPagination(c, http.StatusOK, "OK", []string{}, 0, -5, 10)
	})

	// Assert
	// A zero limit would divide by zero computing totalPages; falling back beats
	// returning a 500 for a bad query string.
	p := body.Meta.Pagination
	if p == nil {
		t.Fatal("no pagination block")
	}
	if p.Page != 1 || p.Limit != 50 {
		t.Errorf("page/limit = %d/%d; want 1/50", p.Page, p.Limit)
	}
}

func TestSuccessWithPagination_noItems_isZeroPagesNotOne(t *testing.T) {
	// Act
	_, body := record(t, func(c *gin.Context) {
		SuccessWithPagination(c, http.StatusOK, "OK", []string{}, 1, 20, 0)
	})

	// Assert
	// An empty list has no pages. Reporting one page invites a client to fetch it.
	if got := body.Meta.Pagination.TotalPages; got != 0 {
		t.Errorf("totalPages = %d; want 0", got)
	}
}

func TestRequestID_generatesOneWhenTheMiddlewareDidNotRun(t *testing.T) {
	// Arrange
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	// Act
	got := RequestID(c)

	// Assert
	// Every response carries a correlation id, including one produced on a path
	// that skipped the middleware — an untraceable response is the one you need.
	if got == "" {
		t.Error("RequestID returned empty")
	}
}

func TestRequestID_nilContext_stillReturnsAnID(t *testing.T) {
	// Act
	got := RequestID(nil)

	// Assert
	if got == "" {
		t.Error("RequestID(nil) returned empty")
	}
}

func TestNowISO_usesTheConfiguredZone(t *testing.T) {
	// Arrange
	// Restore the package default so test order does not matter.
	t.Cleanup(func() { SetLocation(time.UTC) })
	jakarta := time.FixedZone("WIB", 7*60*60)

	// Act
	SetLocation(jakarta)
	got := NowISO()

	// Assert
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("NowISO() = %q, which is not RFC 3339: %v", got, err)
	}
	if _, offset := parsed.Zone(); offset != 7*60*60 {
		t.Errorf("offset = %ds; want 25200 (+07:00)", offset)
	}
}

func TestSetLocation_ignoresNil(t *testing.T) {
	// Arrange
	t.Cleanup(func() { SetLocation(time.UTC) })
	jakarta := time.FixedZone("WIB", 7*60*60)
	SetLocation(jakarta)

	// Act
	SetLocation(nil)
	got := NowISO()

	// Assert
	// A nil zone from a misread config must not silently move every timestamp to
	// UTC; the previous setting stands.
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("NowISO() = %q: %v", got, err)
	}
	if _, offset := parsed.Zone(); offset != 7*60*60 {
		t.Errorf("offset = %ds; want the previous zone to survive a nil", offset)
	}
}

func TestErrorCodes_areTheNinePublishedStrings(t *testing.T) {
	// Arrange
	// Adding a tenth is an API change, not a detail — so this list is the guard.
	want := map[string]bool{
		"VALIDATION_ERROR": true, "UNAUTHORIZED": true, "FORBIDDEN": true,
		"NOT_FOUND": true, "RATE_LIMITED": true, "INTERNAL_ERROR": true,
		"SERVICE_UNAVAILABLE": true, "UNKNOWN_METRIC": true, "ALREADY_RESOLVED": true,
	}
	got := []string{
		ErrCodeValidation, ErrCodeUnauthorized, ErrCodeForbidden, ErrCodeNotFound,
		ErrCodeRateLimited, ErrCodeInternal, ErrCodeUnavailable,
		ErrCodeUnknownMetric, ErrCodeAlreadyResolved,
	}

	// Act & Assert
	if len(got) != len(want) {
		t.Fatalf("%d codes declared; want %d", len(got), len(want))
	}
	for _, code := range got {
		if !want[code] {
			t.Errorf("%q is not one of the nine published codes", code)
		}
	}
}
