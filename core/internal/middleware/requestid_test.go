package middleware

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestRequestID_generatesOneWhenTheCallerSentNone(t *testing.T) {
	rec := serve(t, newRequest(t, "/v1/chats"), RequestID())

	id := rec.Header().Get(HeaderRequestID)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("expected a uuid in the response header, got %q", id)
	}
	if got := decode(t, rec).Meta.RequestID; got != id {
		t.Fatalf("header says %q but the envelope says %q", id, got)
	}
}

func TestRequestID_reusesTheCallersIDSoATraceSurvives(t *testing.T) {
	// A trace that restarts at every hop is not a trace, and here a hop is normal:
	// the goal engine dispatches a task, core runs it, and both write log lines
	// somebody has to join up afterwards.
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderRequestID, "trace-from-the-goal-engine")

	rec := serve(t, req, RequestID())

	if got := rec.Header().Get(HeaderRequestID); got != "trace-from-the-goal-engine" {
		t.Fatalf("expected the caller's id, got %q", got)
	}
	if got := decode(t, rec).Meta.RequestID; got != "trace-from-the-goal-engine" {
		t.Fatalf("expected the caller's id in the envelope, got %q", got)
	}
}

func TestRequestID_refusesAnIDThatIsAnAttack(t *testing.T) {
	// The id is echoed into a response header and written into every log line for the
	// request. A newline in it splits the header or forges a log entry, so a hostile
	// id is replaced rather than sanitised in place.
	cases := []struct {
		name string
		id   string
	}{
		{"header splitting", "abc\r\nX-Admin: true"},
		{"log forging", "abc\ninjected log line"},
		{"a tab", "abc\tdef"},
		{"non-ascii", "abc　def"},
		{"a null byte", "abc\x00def"},
		{"far too long", strings.Repeat("a", maxInboundRequestIDLength+1)},
		{"only whitespace", "   "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := newRequest(t, "/v1/chats")
			req.Header.Set(HeaderRequestID, c.id)

			rec := serve(t, req, RequestID())

			got := rec.Header().Get(HeaderRequestID)
			if _, err := uuid.Parse(got); err != nil {
				t.Fatalf("expected a generated uuid, got %q", got)
			}
			// Replaced, not sanitised in place. Compared whole rather than by
			// substring: every id above fails uuid.Parse, so the line before this one
			// already proves none of them survived — and a substring check against a
			// random hex uuid is a test that fails roughly once in a few hundred runs
			// when the generator happens to emit those characters. CI found exactly
			// that, on "abc" against a851b6ca-…-699a3abc0ca0.
			if got == c.id {
				t.Fatalf("the caller's id survived: %q", got)
			}
		})
	}
}

func TestRequestID_keepsAnIDAtExactlyTheLimit(t *testing.T) {
	id := strings.Repeat("a", maxInboundRequestIDLength)
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderRequestID, id)

	rec := serve(t, req, RequestID())

	if got := rec.Header().Get(HeaderRequestID); got != id {
		t.Fatalf("expected the id kept, got %q", got)
	}
}

func TestRequestID_isReadableFromTheGinContext(t *testing.T) {
	// Handlers read it from here to build the actor they pass into the service layer.
	var seen string
	rec := serveWith(t, newRequest(t, "/v1/chats"), func(c *gin.Context) {
		seen = c.GetString(utils.ContextKeyRequestID)
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID())

	if seen == "" {
		t.Fatal("expected the id on the context")
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("context has %q, header has %q", seen, got)
	}
}

func TestSanitiseRequestID_acceptsOrdinaryTraceIDs(t *testing.T) {
	for _, id := range []string{
		"5f3a1c2e-9b7d-4a1e-8c6f-2d0e4b8a1c33",
		"req_01HZY5K3",
		"  trimmed  ",
	} {
		if got := sanitiseRequestID(id); got != strings.TrimSpace(id) {
			t.Fatalf("expected %q kept, got %q", id, got)
		}
	}
}
