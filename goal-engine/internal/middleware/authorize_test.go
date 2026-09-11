package middleware

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

func TestRequireOperatorLetsAHumanThrough(t *testing.T) {
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, testSecret)

	rec := serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()), RequireOperator(discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
}

func TestRequireOperatorRefusesABot(t *testing.T) {
	// This is the check that keeps an agent from clearing its own spend. If it
	// ever stops holding, the approval gate is decoration.
	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, otherSecret)

	var reached bool
	rec := serveWith(t, req, func(c *gin.Context) {
		reached = true
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), APIKeyAuth(testCredentials(), discardLog()), RequireOperator(discardLog()))

	if reached {
		t.Fatal("the handler ran for a bot")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body.Success || body.Error == nil || body.Error.Code != utils.ErrCodeForbidden {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}

func TestRequireOperatorRecordsWhichAgentTried(t *testing.T) {
	// An agent reaching for an operator-only route is either a bug in its brief or
	// a stolen key. Either way somebody has to be able to find out which one it was.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/goals")
	req.Header.Set(HeaderAPIKey, otherSecret)
	req.Header.Set(HeaderRequestID, "trace-42")

	serve(t, req, RequestID(), APIKeyAuth(testCredentials(), discardLog()), RequireOperator(log))

	entry := entryFrom(t, &logged)
	for field, want := range map[string]any{
		"requestId": "trace-42",
		"principal": "ci",
		"role":      string(RoleBot),
		"level":     "warn",
	} {
		if entry[field] != want {
			t.Fatalf("%s: expected %v, got %v", field, want, entry[field])
		}
	}
}

func TestRequireOperatorTreatsAMissingCallerAsUnauthenticated(t *testing.T) {
	// Mounted without APIKeyAuth in front of it, the honest answer is 401: nobody
	// was identified. A 403 would read as "your key is not enough" and send the
	// operator looking at their key instead of at the router.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	rec := serve(t, newRequest(t, "/goals"), RequestID(), RequireOperator(log))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
	if got := entryFrom(t, &logged)["level"]; got != "error" {
		t.Fatalf("expected the wiring bug logged at error, got %v", got)
	}
}
