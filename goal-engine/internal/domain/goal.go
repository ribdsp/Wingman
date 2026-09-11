// Package domain holds Wingman's goal-driven autonomy rules.
//
// Everything in this package is pure: no database, no HTTP, no clock reads.
// Time and observed metric values are passed in, decisions come out. That is
// deliberate — these rules decide when an autonomous agent is allowed to act,
// so they have to be exhaustively testable without infrastructure.
package domain

import "time"

// Comparator expresses which direction of movement counts as progress.
type Comparator string

const (
	// ComparatorGTE marks a "reach" goal: higher is better, e.g. "10k active
	// users this week". A reach goal is settled the moment it is reached.
	ComparatorGTE Comparator = "gte"
	// ComparatorLTE marks a "stay under" goal: lower is better, e.g. "keep
	// churn below 2%". A stay-under goal can regress, so it is only settled
	// when its period closes.
	ComparatorLTE Comparator = "lte"
)

// GoalStatus is the lifecycle state of a goal.
type GoalStatus string

const (
	GoalStatusActive   GoalStatus = "active"
	GoalStatusPaused   GoalStatus = "paused"
	GoalStatusAchieved GoalStatus = "achieved"
	GoalStatusMissed   GoalStatus = "missed"
	GoalStatusArchived GoalStatus = "archived"
)

// Decision is the outcome of evaluating one goal at one point in time.
type Decision string

const (
	// DecisionNoop means the goal is on pace; the agent is left alone.
	DecisionNoop Decision = "noop"
	// DecisionTrigger means the goal is behind pace and an agent task should
	// be dispatched. This is the only decision that wakes an agent.
	DecisionTrigger Decision = "trigger"
	// DecisionCooldownSkipped means the goal is behind pace but a trigger was
	// dispatched too recently.
	DecisionCooldownSkipped Decision = "cooldown_skipped"
	// DecisionTriggerBudgetExhausted means the goal is behind pace but it has
	// already used up its allowed triggers for this period.
	DecisionTriggerBudgetExhausted Decision = "trigger_budget_exhausted"
	// DecisionAchieved means the target condition is satisfied and settled.
	DecisionAchieved Decision = "achieved"
	// DecisionMissed means the period closed without the target being met.
	DecisionMissed Decision = "missed"
	// DecisionSkippedNotStarted means the goal period has not begun yet.
	DecisionSkippedNotStarted Decision = "skipped_not_started"
	// DecisionSkippedInactive means the goal is paused, archived or already
	// settled.
	DecisionSkippedInactive Decision = "skipped_inactive"
	// DecisionSkippedInvalidSample means the observed metric was not a finite
	// number. This has its own decision on purpose: every float comparison
	// against NaN is false, so without an explicit guard a broken metric query
	// would read as "off track" and wake an agent.
	DecisionSkippedInvalidSample Decision = "skipped_invalid_sample"
	// DecisionHalted means the global kill switch is engaged, so no autonomous
	// action may be taken regardless of pace.
	DecisionHalted Decision = "halted"
)

// DefaultToleranceRatio is the grace band applied to pace checks when a goal
// does not set its own. 5% behind the ideal line is not yet "off track".
const DefaultToleranceRatio = 0.05

// Goal is a business target Wingman watches on the operator's behalf.
type Goal struct {
	ID string
	// Product scopes the goal to one business line, e.g. "acme".
	Product string
	Title   string
	// SourceText keeps the natural-language phrasing the goal was created
	// from, so an agent can be briefed in the operator's own words.
	SourceText string
	// MetricKey references a metric declared in the operator's metric
	// registry. Goals may not define their own SQL — see docs/goal-engine.md.
	MetricKey   string
	Comparator  Comparator
	TargetValue float64
	// BaselineValue is the metric value when the period opened. Nil means it
	// has not been captured yet; the first observation of the period fills it.
	BaselineValue *float64
	PeriodStart   time.Time
	PeriodEnd     time.Time
	Status        GoalStatus
	// ToleranceRatio widens the on-track band. Zero falls back to
	// DefaultToleranceRatio.
	ToleranceRatio float64
	// TriggerCooldown is the minimum gap between two dispatched triggers for
	// this goal. It is the main defence against an agent task storm.
	TriggerCooldown time.Duration
	// MaxTriggersPerPeriod caps how often this goal may wake an agent within
	// one period. Zero means unlimited.
	MaxTriggersPerPeriod int
	// BotID and ChannelID say which Wingman bot to task, and where it reports.
	BotID     string
	ChannelID string
}

// EvaluationInput is everything Evaluate needs. The caller supplies the clock
// and the persisted trigger history; Evaluate itself reads neither.
type EvaluationInput struct {
	Goal     Goal
	Observed float64
	Now      time.Time
	// LastTriggeredAt is when this goal last dispatched a trigger, if ever.
	LastTriggeredAt *time.Time
	// TriggersThisPeriod counts triggers already dispatched in this period.
	TriggersThisPeriod int
	// KillSwitchEngaged halts all autonomous action when true.
	KillSwitchEngaged bool
}

// Evaluation is the immutable record of one goal check. Every evaluation is
// persisted, whether or not it triggered anything: a monitor that only records
// its interventions cannot answer "was it watching?".
type Evaluation struct {
	GoalID        string
	ObservedValue float64
	TargetValue   float64
	BaselineValue float64
	// ExpectedValue is where the metric should be right now if progress were
	// perfectly linear across the period.
	ExpectedValue float64
	// ProgressRatio is how far the metric has travelled from baseline to
	// target: 0 means no movement, 1 means target reached.
	ProgressRatio float64
	// ElapsedRatio is how much of the period has passed, clamped to [0,1].
	ElapsedRatio float64
	// PaceRatio is ProgressRatio divided by ElapsedRatio. Below 1 means behind
	// schedule.
	PaceRatio float64
	OnTrack   bool
	// TargetMet reports whether the raw target condition holds right now,
	// independent of whether the goal is settled.
	TargetMet   bool
	Decision    Decision
	Reason      string
	EvaluatedAt time.Time
}

// ShouldDispatch reports whether this evaluation asks the trigger bridge to
// wake an agent.
func (e Evaluation) ShouldDispatch() bool {
	return e.Decision == DecisionTrigger
}
