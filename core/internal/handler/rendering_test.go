package handler

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// What a response says, exactly.
//
// The assertions here are about wire shape rather than about permission: a null where a
// client expects a null, seconds where a Go duration would have rendered nanoseconds, a
// derived boolean present rather than left to be worked out from three timestamps, and —
// the ones that matter most — no credential of any kind in a body that is not sign-in.
//
// Several of them read the raw body rather than a decoded struct. That is deliberate: an
// omitted field and a null field both decode into a nil pointer, and the difference between
// them is exactly what a client branching on "no daily cap" sees.

// assertPagination checks the envelope's pagination block, which is nested under meta.
func assertPagination(t *testing.T, env envelope, wantPage, wantLimit, wantTotalItems, wantTotalPages int) {
	t.Helper()

	if env.Meta.Pagination == nil {
		t.Fatal("meta.pagination is absent on a listing that pages")
	}
	got := *env.Meta.Pagination
	if got.Page != wantPage || got.Limit != wantLimit ||
		got.TotalItems != wantTotalItems || got.TotalPages != wantTotalPages {
		t.Errorf("pagination = %+v, want page %d, limit %d, %d items, %d pages",
			got, wantPage, wantLimit, wantTotalItems, wantTotalPages)
	}
}

// assertNoPagination is for the three listings that deliberately carry no block.
//
// Their services do not count the table, and a pagination block with an invented total is
// worse than none: a client paging on it stops early or never stops.
func assertNoPagination(t *testing.T, env envelope) {
	t.Helper()

	if env.Meta.Pagination != nil {
		t.Errorf("meta.pagination = %+v, want none: this listing has no total to report",
			*env.Meta.Pagination)
	}
}

func TestRunView_rendersNoDailyCapAsNullAndACapAsANumber(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusSucceeded,
	})
	capped := f.seedRun("run-capped", task.ID, person.ID, domain.StopCompleted)

	// The same run with the sentinel instead of a number. A copy rather than a mutation,
	// because a stored run is a record of what actually bounded it.
	uncapped := capped
	uncapped.Run.ID = "run-uncapped"
	uncapped.Run.Limits.MaxTokensPerUserDay = domain.NoUserDailyCap
	f.runs.put(uncapped, nil, nil)

	// Act
	cappedRec := f.do(http.MethodGet, "/v1/runs/run-capped", token, nil)
	uncappedRec := f.do(http.MethodGet, "/v1/runs/run-uncapped", token, nil)

	// Assert
	var cappedView, uncappedView runView
	dataInto(t, decode(t, cappedRec, http.StatusOK), &cappedView)
	dataInto(t, decode(t, uncappedRec, http.StatusOK), &uncappedView)

	if cappedView.Limits.MaxTokensUserDay == nil || *cappedView.Limits.MaxTokensUserDay != 200000 {
		t.Errorf("maxTokensPerUserDay = %v, want 200000", cappedView.Limits.MaxTokensUserDay)
	}
	if uncappedView.Limits.MaxTokensUserDay != nil {
		t.Errorf("maxTokensPerUserDay = %v, want nil", *uncappedView.Limits.MaxTokensUserDay)
	}

	// Literally null, and present. Omitted would be indistinguishable from a field this
	// version of core does not send, and 0 would read as "nothing allowed" — the opposite of
	// what the sentinel means.
	if body := uncappedRec.Body.String(); !strings.Contains(body, `"maxTokensPerUserDay":null`) {
		t.Errorf("no daily cap did not render as null; body: %s", body)
	}
	if body := uncappedRec.Body.String(); strings.Contains(body, `"maxTokensPerUserDay":-1`) {
		t.Errorf("the NoUserDailyCap sentinel leaked into the response; body: %s", body)
	}
}

func TestRunView_rendersTimeoutsInSecondsNotNanoseconds(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusSucceeded,
	})
	f.seedRun("run-1", task.ID, person.ID, domain.StopCompleted)

	// Act
	rec := f.do(http.MethodGet, "/v1/runs/run-1", token, nil)

	// Assert
	var run runView
	dataInto(t, decode(t, rec, http.StatusOK), &run)
	if run.Limits.StepTimeoutSeconds != 90 {
		t.Errorf("stepTimeoutSeconds = %v, want 90", run.Limits.StepTimeoutSeconds)
	}
	if run.Limits.SandboxTimeoutSeconds != 30 {
		t.Errorf("sandboxTimeoutSeconds = %v, want 30", run.Limits.SandboxTimeoutSeconds)
	}
	// A Go duration marshals as nanoseconds, which no client would read correctly by
	// accident — and a sandbox timeout read as 90 billion seconds is a sandbox with none.
	if body := rec.Body.String(); strings.Contains(body, "90000000000") {
		t.Errorf("a duration rendered as nanoseconds; body: %s", body)
	}
}

func TestRunView_rendersInFlightAndCancelledExplicitly(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusRunning,
	})
	f.seedRun("run-live", task.ID, person.ID, "")

	// Act
	var live runView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/runs/run-live", token, nil), http.StatusOK), &live)

	decode(t, f.do(http.MethodPost, "/v1/runs/run-live/cancel", token, nil), http.StatusOK)

	var cancelling runView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/runs/run-live", token, nil), http.StatusOK), &cancelling)

	// Assert
	//
	// isInFlight rather than leaving a client to infer it from an absent stop reason, and
	// isCancelled separately from it: a run that has been asked to stop is still running
	// until it reaches its next step boundary, and showing it as finished would be a lie
	// about what is still spending tokens.
	if !live.IsInFlight || live.IsCancelled || live.FinishedAt != nil || live.Stop != "" {
		t.Errorf("in-flight run = %+v, want isInFlight with no stop and no finishedAt", live)
	}
	if !cancelling.IsInFlight || !cancelling.IsCancelled {
		t.Errorf("cancelling run = %+v, want both isInFlight and isCancelled", cancelling)
	}
}

func TestSessionView_rendersIsLiveForALiveAndAnEndedSession(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	f.signIn(person) // a second device

	var before []sessionView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/me/sessions", token, nil), http.StatusOK), &before)
	if len(before) != 2 {
		t.Fatalf("sessions = %d, want 2", len(before))
	}

	// Act: end the second device from the first.
	decode(t, f.do(http.MethodDelete, "/v1/me/sessions/"+before[1].ID, token, nil), http.StatusOK)

	var after []sessionView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/me/sessions", token, nil), http.StatusOK), &after)

	// Assert
	if len(after) != 2 {
		t.Fatalf("sessions = %d, want the ended one still listed", len(after))
	}
	if !after[0].IsLive || after[0].RevokedAt != nil {
		t.Errorf("current session = %+v, want live and not revoked", after[0])
	}
	if after[1].IsLive || after[1].RevokedAt == nil {
		t.Errorf("ended session = %+v, want isLive false with a revokedAt", after[1])
	}
	// Recognising an unfamiliar sign-in is the whole purpose of the list, so both of these
	// are rendered.
	if after[0].CreatedIP == "" || after[0].UserAgent == "" {
		t.Errorf("session = %+v, want the address and user agent it was created from", after[0])
	}
}

func TestChatView_rendersIsArchived_andArchivedChatsAreLeftOutUnlessAskedFor(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("person@wingman.test", "A person")

	var chat chatView
	dataInto(t, decode(t, f.do(http.MethodPost, "/v1/chats", token, titleRequest{Title: "Quarter review"}),
		http.StatusCreated), &chat)
	if chat.IsArchived || chat.ArchivedAt != nil {
		t.Errorf("new chat = %+v, want not archived", chat)
	}

	// Act
	decode(t, f.do(http.MethodPost, "/v1/chats/"+chat.ID+"/archive", token, nil), http.StatusOK)

	var archived chatView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/chats/"+chat.ID, token, nil), http.StatusOK), &archived)

	var visible, all []chatView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/chats", token, nil), http.StatusOK), &visible)
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/chats?includeArchived=true", token, nil), http.StatusOK), &all)

	// Assert
	if !archived.IsArchived || archived.ArchivedAt == nil {
		t.Errorf("archived chat = %+v, want isArchived with an archivedAt", archived)
	}
	// Archived rather than deleted, and still readable by id: a chat's messages are the
	// visible half of an append-only run transcript.
	if len(visible) != 0 {
		t.Errorf("default listing returned %d chats, want the archived one left out", len(visible))
	}
	if len(all) != 1 {
		t.Errorf("?includeArchived=true returned %d chats, want 1", len(all))
	}
}

func TestSendMessage_rendersTheChatMessageAndTaskItCreated(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("person@wingman.test", "A person")

	// Act
	rec := f.do(http.MethodPost, "/v1/messages", token, sendRequest{Text: "How did yesterday go?"})

	// Assert
	var sent sentView
	dataInto(t, decode(t, rec, http.StatusCreated), &sent)

	// One round trip for the first message of a conversation: the chat is created, named
	// after what was said, and the task that will answer comes back with it.
	if sent.Chat.ID == "" || sent.Chat.Title != "How did yesterday go?" {
		t.Errorf("chat = %+v, want one named after the message", sent.Chat)
	}
	if sent.Message.Role != string(repository.MessageRoleUser) || sent.Message.ChatID != sent.Chat.ID {
		t.Errorf("message = %+v, want a user message in the new chat", sent.Message)
	}
	if sent.Task.Source != string(domain.TaskSourceUser) || sent.Task.Status != string(domain.TaskStatusQueued) {
		t.Errorf("task = %+v, want a queued task sourced from a person", sent.Task)
	}
}

func TestLedgerView_rendersIsReadableFalseRatherThanZeroSpend(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	f.runs.putLedger(person.ID, domain.Ledger{Readable: false})

	// Act
	rec := f.do(http.MethodGet, "/v1/me/ledger", token, nil)

	// Assert
	var ledger ledgerView
	dataInto(t, decode(t, rec, http.StatusOK), &ledger)
	if ledger.IsReadable {
		t.Error("isReadable = true, want false")
	}
	// Present in the body, not omitted. A client that saw only tokensToday: 0 would tell
	// somebody their budget is untouched, when what happened is that the counter could not
	// be read at all.
	if body := rec.Body.String(); !strings.Contains(body, `"isReadable":false`) {
		t.Errorf("isReadable was not rendered; body: %s", body)
	}
}

func TestListings_pageWhereTheyCanCountAndOmitPaginationWhereTheyCannot(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	for _, title := range []string{"one", "two", "three", "four", "five"} {
		decode(t, f.do(http.MethodPost, "/v1/chats", token, titleRequest{Title: title}), http.StatusCreated)
	}
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusSucceeded,
	})
	f.seedRun("run-1", task.ID, person.ID, domain.StopCompleted)

	// Act + Assert: five chats, two at a time, second page.
	var page2 []chatView
	env := decode(t, f.do(http.MethodGet, "/v1/chats?limit=2&offset=2", token, nil), http.StatusOK)
	dataInto(t, env, &page2)
	if len(page2) != 2 || page2[0].Title != "three" {
		t.Errorf("page 2 = %+v, want two chats starting at \"three\"", page2)
	}
	assertPagination(t, env, 2, 2, 5, 3)

	// A page number on the way in produces the same window, because pagination metadata is
	// rendered as pages and a client should be able to send back what it was given.
	var byPage []chatView
	dataInto(t, decode(t, f.do(http.MethodGet, "/v1/chats?limit=2&page=2", token, nil), http.StatusOK), &byPage)
	if len(byPage) != 2 || byPage[0].Title != page2[0].Title {
		t.Errorf("?page=2 = %+v, want the same window as ?offset=2 (%+v)", byPage, page2)
	}

	// Accounts and tasks page too, and both are counted.
	assertPagination(t, decode(t, f.asOperator(http.MethodGet, "/v1/accounts?limit=1", nil), http.StatusOK),
		1, 1, 2, 2)
	assertPagination(t, decode(t, f.do(http.MethodGet, "/v1/tasks", token, nil), http.StatusOK),
		1, defaultRenderedLimit, 1, 1)

	// These three do not. Their services return a window without a count, and inventing one
	// here would be worse than offering none.
	assertNoPagination(t, decode(t, f.do(http.MethodGet, "/v1/me/sessions", token, nil), http.StatusOK))
	assertNoPagination(t, decode(t, f.do(http.MethodGet, "/v1/runs/run-1/steps", token, nil), http.StatusOK))
	assertNoPagination(t, decode(t, f.do(http.MethodGet, "/v1/tasks/"+task.ID+"/runs", token, nil), http.StatusOK))
}

func TestNoResponseCarriesACredential_exceptTheTokenSignInMints(t *testing.T) {
	// Arrange
	f := newFixture(t)
	person, token := f.newAccount("person@wingman.test", "A person")
	task := f.seedTask(domain.Task{
		OwnerUserID: person.ID, Source: domain.TaskSourceUser, Brief: "work", Status: domain.TaskStatusSucceeded,
	})
	f.seedRun("run-1", task.ID, person.ID, domain.StopCompleted)
	decode(t, f.do(http.MethodPost, "/v1/messages", token, sendRequest{Text: "hello"}), http.StatusCreated)

	// The stored form of the caller's own live token, derived the way the middleware derives
	// it. If this string ever appeared in a body, a leaked response would be a leaked
	// session even though the plaintext never left the client.
	storedToken, err := auth.ParseSessionToken(token)
	if err != nil {
		t.Fatalf("parse the fixture's session token: %v", err)
	}
	secrets := map[string]string{
		"stored password hash":      hashedFixturePassword(),
		"stored session token hash": storedToken,
		"operator API key":          operatorSecret,
		"bot API key":               botSecret,
	}

	type call struct {
		requestOf
		token string
	}
	calls := []call{
		{requestOf{http.MethodGet, "/v1/me", nil}, token},
		{requestOf{http.MethodGet, "/v1/me/sessions", nil}, token},
		{requestOf{http.MethodGet, "/v1/me/ledger", nil}, token},
		{requestOf{http.MethodGet, "/v1/chats", nil}, token},
		{requestOf{http.MethodGet, "/v1/tasks", nil}, token},
		{requestOf{http.MethodGet, "/v1/runs/run-1", nil}, token},
		{requestOf{http.MethodGet, "/v1/runs/run-1/steps", nil}, token},
		{requestOf{http.MethodGet, "/v1/runs/run-1/cost", nil}, token},
		{requestOf{http.MethodGet, "/v1/reference", nil}, token},
		{requestOf{http.MethodGet, "/v1/accounts", nil}, operatorSecret},
	}

	// Act + Assert
	for _, c := range calls {
		t.Run(c.String(), func(t *testing.T) {
			rec := f.do(c.method, c.path, c.token, c.body)
			decode(t, rec, http.StatusOK)
			assertBodyOmits(t, rec, secrets)
			// The plaintext token is a credential too. It is returned exactly once, by
			// sign-in, and echoing it anywhere else would put a live session in every log
			// and proxy cache that saw the response.
			assertBodyOmits(t, rec, map[string]string{"caller's plaintext session token": token})
		})
	}

	// The one exception, asserted so it stays the only one: sign-in returns a plaintext
	// token, and not the form the database holds.
	var signedIn signedInView
	signInRec := f.unauthenticated(http.MethodPost, "/v1/auth/signin",
		signInRequest{Email: "person@wingman.test", Password: fixturePassword})
	dataInto(t, decode(t, signInRec, http.StatusOK), &signedIn)
	if signedIn.Token == "" {
		t.Error("sign-in returned no token; a client has nothing to authenticate with")
	}
	mintedStored, err := auth.ParseSessionToken(signedIn.Token)
	if err != nil {
		t.Fatalf("the minted token is not shaped like one: %v", err)
	}
	assertBodyOmits(t, signInRec, map[string]string{
		"stored form of the token it just minted": mintedStored,
		"stored password hash":                    hashedFixturePassword(),
	})
}

func TestInternalFailure_rendersNothingAboutTheCause(t *testing.T) {
	// Arrange
	f := newFixture(t)
	_, token := f.newAccount("person@wingman.test", "A person")
	f.chats.err = errBoom

	// Act
	rec := f.do(http.MethodGet, "/v1/chats", token, nil)

	// Assert
	assertErrorCode(t, rec, http.StatusInternalServerError, "INTERNAL_ERROR")
	// An error from a repository can carry a query, a column name or a DSN. The detail is
	// logged against the request id the response does carry — which decode already asserted
	// is present, because a 500 that promises the id will find it in the logs and then omits
	// it is a promise the service cannot keep.
	assertBodyOmits(t, rec, map[string]string{"cause of the failure": "boom"})
}

func TestReference_listsTheStopReasonsInLadderOrder(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// The ladder, spelled out rather than compared against stopReasonNames() or
	// domain.AllStopReasons() — a test that called the source it is checking would agree
	// with any reordering, and the order is the safety model. This is domain.Decide's
	// sequence: a finished run is recognised before the guards on *continuing*, because
	// filing a run whose model already answered under "halted" would record its tool
	// calls' side effects under a word that means nothing happened. Then cancellation and
	// an unretryable provider failure, then the four that mean "I could not tell" or "the
	// allowance is gone", then the two caps. The last two are not ladder outcomes at all:
	// tool_denied is decided per call by ClassifyTool, and abandoned is recorded by the
	// sweep for a run whose worker died.
	wantStopReasons := []string{
		"completed",
		"cancelled",
		"provider_error",
		"halted",
		"budget_unreadable",
		"run_budget_exhausted",
		"user_budget_exhausted",
		"iteration_cap",
		"tool_call_cap",
		"tool_denied",
		"abandoned",
	}

	// Act
	var ref struct {
		TaskSources        []string `json:"taskSources"`
		TaskStatuses       []string `json:"taskStatuses"`
		StopReasons        []string `json:"stopReasons"`
		StepKinds          []string `json:"stepKinds"`
		MessageRoles       []string `json:"messageRoles"`
		OperatorOnlyRoutes []string `json:"operatorOnlyRoutes"`
	}
	dataInto(t, decode(t, f.asOperator(http.MethodGet, "/v1/reference", nil), http.StatusOK), &ref)

	// Assert
	if !slices.Equal(ref.StopReasons, wantStopReasons) {
		t.Errorf("stopReasons =\n%v\nwant\n%v", ref.StopReasons, wantStopReasons)
	}
	if !slices.Equal(ref.OperatorOnlyRoutes, operatorOnlyRoutes) {
		t.Errorf("operatorOnlyRoutes = %v, want %v", ref.OperatorOnlyRoutes, operatorOnlyRoutes)
	}
	// The rest exist so a client does not hardcode a list it discovered from traffic.
	for name, values := range map[string][]string{
		"taskSources":  ref.TaskSources,
		"taskStatuses": ref.TaskStatuses,
		"stepKinds":    ref.StepKinds,
		"messageRoles": ref.MessageRoles,
	} {
		if len(values) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

// assertRenderedOnce is a guard against a view struct gaining a field that renders a secret
// under a new name. It is cheap and it has caught nothing yet, which is the point.
func assertRenderedOnce(t *testing.T, rec *httptest.ResponseRecorder, needle string, want int) {
	t.Helper()

	if got := strings.Count(rec.Body.String(), needle); got != want {
		t.Errorf("%q appears %d times in the body, want %d", needle, got, want)
	}
}

func TestSignIn_rendersTheTokenOnceAndNoPasswordField(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.newAccount("person@wingman.test", "A person")

	// Act
	rec := f.unauthenticated(http.MethodPost, "/v1/auth/signin",
		signInRequest{Email: "person@wingman.test", Password: fixturePassword})

	// Assert
	var signedIn signedInView
	dataInto(t, decode(t, rec, http.StatusOK), &signedIn)
	assertRenderedOnce(t, rec, signedIn.Token, 1)
	// Not the hash, not a placeholder, not an empty string under a password key. A view
	// struct is where "the hash never leaves the repository layer" would otherwise quietly
	// stop being true.
	for _, forbidden := range []string{`"password"`, `"passwordHash"`, `"currentPassword"`} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("sign-in rendered a %s field", forbidden)
		}
	}
	if signedIn.User.Email != "person@wingman.test" || !signedIn.ExpiresAt.After(testNow) {
		t.Errorf("signed in = %+v, want the account and an expiry in the future", signedIn)
	}
}
