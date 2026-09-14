package handler

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/middleware"
	"github.com/ribdsp/wingman/core/internal/repository"
)

// In-memory stand-ins for every port the five services take.
//
// They are duplicated from internal/service's fakes rather than shared, because a Go test
// file is not importable. That duplication is the price of testing the transport layer
// against real services, and it buys the thing worth having: a request in one of these
// tests goes through the same middleware, the same authorisation and the same rendering it
// would in production, so a rule enforced in the wrong layer shows up as a failure here.
//
// What they do not do is enforce anything. Scoping, ownership and role checks are the
// services' and the middleware's; a fake that also refused would hide a missing check
// rather than reveal one. The one exception is user_id scoping in the chat and task stores,
// which is what makes "somebody else's id answers 404" a real assertion rather than a
// coincidence.

// errBoom is the failure a fake returns when a test is about what happens when storage
// breaks. It carries a recognisable word so a test can assert it never reaches a response
// body.
var errBoom = errors.New("boom: the storage layer failed, and this text must never be rendered")

// memUsers is the accounts table.
type memUsers struct {
	mu    sync.Mutex
	seq   int
	byID  map[string]repository.User
	hash  map[string]string // userID → password hash
	err   error             // returned by every method when set
	failN map[string]error  // method name → error, for one-method failures
}

func newMemUsers() *memUsers {
	return &memUsers{
		byID:  map[string]repository.User{},
		hash:  map[string]string{},
		failN: map[string]error{},
	}
}

func (m *memUsers) fault(method string) error {
	if m.err != nil {
		return m.err
	}
	return m.failN[method]
}

func (m *memUsers) Create(_ context.Context, email, displayName, passwordHash string) (repository.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("Create"); err != nil {
		return repository.User{}, err
	}
	for _, existing := range m.byID {
		if strings.EqualFold(existing.Email, email) {
			return repository.User{}, repository.ErrConflict
		}
	}

	m.seq++
	user := repository.User{
		ID:          "user-" + strconv.Itoa(m.seq),
		Email:       email,
		DisplayName: displayName,
		IsActive:    true,
		CreatedAt:   testNow,
	}
	m.byID[user.ID] = user
	m.hash[user.ID] = passwordHash
	return user, nil
}

func (m *memUsers) GetByID(_ context.Context, id string) (repository.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("GetByID"); err != nil {
		return repository.User{}, err
	}
	user, ok := m.byID[id]
	if !ok {
		return repository.User{}, repository.ErrNotFound
	}
	return user, nil
}

func (m *memUsers) GetByEmail(_ context.Context, email string) (repository.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("GetByEmail"); err != nil {
		return repository.User{}, err
	}
	for _, user := range m.byID {
		if strings.EqualFold(user.Email, email) {
			return user, nil
		}
	}
	return repository.User{}, repository.ErrNotFound
}

func (m *memUsers) Count(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("Count"); err != nil {
		return 0, err
	}
	return len(m.byID), nil
}

func (m *memUsers) GetCredentialsByEmail(_ context.Context, email string) (repository.Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("GetCredentialsByEmail"); err != nil {
		return repository.Credentials{}, err
	}
	for _, user := range m.byID {
		if strings.EqualFold(user.Email, email) {
			return repository.Credentials{
				UserID:       user.ID,
				PasswordHash: m.hash[user.ID],
				IsActive:     user.IsActive,
			}, nil
		}
	}
	return repository.Credentials{}, repository.ErrNotFound
}

func (m *memUsers) UpdatePassword(_ context.Context, userID, passwordHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("UpdatePassword"); err != nil {
		return err
	}
	if _, ok := m.byID[userID]; !ok {
		return repository.ErrNotFound
	}
	m.hash[userID] = passwordHash
	return nil
}

// List returns users in id order, so a paging assertion is deterministic.
func (m *memUsers) List(_ context.Context, limit, offset int) ([]repository.User, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("List"); err != nil {
		return nil, 0, err
	}

	ordered := make([]repository.User, 0, len(m.byID))
	for i := 1; i <= m.seq; i++ {
		if user, ok := m.byID["user-"+strconv.Itoa(i)]; ok {
			ordered = append(ordered, user)
		}
	}
	return window(ordered, limit, offset), len(ordered), nil
}

func (m *memUsers) SetActive(_ context.Context, userID string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault("SetActive"); err != nil {
		return err
	}
	user, ok := m.byID[userID]
	if !ok {
		return repository.ErrNotFound
	}
	user.IsActive = active
	m.byID[userID] = user
	return nil
}

// memSessions is the sessions table, plus the token resolution the middleware needs.
type memSessions struct {
	mu       sync.Mutex
	seq      int
	rows     map[string]repository.Session // sessionID → row
	byHash   map[string]string             // stored token → sessionID
	names    map[string]string             // userID → display name, for the Caller
	err      error
	resolve  error // returned by ResolveSession only
	revoking error // returned by every revoke method only
}

func newMemSessions() *memSessions {
	return &memSessions{
		rows:   map[string]repository.Session{},
		byHash: map[string]string{},
		names:  map[string]string{},
	}
}

func (m *memSessions) Create(_ context.Context, input repository.NewSession) (repository.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.Session{}, m.err
	}

	m.seq++
	session := repository.Session{
		ID:        "session-" + strconv.Itoa(m.seq),
		UserID:    input.UserID,
		UserAgent: input.UserAgent,
		CreatedIP: input.CreatedIP,
		CreatedAt: testNow,
		ExpiresAt: input.ExpiresAt,
	}
	m.rows[session.ID] = session
	m.byHash[input.TokenHash] = session.ID
	return session, nil
}

func (m *memSessions) ListForUser(_ context.Context, userID string, limit, offset int) ([]repository.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}

	ordered := make([]repository.Session, 0, len(m.rows))
	for i := 1; i <= m.seq; i++ {
		row, ok := m.rows["session-"+strconv.Itoa(i)]
		if ok && row.UserID == userID {
			ordered = append(ordered, row)
		}
	}
	return window(ordered, limit, offset), nil
}

func (m *memSessions) RevokeByToken(_ context.Context, tokenHash string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.revokeFault(); err != nil {
		return err
	}
	id, ok := m.byHash[tokenHash]
	if !ok {
		return repository.ErrNotFound
	}
	return m.revoke(id, at)
}

func (m *memSessions) RevokeByID(_ context.Context, userID, sessionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.revokeFault(); err != nil {
		return err
	}
	row, ok := m.rows[sessionID]
	// Scoped on the user id, exactly as the repository's UPDATE is: somebody else's
	// session id is not found rather than forbidden.
	if !ok || row.UserID != userID || row.RevokedAt != nil {
		return repository.ErrNotFound
	}
	return m.revoke(sessionID, at)
}

func (m *memSessions) RevokeAllForUser(_ context.Context, userID string, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.revokeFault(); err != nil {
		return 0, err
	}

	revoked := 0
	for id, row := range m.rows {
		if row.UserID == userID && row.RevokedAt == nil {
			_ = m.revoke(id, at)
			revoked++
		}
	}
	return revoked, nil
}

func (m *memSessions) DeleteExpired(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	deleted := 0
	for id, row := range m.rows {
		if row.ExpiresAt.Before(before) {
			delete(m.rows, id)
			deleted++
		}
	}
	return deleted, nil
}

// ResolveSession is middleware.SessionStore. In production this is an adapter in cmd over
// the same repository; here it is the same map, so a token minted by signing in
// authenticates the next request the way it would on a real instance.
//
// Liveness is judged against testNow rather than the instant the middleware passes in.
// middleware.Authenticate reads the real clock and cannot be given another, while every
// row here is stamped at a fixed one — comparing the two would make the suite pass or fail
// depending on the day it is run.
func (m *memSessions) ResolveSession(_ context.Context, storedToken string, _ time.Time) (middleware.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolve != nil {
		return middleware.Session{}, m.resolve
	}

	id, ok := m.byHash[storedToken]
	if !ok {
		return middleware.Session{}, middleware.ErrNoSession
	}
	row := m.rows[id]
	if !row.Live(testNow) {
		return middleware.Session{}, middleware.ErrNoSession
	}
	return middleware.Session{UserID: row.UserID, Name: m.names[row.UserID]}, nil
}

func (m *memSessions) revokeFault() error {
	if m.err != nil {
		return m.err
	}
	return m.revoking
}

// setName records the handle ResolveSession reports for an account, which is what the
// audit log ends up recording for a signed-in person.
func (m *memSessions) setName(userID, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names[userID] = name
}

// revoke marks one row ended. Called with the lock held.
func (m *memSessions) revoke(sessionID string, at time.Time) error {
	row := m.rows[sessionID]
	when := at
	row.RevokedAt = &when
	m.rows[sessionID] = row
	return nil
}

// memChats is the chats and messages tables, scoped by user id.
type memChats struct {
	mu     sync.Mutex
	seq    int
	msgSeq int
	chats  map[string]repository.Chat
	// channelChats maps account+platform+conversation to a chat id, standing in for the
	// unique index the real table carries.
	channelChats map[string]string
	messages     []repository.Message
	err          error
}

func newMemChats() *memChats {
	return &memChats{chats: map[string]repository.Chat{}, channelChats: map[string]string{}}
}

func (m *memChats) CreateChat(_ context.Context, userID, title string) (repository.Chat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.Chat{}, m.err
	}

	m.seq++
	chat := repository.Chat{
		ID:        "chat-" + strconv.Itoa(m.seq),
		UserID:    userID,
		Title:     title,
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}
	m.chats[chat.ID] = chat
	return chat, nil
}

// EnsureChannelChat finds or creates the chat one channel conversation lives in.
func (m *memChats) EnsureChannelChat(_ context.Context, in repository.ChannelChat) (repository.Chat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.Chat{}, m.err
	}

	key := in.UserID + "|" + string(in.Kind) + "|" + in.ConversationID
	if id, ok := m.channelChats[key]; ok {
		return m.chats[id], nil
	}

	m.seq++
	chat := repository.Chat{
		ID:        "chat-" + strconv.Itoa(m.seq),
		UserID:    in.UserID,
		Title:     in.Title,
		CreatedAt: testNow,
		UpdatedAt: testNow,
	}
	m.chats[chat.ID] = chat
	m.channelChats[key] = chat.ID
	return chat, nil
}

func (m *memChats) GetChat(_ context.Context, userID, chatID string) (repository.Chat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.Chat{}, m.err
	}
	chat, ok := m.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.Chat{}, repository.ErrNotFound
	}
	return chat, nil
}

func (m *memChats) ListChats(_ context.Context, userID string, includeArchived bool, limit, offset int) ([]repository.Chat, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, 0, m.err
	}

	ordered := make([]repository.Chat, 0, len(m.chats))
	for i := 1; i <= m.seq; i++ {
		chat, ok := m.chats["chat-"+strconv.Itoa(i)]
		switch {
		case !ok, chat.UserID != userID:
			continue
		case chat.ArchivedAt != nil && !includeArchived:
			continue
		}
		ordered = append(ordered, chat)
	}
	return window(ordered, limit, offset), len(ordered), nil
}

func (m *memChats) RenameChat(_ context.Context, userID, chatID, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	chat, ok := m.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.ErrNotFound
	}
	chat.Title = title
	chat.UpdatedAt = testNow
	m.chats[chatID] = chat
	return nil
}

func (m *memChats) ArchiveChat(_ context.Context, userID, chatID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	chat, ok := m.chats[chatID]
	if !ok || chat.UserID != userID {
		return repository.ErrNotFound
	}
	when := at
	chat.ArchivedAt = &when
	m.chats[chatID] = chat
	return nil
}

func (m *memChats) AppendMessage(_ context.Context, input repository.NewMessage) (repository.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.Message{}, m.err
	}
	chat, ok := m.chats[input.ChatID]
	if !ok || chat.UserID != input.UserID {
		return repository.Message{}, repository.ErrNotFound
	}

	m.msgSeq++
	message := repository.Message{
		ID:        "message-" + strconv.Itoa(m.msgSeq),
		ChatID:    input.ChatID,
		UserID:    input.UserID,
		RunID:     input.RunID,
		Role:      input.Role,
		Content:   input.Content,
		TokensIn:  input.TokensIn,
		TokensOut: input.TokensOut,
		CreatedAt: testNow,
	}
	m.messages = append(m.messages, message)
	return message, nil
}

func (m *memChats) ListMessages(_ context.Context, userID, chatID string, limit, offset int) ([]repository.Message, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, 0, m.err
	}
	chat, ok := m.chats[chatID]
	if !ok || chat.UserID != userID {
		return nil, 0, repository.ErrNotFound
	}

	matching := make([]repository.Message, 0, len(m.messages))
	for _, message := range m.messages {
		if message.ChatID == chatID {
			matching = append(matching, message)
		}
	}
	return window(matching, limit, offset), len(matching), nil
}

func (m *memChats) RecentMessages(ctx context.Context, userID, chatID string, limit int) ([]repository.Message, error) {
	messages, _, err := m.ListMessages(ctx, userID, chatID, limit, 0)
	return messages, err
}

// memTasks is the tasks table, including the unique index on the idempotency key.
type memTasks struct {
	mu     sync.Mutex
	seq    int
	byID   map[string]domain.Task
	byKey  map[string]string // idempotency key → task id
	err    error
	create error // returned by Create only, so a conflict path can be forced
}

func newMemTasks() *memTasks {
	return &memTasks{byID: map[string]domain.Task{}, byKey: map[string]string{}}
}

func (m *memTasks) Create(_ context.Context, input repository.NewTask) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Task{}, m.err
	}
	if m.create != nil {
		return domain.Task{}, m.create
	}
	if key := input.Task.IdempotencyKey; key != "" {
		if _, taken := m.byKey[key]; taken {
			// What the unique index does. The service reads the original back.
			return domain.Task{}, repository.ErrConflict
		}
	}

	m.seq++
	task := input.Task
	task.ID = "task-" + strconv.Itoa(m.seq)
	task.CreatedAt = testNow
	if task.Status == "" {
		task.Status = domain.TaskStatusQueued
	}
	m.byID[task.ID] = task
	if task.IdempotencyKey != "" {
		m.byKey[task.IdempotencyKey] = task.ID
	}
	return task, nil
}

func (m *memTasks) GetByIdempotencyKey(_ context.Context, key string) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Task{}, m.err
	}
	id, ok := m.byKey[key]
	if !ok {
		return domain.Task{}, repository.ErrNotFound
	}
	return m.byID[id], nil
}

func (m *memTasks) GetForUser(_ context.Context, userID, taskID string) (domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Task{}, m.err
	}
	task, ok := m.byID[taskID]
	if !ok || task.OwnerUserID != userID {
		return domain.Task{}, repository.ErrNotFound
	}
	return task, nil
}

func (m *memTasks) ListForUser(_ context.Context, userID string, limit, offset int) ([]domain.Task, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, 0, m.err
	}

	ordered := make([]domain.Task, 0, len(m.byID))
	for i := 1; i <= m.seq; i++ {
		task, ok := m.byID["task-"+strconv.Itoa(i)]
		if ok && task.OwnerUserID == userID {
			ordered = append(ordered, task)
		}
	}
	return window(ordered, limit, offset), len(ordered), nil
}

// count is how many tasks exist at all, for the tests whose point is that a refused
// request filed none.
func (m *memTasks) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// memRuns is the runs, run_steps and token_spend tables.
type memRuns struct {
	mu      sync.Mutex
	order   []string // run ids in insertion order, so a listing is deterministic
	records map[string]repository.RunRecord
	steps   map[string][]domain.Step
	spend   map[string][]repository.Spend
	ledger  map[string]domain.Ledger
	err     error
	cancel  error // returned by RequestCancel only
}

func newMemRuns() *memRuns {
	return &memRuns{
		records: map[string]repository.RunRecord{},
		steps:   map[string][]domain.Step{},
		spend:   map[string][]repository.Spend{},
		ledger:  map[string]domain.Ledger{},
	}
}

func (m *memRuns) GetForUser(_ context.Context, userID, runID string) (repository.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.RunRecord{}, m.err
	}
	record, ok := m.records[runID]
	if !ok || record.Run.OwnerUserID != userID {
		return repository.RunRecord{}, repository.ErrNotFound
	}
	return record, nil
}

// Get is RunAuditor: any run, whoever it belongs to.
func (m *memRuns) Get(_ context.Context, runID string) (repository.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return repository.RunRecord{}, m.err
	}
	record, ok := m.records[runID]
	if !ok {
		return repository.RunRecord{}, repository.ErrNotFound
	}
	return record, nil
}

func (m *memRuns) ListForTask(_ context.Context, taskID string, limit, offset int) ([]repository.RunRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}

	matching := make([]repository.RunRecord, 0, len(m.records))
	for _, id := range m.order {
		if record := m.records[id]; record.Run.TaskID == taskID {
			matching = append(matching, record)
		}
	}
	return window(matching, limit, offset), nil
}

func (m *memRuns) Steps(_ context.Context, runID string, limit, offset int) ([]domain.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return window(m.steps[runID], limit, offset), nil
}

func (m *memRuns) RequestCancel(_ context.Context, userID, runID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.cancel != nil {
		return m.cancel
	}
	record, ok := m.records[runID]
	if !ok || record.Run.OwnerUserID != userID || !record.Run.InFlight() || record.Cancelled() {
		return repository.ErrNotFound
	}
	when := at
	record.CancelRequestedAt = &when
	m.records[runID] = record
	return nil
}

func (m *memRuns) Ledger(_ context.Context, userID string, _ time.Time) (domain.Ledger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Ledger{}, m.err
	}
	ledger, ok := m.ledger[userID]
	if !ok {
		// An account that has spent nothing today still has a readable ledger. A false
		// here would mean "no answer", which is a different thing.
		return domain.Ledger{Readable: true}, nil
	}
	return ledger, nil
}

func (m *memRuns) ForRun(_ context.Context, runID string) ([]repository.Spend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.spend[runID], nil
}

// putLedger seeds one account's spend for today, for the tests about how a ledger renders.
func (m *memRuns) putLedger(userID string, ledger domain.Ledger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ledger[userID] = ledger
}

// put records a run and its transcript, for tests that need one to read back.
func (m *memRuns) put(record repository.RunRecord, steps []domain.Step, charges []repository.Spend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, seen := m.records[record.Run.ID]; !seen {
		m.order = append(m.order, record.Run.ID)
	}
	m.records[record.Run.ID] = record
	m.steps[record.Run.ID] = steps
	m.spend[record.Run.ID] = charges
}

// window applies a limit and offset the way a repository's LIMIT/OFFSET would, so a paging
// assertion in a handler test means the same thing it would against Postgres.
func window[T any](rows []T, limit, offset int) []T {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return nil
	}
	rows = rows[offset:]
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}
