package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/core"
	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/metrics"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

const (
	// defaultBatchSize bounds how many goals one tick examines. A tick that tried
	// to cover an unbounded number of goals would hold its database connections
	// for an unbounded time.
	defaultBatchSize = 100

	// maxTickErrors bounds the errors carried back from one tick. Every error is
	// logged regardless; this only caps what the caller has to hold in memory.
	maxTickErrors = 20

	// dispatchKeyPrefix namespaces this service's idempotency keys inside Wingman
	// core, whose own task keys may come from elsewhere.
	dispatchKeyPrefix = "wingman:eval:"

	// defaultMaxSampleAge is how old the last reported value of a push metric may
	// be before a goal measured on it stops being evaluated.
	//
	// The default assumes a daily feed and allows a missed run: 26 hours is one day
	// plus enough slack that a job which ran late does not read as a dead one.
	defaultMaxSampleAge = 26 * time.Hour
)

// TickResult summarises one monitor pass. It is returned rather than only logged
// so a health endpoint and the worker loop can both read it.
type TickResult struct {
	StartedAt time.Time
	Duration  time.Duration
	// Halted is true when the kill switch stopped the tick before any goal was
	// examined.
	Halted    bool
	Checked   int
	Triggered int
	Settled   int
	Failed    int
	// Decisions counts each decision the tick produced, for observability.
	Decisions map[domain.Decision]int
	Errors    []string
}

// MonitorDeps is everything the monitor needs. Storage and outbound calls are
// required; the rest have sane defaults.
type MonitorDeps struct {
	Goals       GoalStore
	Samples     SampleStore
	Evaluations EvaluationStore
	Dispatches  DispatchStore
	Flags       FlagStore
	Audit       AuditSink
	Metrics     MetricLookup
	Sampler     Sampler
	Tasks       TaskCreator

	// LatestSample reads back the last reported value of a metric. It is required
	// because a push metric cannot be pulled: for those the monitor reads what was
	// reported instead of asking the sampler.
	LatestSample SampleReader

	// DefaultBotID and DefaultChannelID are used for goals that name no bot of
	// their own, so one operator-level default keeps a goal from being created in
	// a state where it can never be acted on.
	DefaultBotID     string
	DefaultChannelID string

	// Location renders times in briefs and is the operator's timezone. Defaults
	// to UTC.
	Location *time.Location
	// Clock defaults to time.Now.
	Clock Clock
	// BatchSize defaults to defaultBatchSize.
	BatchSize int
	// MaxSampleAge defaults to defaultMaxSampleAge. It only bounds push metrics;
	// a pulled one is read fresh every tick.
	MaxSampleAge time.Duration
	Logger       zerolog.Logger
}

// Monitor is the goal-driven autonomy loop: it observes metrics, applies the pace
// rules, and asks Wingman core to wake an agent when a goal falls behind.
type Monitor struct {
	goals            GoalStore
	samples          SampleStore
	latestSample     SampleReader
	evaluations      EvaluationStore
	dispatches       DispatchStore
	flags            FlagStore
	audit            AuditSink
	metrics          MetricLookup
	sampler          Sampler
	tasks            TaskCreator
	defaultBotID     string
	defaultChannelID string
	location         *time.Location
	clock            Clock
	batchSize        int
	maxSampleAge     time.Duration
	log              zerolog.Logger
}

// NewMonitor validates its wiring and returns a ready monitor.
//
// Missing dependencies are reported at construction rather than panicking mid-
// tick: this loop runs unattended, so a wiring mistake has to fail at startup
// where somebody is watching.
func NewMonitor(deps MonitorDeps) (*Monitor, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Goals != nil, "Goals")
	require(deps.Samples != nil, "Samples")
	require(deps.LatestSample != nil, "LatestSample")
	require(deps.Evaluations != nil, "Evaluations")
	require(deps.Dispatches != nil, "Dispatches")
	require(deps.Flags != nil, "Flags")
	require(deps.Audit != nil, "Audit")
	require(deps.Metrics != nil, "Metrics")
	require(deps.Sampler != nil, "Sampler")
	require(deps.Tasks != nil, "Tasks")
	if len(missing) > 0 {
		return nil, fmt.Errorf("monitor: missing dependencies: %v", missing)
	}

	m := &Monitor{
		goals:            deps.Goals,
		samples:          deps.Samples,
		latestSample:     deps.LatestSample,
		evaluations:      deps.Evaluations,
		dispatches:       deps.Dispatches,
		flags:            deps.Flags,
		audit:            deps.Audit,
		metrics:          deps.Metrics,
		sampler:          deps.Sampler,
		tasks:            deps.Tasks,
		defaultBotID:     deps.DefaultBotID,
		defaultChannelID: deps.DefaultChannelID,
		location:         deps.Location,
		clock:            deps.Clock,
		batchSize:        deps.BatchSize,
		maxSampleAge:     deps.MaxSampleAge,
		log:              deps.Logger,
	}
	if m.location == nil {
		m.location = time.UTC
	}
	if m.clock == nil {
		m.clock = time.Now
	}
	if m.batchSize <= 0 {
		m.batchSize = defaultBatchSize
	}
	if m.maxSampleAge <= 0 {
		m.maxSampleAge = defaultMaxSampleAge
	}
	return m, nil
}

// Tick runs one monitor pass over the goals whose period has opened.
//
// One goal's failure does not abort the pass: a broken metric query on a single
// goal must not stop every other goal from being watched. Tick-level failures —
// an unreadable kill switch, an unreadable goal list — do abort, because
// continuing would mean acting on an unknown safety state.
func (m *Monitor) Tick(ctx context.Context) (TickResult, error) {
	started := m.clock()
	result := TickResult{StartedAt: started, Decisions: map[domain.Decision]int{}}

	// An unreadable kill switch is not "off". Refusing to run is the only safe
	// reading, so this failure aborts the tick.
	engaged, err := m.flags.KillSwitchEngaged(ctx)
	if err != nil {
		result.Duration = m.clock().Sub(started)
		return result, fmt.Errorf("monitor: read kill switch: %w", err)
	}
	if engaged {
		// The kill switch outranks everything, including settling goals: it means
		// the engine is paused, not that bookkeeping continues without agents.
		result.Halted = true
		result.Duration = m.clock().Sub(started)
		m.log.Warn().Msg("kill switch engaged: monitor tick skipped")
		return result, nil
	}

	goals, err := m.goals.ListDue(ctx, started, m.batchSize)
	if err != nil {
		result.Duration = m.clock().Sub(started)
		return result, fmt.Errorf("monitor: list due goals: %w", err)
	}

	for _, goal := range goals {
		result.Checked++
		outcome, err := m.checkGoal(ctx, goal)
		if outcome.decision != "" {
			result.Decisions[outcome.decision]++
		}
		switch {
		case err != nil:
			result.Failed++
			m.addError(&result, goal.ID, err)
		case outcome.dispatched:
			result.Triggered++
		case outcome.settled:
			result.Settled++
		}

		// A cancelled context means shutdown or a deadline: stop rather than
		// hammering a database that is going away.
		if ctx.Err() != nil {
			m.addError(&result, goal.ID, ctx.Err())
			break
		}
	}

	result.Duration = m.clock().Sub(started)
	m.log.Info().
		Int("checked", result.Checked).
		Int("triggered", result.Triggered).
		Int("settled", result.Settled).
		Int("failed", result.Failed).
		Dur("duration", result.Duration).
		Msg("monitor tick complete")
	return result, nil
}

// goalOutcome is what one goal check produced. The decision and what was actually
// done are separate: a decision to trigger that was withheld must not be counted
// as an agent having been woken.
type goalOutcome struct {
	decision   domain.Decision
	dispatched bool
	settled    bool
}

// checkGoal observes one goal and acts on the decision. The decision is returned
// even alongside an error, so a trigger that was decided but failed to dispatch
// is still counted as the decision it was.
func (m *Monitor) checkGoal(ctx context.Context, record repository.GoalRecord) (goalOutcome, error) {
	goal := record.Goal
	var outcome goalOutcome

	def, ok := m.metrics.Get(goal.MetricKey)
	if !ok {
		return outcome, fmt.Errorf("goal %s references metric %q, which is not declared in the metric registry",
			goal.ID, goal.MetricKey)
	}

	seen, err := m.observe(ctx, def, goal.ID)
	if err != nil {
		return outcome, err
	}
	now := m.clock()

	baselineJustCaptured := goal.BaselineValue == nil
	if baselineJustCaptured {
		baseline, err := m.captureBaseline(ctx, goal, seen.value)
		if err != nil {
			return outcome, err
		}
		goal.BaselineValue = &baseline
	}

	history, err := m.dispatches.History(ctx, goal.ID, goal.PeriodStart)
	if err != nil {
		return outcome, fmt.Errorf("read trigger history for goal %s: %w", goal.ID, err)
	}

	evaluation := domain.Evaluate(domain.EvaluationInput{
		Goal:               goal,
		Observed:           seen.value,
		Now:                now,
		LastTriggeredAt:    history.LastTriggeredAt,
		TriggersThisPeriod: history.CountThisPeriod,
	})
	outcome.decision = evaluation.Decision

	evalRecord, err := m.evaluations.Insert(ctx, evaluation, &seen.sampleID)
	if err != nil {
		return outcome, fmt.Errorf("store evaluation for goal %s: %w", goal.ID, err)
	}

	switch evaluation.Decision {
	case domain.DecisionTrigger:
		// On the tick that captured the baseline there is exactly one observation
		// and no history, so "no progress yet" is a restatement of the baseline
		// rather than a finding. Waking an agent on that is how a backdated goal
		// produces a task nobody asked for, before any human has seen a number.
		if baselineJustCaptured {
			m.log.Info().Str("goalId", goal.ID).
				Msg("baseline captured this tick: trigger withheld until there is a second observation")
			return outcome, nil
		}
		if err := m.dispatchTrigger(ctx, goal, evaluation, evalRecord.ID); err != nil {
			return outcome, err
		}
		outcome.dispatched = true
	case domain.DecisionAchieved:
		if err := m.settle(ctx, goal, evaluation, domain.GoalStatusAchieved); err != nil {
			return outcome, err
		}
		outcome.settled = true
	case domain.DecisionMissed:
		if err := m.settle(ctx, goal, evaluation, domain.GoalStatusMissed); err != nil {
			return outcome, err
		}
		outcome.settled = true
	}

	return outcome, nil
}

// observation is the number one goal check will be judged on, and the stored row
// it came from. The row's id is carried so the evaluation can be traced back to
// the exact reading that produced it.
type observation struct {
	value    float64
	sampleID int64
}

// observe gets the current value of a metric.
//
// Which way it comes depends on what the operator declared. A SQL or HTTP metric
// is read fresh and the reading is stored. A push metric cannot be read at all —
// the number lives somewhere the engine has no route to — so the last value
// somebody reported is used, and nothing new is stored: re-inserting a reported
// value once per tick would turn one observation into a flat line of duplicates
// and make a dead feed look like a steady one.
func (m *Monitor) observe(ctx context.Context, def metrics.Definition, goalID string) (observation, error) {
	if def.Source == metrics.SourcePush {
		return m.readReported(ctx, def, goalID)
	}
	return m.pull(ctx, def, goalID)
}

// pull reads a metric the engine can reach and stores what it saw.
func (m *Monitor) pull(ctx context.Context, def metrics.Definition, goalID string) (observation, error) {
	sampleStart := m.clock()
	sample, err := m.sampler.Sample(ctx, def)
	if err != nil {
		return observation{}, fmt.Errorf("sample metric %q for goal %s: %w", def.Key, goalID, err)
	}
	now := m.clock()

	observedAt := sample.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	stored, err := m.samples.Insert(ctx, repository.SampleInput{
		MetricKey:  def.Key,
		Value:      sample.Value,
		ObservedAt: observedAt,
		Source:     string(def.Source),
		Duration:   now.Sub(sampleStart),
	})
	if err != nil {
		return observation{}, fmt.Errorf("store sample for goal %s: %w", goalID, err)
	}
	return observation{value: sample.Value, sampleID: stored.ID}, nil
}

// readReported uses the last value somebody posted for a push metric.
//
// A missing or stale value fails this goal's check rather than skipping it
// quietly. That is the whole point: if the feed behind a goal dies, the last
// number stays where it was, the goal looks exactly as on-track as it was the
// moment the feed stopped, and nothing would ever be triggered again. A tick
// error is visible in the tick result and in the logs; a silent skip is not.
func (m *Monitor) readReported(ctx context.Context, def metrics.Definition, goalID string) (observation, error) {
	stored, err := m.latestSample.Latest(ctx, def.Key)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return observation{}, fmt.Errorf("metric %q is reported rather than read, and no value has been posted yet: goal %s cannot be evaluated until one is",
				def.Key, goalID)
		}
		return observation{}, fmt.Errorf("read last reported value of %q for goal %s: %w", def.Key, goalID, err)
	}

	if age := m.clock().Sub(stored.ObservedAt); age > m.maxSampleAge {
		return observation{}, fmt.Errorf("the last reported value of %q is %s old, past the %s limit: goal %s is not evaluated on a number this stale",
			def.Key, age.Round(time.Minute), m.maxSampleAge, goalID)
	}
	return observation{value: stored.Value, sampleID: stored.ID}, nil
}

// captureBaseline stores the value the period started from.
//
// The write is conditional on the baseline still being unset, so a concurrent
// monitor cannot move the definition of progress mid-period. The returned value
// is the one this tick evaluates against.
func (m *Monitor) captureBaseline(ctx context.Context, goal domain.Goal, value float64) (float64, error) {
	if err := m.goals.SetBaseline(ctx, goal.ID, value); err != nil {
		return 0, fmt.Errorf("capture baseline for goal %s: %w", goal.ID, err)
	}
	m.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorSystem,
		ActorID:     "monitor",
		Action:      ActionBaselineCaptured,
		SubjectType: SubjectGoal,
		SubjectID:   goal.ID,
		Outcome:     "ok",
		Detail: detailJSON(map[string]any{
			"metricKey": goal.MetricKey,
			"baseline":  value,
			"product":   goal.Product,
		}),
	})
	return value, nil
}

// settle marks a goal finished and records why.
func (m *Monitor) settle(ctx context.Context, goal domain.Goal, ev domain.Evaluation, status domain.GoalStatus) error {
	if err := m.goals.UpdateStatus(ctx, goal.ID, status); err != nil {
		return fmt.Errorf("settle goal %s as %s: %w", goal.ID, status, err)
	}
	m.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorSystem,
		ActorID:     "monitor",
		Action:      ActionGoalSettled,
		SubjectType: SubjectGoal,
		SubjectID:   goal.ID,
		Outcome:     string(status),
		Detail: detailJSON(map[string]any{
			"observed": ev.ObservedValue,
			"target":   ev.TargetValue,
			"reason":   ev.Reason,
			"product":  goal.Product,
		}),
	})
	m.log.Info().Str("goalId", goal.ID).Str("status", string(status)).Msg("goal settled")
	return nil
}

// dispatchTrigger records the intent to wake an agent, then wakes it.
//
// The order matters: the dispatch row is written first, so a crash between the
// two leaves a pending row that the retry worker can find. A task sent with no
// record of it is the one outcome that would let the engine act invisibly.
func (m *Monitor) dispatchTrigger(ctx context.Context, goal domain.Goal, ev domain.Evaluation, evaluationID string) error {
	botID := goal.BotID
	if botID == "" {
		botID = m.defaultBotID
	}
	channelID := goal.ChannelID
	if channelID == "" {
		channelID = m.defaultChannelID
	}
	if botID == "" {
		return fmt.Errorf("goal %s names no bot and no default bot is configured: refusing to dispatch a task nobody will run", goal.ID)
	}

	brief := BuildBrief(goal, ev, m.location)
	request := core.TaskRequest{
		BotID:          botID,
		ChannelID:      channelID,
		Brief:          brief,
		IdempotencyKey: dispatchKeyPrefix + evaluationID,
		Metadata: map[string]string{
			"source":       "wingman-goal-engine",
			"goalId":       goal.ID,
			"evaluationId": evaluationID,
			"product":      goal.Product,
			"metricKey":    goal.MetricKey,
			"paceRatio":    strconv.FormatFloat(ev.PaceRatio, 'f', 4, 64),
		},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode task payload for goal %s: %w", goal.ID, err)
	}

	dispatch, err := m.dispatches.Create(ctx, repository.DispatchInput{
		GoalID:         goal.ID,
		EvaluationID:   evaluationID,
		BotID:          botID,
		ChannelID:      channelID,
		IdempotencyKey: request.IdempotencyKey,
		Brief:          brief,
		RequestPayload: string(payload),
	})
	if errors.Is(err, repository.ErrConflict) {
		// This evaluation was already dispatched. Duplicating it would double the
		// work the agent does, so the conflict is the answer, not a problem.
		m.log.Info().Str("goalId", goal.ID).Str("evaluationId", evaluationID).
			Msg("trigger already dispatched for this evaluation")
		return nil
	}
	if err != nil {
		return fmt.Errorf("record dispatch for goal %s: %w", goal.ID, err)
	}

	response, err := m.tasks.CreateTask(ctx, request)
	if err != nil {
		m.failDispatch(ctx, dispatch.ID, goal, evaluationID, err)
		return fmt.Errorf("dispatch trigger for goal %s: %w", goal.ID, err)
	}

	if err := m.dispatches.MarkSent(ctx, dispatch.ID, response.StatusCode, response.TaskID); err != nil {
		return fmt.Errorf("mark dispatch %s sent for goal %s: %w", dispatch.ID, goal.ID, err)
	}
	m.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorSystem,
		ActorID:     "monitor",
		Action:      ActionTriggerDispatch,
		SubjectType: SubjectGoal,
		SubjectID:   goal.ID,
		Outcome:     "sent",
		Detail: detailJSON(map[string]any{
			"dispatchId":     dispatch.ID,
			"evaluationId":   evaluationID,
			"botId":          botID,
			"channelId":      channelID,
			"externalTaskId": response.TaskID,
			"dryRun":         response.DryRun,
			"reason":         ev.Reason,
		}),
	})
	m.log.Info().
		Str("goalId", goal.ID).
		Str("dispatchId", dispatch.ID).
		Str("externalTaskId", response.TaskID).
		Bool("dryRun", response.DryRun).
		Msg("agent task dispatched")
	return nil
}

// failDispatch records a failed hand-off. The dispatch row and the audit log are
// both updated on a best-effort basis: the caller is already returning an error,
// and losing the reason is worse than logging twice.
func (m *Monitor) failDispatch(ctx context.Context, dispatchID string, goal domain.Goal, evaluationID string, cause error) {
	var status *int
	var coreErr *core.Error
	if errors.As(cause, &coreErr) && coreErr.StatusCode > 0 {
		code := coreErr.StatusCode
		status = &code
	}

	if err := m.dispatches.MarkFailed(ctx, dispatchID, status, cause.Error()); err != nil {
		m.log.Error().Err(err).Str("dispatchId", dispatchID).Msg("could not mark dispatch failed")
	}
	m.appendAudit(ctx, repository.AuditEvent{
		ActorType:   repository.ActorSystem,
		ActorID:     "monitor",
		Action:      ActionTriggerFailed,
		SubjectType: SubjectGoal,
		SubjectID:   goal.ID,
		Outcome:     "failed",
		Detail: detailJSON(map[string]any{
			"dispatchId":   dispatchID,
			"evaluationId": evaluationID,
			"error":        cause.Error(),
			"retryable":    core.IsRetryable(cause),
		}),
	})
}

// appendAudit writes one audit entry, logging loudly if it cannot.
//
// A failed audit write never fails the caller: by the time it runs, the action it
// describes has already happened, and returning an error would only make the
// caller retry something that must not be repeated.
func (m *Monitor) appendAudit(ctx context.Context, event repository.AuditEvent) {
	if event.At.IsZero() {
		event.At = m.clock()
	}
	if _, err := m.audit.Append(ctx, event); err != nil {
		m.log.Error().Err(err).
			Str("action", event.Action).
			Str("subjectId", event.SubjectID).
			Msg("could not append audit event")
	}
}

// addError logs a per-goal failure and keeps a bounded copy on the result.
func (m *Monitor) addError(result *TickResult, goalID string, err error) {
	m.log.Error().Err(err).Str("goalId", goalID).Msg("goal check failed")
	if len(result.Errors) < maxTickErrors {
		result.Errors = append(result.Errors, fmt.Sprintf("goal %s: %v", goalID, err))
	}
}
