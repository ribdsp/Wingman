package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The fakes below stand in for the repositories. They are in-memory and single-threaded,
// and they copy the two behaviours of the real ones that the services are built around:
// a query scoped by user id answers ErrNotFound for somebody else's row, and a duplicate
// idempotency key answers ErrConflict.
//
// Each has explicit fail* fields rather than a general hook, because the failures worth
// testing are specific — a ledger that cannot be read, a revocation that fails after a
// password was already changed — and a test that names the field says which one it means.

// Asserted at compile time rather than left to the first test that wires one up. A port
// that gains a method should break here, in one place that names the port, rather than in
// whichever test happened to use that fake.
var (
	_ UserStore      = (*fakeUsers)(nil)
	_ PasswordStore  = (*fakeUsers)(nil)
	_ UserAdmin      = (*fakeUsers)(nil)
	_ SessionStore   = (*fakeSessions)(nil)
	_ SessionRevoker = (*fakeSessions)(nil)
	_ SessionJanitor = (*fakeSessions)(nil)
	_ ChatStore      = (*fakeChats)(nil)
	_ MessageStore   = (*fakeChats)(nil)
	_ TaskStore      = (*fakeTasks)(nil)
	_ TaskReader     = (*fakeTasks)(nil)
	_ TaskQueue      = (*fakeTasks)(nil)
	_ TaskJanitor    = (*fakeTasks)(nil)
	_ RunStore       = (*fakeRuns)(nil)
	_ RunReader      = (*fakeRuns)(nil)
	_ RunAuditor     = (*fakeRuns)(nil)
	_ RunCanceller   = (*fakeRuns)(nil)
	_ RunJanitor     = (*fakeRuns)(nil)
	_ SpendReader    = (*fakeSpend)(nil)
	_ AgentRunner    = (*fakeAgent)(nil)
	_ ToolSource     = (*fakeTools)(nil)
	_ Workspaces     = (*fakeWorkspaces)(nil)
	_ SpendReporter  = (*fakeReporter)(nil)
)

// errBoom is the injected failure. Its text is deliberately unlike anything the package
// produces, so a test asserting on a wrapped error cannot pass by accident.
var errBoom = errors.New("boom: injected failure")

// silentLogger keeps test output readable. The services log at info on success paths, and
// none of it is what is being asserted.
func silentLogger() zerolog.Logger { return zerolog.New(io.Discard) }

// fixedClock is a Clock that does not move, so a test can assert on an exact timestamp.
func fixedClock(at time.Time) Clock { return func() time.Time { return at } }

var testNow = time.Date(2026, 3, 14, 9, 30, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// accounts

type storedAccount struct {
	user repository.User
	hash string
}

// fakeUsers is UserStore, PasswordStore and UserAdmin over a map. The three ports are
// separate for the reasons in ports.go; one fake satisfies all three because a test
// wiring three views of the same table is a test about the table.
type fakeUsers struct {
	accounts map[string]*storedAccount
	order    []string
	nextID   int

	failCreate     error
	failGetByID    error
	failGetByEmail error
	failCreds      error
	failUpdate     error
	failList       error
	failSetActive  error
	failCount      error

	updatedHashes int
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{accounts: map[string]*storedAccount{}}
}

func (f *fakeUsers) Create(_ context.Context, email, displayName, passwordHash string) (repository.User, error) {
	if f.failCreate != nil {
		return repository.User{}, f.failCreate
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, stored := range f.accounts {
		if stored.user.Email == email {
			return repository.User{}, repository.ErrConflict
		}
	}

	f.nextID++
	id := fmt.Sprintf("user-%d", f.nextID)
	user := repository.User{
		ID:          id,
		Email:       email,
		DisplayName: displayName,
		IsActive:    true,
		CreatedAt:   testNow,
	}
	f.accounts[id] = &storedAccount{user: user, hash: passwordHash}
	f.order = append(f.order, id)
	return user, nil
}

func (f *fakeUsers) GetByID(_ context.Context, id string) (repository.User, error) {
	if f.failGetByID != nil {
		return repository.User{}, f.failGetByID
	}
	stored, ok := f.accounts[id]
	if !ok {
		return repository.User{}, repository.ErrNotFound
	}
	return stored.user, nil
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (repository.User, error) {
	if f.failGetByEmail != nil {
		return repository.User{}, f.failGetByEmail
	}
	if stored := f.byEmail(email); stored != nil {
		return stored.user, nil
	}
	return repository.User{}, repository.ErrNotFound
}

func (f *fakeUsers) Count(_ context.Context) (int, error) {
	if f.failCount != nil {
		return 0, f.failCount
	}
	return len(f.accounts), nil
}

func (f *fakeUsers) GetCredentialsByEmail(_ context.Context, email string) (repository.Credentials, error) {
	if f.failCreds != nil {
		return repository.Credentials{}, f.failCreds
	}
	stored := f.byEmail(email)
	if stored == nil {
		return repository.Credentials{}, repository.ErrNotFound
	}
	return repository.Credentials{
		UserID:       stored.user.ID,
		PasswordHash: stored.hash,
		IsActive:     stored.user.IsActive,
	}, nil
}

func (f *fakeUsers) UpdatePassword(_ context.Context, userID, passwordHash string) error {
	if f.failUpdate != nil {
		return f.failUpdate
	}
	stored, ok := f.accounts[userID]
	if !ok {
		return repository.ErrNotFound
	}
	stored.hash = passwordHash
	f.updatedHashes++
	return nil
}

func (f *fakeUsers) List(_ context.Context, limit, offset int) ([]repository.User, int, error) {
	if f.failList != nil {
		return nil, 0, f.failList
	}
	users := make([]repository.User, 0, len(f.order))
	for _, id := range f.order {
		users = append(users, f.accounts[id].user)
	}
	return page(users, limit, offset), len(users), nil
}

func (f *fakeUsers) SetActive(_ context.Context, userID string, active bool) error {
	if f.failSetActive != nil {
		return f.failSetActive
	}
	stored, ok := f.accounts[userID]
	if !ok {
		return repository.ErrNotFound
	}
	stored.user.IsActive = active
	return nil
}

func (f *fakeUsers) byEmail(email string) *storedAccount {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, id := range f.order {
		if f.accounts[id].user.Email == email {
			return f.accounts[id]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// sessions

// fakeSessions is SessionStore, SessionRevoker and SessionJanitor.
type fakeSessions struct {
	rows   []repository.Session
	hashes map[string]string // session id → stored token hash
	nextID int

	failCreate     error
	failList       error
	failByToken    error
	failByID       error
	failAll        error
	failDelete     error
	revokeAllCalls int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{hashes: map[string]string{}}
}

func (f *fakeSessions) Create(_ context.Context, input repository.NewSession) (repository.Session, error) {
	if f.failCreate != nil {
		return repository.Session{}, f.failCreate
	}
	f.nextID++
	session := repository.Session{
		ID:        fmt.Sprintf("session-%d", f.nextID),
		UserID:    input.UserID,
		UserAgent: input.UserAgent,
		CreatedIP: input.CreatedIP,
		CreatedAt: testNow,
		ExpiresAt: input.ExpiresAt,
	}
	f.rows = append(f.rows, session)
	f.hashes[session.ID] = input.TokenHash
	return session, nil
}

func (f *fakeSessions) ListForUser(_ context.Context, userID string, limit, offset int) ([]repository.Session, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	out := []repository.Session{}
	for _, session := range f.rows {
		if session.UserID == userID {
			out = append(out, session)
		}
	}
	return page(out, limit, offset), nil
}

func (f *fakeSessions) RevokeByToken(_ context.Context, tokenHash string, at time.Time) error {
	if f.failByToken != nil {
		return f.failByToken
	}
	for i := range f.rows {
		if f.hashes[f.rows[i].ID] != tokenHash || f.rows[i].RevokedAt != nil {
			continue
		}
		f.rows[i].RevokedAt = &at
		return nil
	}
	return repository.ErrNotFound
}

func (f *fakeSessions) RevokeByID(_ context.Context, userID, sessionID string, at time.Time) error {
	if f.failByID != nil {
		return f.failByID
	}
	for i := range f.rows {
		// The user id is part of the match, exactly as it is in the real query: somebody
		// else's session id is simply not found.
		if f.rows[i].ID != sessionID || f.rows[i].UserID != userID || f.rows[i].RevokedAt != nil {
			continue
		}
		f.rows[i].RevokedAt = &at
		return nil
	}
	return repository.ErrNotFound
}

func (f *fakeSessions) RevokeAllForUser(_ context.Context, userID string, at time.Time) (int, error) {
	f.revokeAllCalls++
	if f.failAll != nil {
		return 0, f.failAll
	}
	revoked := 0
	for i := range f.rows {
		if f.rows[i].UserID != userID || f.rows[i].RevokedAt != nil {
			continue
		}
		f.rows[i].RevokedAt = &at
		revoked++
	}
	return revoked, nil
}

func (f *fakeSessions) DeleteExpired(_ context.Context, before time.Time) (int, error) {
	if f.failDelete != nil {
		return 0, f.failDelete
	}
	kept := make([]repository.Session, 0, len(f.rows))
	deleted := 0
	for _, session := range f.rows {
		if session.ExpiresAt.Before(before) {
			deleted++
			continue
		}
		kept = append(kept, session)
	}
	f.rows = kept
	return deleted, nil
}

func (f *fakeSessions) live(userID string) int {
	live := 0
	for _, session := range f.rows {
		if session.UserID == userID && session.RevokedAt == nil {
			live++
		}
	}
	return live
}

// ---------------------------------------------------------------------------
// chats and messages

// fakeChats is ChatStore and MessageStore. EnsureChannelChat, its third port, is in
// fakes_channel_test.go with the rest of the inbound path.
type fakeChats struct {
	chats    map[string]*repository.Chat
	order    []string
	messages map[string][]repository.Message
	nextChat int
	nextMsg  int
	// channelChats maps account+platform+conversation to a chat id, which is what the
	// real table's unique index does.
	channelChats map[string]string

	failCreate   error
	failGet      error
	failList     error
	failRename   error
	failArchive  error
	failAppend   error
	failMessages error
	failRecent   error
	failEnsure   error
}

func newFakeChats() *fakeChats {
	return &fakeChats{
		chats:        map[string]*repository.Chat{},
		messages:     map[string][]repository.Message{},
		channelChats: map[string]string{},
	}
}

func (f *fakeChats) CreateChat(_ context.Context, userID, title string) (repository.Chat, error) {
	if f.failCreate != nil {
		return repository.Chat{}, f.failCreate
	}
	f.nextChat++
	chat := repository.Chat{
		ID:        fmt.Sprintf("chat-%d", f.nextChat),
		UserID:    userID,
		Title:     title,
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}
	f.chats[chat.ID] = &chat
	f.order = append(f.order, chat.ID)
	return chat, nil
}

func (f *fakeChats) GetChat(_ context.Context, userID, chatID string) (repository.Chat, error) {
	if f.failGet != nil {
		return repository.Chat{}, f.failGet
	}
	chat, ok := f.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.Chat{}, repository.ErrNotFound
	}
	return *chat, nil
}

func (f *fakeChats) ListChats(_ context.Context, userID string, includeArchived bool, limit, offset int) ([]repository.Chat, int, error) {
	if f.failList != nil {
		return nil, 0, f.failList
	}
	out := []repository.Chat{}
	for _, id := range f.order {
		chat := f.chats[id]
		if chat.UserID != userID {
			continue
		}
		if chat.ArchivedAt != nil && !includeArchived {
			continue
		}
		out = append(out, *chat)
	}
	return page(out, limit, offset), len(out), nil
}

func (f *fakeChats) RenameChat(_ context.Context, userID, chatID, title string) error {
	if f.failRename != nil {
		return f.failRename
	}
	chat, ok := f.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.ErrNotFound
	}
	chat.Title = title
	return nil
}

func (f *fakeChats) ArchiveChat(_ context.Context, userID, chatID string, at time.Time) error {
	if f.failArchive != nil {
		return f.failArchive
	}
	chat, ok := f.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.ErrNotFound
	}
	chat.ArchivedAt = &at
	return nil
}

func (f *fakeChats) AppendMessage(_ context.Context, input repository.NewMessage) (repository.Message, error) {
	if f.failAppend != nil {
		return repository.Message{}, f.failAppend
	}
	// Ownership is enforced in the INSERT in the real repository, so a message for
	// somebody else's chat is not written and not found.
	chat, ok := f.chats[input.ChatID]
	if !ok || chat.UserID != input.UserID {
		return repository.Message{}, repository.ErrNotFound
	}

	f.nextMsg++
	message := repository.Message{
		ID:        fmt.Sprintf("message-%d", f.nextMsg),
		ChatID:    input.ChatID,
		UserID:    input.UserID,
		RunID:     input.RunID,
		Role:      input.Role,
		Content:   input.Content,
		TokensIn:  input.TokensIn,
		TokensOut: input.TokensOut,
		CreatedAt: testNow,
	}
	f.messages[input.ChatID] = append(f.messages[input.ChatID], message)
	return message, nil
}

func (f *fakeChats) ListMessages(_ context.Context, userID, chatID string, limit, offset int) ([]repository.Message, int, error) {
	if f.failMessages != nil {
		return nil, 0, f.failMessages
	}
	chat, ok := f.chats[chatID]
	if !ok || chat.UserID != userID {
		return nil, 0, repository.ErrNotFound
	}
	all := f.messages[chatID]
	return page(all, limit, offset), len(all), nil
}

func (f *fakeChats) RecentMessages(_ context.Context, userID, chatID string, limit int) ([]repository.Message, error) {
	if f.failRecent != nil {
		return nil, f.failRecent
	}
	chat, ok := f.chats[chatID]
	if !ok || chat.UserID != userID {
		return nil, repository.ErrNotFound
	}
	all := f.messages[chatID]
	if limit > 0 && len(all) > limit {
		// The tail, oldest first — what the real query returns.
		all = all[len(all)-limit:]
	}
	out := make([]repository.Message, len(all))
	copy(out, all)
	return out, nil
}

// ---------------------------------------------------------------------------
// tasks

// fakeTasks is TaskStore, TaskReader, TaskQueue and TaskJanitor.
type fakeTasks struct {
	tasks  map[string]*domain.Task
	chatOf map[string]string
	byKey  map[string]string
	order  []string
	nextID int

	failCreate    error
	failByKey     error
	failGet       error
	failList      error
	failClaim     error
	failSetStatus error
	failRequeue   error

	// statuses records every SetStatus call, in order, as "taskID=status".
	statuses []string
	requeued int
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{
		tasks:  map[string]*domain.Task{},
		chatOf: map[string]string{},
		byKey:  map[string]string{},
	}
}

func (f *fakeTasks) Create(_ context.Context, input repository.NewTask) (domain.Task, error) {
	if f.failCreate != nil {
		return domain.Task{}, f.failCreate
	}
	task := input.Task
	if key := task.IdempotencyKey; key != "" {
		if _, taken := f.byKey[key]; taken {
			return domain.Task{}, repository.ErrConflict
		}
	}

	f.nextID++
	task.ID = fmt.Sprintf("task-%d", f.nextID)
	task.CreatedAt = testNow
	f.tasks[task.ID] = &task
	f.order = append(f.order, task.ID)
	if task.IdempotencyKey != "" {
		f.byKey[task.IdempotencyKey] = task.ID
	}
	if input.ChatID != "" {
		f.chatOf[task.ID] = input.ChatID
	}
	return task, nil
}

func (f *fakeTasks) GetByIdempotencyKey(_ context.Context, key string) (domain.Task, error) {
	if f.failByKey != nil {
		return domain.Task{}, f.failByKey
	}
	id, ok := f.byKey[key]
	if !ok {
		return domain.Task{}, repository.ErrNotFound
	}
	return *f.tasks[id], nil
}

func (f *fakeTasks) GetForUser(_ context.Context, userID, taskID string) (domain.Task, error) {
	if f.failGet != nil {
		return domain.Task{}, f.failGet
	}
	task, ok := f.tasks[taskID]
	if !ok || task.OwnerUserID != userID {
		return domain.Task{}, repository.ErrNotFound
	}
	return *task, nil
}

func (f *fakeTasks) ListForUser(_ context.Context, userID string, limit, offset int) ([]domain.Task, int, error) {
	if f.failList != nil {
		return nil, 0, f.failList
	}
	out := []domain.Task{}
	for _, id := range f.order {
		if f.tasks[id].OwnerUserID == userID {
			out = append(out, *f.tasks[id])
		}
	}
	return page(out, limit, offset), len(out), nil
}

func (f *fakeTasks) ClaimQueued(_ context.Context) (domain.Task, string, error) {
	if f.failClaim != nil {
		return domain.Task{}, "", f.failClaim
	}
	for _, id := range f.order {
		task := f.tasks[id]
		if task.Status != domain.TaskStatusQueued {
			continue
		}
		task.Status = domain.TaskStatusRunning
		return *task, f.chatOf[id], nil
	}
	return domain.Task{}, "", repository.ErrNotFound
}

func (f *fakeTasks) SetStatus(_ context.Context, taskID string, status domain.TaskStatus) error {
	f.statuses = append(f.statuses, taskID+"="+string(status))
	if f.failSetStatus != nil {
		return f.failSetStatus
	}
	task, ok := f.tasks[taskID]
	if !ok {
		return repository.ErrNotFound
	}
	task.Status = status
	return nil
}

func (f *fakeTasks) RequeueStale(_ context.Context, _ time.Time) (int, error) {
	if f.failRequeue != nil {
		return 0, f.failRequeue
	}
	return f.requeued, nil
}

// ---------------------------------------------------------------------------
// runs

// fakeRuns is RunStore, RunReader, RunAuditor, RunCanceller and RunJanitor.
type fakeRuns struct {
	records map[string]*repository.RunRecord
	steps   map[string][]domain.Step
	order   []string
	nextID  int

	failStart   error
	failGet     error
	failForUser error
	failForTask error
	failSteps   error
	failCancel  error
	failSweep   error

	failed int
	// started records the runs Start was asked for, so a test can assert on the limits
	// and the model a run was actually given.
	started []domain.Run
}

func newFakeRuns() *fakeRuns {
	return &fakeRuns{records: map[string]*repository.RunRecord{}, steps: map[string][]domain.Step{}}
}

func (f *fakeRuns) Start(_ context.Context, run domain.Run) (repository.RunRecord, error) {
	f.started = append(f.started, run)
	if f.failStart != nil {
		return repository.RunRecord{}, f.failStart
	}
	f.nextID++
	run.ID = fmt.Sprintf("run-%d", f.nextID)
	run.StartedAt = testNow
	record := repository.RunRecord{Run: run}
	f.records[run.ID] = &record
	f.order = append(f.order, run.ID)
	return record, nil
}

func (f *fakeRuns) Get(_ context.Context, runID string) (repository.RunRecord, error) {
	if f.failGet != nil {
		return repository.RunRecord{}, f.failGet
	}
	record, ok := f.records[runID]
	if !ok {
		return repository.RunRecord{}, repository.ErrNotFound
	}
	return *record, nil
}

func (f *fakeRuns) GetForUser(_ context.Context, userID, runID string) (repository.RunRecord, error) {
	if f.failForUser != nil {
		return repository.RunRecord{}, f.failForUser
	}
	record, ok := f.records[runID]
	if !ok || record.Run.OwnerUserID != userID {
		return repository.RunRecord{}, repository.ErrNotFound
	}
	return *record, nil
}

func (f *fakeRuns) ListForTask(_ context.Context, taskID string, limit, offset int) ([]repository.RunRecord, error) {
	if f.failForTask != nil {
		return nil, f.failForTask
	}
	out := []repository.RunRecord{}
	for _, id := range f.order {
		if f.records[id].Run.TaskID == taskID {
			out = append(out, *f.records[id])
		}
	}
	return page(out, limit, offset), nil
}

func (f *fakeRuns) Steps(_ context.Context, runID string, limit, offset int) ([]domain.Step, error) {
	if f.failSteps != nil {
		return nil, f.failSteps
	}
	return page(f.steps[runID], limit, offset), nil
}

func (f *fakeRuns) RequestCancel(_ context.Context, userID, runID string, at time.Time) error {
	if f.failCancel != nil {
		return f.failCancel
	}
	record, ok := f.records[runID]
	if !ok || record.Run.OwnerUserID != userID || !record.Run.InFlight() || record.Cancelled() {
		return repository.ErrNotFound
	}
	record.CancelRequestedAt = &at
	return nil
}

func (f *fakeRuns) FailAbandoned(_ context.Context, _, _ time.Time) (int, error) {
	if f.failSweep != nil {
		return 0, f.failSweep
	}
	return f.failed, nil
}

// ---------------------------------------------------------------------------
// spend, agent, tools, workspaces, reporting

// fakeSpend is SpendReader.
type fakeSpend struct {
	ledger  domain.Ledger
	charges map[string][]repository.Spend

	failLedger error
	failForRun error
}

func newFakeSpend() *fakeSpend {
	return &fakeSpend{
		ledger:  domain.Ledger{Readable: true},
		charges: map[string][]repository.Spend{},
	}
}

func (f *fakeSpend) Ledger(_ context.Context, _ string, _ time.Time) (domain.Ledger, error) {
	if f.failLedger != nil {
		return domain.Ledger{}, f.failLedger
	}
	return f.ledger, nil
}

func (f *fakeSpend) ForRun(_ context.Context, runID string) ([]repository.Spend, error) {
	if f.failForRun != nil {
		return nil, f.failForRun
	}
	return f.charges[runID], nil
}

// fakeAgent is AgentRunner. It records what it was asked to run, which is how the runner
// tests assert that the task, the workspace and the run's limits reached the loop.
type fakeAgent struct {
	outcome agent.Outcome
	err     error
	inputs  []agent.Input
}

func (f *fakeAgent) Run(_ context.Context, in agent.Input) (agent.Outcome, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return agent.Outcome{}, f.err
	}
	return f.outcome, nil
}

// fakeTools is ToolSource. The offering is a real one from an empty registry: the point
// of the port is that a run gets a snapshot, and a nil snapshot is a thing the loop
// refuses rather than a case to model here.
type fakeTools struct {
	err error
}

func (f *fakeTools) Offer(ctx context.Context) (*tool.Offering, error) {
	if f.err != nil {
		return nil, f.err
	}
	return tool.NewRegistry(nil).Offer(ctx)
}

// fakeWorkspaces is Workspaces.
type fakeWorkspaces struct {
	root string
	err  error
}

func (f *fakeWorkspaces) For(userID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.root + "/" + userID, nil
}

// fakeReporter is SpendReporter.
type fakeReporter struct {
	samples []goalengine.TokenSample
	err     error
}

func (f *fakeReporter) ReportTokens(_ context.Context, sample goalengine.TokenSample) error {
	f.samples = append(f.samples, sample)
	if f.err != nil {
		return f.err
	}
	return nil
}

// page applies limit and offset the way the repositories do: an out-of-range offset is an
// empty result rather than an error, and a zero limit means everything.
func page[T any](rows []T, limit, offset int) []T {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []T{}
	}
	rows = rows[offset:]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]T, len(rows))
	copy(out, rows)
	return out
}
