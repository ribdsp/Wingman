// Command core runs Wingman core: the service that holds accounts, receives work —
// from a person in a chat or from the goal engine's trigger — and drives an agent
// loop that is bounded by budgets, tool grants and a sandbox.
//
// Startup order here is deliberate, and it is the same order the goal engine uses.
// Configuration and the three operator-owned tool files are read first, because a
// typo in a tool grant should fail before any connection is opened or any credential
// is used. Then the database, then the sandbox, then the tools, the provider and the
// agent loop, then the services, and only then the listener — so a process that is
// accepting requests is a process that can answer them.
//
// This package is also the only place two ports declared in the same shape are joined
// (internal/sandbox and internal/tool each declare their own Sandbox so neither
// imports the other), and the only place a repository is adapted to a loop's port.
// Both live in wiring.go: the wiring belongs where the wiring is.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	// The zone database is compiled in so TIMEZONE=Asia/Jakarta resolves inside a
	// scratch container, which carries no /usr/share/zoneinfo. Without it the service
	// would fall back to UTC and every rendered timestamp — and every daily token cap
	// boundary — would be wrong by seven hours.
	_ "time/tzdata"

	"github.com/ribdsp/wingman/core/internal/agent"
	"github.com/ribdsp/wingman/core/internal/channel"
	"github.com/ribdsp/wingman/core/internal/config"
	"github.com/ribdsp/wingman/core/internal/database"
	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/goalengine"
	"github.com/ribdsp/wingman/core/internal/handler"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/repository"
	"github.com/ribdsp/wingman/core/internal/sandbox"
	"github.com/ribdsp/wingman/core/internal/service"
	"github.com/ribdsp/wingman/core/internal/tool"
	"github.com/ribdsp/wingman/core/internal/utils"
)

const (
	// sweepInterval is how often the crash sweeps and the session sweep run. The
	// sweeps themselves are bounded by staleAfter, so this only decides how quickly a
	// crashed worker's task is noticed, not what counts as crashed.
	sweepInterval = 5 * time.Minute

	// serverReadHeaderTimeout bounds how long a client may take to send its headers,
	// which is what makes a slowloris expensive for the client rather than for this
	// process.
	serverReadHeaderTimeout = 10 * time.Second
	serverReadTimeout       = 30 * time.Second
	serverWriteTimeout      = 60 * time.Second
	serverIdleTimeout       = 120 * time.Second
	// serverMaxHeaderBytes is well above any legitimate request to this API.
	serverMaxHeaderBytes = 1 << 20

	// healthcheckTimeout bounds the probe a container runtime runs. It is short on
	// purpose: a readiness answer that takes longer than this is already a failure.
	healthcheckTimeout = 3 * time.Second
	// defaultHealthcheckPort mirrors the config package's default port — 8081, because
	// the goal engine has 8080 and the two run side by side. The probe deliberately
	// does not load the configuration: it should work in a container whose database
	// credentials have just been rotated out from under it.
	defaultHealthcheckPort = "8081"

	// staleAfterHeadroom multiplies the longest a run could legitimately take before
	// the sweep is willing to call its worker dead. Two, because the bound below counts
	// model and sandbox time but not the pauses between retries, and because the cost
	// of being wrong in this direction is a task requeued late while the cost of being
	// wrong in the other is a live run's work thrown away and paid for twice.
	staleAfterHeadroom = 2
)

// version is the build identity of this binary, stamped by the Dockerfile with
// -ldflags. A plain `go build` leaves it "dev".
//
// It is logged at startup and sent as the client version to every MCP server and HTTP
// tool, because "which build made that tool call" is a question an operator reading a
// transcript will eventually ask.
var version = "dev"

func main() {
	// Subcommands are matched before anything is loaded, so that a probe or a
	// first-account creation does not pay for — or fail on — wiring it does not use.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			// This exists because the runtime image has no shell and no curl for a
			// HEALTHCHECK to call. It probes /readyz over the loopback and turns the answer
			// into an exit code, which is all a container runtime reads.
			if err := healthcheck(); err != nil {
				fmt.Fprintf(os.Stderr, "core: healthcheck: %v\n", err)
				os.Exit(1)
			}
			return
		case "createuser":
			if err := createUser(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "core: createuser: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	if err := run(); err != nil {
		// The logger may not exist yet when configuration is what failed, so this one
		// line goes to stderr directly.
		fmt.Fprintf(os.Stderr, "core: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	// Every timestamp the API renders goes through this, so it is set before anything
	// can answer a request.
	utils.SetLocation(cfg.Location)

	// A signal cancels this context, which is what stops the workers and starts the
	// graceful shutdown below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The operator's three files, before any connection: a tool list that does not
	// parse is a tool list nobody meant to run with.
	grants, err := tool.LoadGrants(cfg.Tools.GrantsPath)
	if err != nil {
		return err
	}
	mcpServers, err := tool.LoadMCPServers(cfg.Tools.MCPPath)
	if err != nil {
		return err
	}
	httpServices, err := tool.LoadHTTPServices(cfg.Tools.HTTPPath)
	if err != nil {
		return err
	}

	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn().Err(err).Msg("closing the database")
		}
	}()

	if cfg.AutoMigrate {
		if err := database.Migrate(db.DB, cfg.MigrationsDir); err != nil {
			return err
		}
		log.Info().Str("dir", cfg.MigrationsDir).Msg("migrations applied")
	}

	users := repository.NewUserRepository(db)
	sessions := repository.NewSessionRepository(db)
	chats := repository.NewChatRepository(db)
	tasks := repository.NewTaskRepository(db)
	runs := repository.NewRunRepository(db)
	links := repository.NewChannelRepository(db)
	linkCodes := repository.NewLinkCodeRepository(db)
	// The location decides where a day starts, and the daily token cap is a per-day
	// number, so the ledger and the rendered timestamps have to agree about it.
	spend := repository.NewSpendRepository(db, cfg.Location)

	// One Workspaces for both: the sandbox executes inside a workspace and the runner
	// resolves which one a user gets, and the two disagreeing about the root would be a
	// run writing where nothing checks the path.
	workspaces, err := sandbox.NewWorkspaces(cfg.Sandbox.WorkspaceRoot)
	if err != nil {
		return err
	}
	box, err := newSandbox(cfg.Sandbox, workspaces)
	if err != nil {
		return err
	}

	registry, closeTools := newToolRegistry(grants, box, mcpServers, httpServices, log)
	defer closeTools()

	model, err := provider.New(cfg.Providers.Default, provider.Options{APIKey: providerKey(cfg.Providers)})
	if err != nil {
		return err
	}

	// Two things come from the goal engine and they are not the same absence. With no
	// base URL configured there is no kill switch to read — Absent answers "not
	// engaged" — and no gate, which the loop reads as "nothing may spend money". With
	// one configured, an unreadable switch means engaged.
	var (
		halt     agent.Halt = goalengine.Absent{}
		gate     agent.SpendGate
		reporter service.SpendReporter
	)
	if cfg.GoalEngine.Configured() {
		engine, err := goalengine.New(cfg.GoalEngine.BaseURL, cfg.GoalEngine.APIKey,
			goalengine.WithTimeout(cfg.GoalEngine.Timeout),
			goalengine.WithSpendMetric(cfg.GoalEngine.SpendMetric),
		)
		if err != nil {
			return err
		}
		halt = engine
		gate = spendGate{engine: engine}
		if cfg.GoalEngine.ReportSpend {
			reporter = engine
		}
	}

	loop, err := agent.New(agent.Config{
		Provider:   model,
		Model:      cfg.Providers.DefaultModel,
		Transcript: runs,
		Runs:       cancellableRuns{RunRepository: runs},
		Budget:     ledger{SpendRepository: spend},
		Halt:       halt,
		Gate:       gate,
		Grants:     registry.Grants(),
		Logger:     log,
	})
	if err != nil {
		return err
	}

	accounts, err := service.NewAccounts(service.AccountsDeps{
		Users:            users,
		Passwords:        users,
		Admin:            users,
		Sessions:         sessions,
		Revoker:          sessions,
		OpenRegistration: cfg.Auth.OpenRegistration,
		SessionTTL:       cfg.Auth.SessionTTL,
		Logger:           log,
	})
	if err != nil {
		return err
	}

	// Resolved once, here, rather than per dispatch: an unattended task with no owner
	// has no budget to spend against and no ledger to appear in, and discovering that
	// at three in the morning when a trigger fires is discovering it too late.
	var unattendedOwner string
	if cfg.UnattendedOwner != "" {
		owner, err := accounts.ResolveOwner(ctx, cfg.UnattendedOwner)
		if err != nil {
			return fmt.Errorf("CORE_UNATTENDED_OWNER: %w", err)
		}
		unattendedOwner = owner.ID
	}

	sessionService, err := service.NewSessions(service.SessionsDeps{
		Sessions: sessions,
		Revoker:  sessions,
		Janitor:  sessions,
		Logger:   log,
	})
	if err != nil {
		return err
	}
	chatService, err := service.NewChats(service.ChatsDeps{Chats: chats, Messages: chats, Logger: log})
	if err != nil {
		return err
	}
	// Built whether or not a platform is connected. Minting a code, listing connections and
	// disconnecting one are routes a signed-in person has either way — and somebody who
	// connected Telegram last month still has to be able to disconnect it after the token
	// was removed from the environment.
	channelService, err := service.NewChannels(service.ChannelsDeps{
		Codes:   linkCodes,
		Links:   links,
		Janitor: linkCodes,
		CodeTTL: cfg.Channels.LinkCodeTTL,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	taskService, err := service.NewTasks(service.TasksDeps{
		Tasks:           tasks,
		Reader:          tasks,
		Chats:           chats,
		Messages:        chats,
		ChannelChats:    chats,
		UnattendedOwner: unattendedOwner,
		Logger:          log,
	})
	if err != nil {
		return err
	}
	runService, err := service.NewRuns(service.RunsDeps{
		Runs:            runs,
		Auditor:         runs,
		Tasks:           tasks,
		Canceller:       runs,
		Spend:           spend,
		UnattendedOwner: unattendedOwner,
		Logger:          log,
	})
	if err != nil {
		return err
	}
	// The chat platforms. All of this is absent on an instance reached only over HTTP, which
	// is the default: no inbound path, no connections, and a runner with nowhere to deliver
	// an answer except the chat row itself.
	//
	// The inbox reads the same kill switch the agent loop does. With no goal engine
	// configured that is Absent, which answers "not engaged" — so channels work on a
	// standalone instance. With one configured, an unreadable switch means engaged, and an
	// accepted message is refused rather than queued.
	var (
		hub     *channel.Hub
		replier service.ChannelReplier
	)
	if cfg.Channels.Configured() {
		inbox, err := service.NewInbox(service.InboxDeps{
			Identities:  links,
			Linker:      links,
			Codes:       linkCodes,
			Work:        taskService,
			Halt:        halt,
			AllowGroups: cfg.Channels.AllowGroups,
			MinInterval: cfg.Channels.MinInterval,
			Logger:      log,
		})
		if err != nil {
			return err
		}

		connected, err := newChannelHub(cfg.Channels, inbox, log)
		if err != nil {
			return err
		}
		hub = connected
		defer func() {
			// Registered after the tool sources and the pool, so it runs before them — and
			// after workers.Wait(), which is what keeps a run's last delivery from going into
			// a connection this has already closed.
			if err := hub.Close(); err != nil {
				log.Warn().Err(err).Msg("closing the channel connections")
			}
		}()

		deliverer, err := channel.NewReplier(channelTargets{ChatRepository: chats}, hub, log)
		if err != nil {
			return err
		}
		replier = deliverer
	}

	runner, err := service.NewRunner(service.RunnerDeps{
		Queue:       tasks,
		Runs:        runs,
		Agent:       loop,
		Tools:       registry,
		Workspaces:  workspaces,
		Messages:    chats,
		Spend:       spend,
		Reporter:    reporter,
		TaskJanitor: tasks,
		RunJanitor:  runs,
		Replier:     replier,
		Provider:    model.Name(),
		Model:       cfg.Providers.DefaultModel,
		Limits:      cfg.Limits,
		StaleAfter:  staleAfter(cfg.Limits),
		Logger:      log,
	})
	if err != nil {
		return err
	}

	// Whoever the goal engine has something to tell is the unattended owner, on whichever
	// chat accounts they linked. Built after the hub, because the hub is where a message
	// leaves — and built whether or not one exists, since the route answers "nobody to tell"
	// rather than 404 on an instance with no platform connected.
	notifications, err := service.NewNotifications(service.NotificationsDeps{
		Directory: links,
		Sender:    directSender(hub),
		OwnerID:   unattendedOwner,
		Logger:    log,
	})
	if err != nil {
		return err
	}

	h, err := handler.New(handler.Deps{
		Accounts:      accounts,
		Sessions:      sessionService,
		Chats:         chatService,
		Tasks:         taskService,
		Runs:          runService,
		Channels:      channelService,
		Notifications: notifications,
		// What is connected, not what is configured. /v1/reference publishes it so a client
		// does not offer to connect a platform on which a minted code could never arrive.
		ChannelsConnected: connectedKinds(hub),
		Ready:             readiness(db),
		Logger:            log,
	})
	if err != nil {
		return err
	}
	router, err := handler.NewRouter(handler.RouterDeps{
		Handler:        h,
		Credentials:    handler.CredentialsFrom(cfg.APIKeys),
		Sessions:       newSessionStore(sessions, log),
		RateLimit:      cfg.RateLimit,
		TrustedProxies: cfg.TrustedProxies,
		Logger:         log,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaderBytes,
	}

	var workers sync.WaitGroup
	for worker := 1; worker <= cfg.Runner.Workers; worker++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			runQueueWorker(ctx, runner, cfg.Runner.QueuePoll, log.With().Int("worker", id).Logger())
		}(worker)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		runSweeps(ctx, runner, sessionService, channelService, log)
	}()
	if hub != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// Blocks until the context is cancelled. Waiting for it here, with the queue
			// workers, is what makes a signal stop receiving messages before the connections
			// are closed — rather than a message arriving into a process on its way out.
			hub.Run(ctx)
		}()
	}

	logStartup(log, cfg, registry, unattendedOwner, connectedKinds(hub))

	// A listen failure has to reach the shutdown path below rather than kill the
	// process from inside a goroutine, or the database pool and the MCP sessions would
	// never be closed.
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		stop()
		workers.Wait()
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info().Dur("grace", cfg.ShutdownGrace).Msg("shutting down")
	}

	// The workers watch the same context and stop on their own; waiting for them
	// before the deferred closers run is what keeps a run in flight from writing its
	// last transcript row to a closed pool.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	workers.Wait()
	if shutdownErr != nil {
		return fmt.Errorf("graceful shutdown: %w", shutdownErr)
	}
	log.Info().Msg("stopped")
	return nil
}

// providerKey picks the credential for the configured default provider.
//
// Config has already refused to boot if the matching one is missing, so there is no
// error to return here — and no key to log, now or anywhere else.
func providerKey(providers config.ProvidersConfig) string {
	if providers.Default == config.ProviderOpenAI {
		return providers.OpenAIAPIKey
	}
	return providers.AnthropicAPIKey
}

// staleAfter is how long a run may be in flight before the sweep treats its worker as
// gone.
//
// Derived from the operator's own limits rather than configured separately, because
// the only correct answer is "longer than a legitimate run" and the limits are what
// bound a legitimate run: every iteration is at most one model call plus one sandbox
// execution. An operator who allows two hundred iterations of fifteen minutes has runs
// that take days, and a fixed timeout would have the sweep requeue them while they
// were still working. Runner raises anything below its own floor.
func staleAfter(limits domain.RunLimits) time.Duration {
	bounded := limits.WithDefaults()
	perIteration := bounded.StepTimeout + bounded.SandboxTimeout
	return staleAfterHeadroom * time.Duration(bounded.MaxIterations) * perIteration
}

// runQueueWorker takes one queued task at a time.
//
// It asks again immediately after a run rather than waiting for the next tick: a queue
// with ten tasks in it should not take ten poll intervals to drain. The pause is only
// for an empty queue.
func runQueueWorker(ctx context.Context, runner *service.Runner, poll time.Duration, log zerolog.Logger) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("queue worker stopped")
			return
		case <-timer.C:
		}

		ran, err := runner.RunNext(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Shutdown cancelled the work rather than breaking it. The run row is closed by
			// the sweep on the next boot; saying "failed" here would be reporting our own
			// shutdown as a fault.
			log.Info().Msg("queue worker stopped mid-task")
			return
		case err != nil:
			// This is the process failing, not the run: a run that hit a cap or was halted
			// comes back as ran, with its reason on the row.
			log.Error().Err(err).Msg("taking work off the queue failed")
		}

		if ran {
			// Zero rather than poll, so a backlog drains at the speed of the runs.
			timer.Reset(0)
			continue
		}
		timer.Reset(poll)
	}
}

// runSweeps closes work whose worker died, deletes expired sessions and drops spent link
// codes.
//
// It sweeps once at startup, because the most likely reason a run is abandoned is the
// crash that this process just restarted from, and a task left claimed by a worker
// that no longer exists is a task nobody will ever pick up.
func runSweeps(
	ctx context.Context,
	runner *service.Runner,
	sessions *service.Sessions,
	channels *service.Channels,
	log zerolog.Logger,
) {
	sweep := func() {
		if failed, requeued, err := runner.Sweep(ctx); err != nil {
			log.Error().Err(err).Msg("sweeping abandoned work failed")
		} else if failed > 0 || requeued > 0 {
			log.Warn().Int("runsFailed", failed).Int("tasksRequeued", requeued).
				Msg("work whose worker had gone was closed")
		}

		// Expired sessions are already refused by Resolve, so deleting them is hygiene
		// and a failure here is not a reason to stop sweeping runs.
		if deleted, err := sessions.Sweep(ctx); err != nil {
			log.Error().Err(err).Msg("deleting expired sessions failed")
		} else if deleted > 0 {
			log.Info().Int("deleted", deleted).Msg("expired sessions removed")
		}

		// The same reasoning for link codes: a spent or expired one is already refused when
		// it is redeemed, so this only keeps the table to the one live row per account it is
		// meant to hold. Swept even on an instance with no channel connected — the codes
		// were mintable before the token was removed from the environment.
		if _, err := channels.Sweep(ctx); err != nil {
			log.Error().Err(err).Msg("deleting spent link codes failed")
		}
	}

	sweep()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("sweeps stopped")
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// healthcheck probes this service's own readiness endpoint and reports the result as
// an error, which main turns into an exit code.
//
// It reads only PORT: the point of a healthcheck is to answer while the service is
// unwell, so it must not depend on the same configuration that may be what is wrong.
// /readyz is used rather than /healthz because it exercises the database too.
func healthcheck() error {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultHealthcheckPort
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz returned %s", resp.Status)
	}
	return nil
}

// newLogger builds the process logger.
//
// Production logs JSON, because that is what a log shipper can index. Development logs
// to a console writer, because that is what a human can read. The level falls back to
// info rather than failing: a bad LOG_LEVEL should not stop a service that is
// otherwise configured correctly, but it should say so.
func newLogger(cfg config.Config) zerolog.Logger {
	level, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(cfg.LogLevel)))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}

	var log zerolog.Logger
	if cfg.IsProduction() {
		log = zerolog.New(os.Stdout)
	} else {
		log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}
	log = log.Level(level).With().Timestamp().Str("service", "core").Logger()

	if err != nil {
		log.Warn().Str("configured", cfg.LogLevel).Msg("unrecognised LOG_LEVEL, using info")
	}
	return log
}

// logStartup records what this process will and will not do, in one line an operator
// can read after a deploy, followed by the warnings worth reading twice.
//
// Credentials are counted, never printed — and neither is the model, the DSN, nor the
// goal engine's key. Channels are named by platform, which is not a secret and is the
// thing an operator is checking for. The unattended owner is an email address the operator
// wrote themselves, and it is the one identifier here worth confirming: it is whose budget
// every trigger will spend.
func logStartup(
	log zerolog.Logger,
	cfg config.Config,
	registry *tool.Registry,
	unattendedOwner string,
	channels []string,
) {
	operators := 0
	for _, key := range cfg.APIKeys {
		if key.Role == config.RoleOperator {
			operators++
		}
	}

	log.Info().
		Str("version", version).
		Str("env", cfg.AppEnv).
		Int("port", cfg.Port).
		Str("timezone", cfg.Location.String()).
		Str("provider", cfg.Providers.Default).
		Int("toolGrants", registry.Grants().Len()).
		Int("operatorKeys", operators).
		Int("botKeys", len(cfg.APIKeys)-operators).
		Str("sandbox", cfg.Sandbox.Backend).
		Int("runWorkers", cfg.Runner.Workers).
		Bool("goalEngine", cfg.GoalEngine.Configured()).
		Bool("openRegistration", cfg.Auth.OpenRegistration).
		Strs("channels", channels).
		Msg("core listening")

	if !cfg.GoalEngine.Configured() {
		// Both halves of that sentence matter. Nothing halts every run at once, and
		// nothing can approve a spend, so a tool that wants money is refused rather than
		// escalated.
		log.Warn().Msg("no goal engine is configured: there is no kill switch, and no tool call may spend money")
	}
	if cfg.Auth.OpenRegistration {
		log.Warn().Msg("CORE_OPEN_REGISTRATION is on: anyone who can reach this service can create an account and run code in your sandbox on your API key")
	}
	if cfg.Sandbox.Backend == config.SandboxLocal {
		log.Warn().Msg("SANDBOX_BACKEND=local runs tool calls as this process, with this process's filesystem and network: use docker in front of anybody else")
	}
	if len(channels) > 0 && cfg.Channels.AllowGroups {
		// Both halves again. The bot still has to be addressed, but once it is, the run is
		// filed against whoever linked the chat and spends their daily allowance.
		log.Warn().Msg("CHANNEL_ALLOW_GROUPS is on: anybody who can type in a shared room can start a run on the linked person's token budget")
	}
	if unattendedOwner == "" {
		log.Info().Msg("no unattended owner is configured: nothing can dispatch work that nobody asked for")
	}
	if len(cfg.TrustedProxies) == 0 {
		log.Info().Msg("no trusted proxies configured: the rate limiter keys on the peer address, which is correct only when nothing sits in front of this service")
	}
}
