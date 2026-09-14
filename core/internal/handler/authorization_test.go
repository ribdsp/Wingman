package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// Who may reach what.
//
// Every assertion in this file is about a refusal, and each refusal happens in a specific
// layer: the route guards for the operator-only and person-only groups, the service for
// dispatch and for every read scoped by account. The fakes enforce none of it — see the
// note at the top of fakes_test.go — so a check deleted from either layer fails a test
// here rather than passing review.
//
// Published error codes are asserted as literals rather than through utils.ErrCodeForbidden
// and friends. The nine codes are a documented contract (docs/api.md); a test written
// against the constant would follow a change to its value instead of catching it.

// assertNotFound is 404 with the flat message.
//
// The message matters as much as the status: respondError deliberately renders "Not found."
// for every absence rather than the service's own sentence, because "that chat belongs to
// somebody else" confirms an id exists and an id that can be confirmed can be enumerated.
func assertNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	env := decode(t, rec, http.StatusNotFound)
	if env.Error == nil {
		t.Fatalf("no error object on a 404; body: %s", rec.Body.String())
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("error code = %q, want %q", env.Error.Code, "NOT_FOUND")
	}
	if env.Error.Message != "Not found." {
		t.Errorf("error message = %q, want the flat %q; a specific message confirms the row exists",
			env.Error.Message, "Not found.")
	}
}

func TestOperatorOnlyRoutes_refuseABotKeyAndAPersonsSession(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")
	inactive := false

	routes := []requestOf{
		{http.MethodPost, "/v1/accounts", newAccountRequest{
			Email: "made-by-a-bot@wingman.test", DisplayName: "Nope", Password: fixturePassword,
		}},
		{http.MethodGet, "/v1/accounts", nil},
		{http.MethodPut, "/v1/accounts/" + f.unattendedOwner + "/active", setActiveRequest{IsActive: &inactive}},
	}
	// The sweep and the list a client reads from /v1/reference have to describe the same
	// routes. A fourth added to the router and not to this sweep is what this catches.
	if len(routes) != len(operatorOnlyRoutes) {
		t.Fatalf("sweeping %d routes, operatorOnlyRoutes names %d", len(routes), len(operatorOnlyRoutes))
	}

	for _, route := range routes {
		t.Run(route.String(), func(t *testing.T) {
			// Act + Assert
			assertErrorCode(t, f.asBot(route.method, route.path, route.body), http.StatusForbidden, "FORBIDDEN")
			assertErrorCode(t, f.do(route.method, route.path, person, route.body), http.StatusForbidden, "FORBIDDEN")
		})
	}

	// Nothing was created along the way.
	if got, err := f.users.Count(context.Background()); err != nil || got != 2 {
		t.Errorf("accounts = %d (err %v), want the 2 seeded; a refused create still made one", got, err)
	}
}

func TestPersonOnlyRoutes_refuseBothMachineKeys(t *testing.T) {
	// Arrange
	f := newFixture(t)

	routes := []requestOf{
		{http.MethodGet, "/v1/me", nil},
		{http.MethodPut, "/v1/me/password", changePasswordRequest{
			CurrentPassword: fixturePassword, NewPassword: "another long passphrase",
		}},
		{http.MethodGet, "/v1/me/ledger", nil},
		{http.MethodGet, "/v1/me/sessions", nil},
		{http.MethodDelete, "/v1/me/sessions", nil},
		{http.MethodDelete, "/v1/me/sessions/session-1", nil},
		{http.MethodPost, "/v1/chats", titleRequest{Title: "Hello"}},
		{http.MethodGet, "/v1/chats", nil},
		{http.MethodGet, "/v1/chats/chat-1", nil},
		{http.MethodPut, "/v1/chats/chat-1/title", titleRequest{Title: "Hello"}},
		{http.MethodPost, "/v1/chats/chat-1/archive", nil},
		{http.MethodGet, "/v1/chats/chat-1/messages", nil},
		{http.MethodPost, "/v1/messages", sendRequest{Text: "do the thing"}},
		// Connecting a chat account is a person's operation and only a person's. A machine
		// key owns no Telegram account, so a code minted under one could only ever be used
		// to attach somebody's chat account to a credential nobody holds.
		{http.MethodPost, "/v1/channels/link-codes", nil},
		{http.MethodGet, "/v1/channels", nil},
		{http.MethodDelete, "/v1/channels/identity-1", nil},
	}

	for _, route := range routes {
		t.Run(route.String(), func(t *testing.T) {
			// Act + Assert
			//
			// An operator key is refused exactly as firmly as a bot key. These routes answer
			// "show me *my* conversations", and a credential that came out of an environment
			// variable has no answer to that — the operator's own account, if they have one,
			// signs in like anybody else.
			assertErrorCode(t, f.asOperator(route.method, route.path, route.body), http.StatusForbidden, "FORBIDDEN")
			assertErrorCode(t, f.asBot(route.method, route.path, route.body), http.StatusForbidden, "FORBIDDEN")
		})
	}
}

func TestEveryAuthenticatedRoute_refusesARequestCarryingNoCredential(t *testing.T) {
	// Arrange
	f := newFixture(t)

	routes := []requestOf{
		{http.MethodGet, "/v1/reference", nil},
		{http.MethodPost, "/v1/auth/signout", nil},
		{http.MethodGet, "/v1/me", nil},
		{http.MethodGet, "/v1/accounts", nil},
		{http.MethodGet, "/v1/chats", nil},
		{http.MethodPost, "/v1/messages", sendRequest{Text: "hello"}},
		{http.MethodPost, "/v1/channels/link-codes", nil},
		{http.MethodGet, "/v1/channels", nil},
		{http.MethodPost, "/v1/tasks", dispatchRequest{Brief: "work", IdempotencyKey: "k-1"}},
		{http.MethodGet, "/v1/tasks", nil},
		{http.MethodGet, "/v1/tasks/task-1", nil},
		{http.MethodGet, "/v1/tasks/task-1/runs", nil},
		{http.MethodGet, "/v1/runs/run-1", nil},
		{http.MethodGet, "/v1/runs/run-1/steps", nil},
		{http.MethodGet, "/v1/runs/run-1/cost", nil},
		{http.MethodPost, "/v1/runs/run-1/cancel", nil},
		{http.MethodPost, "/v1/notifications", notifyRequest{
			Kind: "trigger", SubjectID: "goal-1", Headline: "A goal fell behind.",
		}},
		// The path the goal engine calls. It is authenticated by the same middleware as
		// /v1, which is the point of asserting it here: a route added to the bare engine
		// rather than to a group is how an endpoint ends up open.
		{http.MethodPost, "/api/v1/tasks", dispatchRequest{Brief: "work", IdempotencyKey: "k-1"}},
	}

	for _, route := range routes {
		t.Run(route.String(), func(t *testing.T) {
			// Act + Assert
			assertErrorCode(t, f.unauthenticated(route.method, route.path, route.body),
				http.StatusUnauthorized, "UNAUTHORIZED")
		})
	}
}

func TestDispatch_refusesAPersonOnBothPaths_andFilesNothing(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, person := f.newAccount("person@wingman.test", "A person")

	for _, path := range []string{"/v1/tasks", "/api/v1/tasks"} {
		t.Run(path, func(t *testing.T) {
			// Act
			rec := f.do(http.MethodPost, path, person, dispatchRequest{
				Brief: "spend some money while nobody is watching", IdempotencyKey: "k-1",
			})

			// Assert
			assertErrorCode(t, rec, http.StatusForbidden, "FORBIDDEN")
		})
	}

	// Refused before anything was written. Unattended work is billed to the operator's
	// account, so a person who could dispatch would be spending somebody else's budget.
	if filed := f.tasks.count(); filed != 0 {
		t.Errorf("tasks filed = %d, want 0", filed)
	}
}

func TestDispatch_isAllowedForBothMachineRoles(t *testing.T) {
	// Arrange
	f := newFixture(t)

	callers := map[string]func(method, path string, body any) *httptest.ResponseRecorder{
		"operator": f.asOperator,
		"bot":      f.asBot,
	}
	for name, call := range callers {
		t.Run(name, func(t *testing.T) {
			// Act
			rec := call(http.MethodPost, "/v1/tasks", dispatchRequest{
				BotID: "sales", ChannelID: "ops", Brief: "check yesterday's numbers", IdempotencyKey: "key-" + name,
			})

			// Assert
			var task taskView
			dataInto(t, decode(t, rec, http.StatusCreated), &task)
			if task.Source != string(domain.TaskSourceGoalEngine) {
				t.Errorf("source = %q, want %q", task.Source, domain.TaskSourceGoalEngine)
			}
		})
	}
}

func TestAnotherAccountsRows_answer404_neverAConfirming403(t *testing.T) {
	// Arrange
	f := newFixture(t)
	one, tokenOne := f.newAccount("one@wingman.test", "Account one")
	_, tokenTwo := f.newAccount("two@wingman.test", "Account two")

	// Seeded through the routes a client would use, so the ids under test are real ones.
	var chat chatView
	dataInto(t, decode(t, f.do(http.MethodPost, "/v1/chats", tokenOne, titleRequest{Title: "One's chat"}),
		http.StatusCreated), &chat)

	task := f.seedTask(domain.Task{
		OwnerUserID: one.ID,
		Source:      domain.TaskSourceUser,
		Brief:       "one's work",
		Status:      domain.TaskStatusQueued,
	})
	run := f.seedRun("run-one", task.ID, one.ID, domain.StopCompleted)
	identity := f.channels.seed(one.ID, repository.ChannelTelegram, "tg-11111", "One on Telegram")

	routes := []requestOf{
		{http.MethodGet, "/v1/chats/" + chat.ID, nil},
		{http.MethodPut, "/v1/chats/" + chat.ID + "/title", titleRequest{Title: "Mine now"}},
		{http.MethodPost, "/v1/chats/" + chat.ID + "/archive", nil},
		{http.MethodGet, "/v1/chats/" + chat.ID + "/messages", nil},
		{http.MethodPost, "/v1/messages", sendRequest{ChatID: chat.ID, Text: "in your chat"}},
		{http.MethodGet, "/v1/tasks/" + task.ID, nil},
		{http.MethodGet, "/v1/tasks/" + task.ID + "/runs", nil},
		{http.MethodGet, "/v1/runs/" + run.Run.ID, nil},
		{http.MethodGet, "/v1/runs/" + run.Run.ID + "/steps", nil},
		{http.MethodGet, "/v1/runs/" + run.Run.ID + "/cost", nil},
		// A finished run answers 409 to its owner. To anybody else it is 404, because the
		// read is scoped before the state is considered — the ordering is the point.
		{http.MethodPost, "/v1/runs/" + run.Run.ID + "/cancel", nil},
		// Disconnecting somebody else's chat account. A 403 here would confirm the identity
		// exists, and an identity id is the handle on a real person's Telegram account.
		{http.MethodDelete, "/v1/channels/" + identity.ID, nil},
	}

	for _, route := range routes {
		t.Run(route.String(), func(t *testing.T) {
			// Act + Assert
			assertNotFound(t, f.do(route.method, route.path, tokenTwo, route.body))
		})
	}
}

func TestOperator_readsAnyAccountsRun(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, _ := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "their work", Status: domain.TaskStatusSucceeded,
	})
	f.seedRun("run-theirs", task.ID, person.ID, domain.StopCompleted)

	// Act
	var run runView
	dataInto(t, decode(t, f.asOperator(http.MethodGet, "/v1/runs/run-theirs", nil), http.StatusOK), &run)

	var steps []stepView
	dataInto(t, decode(t, f.asOperator(http.MethodGet, "/v1/runs/run-theirs/steps", nil), http.StatusOK), &steps)

	// Assert
	//
	// Whoever runs the instance pays for the tokens and answers for what the agent did, so
	// they can read any run and its transcript. What they cannot read is a chat — that is
	// asserted in TestPersonOnlyRoutes_refuseBothMachineKeys, and the difference between the
	// two is deliberate: the record of what an agent did is an operator's business, the
	// conversation somebody had with it is not.
	if run.ID != "run-theirs" {
		t.Errorf("run id = %q, want %q", run.ID, "run-theirs")
	}
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
}

func TestBot_seesTheUnattendedOwnersWorkAndNothingElse(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, _ := f.newAccount("person@wingman.test", "A person")

	theirs := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "a person's work", Status: domain.TaskStatusQueued,
	})
	unattended := f.seedTask(domain.Task{
		OwnerUserID:    f.unattendedOwner,
		Source:         domain.TaskSourceGoalEngine,
		Brief:          "revenue is behind pace",
		IdempotencyKey: "goal-1:2026-09-12",
		Status:         domain.TaskStatusQueued,
	})
	f.seedRun("run-theirs", theirs.ID, person.ID, domain.StopCompleted)
	f.seedRun("run-unattended", unattended.ID, f.unattendedOwner, domain.StopCompleted)

	// Act + Assert: the run it started is readable.
	var run runView
	dataInto(t, decode(t, f.asBot(http.MethodGet, "/v1/runs/run-unattended", nil), http.StatusOK), &run)
	if run.ID != "run-unattended" {
		t.Errorf("run id = %q, want %q", run.ID, "run-unattended")
	}

	// A person's is not. The bot key is the goal engine's, and the goal engine has no
	// business reading what somebody asked their agent in a chat.
	assertNotFound(t, f.asBot(http.MethodGet, "/v1/runs/run-theirs", nil))

	// And a listing is scoped the same way rather than filtered client-side.
	var tasks []taskView
	dataInto(t, decode(t, f.asBot(http.MethodGet, "/v1/tasks", nil), http.StatusOK), &tasks)
	if len(tasks) != 1 || tasks[0].ID != unattended.ID {
		t.Fatalf("bot listed %d tasks (%+v), want only %s", len(tasks), tasks, unattended.ID)
	}
}

func TestLedger_isTheCallersOwnAccountOnly(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	f.runs.putLedger(person.ID, domain.Ledger{Readable: true, TokensToday: 4200})

	// Act
	var ledger ledgerView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/me/ledger", token, nil), http.StatusOK), &ledger)

	// Assert
	if !ledger.IsReadable || ledger.TokensToday != 4200 {
		t.Errorf("ledger = %+v, want readable with 4200 tokens", ledger)
	}

	// An operator asking is refused rather than given a total. "The ledger" without an
	// account named is a question with no answer; instance-wide spend is the goal engine's,
	// through the samples core pushes it.
	assertErrorCode(t, f.asOperator(http.MethodGet, "/v1/me/ledger", nil), http.StatusForbidden, "FORBIDDEN")
}

func TestRevokeSession_someoneElsesIdIsNotFound(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, tokenOne := f.newAccount("one@wingman.test", "Account one")
	_, tokenTwo := f.newAccount("two@wingman.test", "Account two")

	var sessions []sessionView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/me/sessions", tokenOne, nil), http.StatusOK), &sessions)
	if len(sessions) != 1 {
		t.Fatalf("account one has %d sessions, want 1", len(sessions))
	}

	// Act
	rec := f.do(http.MethodDelete, "/v1/me/sessions/"+sessions[0].ID, tokenTwo, nil)

	// Assert: not found, and account one is still signed in.
	assertNotFound(t, rec)
	decode(t, f.do(http.MethodGet, "/v1/me", tokenOne, nil), http.StatusOK)
}

func TestRegistration_refusedWhenClosedAndAllowedWhenOpen(t *testing.T) {
	// Arrange
	closed := newFixture(t)
	open := newFixture(t, withOpenRegistration)
	body := newAccountRequest{Email: "newcomer@wingman.test", DisplayName: "Newcomer", Password: fixturePassword}

	// Act + Assert
	//
	// Closed is the default, and this is the assertion that matters most in the file: a
	// self-hosted box found on the internet with open sign-up is a box running strangers'
	// code in your sandbox on your API key.
	assertErrorCode(t, closed.unauthenticated(http.MethodPost, "/v1/auth/register", body),
		http.StatusForbidden, "FORBIDDEN")

	var user userView
	dataInto(t, decode(t, open.unauthenticated(http.MethodPost, "/v1/auth/register", body),
		http.StatusCreated), &user)
	if user.Email != body.Email || !user.IsActive {
		t.Errorf("registered user = %+v, want an active %s", user, body.Email)
	}
}

func TestDeactivatingAnAccount_endsItsSessionsImmediately(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	decode(t, f.do(http.MethodGet, "/v1/me", token, nil), http.StatusOK)
	inactive := false

	// Act
	decode(t, f.asOperator(http.MethodPut, "/v1/accounts/"+person.ID+"/active",
		setActiveRequest{IsActive: &inactive}), http.StatusOK)

	// Assert: the token that worked a moment ago no longer authenticates. Locking somebody
	// out has to take effect now, not when their session happens to expire.
	assertErrorCode(t, f.do(http.MethodGet, "/v1/me", token, nil), http.StatusUnauthorized, "UNAUTHORIZED")
}

func TestChangingAPassword_endsEverySessionIncludingTheCallers(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	elsewhere := f.signIn(person)

	// Act
	decode(t, f.do(http.MethodPut, "/v1/me/password", token, changePasswordRequest{
		CurrentPassword: fixturePassword, NewPassword: "a much better passphrase",
	}), http.StatusOK)

	// Assert: both devices are signed out. Somebody changing their password because they
	// think it was stolen is telling you to end the thief's session too, and there is no way
	// to tell which of the two that is.
	assertErrorCode(t, f.do(http.MethodGet, "/v1/me", token, nil), http.StatusUnauthorized, "UNAUTHORIZED")
	assertErrorCode(t, f.do(http.MethodGet, "/v1/me", elsewhere, nil), http.StatusUnauthorized, "UNAUTHORIZED")
}

func TestCancelRun_operatorCancelsSomebodyElsesRun_recordedAgainstItsOwner(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, _ := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "a runaway loop", Status: domain.TaskStatusRunning,
	})
	// An empty stop reason is a run still in flight.
	f.seedRun("run-live", task.ID, person.ID, "")

	// Act
	rec := f.asOperator(http.MethodPost, "/v1/runs/run-live/cancel", nil)

	// Assert
	decode(t, rec, http.StatusOK)

	// The cancellation was recorded against the run's owner, not the operator who asked.
	// This is observable because the fake's RequestCancel matches on the user id it is
	// given, exactly as the repository's UPDATE does: had the service passed the caller's
	// id, the update would have matched nothing and the row would still be uncancelled.
	record, err := f.runs.Get(context.Background(), "run-live")
	if err != nil {
		t.Fatalf("read the run back: %v", err)
	}
	if !record.Cancelled() {
		t.Error("run is not marked cancelled; the request was recorded against the wrong account")
	}
}

func TestCancelRun_twiceIs200_andAFinishedRunIs409(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusRunning,
	})
	f.seedRun("run-live", task.ID, person.ID, "")
	f.seedRun("run-done", task.ID, person.ID, domain.StopCompleted)

	// Act + Assert: asking twice is not an error. Somebody clicking again because nothing
	// has visibly happened yet is not making a mistake.
	decode(t, f.do(http.MethodPost, "/v1/runs/run-live/cancel", token, nil), http.StatusOK)
	decode(t, f.do(http.MethodPost, "/v1/runs/run-live/cancel", token, nil), http.StatusOK)

	// A run that already finished is the one case where the answer is "that is not a thing
	// you can do now".
	assertErrorCode(t, f.do(http.MethodPost, "/v1/runs/run-done/cancel", token, nil),
		http.StatusConflict, "ALREADY_RESOLVED")
}

func TestSignOut_withAMachineKeyIsStill200(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	rec := f.asBot(http.MethodPost, "/v1/auth/signout", nil)

	// Assert: a machine key is not a session, so nothing is ended — and the answer is still
	// 200, because a sign-out that failed would leave a client unable to clear its own
	// state. Nor is it a way to end somebody else's: the route ends the session presented,
	// and there is no id in it to name another.
	decode(t, rec, http.StatusOK)
}
