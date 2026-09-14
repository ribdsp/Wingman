package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/auth"
	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/service"
)

func init() {
	// Without this gin writes a startup banner and a debug line per route to stderr,
	// which buries the one line a failing test prints.
	gin.SetMode(gin.TestMode)
}

// testNow is the instant the fixture's clock returns and every fake stamps its rows
// with. Fixed, because a rendering that reports whether a session is still live has to
// be assertable, and "now" is not.
var testNow = time.Date(2026, 9, 12, 9, 30, 0, 0, time.UTC)

const (
	operatorName = "ops"
	botName      = "goal-engine"
	// The two machine secrets. Long enough to be realistic rather than to satisfy
	// anything here: the fixture builds credentials directly, so config's minimum
	// length is not enforced on this path.
	operatorSecret = "operator-key-4f3a9c2e7b81d6054e9a3c7f2b18d640"
	botSecret      = "bot-key-91c4e7a2f83b06d5497ec13ab8f20d76"

	// fixturePassword is what every seeded account's password is.
	fixturePassword = "correct horse battery staple"

	// unattendedOwnerEmail is CORE_UNATTENDED_OWNER's account — the one unattended work
	// is filed against and billed to.
	unattendedOwnerEmail = "unattended@wingman.test"

	fixtureSessionTTL = 24 * time.Hour
	// fixtureLinkCodeTTL is inside the service's own 1m–1h clamp, so the expiry a test
	// asserts is the one the fixture asked for rather than a clamped one.
	fixtureLinkCodeTTL = 15 * time.Minute
)

// hashedFixturePassword is argon2id over fixturePassword, computed once.
//
// Hashing costs 64 MiB and a few hundred milliseconds by design (see
// internal/auth/password.go), so seeding a second account must not pay for it again.
// Tests that are about hashing live in internal/auth, not here.
var hashedFixturePassword = sync.OnceValue(func() string {
	hash, err := auth.HashPassword(fixturePassword)
	if err != nil {
		panic("fixture: hashing the test password failed: " + err.Error())
	}
	return hash
})

// fixture is a whole HTTP stack over in-memory storage.
//
// Real middleware, real services, fake repositories. That combination is the point: a
// request in a test here passes through the same authentication, the same authorisation
// and the same rendering it would in production, so a rule enforced in the wrong layer —
// or enforced nowhere — shows up as a failing assertion rather than as a review comment.
type fixture struct {
	t      *testing.T
	router *gin.Engine

	users    *memUsers
	sessions *memSessions
	chats    *memChats
	tasks    *memTasks
	runs     *memRuns
	channels *memChannels
	// sender is the last step of a notification, where a message would leave for a chat
	// platform. Kept to hand so a test can read what was delivered — and, more to the
	// point, assert that nothing was.
	sender *memDirectSender

	// unattendedOwner is the account id the goal engine's dispatches are filed against,
	// resolved before the services are built exactly as cmd resolves it at boot.
	unattendedOwner string
}

// fixtureConfig is the handful of knobs a test needs to vary. Everything else is fixed,
// because a fixture with a setting per test is a fixture nobody can read.
type fixtureConfig struct {
	openRegistration bool
	ready            func(c *gin.Context) error
	// noChannels is an instance with no platform connected, which is the default way core
	// runs: reached over HTTP by a web client and by the goal engine, and listening on no
	// chat platform at all.
	noChannels bool
}

// withOpenRegistration is CORE_OPEN_REGISTRATION=true. Closed is the default here
// because closed is the default in config, and the tests that matter most are the ones
// asserting the closed instance refuses.
func withOpenRegistration(cfg *fixtureConfig) { cfg.openRegistration = true }

// withBrokenReadiness makes the injected readiness check fail with an error carrying a
// DSN, so a test can assert the DSN does not come back out.
func withBrokenReadiness(cfg *fixtureConfig) { cfg.ready = readyBroken }

// withNoChannelsConnected is an instance listening on no chat platform.
func withNoChannelsConnected(cfg *fixtureConfig) { cfg.noChannels = true }

// readyOK and readyBroken are the two readiness checks cmd could hand the handler.
func readyOK(_ *gin.Context) error { return nil }

// brokenReadinessDSN is the part of readyBroken's error that must never be rendered. A
// failing pool's error genuinely looks like this, password and all.
const brokenReadinessDSN = "postgres://core:hunter2@core-db:5432/core"

func readyBroken(_ *gin.Context) error {
	return errors.New("ping: dial tcp 10.0.0.5:5432: connection refused, dsn=" + brokenReadinessDSN)
}

func newFixture(t *testing.T, tweaks ...func(*fixtureConfig)) *fixture {
	t.Helper()

	cfg := fixtureConfig{ready: readyOK}
	for _, tweak := range tweaks {
		tweak(&cfg)
	}

	f := &fixture{
		t:        t,
		users:    newMemUsers(),
		sessions: newMemSessions(),
		chats:    newMemChats(),
		tasks:    newMemTasks(),
		runs:     newMemRuns(),
		channels: newMemChannels(),
		sender:   &memDirectSender{},
	}

	// The unattended owner exists before anything is wired, which is the order cmd uses:
	// a boot that cannot resolve the account refuses to start rather than filing work
	// against an empty id.
	owner, err := f.users.Create(context.Background(), unattendedOwnerEmail, "Unattended work", auth.UnmatchableHash())
	if err != nil {
		t.Fatalf("seed the unattended owner: %v", err)
	}
	f.unattendedOwner = owner.ID

	logger := zerolog.Nop()
	clock := func() time.Time { return testNow }

	accounts, err := service.NewAccounts(service.AccountsDeps{
		Users:            f.users,
		Passwords:        f.users,
		Admin:            f.users,
		Sessions:         f.sessions,
		Revoker:          f.sessions,
		OpenRegistration: cfg.openRegistration,
		SessionTTL:       fixtureSessionTTL,
		Clock:            clock,
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("build accounts: %v", err)
	}

	sessions, err := service.NewSessions(service.SessionsDeps{
		Sessions: f.sessions,
		Revoker:  f.sessions,
		Janitor:  f.sessions,
		Clock:    clock,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("build sessions: %v", err)
	}

	chats, err := service.NewChats(service.ChatsDeps{
		Chats:    f.chats,
		Messages: f.chats,
		Clock:    clock,
		Logger:   logger,
	})
	if err != nil {
		t.Fatalf("build chats: %v", err)
	}

	tasks, err := service.NewTasks(service.TasksDeps{
		Tasks:           f.tasks,
		Reader:          f.tasks,
		Chats:           f.chats,
		Messages:        f.chats,
		ChannelChats:    f.chats,
		UnattendedOwner: f.unattendedOwner,
		Clock:           clock,
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("build tasks: %v", err)
	}

	runs, err := service.NewRuns(service.RunsDeps{
		Runs:            f.runs,
		Auditor:         f.runs,
		Tasks:           f.tasks,
		Canceller:       f.runs,
		Spend:           f.runs,
		UnattendedOwner: f.unattendedOwner,
		Clock:           clock,
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("build runs: %v", err)
	}

	channels, err := service.NewChannels(service.ChannelsDeps{
		Codes:   f.channels,
		Links:   f.channels,
		Janitor: f.channels,
		CodeTTL: fixtureLinkCodeTTL,
		Clock:   clock,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("build channels: %v", err)
	}

	// Over f.channels as the directory and f.sender as the last step, with the same owner
	// the tasks service files unattended work against — which is the whole recipient rule:
	// there is no "who" to configure and no recipient in the request.
	notifications, err := service.NewNotifications(service.NotificationsDeps{
		Directory: f.channels,
		Sender:    f.sender,
		OwnerID:   f.unattendedOwner,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("build notifications: %v", err)
	}

	// One platform connected by default, not all three and not none, so a test can tell the
	// "this build can talk to" list apart from the "this instance is listening on" list —
	// which are different answers and the reason both are published.
	connected := []string{string(domain.ChannelTelegram)}
	if cfg.noChannels {
		connected = nil
	}

	h, err := New(Deps{
		Accounts:          accounts,
		Sessions:          sessions,
		Chats:             chats,
		Tasks:             tasks,
		Runs:              runs,
		Channels:          channels,
		Notifications:     notifications,
		ChannelsConnected: connected,
		Ready:             cfg.ready,
		Clock:             clock,
		Logger:            logger,
	})
	if err != nil {
		t.Fatalf("build handlers: %v", err)
	}

	router, err := NewRouter(RouterDeps{
		Handler: h,
		// Through CredentialsFrom rather than by hand, so the role mapping every real
		// boot goes through is on the path a test exercises.
		Credentials: CredentialsFrom([]config.APIKey{
			{Name: operatorName, Secret: operatorSecret, Role: config.RoleOperator},
			{Name: botName, Secret: botSecret, Role: config.RoleBot},
		}),
		Sessions: f.sessions,
		// High enough that no test trips the limiter by accident. The limiter's own
		// behaviour is tested in internal/middleware, where a low limit is the point.
		RateLimit: config.RateLimitConfig{RPS: 1000, Burst: 1000},
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	f.router = router

	return f
}

// do sends a request through the whole router and returns what came back.
//
// A string body is sent verbatim, so a test can post something that is not JSON.
// Anything else is marshalled. A nil body sends none at all.
func (f *fixture) do(method, path, token string, body any) *httptest.ResponseRecorder {
	return f.doWith(method, path, token, body, nil)
}

// doWith is do plus extra request headers, for the routes where a header is part of the
// contract — the goal engine's Idempotency-Key.
func (f *fixture) doWith(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader *bytes.Reader
	switch typed := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(typed))
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			f.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// asOperator, asBot and unauthenticated name the three callers a test does not have to
// seed. A user needs an account, so it gets a token from newAccount instead.
func (f *fixture) asOperator(method, path string, body any) *httptest.ResponseRecorder {
	return f.do(method, path, operatorSecret, body)
}

func (f *fixture) asBot(method, path string, body any) *httptest.ResponseRecorder {
	return f.do(method, path, botSecret, body)
}

func (f *fixture) unauthenticated(method, path string, body any) *httptest.ResponseRecorder {
	return f.do(method, path, "", body)
}

// newAccount seeds an account whose password is fixturePassword and returns it with a
// live session token, because almost every test that needs one needs both.
//
// The session is minted the way sign-in mints it — a plaintext token the caller keeps and
// a SHA-256 the store keeps — so a test asserting that a hash never appears in a response
// is asserting against the same two values production has.
func (f *fixture) newAccount(email, displayName string) (repository.User, string) {
	f.t.Helper()

	user, err := f.users.Create(context.Background(), email, displayName, hashedFixturePassword())
	if err != nil {
		f.t.Fatalf("seed account %s: %v", email, err)
	}
	return user, f.signIn(user)
}

// signIn mints a live session for an existing account and returns the plaintext token.
func (f *fixture) signIn(user repository.User) string {
	f.t.Helper()

	plaintext, stored, err := auth.NewSessionToken()
	if err != nil {
		f.t.Fatalf("mint a session token: %v", err)
	}
	_, err = f.sessions.Create(context.Background(), repository.NewSession{
		UserID:    user.ID,
		TokenHash: stored,
		UserAgent: "fixture/1.0",
		CreatedIP: "192.0.2.10",
		ExpiresAt: testNow.Add(fixtureSessionTTL),
	})
	if err != nil {
		f.t.Fatalf("seed a session: %v", err)
	}
	f.sessions.setName(user.ID, user.DisplayName)
	return plaintext
}

// seedTask files a task directly, for the read paths. Dispatch is exercised over HTTP
// where it is the thing under test.
func (f *fixture) seedTask(task domain.Task) domain.Task {
	f.t.Helper()

	stored, err := f.tasks.Create(context.Background(), repository.NewTask{Task: task})
	if err != nil {
		f.t.Fatalf("seed a task: %v", err)
	}
	return stored
}

// seedRun records a finished run with a two-step transcript and one charge.
//
// The limits are the ones a real run snapshots, and MaxTokensPerUserDay is set rather
// than left at NoUserDailyCap so the rendering of both cases can be asserted.
func (f *fixture) seedRun(id, taskID, ownerUserID string, stop domain.StopReason) repository.RunRecord {
	f.t.Helper()

	finished := testNow.Add(90 * time.Second)
	run := domain.Run{
		ID:          id,
		TaskID:      taskID,
		OwnerUserID: ownerUserID,
		Provider:    "anthropic",
		Model:       "claude-sonnet-5",
		Limits: domain.RunLimits{
			MaxIterations:       8,
			MaxToolCalls:        16,
			MaxTokensPerRun:     40000,
			MaxTokensPerUserDay: 200000,
			StepTimeout:         90 * time.Second,
			SandboxTimeout:      30 * time.Second,
		},
		State:     domain.RunState{Iterations: 2, ToolCalls: 1, TokensUsed: 1400},
		Stop:      stop,
		StartedAt: testNow,
	}
	if stop != "" {
		run.FinishedAt = &finished
	}

	record := repository.RunRecord{Run: run}
	steps := []domain.Step{
		{RunID: id, Index: 1, Kind: domain.StepKindModel, Content: "Checking yesterday's numbers.", TokensIn: 900, TokensOut: 300, At: testNow},
		{RunID: id, Index: 2, Kind: domain.StepKindTool, ToolName: "shell", Content: "exit status 0", At: testNow.Add(time.Second)},
	}
	charges := []repository.Spend{
		{UserID: ownerUserID, RunID: id, Provider: "anthropic", Model: "claude-sonnet-5", TokensIn: 900, TokensOut: 300, OccurredAt: testNow},
		{UserID: ownerUserID, RunID: id, Provider: "anthropic", Model: "claude-sonnet-5", TokensIn: 150, TokensOut: 50, OccurredAt: testNow.Add(time.Second)},
	}
	f.runs.put(record, steps, charges)
	return record
}

// envelope is the shared response shape from internal/utils, decoded.
//
// Data stays raw so a test can unmarshal it into the view it expects — decoding it as
// map[string]any would turn every number into a float64 and make an int64 token count
// awkward to assert.
type envelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
	Meta struct {
		RequestID  string `json:"requestId"`
		Timestamp  string `json:"timestamp"`
		Pagination *struct {
			Page       int `json:"page"`
			Limit      int `json:"limit"`
			TotalItems int `json:"totalItems"`
			TotalPages int `json:"totalPages"`
		} `json:"pagination"`
	} `json:"meta"`
}

// decode asserts the status code and returns the envelope.
//
// It also asserts a request id on every response, success or failure. That is not
// incidental: a 500's message tells the caller the id is how the failure will be found in
// the logs, and a 500 without one is a promise the service cannot keep.
func decode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) envelope {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, wantStatus, rec.Body.String())
	}

	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body: %s", err, rec.Body.String())
	}
	if env.Code != wantStatus {
		t.Errorf("envelope code = %d, want %d", env.Code, wantStatus)
	}
	if env.Meta.RequestID == "" {
		t.Error("meta.requestId is empty; every response has to be traceable")
	}
	if env.Meta.Timestamp == "" {
		t.Error("meta.timestamp is empty")
	}
	return env
}

// dataInto decodes the envelope's data into target.
func dataInto(t *testing.T, env envelope, target any) {
	t.Helper()

	if len(env.Data) == 0 {
		t.Fatalf("response carried no data; message: %q", env.Message)
	}
	if err := json.Unmarshal(env.Data, target); err != nil {
		t.Fatalf("decode data: %v; data: %s", err, env.Data)
	}
}

// errorCode asserts a failure and returns its published code.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) string {
	t.Helper()

	env := decode(t, rec, wantStatus)
	if env.Success {
		t.Errorf("success = true on a %d response", wantStatus)
	}
	if env.Error == nil {
		t.Fatalf("no error object on a %d response; body: %s", wantStatus, rec.Body.String())
	}
	return env.Error.Code
}

// assertErrorCode asserts both the status and the published error code, which is the pair
// a client actually branches on.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if code := errorCode(t, rec, wantStatus); code != wantCode {
		t.Errorf("error code = %q, want %q", code, wantCode)
	}
}

// assertBodyOmits fails when a response contains something that must never be rendered —
// a password hash, a session token's stored form, a machine key, a DSN.
func assertBodyOmits(t *testing.T, rec *httptest.ResponseRecorder, secrets map[string]string) {
	t.Helper()

	body := rec.Body.String()
	for what, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(body, secret) {
			t.Errorf("response body contains the %s", what)
		}
	}
}

// requestOf is the routes a test sweeps over when the assertion is about who may reach
// one rather than about what it returns.
type requestOf struct {
	method string
	path   string
	body   any
}

func (r requestOf) String() string { return r.method + " " + r.path }

// ensure the fixture's fakes satisfy every port the services take. A compile-time check,
// so a port gaining a method fails here rather than in one arbitrary test.
var (
	_ service.UserStore      = (*memUsers)(nil)
	_ service.PasswordStore  = (*memUsers)(nil)
	_ service.UserAdmin      = (*memUsers)(nil)
	_ service.SessionStore   = (*memSessions)(nil)
	_ service.SessionRevoker = (*memSessions)(nil)
	_ service.SessionJanitor = (*memSessions)(nil)
	_ service.ChatStore      = (*memChats)(nil)
	_ service.MessageStore   = (*memChats)(nil)
	_ service.ChannelChats   = (*memChats)(nil)
	_ service.TaskStore      = (*memTasks)(nil)
	_ service.TaskReader     = (*memTasks)(nil)
	_ service.RunReader      = (*memRuns)(nil)
	_ service.RunAuditor     = (*memRuns)(nil)
	_ service.RunCanceller   = (*memRuns)(nil)
	_ service.SpendReader    = (*memRuns)(nil)
)
