package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/channel"
	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/middleware"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/sandbox"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// The joins between packages that deliberately do not know about each other.
//
// Every type in this file is an adapter, and each one exists because the alternative was
// a package importing another package's types, or a repository answering a question that
// is not a repository's to answer. They are here rather than in internal/ because this
// is the only place that knows how this instance is assembled.

const (
	// readinessTimeout bounds the database round trip a readiness probe makes. Short
	// on purpose: a pool that cannot answer in this long is not ready, whatever it
	// would have said given another second.
	readinessTimeout = 2 * time.Second

	// touchInterval is how often a session's "last seen" is written at most.
	//
	// SessionRepository.TouchLastSeen documents the expectation this honours: resolving
	// a session happens on every authenticated request, and a write per request is a
	// write that can be skipped. Last seen is for a person recognising their own
	// devices, not an access log.
	touchInterval = 5 * time.Minute

	// maxTouchTracked bounds the throttle's memory. Reached, it is emptied rather than
	// pruned: the cost of forgetting is one extra write per session, and the cost of a
	// map that only grows is a process that has to be restarted.
	maxTouchTracked = 4096
)

// newSandbox builds the backend the operator asked for.
//
// The docker backend looks up its CLI here, so a box configured for docker without it
// installed fails at startup rather than on the first tool call.
func newSandbox(cfg config.SandboxConfig, workspaces *sandbox.Workspaces) (sandbox.Sandbox, error) {
	switch cfg.Backend {
	case config.SandboxDocker:
		return sandbox.NewDocker(workspaces, sandbox.DockerConfig{
			Image:   cfg.Image,
			Network: cfg.Network,
			Memory:  cfg.Memory,
		})
	case config.SandboxLocal:
		return sandbox.NewLocal(workspaces)
	default:
		// Unreachable: config refuses to boot on an unknown backend. Present so that a
		// third backend added to config without one added here fails loudly rather than
		// silently running on the host.
		return nil, fmt.Errorf("sandbox backend %q is not implemented", cfg.Backend)
	}
}

// sandboxTool is the one-line adapter internal/sandbox and internal/tool were written
// to need.
//
// Both declare the same Exec shape in their own terms so that neither imports the
// other. This is the line that costs, and it buys a tool package that can be tested
// with a fake sandbox and a sandbox package that knows nothing about tools.
type sandboxTool struct{ box sandbox.Sandbox }

func (s sandboxTool) Exec(ctx context.Context, workspace, script string) (tool.ExecResult, error) {
	result, err := s.box.Exec(ctx, workspace, script)
	return tool.ExecResult(result), err
}

// newToolRegistry assembles every source of tools this instance offers, and returns
// the function that closes the ones holding a connection.
//
// The shell is always registered and the MCP and HTTP runners are registered for every
// enabled declaration — registered, not offered. What a run may actually call is
// decided by the grants in tools.yaml, so a source with no granted tools contributes
// nothing to any run. Registering it anyway means an operator who adds a grant gets it
// on the next run rather than after a restart.
func newToolRegistry(
	grants *tool.Grants,
	box sandbox.Sandbox,
	servers []tool.MCPServer,
	services []tool.HTTPService,
	log zerolog.Logger,
) (*tool.Registry, func()) {
	runners := []tool.Runner{tool.NewShell(sandboxTool{box: box})}
	var sessions []*tool.MCP

	for _, server := range tool.EnabledMCPServers(servers) {
		// NewMCP does not connect. A server that is down must not stop this process from
		// booting, or one unreachable tool source takes the whole instance with it.
		client := tool.NewMCP(server, version)
		sessions = append(sessions, client)
		runners = append(runners, client)
	}
	for _, service := range tool.EnabledHTTPServices(services) {
		// os.LookupEnv is passed in rather than read here: the credential stays in the
		// environment, and the config file names only the variable.
		runners = append(runners, tool.NewHTTPAPI(service, version, os.LookupEnv))
	}

	log.Info().
		Int("mcpServers", len(sessions)).
		Int("httpServices", len(tool.EnabledHTTPServices(services))).
		Strs("grantedTools", grants.Names()).
		Msg("tool sources registered")

	return tool.NewRegistry(grants, runners...), func() {
		for _, client := range sessions {
			// A failure here is logged and dropped. The process is stopping; a server that
			// cannot be told so will notice when the pipe closes.
			if err := client.Close(); err != nil {
				log.Warn().Err(err).Str("source", client.Label()).Msg("closing a tool source")
			}
		}
	}
}

// newChannelHub connects to whichever chat platforms the operator gave credentials for.
//
// Nothing here dials: each constructor checks what it can about its token without asking
// the platform — Telegram's has a checkable shape, the others only have to be present —
// and builds a client. The connection itself is opened by Hub.Run. That is deliberate: a
// platform having an outage while this process starts must not stop the process, because
// the other platforms and every HTTP route still work. A well-formed token the platform
// itself rejects fails on the first poll, which the hub logs and the other channels
// survive.
//
// An empty result is normal and is not an error: an instance reached only over HTTP has no
// channels, and NewHub is happy with none.
func newChannelHub(cfg config.ChannelsConfig, inbound channel.Handler, log zerolog.Logger) (*channel.Hub, error) {
	var channels []channel.Channel

	if cfg.TelegramToken != "" {
		telegram, err := channel.NewTelegram(channel.TelegramConfig{
			Token:   cfg.TelegramToken,
			Handler: inbound,
			Logger:  log,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, telegram)
	}
	// Both tokens or neither. Config has already refused half a Slack app, so this only
	// has to agree with it rather than report it again.
	if cfg.SlackConfigured() {
		slack, err := channel.NewSlack(channel.SlackConfig{
			BotToken: cfg.SlackBotToken,
			AppToken: cfg.SlackAppToken,
			Handler:  inbound,
			Logger:   log,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, slack)
	}
	if cfg.DiscordToken != "" {
		discord, err := channel.NewDiscord(channel.DiscordConfig{
			Token:   cfg.DiscordToken,
			Handler: inbound,
			Logger:  log,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, discord)
	}

	return channel.NewHub(log, channels...)
}

// channelTargets adapts the chat repository to the replier's lookup.
//
// The two structs are field-identical, so the conversion is the whole body — and that is
// the point: internal/channel declares a target of its own precisely so that transport
// never imports a repository, and this is what the choice costs. The method names differ
// because the repository answers about chats in general, most of which are not channel
// conversations at all.
type channelTargets struct{ *repository.ChatRepository }

func (t channelTargets) TargetFor(ctx context.Context, userID, chatID string) (channel.Target, error) {
	target, err := t.ChannelTargetFor(ctx, userID, chatID)
	return channel.Target(target), err
}

// connectedKinds names the platforms this instance holds a connection to, for
// /v1/reference.
//
// A list of names rather than the hub, because internal/handler is deliberately unaware
// that internal/channel exists. Nil when there is no hub, which the handler renders as an
// empty array — never null, because a client that has to tell "none" from "the field is
// missing" will get it wrong once.
func connectedKinds(hub *channel.Hub) []string {
	if hub == nil {
		return nil
	}
	kinds := hub.Kinds()
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return names
}

// directSender is where a notification leaves this process: the hub when a chat platform is
// connected, and a refusal when none is.
//
// A named stub rather than the nil hub, because a typed nil in an interface is not nil. The
// notifier's constructor would accept it, and whether the call then worked would depend on
// Hub's methods happening to survive a nil receiver — which is not a property to rest on for
// the one path whose job is to tell somebody a spend is waiting for them.
func directSender(hub *channel.Hub) service.DirectSender {
	if hub == nil {
		return noChannelsConnected{}
	}
	return hub
}

// noChannelsConnected is the sender on an instance reached only over HTTP.
//
// It refuses rather than reporting a delivery. Nothing is normally asked of it — an owner
// with no chat account linked is answered before a send is attempted — so reaching it means a
// link survives from a deployment where the platform's token was still configured, and
// pretending that message arrived would be the one lie this path must not tell.
type noChannelsConnected struct{}

func (noChannelsConnected) SendDirect(_ context.Context, kind repository.ChannelKind, _, _ string) error {
	return fmt.Errorf("%w: %s", channel.ErrNotConnected, kind)
}

// readiness is the check behind /readyz: can this instance reach its database?
//
// Deliberately not the goal engine and not the kill switch. An instance whose engine is
// unreachable still has to serve the routes a person uses to see what their agent did,
// and an engaged switch was engaged on purpose — reporting either as unready would pull
// core out of a load balancer for a condition that is not core's. The handler logs the
// cause and renders none of it, because a readiness body ends up on status pages and a
// failing pool's error carries the DSN.
func readiness(db *sqlx.DB) func(c *gin.Context) error {
	return func(c *gin.Context) error {
		ctx, cancel := context.WithTimeout(c.Request.Context(), readinessTimeout)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("database is not reachable: %w", err)
		}
		return nil
	}
}

// sessionStore adapts the session repository to what the authentication middleware
// needs, and does the one thing neither of them should: decide how often "last seen" is
// worth a write.
type sessionStore struct {
	sessions *repository.SessionRepository
	log      zerolog.Logger

	mu      sync.Mutex
	touched map[string]time.Time
}

func newSessionStore(sessions *repository.SessionRepository, log zerolog.Logger) *sessionStore {
	return &sessionStore{sessions: sessions, log: log, touched: map[string]time.Time{}}
}

// ResolveSession turns a stored token into the person behind it.
//
// The repository's ErrNotFound becomes middleware.ErrNoSession, and every other error is
// passed through unchanged — that distinction is the whole adapter. A token that is
// unknown, expired or revoked is a failed sign-in and answers 401; a database that
// cannot be read is an outage and answers 503. Collapsing the two would show a person a
// sign-in screen for an outage, and show the operator a spike in failed authentications
// instead of a broken database.
func (s *sessionStore) ResolveSession(ctx context.Context, storedToken string, now time.Time) (middleware.Session, error) {
	live, err := s.sessions.Resolve(ctx, storedToken, now)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return middleware.Session{}, middleware.ErrNoSession
		}
		return middleware.Session{}, err
	}

	if s.shouldTouch(storedToken, now) {
		if err := s.sessions.TouchLastSeen(ctx, storedToken, now); err != nil {
			// Never the request's problem. The person is authenticated; failing here would
			// refuse a valid session over bookkeeping nobody is waiting on.
			s.log.Warn().Err(err).Msg("a session's last-seen time could not be recorded")
		}
	}
	return middleware.Session{UserID: live.UserID, Name: live.DisplayName}, nil
}

// shouldTouch reports whether this session's last-seen time is stale enough to write,
// and records the decision.
func (s *sessionStore) shouldTouch(storedToken string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if last, seen := s.touched[storedToken]; seen && now.Sub(last) < touchInterval {
		return false
	}
	if len(s.touched) >= maxTouchTracked {
		s.touched = map[string]time.Time{}
	}
	s.touched[storedToken] = now
	return true
}

// cancellableRuns adds the one question the run repository should not answer on its own.
//
// Finish passes straight through. CancelRequested is here because "has somebody asked
// this run to stop?" is a reading of a nullable timestamp, and a repository that
// returned a bool would be a repository deciding what cancelled means.
type cancellableRuns struct{ *repository.RunRepository }

func (c cancellableRuns) CancelRequested(ctx context.Context, runID string) (bool, error) {
	record, err := c.Get(ctx, runID)
	if err != nil {
		// Not swallowed into a false. The loop treats an unreadable run row as a reason to
		// stop, which is the same fail-closed rule the kill switch gets: not knowing
		// whether somebody asked us to stop is not permission to continue.
		return false, err
	}
	return record.Cancelled(), nil
}

// ledger adapts the spend repository to the loop's Budget port.
//
// Ledger passes straight through, including its rule that an unreadable ledger comes
// back as domain.Ledger{Readable: false} rather than a zero total. Charge exists because
// the loop's own Charge type carries the same fields under a name that belongs to the
// loop, and translating them here keeps internal/agent from importing a repository.
type ledger struct{ *repository.SpendRepository }

func (l ledger) Charge(ctx context.Context, charge agent.Charge) error {
	return l.Record(ctx, repository.Spend{
		UserID:     charge.UserID,
		RunID:      charge.RunID,
		Provider:   charge.Provider,
		Model:      charge.Model,
		TokensIn:   charge.TokensIn,
		TokensOut:  charge.TokensOut,
		OccurredAt: charge.At,
	})
}

// spendGate files a spending tool call with the goal engine's approval ladder.
//
// Core has no gate of its own and this adapter adds none: it translates the request,
// sends it, and returns whatever came back. An outcome this version does not recognise
// is passed through untouched, because domain.DecideAfterGate treats anything it cannot
// read as pending rather than as approval.
type spendGate struct{ engine *goalengine.Client }

func (g spendGate) Request(ctx context.Context, req agent.SpendRequest) (agent.SpendDecision, error) {
	detail := map[string]string{}
	if req.RunID != "" {
		detail["runId"] = req.RunID
	}
	if req.ToolName != "" {
		detail["toolName"] = req.ToolName
	}

	// Detail is the run and the tool, and nothing else. Whoever has to approve this sees
	// it, and it is stored on the approval row — so it must never carry a brief, a
	// model's words or a tool's arguments.
	decision, err := g.engine.RequestSpend(ctx, goalengine.SpendRequest{
		ActionType:     req.ActionType,
		Amount:         req.Amount,
		Currency:       req.Currency,
		IdempotencyKey: req.IdempotencyKey,
		Detail:         detail,
	})
	if err != nil {
		return agent.SpendDecision{}, err
	}
	return agent.SpendDecision{Outcome: decision.Outcome, Reason: decision.Reason}, nil
}
