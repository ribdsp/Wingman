// Package service is core's orchestration layer: the place that decides whether a
// caller may do a thing, performs the several steps that thing needs in the right
// order, and turns a repository's errors into the handful of sentinels a handler knows
// how to render.
//
// Every dependency arrives as one of the interfaces below rather than as a
// *repository.X, for the same reason goal-engine's service package does it: a service
// that names a concrete repository can only be exercised against a database, and a
// service that names three methods can be exercised against a struct with three
// methods. Each port is satisfied by a repository in production and by a fake in
// fakes_test.go, and the split lines are drawn where the callers differ — read from
// write, a person's own data from an operator's view of everybody's.
//
// Three absences are deliberate.
//
// There is no AuditSink. goal-engine has one because its decisions are about somebody
// else's money and have to be answerable years later. Core's record of what an agent
// did is the transcript — the runs and run_steps rows the agent loop writes as it goes
// — and the business audit log lives next door, written by the service that owns the
// gate. A second, weaker audit log here would produce two accounts of the same event
// with no rule for deciding which one is right.
//
// There is no port for the spending gate, and none for the kill switch either. Both
// belong to the agent loop, are declared in internal/agent/ports.go, and are consulted
// inside the loop before every iteration. A check made once at dispatch time is a check
// that was true before the run started, and says nothing about the twenty minutes after
// it. Inbox does read the switch, through that same agent.Halt rather than through a
// second interface of its own — but for a different question, whether a stranger's
// message should become queued work at all, and it replaces nothing: the loop still
// checks before it starts and between iterations.
//
// There is no port for finishing a run. The loop writes that row itself, because the
// loop is the only thing that knows why it stopped, and a run whose ending could be
// written from two places is a run that can end twice.
package service

import (
	"context"
	"time"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// Clock is the only source of time a service reads.
//
// It is a dependency rather than a call to time.Now so that a test can assert on a
// session's expiry, a revocation's timestamp and a task's day boundary without
// sleeping. internal/domain takes now as a parameter for the same reason; this is that
// rule one layer out.
type Clock func() time.Time

// UserStore is the account surface every caller shares: making one, and reading one
// back by the two identifiers that are allowed to find it.
//
// GetByEmail exists for the operator's own wiring rather than for a request — cmd
// resolves CORE_UNATTENDED_OWNER to a user id at boot, so an unattended dispatch has
// an owner before the first one arrives rather than discovering it does not at three
// in the morning.
type UserStore interface {
	Create(ctx context.Context, email, displayName, passwordHash string) (repository.User, error)
	GetByID(ctx context.Context, id string) (repository.User, error)
	GetByEmail(ctx context.Context, email string) (repository.User, error)
	Count(ctx context.Context) (int, error)
}

// PasswordStore is separated from UserStore because it is the one port that touches a
// credential.
//
// Two methods, both of which handle a hash, in one place a reader can audit. Nothing
// that lists or displays users needs to be able to reach them.
type PasswordStore interface {
	GetCredentialsByEmail(ctx context.Context, email string) (repository.Credentials, error)
	UpdatePassword(ctx context.Context, userID, passwordHash string) error
}

// UserAdmin is the operator's view of the account list.
//
// Split from UserStore because these two are the only account operations a person
// cannot perform on themselves, and keeping them behind their own interface means the
// services a signed-in human reaches cannot deactivate anybody.
type UserAdmin interface {
	List(ctx context.Context, limit, offset int) ([]repository.User, int, error)
	SetActive(ctx context.Context, userID string, active bool) error
}

// SessionStore mints sessions and lists the ones a person has.
//
// The stored token never comes back out: repository.Session carries no hash, so a
// device list cannot be turned into a set of live credentials by whoever can read it.
//
// Resolving a token is not here. That is middleware.SessionStore's, satisfied in cmd by
// an adapter over the same repository, because authentication happens before a service
// is chosen and a service that could resolve a token would be a second front door.
type SessionStore interface {
	Create(ctx context.Context, input repository.NewSession) (repository.Session, error)
	ListForUser(ctx context.Context, userID string, limit, offset int) ([]repository.Session, error)
}

// SessionRevoker is deliberately its own port.
//
// Revocation happens on paths that must not be able to mint a session — changing a
// password, an operator deactivating an account — and a port that could do both would
// let a bug on one of those paths hand out a credential instead of taking one away.
type SessionRevoker interface {
	RevokeByToken(ctx context.Context, tokenHash string, at time.Time) error
	RevokeByID(ctx context.Context, userID, sessionID string, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID string, at time.Time) (int, error)
}

// SessionJanitor is the periodic sweep. Expired sessions are already refused by
// Resolve; deleting them is hygiene, so it is a background job and not part of any
// request.
type SessionJanitor interface {
	DeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// ChatStore is one person's chats. Every method takes the user id as its first
// argument on purpose: the scoping is in the signature, so a caller cannot forget it
// and a reviewer cannot miss it.
type ChatStore interface {
	CreateChat(ctx context.Context, userID, title string) (repository.Chat, error)
	GetChat(ctx context.Context, userID, chatID string) (repository.Chat, error)
	ListChats(ctx context.Context, userID string, includeArchived bool, limit, offset int) ([]repository.Chat, int, error)
	RenameChat(ctx context.Context, userID, chatID, title string) error
	ArchiveChat(ctx context.Context, userID, chatID string, at time.Time) error
}

// MessageStore is the messages in those chats.
//
// Not split from ChatStore by read and write, because appending a message is how a
// chat is read next time: the three methods are one concern, and the runner needs the
// same append the request path uses so an answer lands in the same place a question
// did.
type MessageStore interface {
	AppendMessage(ctx context.Context, input repository.NewMessage) (repository.Message, error)
	ListMessages(ctx context.Context, userID, chatID string, limit, offset int) ([]repository.Message, int, error)
	RecentMessages(ctx context.Context, userID, chatID string, limit int) ([]repository.Message, error)
}

// TaskStore is the dispatch path: create a task, or find the one an idempotency key
// already created.
//
// The two go together because they are one operation seen twice. A retry of a dispatch
// hits the unique index, gets repository.ErrConflict, and reads the original back —
// which is how the same key answers with the same task id rather than starting a
// second run.
type TaskStore interface {
	Create(ctx context.Context, input repository.NewTask) (domain.Task, error)
	GetByIdempotencyKey(ctx context.Context, key string) (domain.Task, error)
}

// TaskReader is a person's own tasks, scoped by their user id.
type TaskReader interface {
	GetForUser(ctx context.Context, userID, taskID string) (domain.Task, error)
	ListForUser(ctx context.Context, userID string, limit, offset int) ([]domain.Task, int, error)
}

// TaskQueue is the worker's end, and it is separated from TaskStore for a reason worth
// stating: the API must never be able to claim work, and a worker must never be able
// to list somebody's tasks. Two interfaces make that a compile-time property rather
// than a convention.
type TaskQueue interface {
	ClaimQueued(ctx context.Context) (domain.Task, string, error)
	SetStatus(ctx context.Context, taskID string, status domain.TaskStatus) error
}

// TaskJanitor requeues work whose worker died mid-run. Its own port because it is the
// only thing in the package that may move a task backwards.
type TaskJanitor interface {
	RequeueStale(ctx context.Context, startedBefore time.Time) (int, error)
}

// RunStore starts a run, and that is all it does.
//
// One method, because Finish belongs to the agent loop: see the package comment. Start
// is here rather than in the loop because a run has to exist before there is anything
// to hand the loop, and because repository.RunRepository.Start is what fills in the
// limits — a run that skipped it would carry zeroes, and domain.Decide reads a zero
// limit as a stop.
type RunStore interface {
	Start(ctx context.Context, run domain.Run) (repository.RunRecord, error)
}

// RunReader is the transcript as a caller reads it back.
type RunReader interface {
	GetForUser(ctx context.Context, userID, runID string) (repository.RunRecord, error)
	ListForTask(ctx context.Context, taskID string, limit, offset int) ([]repository.RunRecord, error)
	Steps(ctx context.Context, runID string, limit, offset int) ([]domain.Step, error)
}

// RunAuditor reads any run, regardless of whose it is. It is the operator's port, and it
// is separate from RunReader so that being able to read everybody's runs is a thing a
// service has to have been handed rather than a user id it forgot to pass.
//
// It exists because the transcript is core's whole record of what an agent did. An
// operator who cannot read it cannot answer "what did this thing do on my instance",
// which is the question the audit log next door exists to answer about money and this one
// answers about actions. It reads runs and steps — not chats, not messages.
type RunAuditor interface {
	Get(ctx context.Context, runID string) (repository.RunRecord, error)
}

// RunCanceller records that somebody asked a run to stop.
//
// It is a request, not a kill: the flag is set here and the loop reads it before its
// next iteration. Its own port because it is the one write on a run that a signed-in
// human may perform, and it is scoped by their user id.
type RunCanceller interface {
	RequestCancel(ctx context.Context, userID, runID string, at time.Time) error
}

// RunJanitor closes runs whose worker died. Same reasoning as TaskJanitor: a
// background job, not a request, and separate so nothing on a request path can end
// somebody else's run.
type RunJanitor interface {
	FailAbandoned(ctx context.Context, startedBefore, at time.Time) (int, error)
}

// SpendReader is the token ledger, read-only.
//
// There is no Record here on purpose. Charging a run is the loop's, through
// agent.Budget, because the loop is what knows a turn completed; a service that could
// also write to the ledger would be a second opinion about how much somebody has
// spent.
type SpendReader interface {
	Ledger(ctx context.Context, userID string, now time.Time) (domain.Ledger, error)
	ForRun(ctx context.Context, runID string) ([]repository.Spend, error)
}

// AgentRunner is the loop, behind an interface so the runner service can be tested
// without a provider.
//
// One method, and it is the whole agent: internal/agent decides everything about how a
// run proceeds, and this package decides only which runs happen and who may ask for
// one.
type AgentRunner interface {
	Run(ctx context.Context, in agent.Input) (agent.Outcome, error)
}

// ToolSource produces the run's tool snapshot.
//
// It returns *tool.Offering rather than agent.Tools because the concrete type carries
// what the runner has to log — the servers that could not be reached, the names two
// servers both claimed — and an interface narrowed to what the loop calls would hide
// exactly the diagnostics an operator needs when a tool goes missing.
type ToolSource interface {
	Offer(ctx context.Context) (*tool.Offering, error)
}

// Workspaces resolves a user's sandbox directory.
//
// Per user, never shared: two people's runs on one instance must not be able to read
// each other's files. internal/sandbox owns the root and the containment check; this
// port is only how the runner asks for a path.
type Workspaces interface {
	For(userID string) (string, error)
}

// SpendReporter files what a run cost with the goal engine, so that cost is a metric
// an operator can write a goal against.
//
// It names goalengine.TokenSample rather than a local shape because there is exactly
// one implementation and one caller, and a translation struct in between would be two
// more types to keep in step for no test that gets easier. A failure here is logged
// and dropped: the run has already happened, and losing the bookkeeping must not lose
// the work.
type SpendReporter interface {
	ReportTokens(ctx context.Context, sample goalengine.TokenSample) error
}

// ChannelResolver answers which account a chat account belongs to.
//
// It is its own port, holding one method, because it is the only one in this file that a
// stranger's message reaches. Everything else about channel identities is reached by
// somebody who signed in; this is reached by whoever found the bot. A service handed
// only this cannot list a person's connections, cannot revoke one and cannot create one
// — which is the entire authorisation story on the inbound path, since a resolved
// identity is the only reason core acts on a message at all.
type ChannelResolver interface {
	Resolve(ctx context.Context, kind repository.ChannelKind, externalID string) (repository.ChannelIdentity, error)
}

// ChannelLinker attaches a chat account to a Wingman account.
//
// Two methods, because a link is made in two situations: the first time, and again after
// somebody revoked it. They are not one method with a flag, because Relink is scoped to
// the user claiming the row and Link is not — a revoked identity may only be picked up
// again by the account that had it, so redeeming a code cannot take over a chat account
// somebody else once connected.
type ChannelLinker interface {
	Link(ctx context.Context, identity repository.ChannelIdentity) (repository.ChannelIdentity, error)
	Relink(ctx context.Context, userID string, kind repository.ChannelKind, externalID string, at time.Time) (repository.ChannelIdentity, error)
}

// ChannelLinks is a person's own view of what they have connected, and the way to undo
// it.
//
// Split from ChannelLinker for the same reason SessionRevoker is split from
// SessionStore: the service a signed-in person reaches should be able to take a
// connection away and not to make one. Making one requires a code that arrived over the
// channel, and that path is Inbox's.
type ChannelLinks interface {
	ListForUser(ctx context.Context, userID string) ([]repository.ChannelIdentity, error)
	Revoke(ctx context.Context, userID, identityID string, at time.Time) error
}

// LinkCodeMinter issues a code for a signed-in person to send over a channel.
type LinkCodeMinter interface {
	Mint(ctx context.Context, input repository.NewLinkCode) (repository.LinkCode, error)
}

// LinkCodeRedeemer spends one, and is deliberately not the same port.
//
// A minter is reached by somebody who proved who they are; a redeemer is reached by an
// unlinked stranger's message. One interface holding both would let the service that
// strangers talk to mint a credential for an account it has not identified yet.
type LinkCodeRedeemer interface {
	Consume(ctx context.Context, codeHash string, at time.Time) (string, error)
}

// LinkCodeJanitor is the periodic sweep, on the same reasoning as SessionJanitor: a
// spent code is already refused by Consume, and deleting it is hygiene rather than part
// of any request.
type LinkCodeJanitor interface {
	DeleteSpent(ctx context.Context, before time.Time) (int, error)
}

// ChannelChats is the one chat operation the inbound path performs.
//
// Separate from ChatStore, rather than a sixth method on it, because a chat carrying a
// channel and a conversation id is a chat that can be *delivered to*. One created with
// somebody else's conversation id would send that person's answers into a stranger's
// thread, so the operation that can do it is kept where a reader can see every caller of
// it at once.
type ChannelChats interface {
	EnsureChannelChat(ctx context.Context, input repository.ChannelChat) (repository.Chat, error)
}

// ChannelWork is how an accepted channel message becomes queued work.
//
// It exists so Inbox depends on the one thing it needs from Tasks rather than on *Tasks,
// which would drag dispatch and a person's task list into the service a stranger's
// message reaches. Satisfied by *Tasks in production, since the point is that a channel
// message ends at the same queue as everything else.
type ChannelWork interface {
	FromChannel(ctx context.Context, in ChannelMessage) (Sent, error)
}

// ChannelReplier delivers a run's answer back to the channel it was asked on.
//
// It takes a chat id, not a channel and a conversation, because the runner does not know
// what a channel is and must not have to: it knows which chat it answered in, and this
// port turns that into a delivery. The implementation lives in internal/channel, where
// the platform limits and the connections are. A failure is logged and dropped, exactly
// like SpendReporter's — the answer is already in the chat, and losing the delivery must
// not lose the run.
type ChannelReplier interface {
	Reply(ctx context.Context, userID, chatID, text string) error
}

// ChannelDirectory is where an account's chat identities are read from, and nothing else.
//
// Deliberately not ChannelLinks, which also revokes: the service that notifies somebody
// must not be able to take a connection away, and the two are reached by different callers
// — a signed-in person for one, a machine with something to report for the other. Same
// split, and the same reason, as SessionRevoker from SessionStore.
//
// It returns revoked rows too, because the repository does; filtering them is the caller's
// job and is asserted by a test, since a revoked identity is somebody who asked not to be
// messaged here again.
type ChannelDirectory interface {
	ListForUser(ctx context.Context, userID string) ([]repository.ChannelIdentity, error)
}

// DirectSender delivers text to one person on one platform.
//
// It takes the person's own id on the platform rather than a conversation, because that is
// what a link stores — turning it into somewhere a message can go is the adapter's problem
// and on one of the three platforms it costs a request. Implemented by *channel.Hub, where
// the connections and the platform limits are.
type DirectSender interface {
	SendDirect(ctx context.Context, kind repository.ChannelKind, externalUserID, text string) error
}
