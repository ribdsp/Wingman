package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/goal-engine/internal/domain"
	"github.com/ribdsp/wingman/goal-engine/internal/repository"
)

// GoalsDeps is everything the goal registry needs. Storage, the metric registry
// and the audit log are required; the rest have sane defaults.
type GoalsDeps struct {
	Goals   GoalRegistry
	Metrics MetricLookup
	Audit   AuditSink

	// Clock defaults to time.Now. It decides what "already in the past" means for
	// a period, which is the one rule here that depends on when you ask.
	Clock  Clock
	Logger zerolog.Logger
}

// Goals is the goal registry: the operator's stated intentions, written down in a
// form the monitor can check.
//
// It deliberately does not consult the kill switch. Halting the engine stops it
// acting, not the operator configuring it — the moment after pulling the switch is
// exactly when somebody wants to fix the goal that caused the trouble.
type Goals struct {
	goals   GoalRegistry
	metrics MetricLookup
	audit   AuditSink
	clock   Clock
	log     zerolog.Logger
}

// NewGoals validates its wiring and returns a ready registry.
func NewGoals(deps GoalsDeps) (*Goals, error) {
	missing := []string{}
	require := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	require(deps.Goals != nil, "Goals")
	require(deps.Metrics != nil, "Metrics")
	require(deps.Audit != nil, "Audit")
	if len(missing) > 0 {
		return nil, fmt.Errorf("goals: missing dependencies: %v", missing)
	}

	g := &Goals{
		goals:   deps.Goals,
		metrics: deps.Metrics,
		audit:   deps.Audit,
		clock:   deps.Clock,
		log:     deps.Logger,
	}
	if g.clock == nil {
		g.clock = time.Now
	}
	return g, nil
}

// Create records a new goal.
//
// A goal that cannot be evaluated would fail on every tick forever, so everything
// that makes it evaluable is checked here rather than discovered at 3am by the
// monitor: the metric has to exist in the operator's registry, the period has to
// be a period, and the safety limits that bound how often it can wake an agent
// are filled in when the caller leaves them blank.
func (g *Goals) Create(ctx context.Context, goal domain.Goal, actor Actor) (repository.GoalRecord, error) {
	actor = actor.normalise()
	if err := actor.validate(); err != nil {
		return repository.GoalRecord{}, err
	}

	goal = goal.WithDefaults()
	if goal.PeriodStart.IsZero() {
		// A goal stated without a start date starts now: "reach 100M by October"
		// is a sentence about the time between saying it and October.
		goal.PeriodStart = g.clock()
	}

	if err := goal.Validate(); err != nil {
		return repository.GoalRecord{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	if err := g.checkMetricExists(goal.MetricKey); err != nil {
		return repository.GoalRecord{}, err
	}
	// Checked against the clock rather than in Validate: a goal whose deadline has
	// already passed is structurally fine but would be settled as missed on the
	// first tick, which is nearly always a typo rather than an intention.
	if !goal.PeriodEnd.After(g.clock()) {
		return repository.GoalRecord{}, fieldError("periodEnd", "is already in the past")
	}

	record, err := g.goals.Create(ctx, goal, actor.ID)
	if err != nil {
		return repository.GoalRecord{}, fmt.Errorf("goals: create: %w", err)
	}

	g.appendAudit(ctx, repository.AuditEvent{
		ActorType:   actor.Type,
		ActorID:     actor.ID,
		Action:      ActionGoalCreated,
		SubjectType: SubjectGoal,
		SubjectID:   record.ID,
		Outcome:     "created",
		RequestID:   actor.RequestID,
		Detail: detailJSON(map[string]any{
			"product":              record.Product,
			"title":                record.Title,
			"metricKey":            record.MetricKey,
			"comparator":           string(record.Comparator),
			"targetValue":          record.TargetValue,
			"periodStart":          record.PeriodStart,
			"periodEnd":            record.PeriodEnd,
			"toleranceRatio":       record.ToleranceRatio,
			"triggerCooldown":      record.TriggerCooldown.String(),
			"maxTriggersPerPeriod": record.MaxTriggersPerPeriod,
		}),
	})
	return record, nil
}

// Get reads one goal.
func (g *Goals) Get(ctx context.Context, id string) (repository.GoalRecord, error) {
	if strings.TrimSpace(id) == "" {
		return repository.GoalRecord{}, fieldError("id", "is required")
	}
	record, err := g.goals.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.GoalRecord{}, ErrNotFound
		}
		return repository.GoalRecord{}, fmt.Errorf("goals: get: %w", err)
	}
	return record, nil
}

// List reads a page of goals, clamping the page size so a caller cannot ask for
// the whole table.
func (g *Goals) List(ctx context.Context, filter repository.GoalFilter) ([]repository.GoalRecord, int, error) {
	if filter.Status != "" {
		switch filter.Status {
		case domain.GoalStatusActive, domain.GoalStatusPaused, domain.GoalStatusAchieved,
			domain.GoalStatusMissed, domain.GoalStatusArchived:
		default:
			return nil, 0, fieldError("status", fmt.Sprintf("%q is not a goal status", filter.Status))
		}
	}
	filter.Limit, filter.Offset = clampPage(filter.Limit, filter.Offset)

	records, total, err := g.goals.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("goals: list: %w", err)
	}
	return records, total, nil
}

// Patch changes a goal that already exists.
//
// This is the most dangerous write in the service. Lowering a target or moving a
// deadline can make a failing goal look met, so the rules are strict: only a
// human may do it at all, a settled goal is not rewritten in place, the engine's
// own verdicts cannot be assigned by hand, the result has to be a valid goal, and
// every changed field is written to the audit log with the value it had before.
//
// An agent may state a new goal — that loop is the point of Wingman — but not
// rewrite the one it is being measured against. Pausing the goal you are failing
// and lowering the target you cannot reach are the same move, so the whole
// operation is the operator's.
func (g *Goals) Patch(ctx context.Context, id string, patch repository.GoalPatch, actor Actor) (repository.GoalRecord, error) {
	actor = actor.normalise()
	if err := actor.validate(); err != nil {
		return repository.GoalRecord{}, err
	}
	if err := actor.requireOperator("changing a goal"); err != nil {
		return repository.GoalRecord{}, err
	}
	if strings.TrimSpace(id) == "" {
		return repository.GoalRecord{}, fieldError("id", "is required")
	}
	if patch.IsEmpty() {
		return repository.GoalRecord{}, fieldError("patch", "changes nothing")
	}

	current, err := g.Get(ctx, id)
	if err != nil {
		return repository.GoalRecord{}, err
	}

	if err := g.checkPatchAllowed(current, patch); err != nil {
		return repository.GoalRecord{}, err
	}

	patched, changes := applyGoalPatch(current.Goal, patch)
	if err := patched.Validate(); err != nil {
		return repository.GoalRecord{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	if patch.PeriodEnd != nil && !patched.PeriodEnd.After(g.clock()) {
		return repository.GoalRecord{}, fieldError("periodEnd", "is already in the past")
	}
	if len(changes) == 0 {
		// Every field in the patch already held the value being written. Storing it
		// would produce an audit entry recording no change, which is worse than
		// nothing: it makes the log look like the target moved when it did not.
		return current, nil
	}

	record, err := g.goals.Patch(ctx, id, patch)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return repository.GoalRecord{}, ErrNotFound
		}
		return repository.GoalRecord{}, fmt.Errorf("goals: patch: %w", err)
	}

	g.appendAudit(ctx, repository.AuditEvent{
		ActorType:   actor.Type,
		ActorID:     actor.ID,
		Action:      ActionGoalUpdated,
		SubjectType: SubjectGoal,
		SubjectID:   record.ID,
		Outcome:     "updated",
		RequestID:   actor.RequestID,
		Detail:      detailJSON(map[string]any{"changes": changes}),
	})
	return record, nil
}

// checkPatchAllowed holds the two rules about who may change what.
func (g *Goals) checkPatchAllowed(current repository.GoalRecord, patch repository.GoalPatch) error {
	// achieved and missed are the monitor's verdicts, reached from recorded
	// observations. Letting the API assign them would let anyone declare a goal met
	// without the evidence, which empties the record of meaning. archived is the
	// operator's way to close a goal out, and paused/active are theirs to set.
	if patch.Status != nil {
		switch *patch.Status {
		case domain.GoalStatusActive, domain.GoalStatusPaused, domain.GoalStatusArchived:
		case domain.GoalStatusAchieved, domain.GoalStatusMissed:
			return fieldError("status", fmt.Sprintf("%q is decided by the engine from recorded metrics, not set by hand", *patch.Status))
		default:
			return fieldError("status", fmt.Sprintf("%q is not a goal status", *patch.Status))
		}
	}

	// A settled goal is a historical record. Reopening it is a legitimate thing to
	// want, but it has to be its own deliberate step: change the status first, then
	// change the target. That keeps "the goal was missed and then the target was
	// lowered" legible in the log instead of arriving as one edit.
	switch current.Status {
	case domain.GoalStatusAchieved, domain.GoalStatusMissed, domain.GoalStatusArchived:
		if hasNonStatusChange(patch) {
			return fieldError("status", fmt.Sprintf(
				"goal is %s: reopen it by setting status to %q before changing anything else",
				current.Status, domain.GoalStatusActive))
		}
	}
	return nil
}

// checkMetricExists refuses a goal that points at a metric nobody declared.
func (g *Goals) checkMetricExists(key string) error {
	if _, ok := g.metrics.Get(key); !ok {
		return fieldError("metricKey", fmt.Sprintf("%q is not a declared metric", key))
	}
	return nil
}

// hasNonStatusChange reports whether a patch touches anything but the status.
func hasNonStatusChange(patch repository.GoalPatch) bool {
	withoutStatus := patch
	withoutStatus.Status = nil
	return !withoutStatus.IsEmpty()
}

// applyGoalPatch returns the goal as it would be after the patch, alongside the
// fields that actually change and what they change from.
//
// The diff is the audit entry. "ops updated goal 7" answers nothing; "ops moved
// targetValue from 100,000,000 to 40,000,000" is the entry somebody needs six
// weeks later.
func applyGoalPatch(goal domain.Goal, patch repository.GoalPatch) (domain.Goal, map[string]any) {
	changes := map[string]any{}
	record := func(field string, from, to any) {
		changes[field] = map[string]any{"from": from, "to": to}
	}

	if patch.Title != nil {
		if next := strings.TrimSpace(*patch.Title); next != goal.Title {
			record("title", goal.Title, next)
			goal.Title = next
		}
	}
	if patch.SourceText != nil {
		if next := strings.TrimSpace(*patch.SourceText); next != goal.SourceText {
			// The text itself can be long; the log records that it changed, not a
			// second copy of a pasted document.
			record("sourceText", len(goal.SourceText), len(next))
			goal.SourceText = next
		}
	}
	if patch.TargetValue != nil && *patch.TargetValue != goal.TargetValue {
		record("targetValue", goal.TargetValue, *patch.TargetValue)
		goal.TargetValue = *patch.TargetValue
	}
	if patch.PeriodEnd != nil && !patch.PeriodEnd.Equal(goal.PeriodEnd) {
		record("periodEnd", goal.PeriodEnd, *patch.PeriodEnd)
		goal.PeriodEnd = *patch.PeriodEnd
	}
	if patch.Status != nil && *patch.Status != goal.Status {
		record("status", string(goal.Status), string(*patch.Status))
		goal.Status = *patch.Status
	}
	if patch.ToleranceRatio != nil && *patch.ToleranceRatio != goal.ToleranceRatio {
		record("toleranceRatio", goal.ToleranceRatio, *patch.ToleranceRatio)
		goal.ToleranceRatio = *patch.ToleranceRatio
	}
	if patch.TriggerCooldown != nil && *patch.TriggerCooldown != goal.TriggerCooldown {
		record("triggerCooldown", goal.TriggerCooldown.String(), patch.TriggerCooldown.String())
		goal.TriggerCooldown = *patch.TriggerCooldown
	}
	if patch.MaxTriggersPerPeriod != nil && *patch.MaxTriggersPerPeriod != goal.MaxTriggersPerPeriod {
		record("maxTriggersPerPeriod", goal.MaxTriggersPerPeriod, *patch.MaxTriggersPerPeriod)
		goal.MaxTriggersPerPeriod = *patch.MaxTriggersPerPeriod
	}
	if patch.BotID != nil {
		if next := strings.TrimSpace(*patch.BotID); next != goal.BotID {
			record("botId", goal.BotID, next)
			goal.BotID = next
		}
	}
	if patch.ChannelID != nil {
		if next := strings.TrimSpace(*patch.ChannelID); next != goal.ChannelID {
			record("channelId", goal.ChannelID, next)
			goal.ChannelID = next
		}
	}
	return goal, changes
}

// appendAudit records a write. A failed audit write never fails the caller — the
// goal was already stored — but it is logged loudly, because an audit log with
// silent gaps is worse than no audit log at all.
func (g *Goals) appendAudit(ctx context.Context, event repository.AuditEvent) {
	if _, err := g.audit.Append(ctx, event); err != nil {
		g.log.Error().Err(err).
			Str("action", event.Action).
			Str("goalId", event.SubjectID).
			Msg("failed to write audit entry")
	}
}
