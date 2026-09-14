// Command goal-engine runs the Wingman goal engine: the service that watches
// business metrics, decides when a goal has fallen behind, and asks Wingman core
// to wake an agent about it.
//
// Startup order here is deliberate. Configuration and the two operator-owned
// config files are read first, because a typo in a metric query should fail before
// any connection is opened. Then the databases, then the samplers, then the
// services, and only then the listener — so a process that is accepting requests
// is a process that can answer them.
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

	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	// The zone database is compiled in so TIMEZONE=Asia/Jakarta resolves inside a
	// scratch container, which carries no /usr/share/zoneinfo. Without it the
	// service would fall back to UTC and every rendered timestamp would be wrong by
	// seven hours.
	_ "time/tzdata"

	"github.com/ribdsp/wingman/goal-engine/internal/config"
	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/database"
	"github.com/ribdsp/wingman/goal-engine/internal/handler"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/policy"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
	"github.com/ribdsp/wingman/goal-engine/internal/service"
	"github.com/ribdsp/wingman/goal-engine/internal/utils"
)

const (
	// expirySweepInterval is how often pending spend requests past their TTL are
	// closed. It runs regardless of MONITOR_ENABLED: an expired request left open is
	// a governance state, not a monitoring one, and an operator who stopped the
	// monitor still needs the queue to reflect reality.
	expirySweepInterval = 5 * time.Minute

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
	// defaultHealthcheckPort mirrors the config package's default port. The probe
	// deliberately does not load the configuration — it should work in a container
	// whose database credentials have just been rotated out from under it.
	defaultHealthcheckPort = "8080"
)

// version is the build identity of this binary, stamped by the Makefile and the
// Dockerfile with -ldflags. A plain `go build` leaves it "dev".
//
// It is logged at startup because the audit trail this service keeps is only as
// useful as the answer to "which build made that decision".
var version = "dev"

func main() {
	// `goal-engine healthcheck` exists because the runtime image has no shell and no
	// curl for a HEALTHCHECK to call. It probes /readyz over the loopback and turns
	// the answer into an exit code, which is all a container runtime reads.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintf(os.Stderr, "goal-engine: healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet when configuration is what failed, so this one
		// line goes to stderr directly.
		fmt.Fprintf(os.Stderr, "goal-engine: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	// Every timestamp the API renders goes through this, so it is set before
	// anything can answer a request.
	utils.SetLocation(cfg.Location)

	// A signal cancels this context, which is what stops the workers and starts the
	// graceful shutdown below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metricRegistry, err := metrics.Load(cfg.MetricsConfigPath)
	if err != nil {
		return err
	}
	policyRegistry, err := policy.Load(cfg.PoliciesConfigPath)
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

	// Read pools for sql metrics. They point at other products' databases, so they
	// are opened separately from this service's own and closed on the way out.
	pools, closePools, err := openDatasourcePools(ctx, metricRegistry, log)
	if err != nil {
		return err
	}
	defer closePools()

	var sqlSampler metrics.Sampler
	if len(pools) > 0 {
		sqlSampler = metrics.NewSQLSampler(pools, cfg.Monitor.SampleTimeout)
	}
	sampler := metrics.NewMuxSampler(sqlSampler, metrics.NewHTTPSampler(nil, cfg.Monitor.SampleTimeout))

	coreClient, err := core.New(cfg.Core.BaseURL, cfg.Core.APIKey,
		core.WithTimeout(cfg.Core.Timeout),
		core.WithTaskPath(cfg.Core.TaskPath),
		core.WithNotifyPath(cfg.Core.NotifyPath),
		core.WithDryRun(cfg.Core.DryRun),
	)
	if err != nil {
		return err
	}

	// Notifications are off unless an operator asked for them, and off is a nil
	// *Notices rather than a flag the services have to consult. That is the whole
	// reason the type tolerates a nil receiver: neither the monitor nor the approval
	// gate carries a question about messaging into a decision.
	var notices *service.Notices
	if cfg.Notify.Enabled {
		notices = service.NewNotices(service.NoticesDeps{
			Notifier:       coreClient,
			ConsoleBaseURL: cfg.Notify.ConsoleBaseURL,
			Logger:         log,
		})
	}

	goalRepo := repository.NewGoalRepository(db)
	sampleRepo := repository.NewSampleRepository(db)
	evaluationRepo := repository.NewEvaluationRepository(db)
	dispatchRepo := repository.NewDispatchRepository(db)
	approvalRepo := repository.NewApprovalRepository(db)
	spendRepo := repository.NewSpendRepository(db)
	flagRepo := repository.NewFlagRepository(db)
	auditRepo := repository.NewAuditRepository(db)

	goals, err := service.NewGoals(service.GoalsDeps{
		Goals:   goalRepo,
		Metrics: metricRegistry,
		Audit:   auditRepo,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	approvals, err := service.NewApprovals(service.ApprovalsDeps{
		Approvals: approvalRepo,
		Spend:     spendRepo,
		Policies:  policyRegistry,
		Flags:     flagRepo,
		Audit:     auditRepo,
		TTL:       cfg.ApprovalTTL,
		Notices:   notices,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	flags, err := service.NewFlags(service.FlagsDeps{
		Flags:  flagRepo,
		Audit:  auditRepo,
		Logger: log,
	})
	if err != nil {
		return err
	}
	auditLog, err := service.NewAuditLog(service.AuditLogDeps{Reader: auditRepo})
	if err != nil {
		return err
	}
	// Read-only over the same rows the monitor writes. It is given the evaluation
	// repository as a reader and the goal repository as a lookup, so nothing serving
	// these routes can insert a verdict or move a target.
	evaluations, err := service.NewEvaluations(service.EvaluationsDeps{
		Reader: evaluationRepo,
		Goals:  goalRepo,
	})
	if err != nil {
		return err
	}
	samples, err := service.NewSamples(service.SamplesDeps{
		Samples: sampleRepo,
		Reader:  sampleRepo,
		Metrics: metricRegistry,
		Audit:   auditRepo,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	monitor, err := service.NewMonitor(service.MonitorDeps{
		Goals:            goalRepo,
		Samples:          sampleRepo,
		LatestSample:     sampleRepo,
		Evaluations:      evaluationRepo,
		Dispatches:       dispatchRepo,
		Flags:            flagRepo,
		Audit:            auditRepo,
		Metrics:          metricRegistry,
		Sampler:          sampler,
		Tasks:            coreClient,
		Notices:          notices,
		DefaultBotID:     cfg.Core.DefaultBotID,
		DefaultChannelID: cfg.Core.DefaultChannelID,
		Location:         cfg.Location,
		BatchSize:        cfg.Monitor.MaxGoalsPerTick,
		MaxSampleAge:     cfg.Monitor.MaxSampleAge,
		Logger:           log,
	})
	if err != nil {
		return err
	}

	h, err := handler.New(handler.Deps{
		Goals:       goals,
		Approvals:   approvals,
		Flags:       flags,
		Audit:       auditLog,
		Evaluations: evaluations,
		Monitor:     monitor,
		Samples:     samples,
		Metrics:     metricRegistry,
		Logger:      log,
	})
	if err != nil {
		return err
	}
	router, err := handler.NewRouter(handler.RouterDeps{
		Handler:        h,
		Credentials:    handler.CredentialsFrom(cfg.APIKeys),
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
	workers.Add(1)
	go func() {
		defer workers.Done()
		runExpirySweeps(ctx, approvals, log)
	}()
	if cfg.Monitor.Enabled {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runMonitorLoop(ctx, monitor, cfg.Monitor.Interval, log)
		}()
	} else {
		log.Warn().Msg("the monitor loop is disabled: goals are only evaluated when POST /v1/monitor/tick is called")
	}

	logStartup(log, cfg, metricRegistry, policyRegistry)

	// A listen failure has to reach the shutdown path below rather than kill the
	// process from inside a goroutine, or the pools would never be closed.
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
	// before closing the pools is what keeps a tick in flight from reading a closed
	// connection.
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

// healthcheck probes this service's own readiness endpoint and reports the result
// as an error, which main turns into an exit code.
//
// It reads only PORT: the point of a healthcheck is to answer while the service is
// unwell, so it must not depend on the same configuration that may be what is
// wrong. /readyz is used rather than /healthz because it exercises the database
// too — a process that is alive but cannot read its own kill switch is not doing
// its job.
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
// Production logs JSON, because that is what a log shipper can index. Development
// logs to a console writer, because that is what a human can read. The level falls
// back to info rather than failing: a bad LOG_LEVEL should not stop a service that
// is otherwise configured correctly, but it should say so.
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
	log = log.Level(level).With().Timestamp().Str("service", "goal-engine").Logger()

	if err != nil {
		log.Warn().Str("configured", cfg.LogLevel).Msg("unrecognised LOG_LEVEL, using info")
	}
	return log
}

// openDatasourcePools opens one read pool per declared datasource.
//
// The returned closer runs even on a partial failure, so a bad DSN in the third
// datasource does not leak the first two. A datasource that cannot be reached is
// fatal: its metrics would fail on every tick, and a goal that is never evaluated
// looks exactly like a goal that is on track.
func openDatasourcePools(ctx context.Context, registry *metrics.Registry, log zerolog.Logger) (map[string]metrics.Querier, func(), error) {
	opened := map[string]*sqlx.DB{}
	closeAll := func() {
		for name, pool := range opened {
			if err := pool.Close(); err != nil {
				log.Warn().Err(err).Str("datasource", name).Msg("closing a metric datasource")
			}
		}
	}

	for _, ds := range registry.Datasources() {
		// The DSN carries a password, so only the name is ever logged.
		pool, err := database.Connect(ctx, ds.DSN(), database.WithMaxOpenConns(ds.MaxOpenConns))
		if err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("metric datasource %q: %w", ds.Name, err)
		}
		opened[ds.Name] = pool
		log.Info().Str("datasource", ds.Name).Int("maxOpenConns", ds.MaxOpenConns).Msg("metric datasource connected")
	}

	pools := make(map[string]metrics.Querier, len(opened))
	for name, pool := range opened {
		// The embedded *sql.DB is passed rather than the sqlx wrapper: the sampler only
		// ever needs a transaction, and handing it less is one fewer way for a metric
		// query to become something else.
		pools[name] = pool.DB
	}
	return pools, closeAll, nil
}

// runMonitorLoop evaluates every active goal on a timer.
//
// The first tick waits one full interval. A crash-looping process would otherwise
// re-sample every metric on each boot, and an operator who wants to see a new goal
// evaluated now has POST /v1/monitor/tick for exactly that.
func runMonitorLoop(ctx context.Context, monitor *service.Monitor, interval time.Duration, log zerolog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("monitor loop stopped")
			return
		case <-ticker.C:
			// A tick may not outlive the gap to the next one: two overlapping ticks would
			// evaluate the same goals against the same cooldown at the same time.
			tickCtx, cancel := context.WithTimeout(ctx, interval)
			result, err := monitor.Tick(tickCtx)
			cancel()
			if err != nil {
				log.Error().Err(err).Msg("monitor tick failed")
				continue
			}
			event := log.Info()
			if result.Failed > 0 {
				event = log.Warn()
			}
			event.
				Bool("halted", result.Halted).
				Int("checked", result.Checked).
				Int("triggered", result.Triggered).
				Int("settled", result.Settled).
				Int("failed", result.Failed).
				Dur("took", result.Duration).
				Msg("monitor tick")
		}
	}
}

// runExpirySweeps closes pending spend requests nobody answered in time.
//
// It sweeps once at startup: a request that expired while the process was down is
// still expired, and leaving it open would let an agent's stale ask be approved
// hours after the moment it was about.
func runExpirySweeps(ctx context.Context, approvals *service.Approvals, log zerolog.Logger) {
	sweep := func() {
		expired, err := approvals.ExpireOverdue(ctx)
		if err != nil {
			log.Error().Err(err).Msg("expiring overdue approvals failed")
			return
		}
		if expired > 0 {
			log.Info().Int("expired", expired).Msg("overdue approvals closed")
		}
	}

	sweep()
	ticker := time.NewTicker(expirySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("approval expiry sweep stopped")
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// logStartup records what this process will and will not do, in one line an
// operator can read after a deploy.
//
// Credentials are counted, never printed — including how many of them can release
// the kill switch, which is the number worth noticing if it is larger than
// expected.
func logStartup(log zerolog.Logger, cfg config.Config, metricRegistry *metrics.Registry, policyRegistry *policy.Registry) {
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
		Int("metrics", metricRegistry.Len()).
		Int("policies", policyRegistry.Len()).
		Int("operatorKeys", operators).
		Int("botKeys", len(cfg.APIKeys)-operators).
		Bool("monitorEnabled", cfg.Monitor.Enabled).
		Dur("monitorInterval", cfg.Monitor.Interval).
		Bool("triggerDryRun", cfg.Core.DryRun).
		Bool("notifyEnabled", cfg.Notify.Enabled).
		Msg("goal engine listening")

	if cfg.Core.DryRun {
		log.Warn().Msg("TRIGGER_DRY_RUN is on: goals are evaluated and recorded, but no task reaches Wingman core")
	}
	// Notifications are best-effort by design, so the one thing worth saying at
	// startup is when they are on but cannot carry a link — a message with nothing to
	// open is a message somebody has to go and find the console for.
	if cfg.Notify.Enabled && cfg.Notify.ConsoleBaseURL == "" {
		log.Warn().Msg("NOTIFY_ENABLED is on with no WEB_BASE_URL: notifications will go out without a link to the console")
	}
	if len(cfg.TrustedProxies) == 0 {
		log.Info().Msg("no trusted proxies configured: the rate limiter keys on the peer address, which is correct only when nothing sits in front of this service")
	}
}
