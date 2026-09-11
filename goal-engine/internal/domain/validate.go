package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Safety floors and defaults applied to a goal when it is created.
//
// These bound how often one goal can wake an agent. A goal with no cooldown and
// no trigger budget would dispatch a task on every monitor tick — a task storm
// whose cost is real money and the operator's attention.
const (
	// DefaultTriggerCooldown is the gap between triggers for a goal that does
	// not set one.
	DefaultTriggerCooldown = 6 * time.Hour
	// MinTriggerCooldown is the shortest gap a goal may ask for. Anything
	// shorter is treated as a mistake rather than an intention.
	MinTriggerCooldown = 15 * time.Minute
	// DefaultMaxTriggersPerPeriod caps how often a goal may wake an agent
	// within one period when it does not set its own cap.
	DefaultMaxTriggersPerPeriod = 5
	// MaxToleranceRatio is the widest grace band a goal may ask for. A goal
	// tolerant of being 50% behind is not being watched.
	MaxToleranceRatio = 0.5
	// MaxTitleLength and MaxSourceTextLength bound the free text a goal carries
	// into an agent brief.
	MaxTitleLength      = 200
	MaxSourceTextLength = 4000
	// MinPeriodLength is the shortest period a pace calculation says anything
	// useful about.
	MinPeriodLength = time.Hour
	// MaxPeriodLength stops a goal being created with a deadline so distant that
	// its pace line is meaningless.
	MaxPeriodLength = 5 * 365 * 24 * time.Hour
)

// ValidationError names the field that is wrong and why, so an API can answer
// with per-field messages instead of one opaque string.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// ValidationErrors is every problem found in one goal, so a caller fixes them in
// one pass rather than one round trip per field.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return "invalid goal: " + strings.Join(parts, "; ")
}

// Fields renders the errors as a field-to-message map for an API response.
func (e ValidationErrors) Fields() map[string]string {
	fields := make(map[string]string, len(e))
	for _, err := range e {
		// The first message for a field is the most specific one, so an
		// additional consequence of the same mistake does not overwrite it.
		if _, seen := fields[err.Field]; !seen {
			fields[err.Field] = err.Message
		}
	}
	return fields
}

// Validate reports every structural problem with a goal.
//
// This is deliberately about the goal alone: whether its metric key exists in the
// operator's registry is a question for the caller, which owns that registry.
// What is checked here is what makes a goal evaluable at all — a goal that cannot
// be evaluated would fail on every tick, forever, and the place to catch that is
// the moment somebody writes it down.
func (g Goal) Validate() error {
	var errs ValidationErrors
	add := func(field, message string) {
		errs = append(errs, ValidationError{Field: field, Message: message})
	}

	if strings.TrimSpace(g.Product) == "" {
		add("product", "is required")
	}
	switch title := strings.TrimSpace(g.Title); {
	case title == "":
		add("title", "is required")
	case len([]rune(title)) > MaxTitleLength:
		add("title", fmt.Sprintf("must be at most %d characters", MaxTitleLength))
	}
	if len([]rune(g.SourceText)) > MaxSourceTextLength {
		add("sourceText", fmt.Sprintf("must be at most %d characters", MaxSourceTextLength))
	}
	if strings.TrimSpace(g.MetricKey) == "" {
		add("metricKey", "is required")
	}

	switch g.Comparator {
	case ComparatorGTE, ComparatorLTE:
	case "":
		add("comparator", "is required")
	default:
		add("comparator", fmt.Sprintf("must be %q or %q", ComparatorGTE, ComparatorLTE))
	}

	// A non-finite target makes every comparison against it false, which an
	// evaluator would read as "behind pace" forever.
	if !isFinite(g.TargetValue) {
		add("targetValue", "must be a finite number")
	}
	if g.BaselineValue != nil && !isFinite(*g.BaselineValue) {
		add("baselineValue", "must be a finite number")
	}

	switch {
	case g.PeriodStart.IsZero():
		add("periodStart", "is required")
	case g.PeriodEnd.IsZero():
		add("periodEnd", "is required")
	case !g.PeriodEnd.After(g.PeriodStart):
		add("periodEnd", "must be after periodStart")
	default:
		length := g.PeriodEnd.Sub(g.PeriodStart)
		if length < MinPeriodLength {
			add("periodEnd", fmt.Sprintf("must be at least %s after periodStart", MinPeriodLength))
		}
		if length > MaxPeriodLength {
			add("periodEnd", "is too far from periodStart for a pace line to mean anything")
		}
	}

	switch g.Status {
	case GoalStatusActive, GoalStatusPaused, GoalStatusAchieved, GoalStatusMissed, GoalStatusArchived:
	case "":
		add("status", "is required")
	default:
		add("status", fmt.Sprintf("%q is not a goal status", g.Status))
	}

	switch {
	case g.ToleranceRatio < 0:
		add("toleranceRatio", "cannot be negative")
	case g.ToleranceRatio > MaxToleranceRatio:
		add("toleranceRatio", fmt.Sprintf("must be at most %.2f", MaxToleranceRatio))
	case math.IsNaN(g.ToleranceRatio):
		add("toleranceRatio", "must be a finite number")
	}

	if g.TriggerCooldown < 0 {
		add("triggerCooldown", "cannot be negative")
	} else if g.TriggerCooldown > 0 && g.TriggerCooldown < MinTriggerCooldown {
		add("triggerCooldown", fmt.Sprintf("must be at least %s", MinTriggerCooldown))
	}
	if g.MaxTriggersPerPeriod < 0 {
		add("maxTriggersPerPeriod", "cannot be negative")
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// WithDefaults returns a copy of the goal with its blank safety limits filled in
// and its text trimmed.
//
// The limits default to something restrictive rather than to zero. Zero cooldown
// and zero trigger budget both mean "no limit" to the evaluator, so a goal created
// without them would be the most aggressive goal in the registry — the opposite of
// what leaving a field blank usually means.
func (g Goal) WithDefaults() Goal {
	out := g
	out.Product = strings.TrimSpace(out.Product)
	out.Title = strings.TrimSpace(out.Title)
	out.SourceText = strings.TrimSpace(out.SourceText)
	out.MetricKey = strings.TrimSpace(out.MetricKey)
	out.BotID = strings.TrimSpace(out.BotID)
	out.ChannelID = strings.TrimSpace(out.ChannelID)

	if out.Status == "" {
		out.Status = GoalStatusActive
	}
	if out.Comparator == "" {
		out.Comparator = ComparatorGTE
	}
	if out.ToleranceRatio == 0 {
		out.ToleranceRatio = DefaultToleranceRatio
	}
	if out.TriggerCooldown == 0 {
		out.TriggerCooldown = DefaultTriggerCooldown
	}
	if out.MaxTriggersPerPeriod == 0 {
		out.MaxTriggersPerPeriod = DefaultMaxTriggersPerPeriod
	}
	return out
}
