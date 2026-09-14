package middleware

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/utils"
)

func TestRequireOperator_letsAnOperatorThrough(t *testing.T) {
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, operatorSecret)

	rec := serve(t, req, RequestID(),
		Authenticate(testCredentials(), newSessions(), discardLog()),
		RequireOperator(discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
}

func TestRequireOperator_refusesABot(t *testing.T) {
	// This is the check that keeps an agent from widening its own tool grants. If it
	// ever stops holding, the approval gate is decoration.
	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, botSecret)

	var reached bool
	rec := serveWith(t, req, func(c *gin.Context) {
		reached = true
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), Authenticate(testCredentials(), newSessions(), discardLog()), RequireOperator(discardLog()))

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

func TestRequireOperator_refusesASignedInUser(t *testing.T) {
	// An account is not an administrator of the box it runs on. Somebody who signs up
	// must not be able to reach the routes that decide what tools cost money.
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_4", Name: "dewi"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	rec := serve(t, req, RequestID(),
		Authenticate(testCredentials(), sessions, discardLog()),
		RequireOperator(discardLog()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body)
	}
}

func TestRequireOperator_recordsWhichCallerTried(t *testing.T) {
	// An agent reaching for an operator-only route is either a bug in its brief or a
	// stolen key. Either way somebody has to be able to find out which one it was.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAPIKey, botSecret)
	req.Header.Set(HeaderRequestID, "trace-42")

	serve(t, req, RequestID(),
		Authenticate(testCredentials(), newSessions(), discardLog()),
		RequireOperator(log))

	entry := entryFrom(t, &logged)
	for field, want := range map[string]any{
		"requestId": "trace-42",
		"principal": "bridge",
		"role":      string(RoleBot),
		"method":    http.MethodGet,
		"level":     "warn",
	} {
		if entry[field] != want {
			t.Fatalf("%s: expected %v, got %v", field, want, entry[field])
		}
	}
}

func TestRequireOperator_treatsAMissingCallerAsUnauthenticated(t *testing.T) {
	// Mounted without Authenticate in front of it, the honest answer is 401: nobody
	// was identified. A 403 would read as "your key is not enough" and send the
	// operator looking at their key instead of at the router.
	var logged bytes.Buffer
	log := zerolog.New(&logged)

	rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), RequireOperator(log))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
	if got := entryFrom(t, &logged)["level"]; got != "error" {
		t.Fatalf("expected the wiring bug logged at error, got %v", got)
	}
}

func TestRequireUser_letsASignedInPersonThrough(t *testing.T) {
	sessions := newSessions()
	token := sessions.mint(t, Session{UserID: "usr_5", Name: "budi"})

	req := newRequest(t, "/v1/chats")
	req.Header.Set(HeaderAuthorization, "Bearer "+token)

	rec := serve(t, req, RequestID(),
		Authenticate(testCredentials(), sessions, discardLog()),
		RequireUser(discardLog()))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	if got := dataOf(t, rec)["userId"]; got != "usr_5" {
		t.Fatalf("expected the owner on the context, got %v", got)
	}
}

func TestRequireUser_refusesAMachineKeyIncludingAnOperators(t *testing.T) {
	// These routes answer "show me *my* conversations", and a machine credential has
	// no answer to that question. Refusing an operator here is the boundary working,
	// not an oversight — whoever runs the instance reads those rows in the database,
	// which leaves a trace that a route would not.
	for _, c := range []struct {
		name   string
		secret string
	}{
		{"an operator key", operatorSecret},
		{"a bot key", botSecret},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := newRequest(t, "/v1/chats")
			req.Header.Set(HeaderAPIKey, c.secret)

			rec := serve(t, req, RequestID(),
				Authenticate(testCredentials(), newSessions(), discardLog()),
				RequireUser(discardLog()))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body)
			}
			if body := decode(t, rec); body.Error == nil || body.Error.Code != utils.ErrCodeForbidden {
				t.Fatalf("unexpected envelope: %+v", body)
			}
		})
	}
}

func TestRequireUser_refusesAUserWithNoIDBehindIt(t *testing.T) {
	// Authenticate refuses to build one of these, so reaching the guard with it means
	// something upstream changed. The guard is the second lock on the same door: the
	// handler behind it would otherwise run a query scoped to nobody.
	var reached bool
	rec := serveWith(t, newRequest(t, "/v1/chats"), func(c *gin.Context) {
		reached = true
		utils.Success(c, http.StatusOK, "ok", nil)
	}, RequestID(), withCaller(Caller{Name: "ghost", Role: RoleUser}), RequireUser(discardLog()))

	if reached {
		t.Fatal("the handler ran for a user with no id")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body)
	}
}

func TestRequireUser_treatsAMissingCallerAsUnauthenticated(t *testing.T) {
	rec := serve(t, newRequest(t, "/v1/chats"), RequestID(), RequireUser(discardLog()))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body)
	}
}

func TestOwnerOf_answersOnlyForAPerson(t *testing.T) {
	cases := []struct {
		name   string
		caller *Caller
		want   string
		wantOK bool
	}{
		{"a signed-in person", &Caller{Name: "budi", Role: RoleUser, UserID: "usr_6"}, "usr_6", true},
		{"an operator", &Caller{Name: "ops", Role: RoleOperator}, "", false},
		{"a bot", &Caller{Name: "bridge", Role: RoleBot}, "", false},
		{"a user with no id", &Caller{Name: "ghost", Role: RoleUser}, "", false},
		{"a user with a blank id", &Caller{Name: "ghost", Role: RoleUser, UserID: "   "}, "", false},
		{"nobody at all", nil, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mw := []gin.HandlerFunc{RequestID()}
			if c.caller != nil {
				mw = append(mw, withCaller(*c.caller))
			}

			var (
				got   string
				gotOK bool
			)
			serveWith(t, newRequest(t, "/v1/chats"), func(ctx *gin.Context) {
				got, gotOK = OwnerOf(ctx)
				utils.Success(ctx, http.StatusOK, "ok", nil)
			}, mw...)

			if got != c.want || gotOK != c.wantOK {
				t.Fatalf("expected %q/%v, got %q/%v", c.want, c.wantOK, got, gotOK)
			}
		})
	}
}

func TestOwnerOf_reportsNobodyForANilContext(t *testing.T) {
	// A WHERE clause would happily accept the empty string this must not return
	// silently.
	if id, ok := OwnerOf(nil); ok || id != "" {
		t.Fatalf("expected no owner, got %q/%v", id, ok)
	}
}
