package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// validGoal is a goal with nothing wrong with it. Each test breaks exactly one
// thing, so a failure names the rule that changed.
func validGoal() Goal {
	return Goal{
		ID:                   "goal-1",
		Product:              "acme",
		Title:                "Reach 100M MRR",
		SourceText:           "Push MRR to 100 million before the end of September.",
		MetricKey:            "mrr.total",
		Comparator:           ComparatorGTE,
		TargetValue:          100_000_000,
		PeriodStart:          time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:            time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Status:               GoalStatusActive,
		ToleranceRatio:       0.05,
		TriggerCooldown:      6 * time.Hour,
		MaxTriggersPerPeriod: 5,
		BotID:                "bot-1",
		ChannelID:            "chan-1",
	}
}

func TestValidateAcceptsAWellFormedGoal(t *testing.T) {
	if err := validGoal().Validate(); err != nil {
		t.Fatalf("expected a valid goal, got %v", err)
	}
}

func TestValidateAcceptsEveryGoalStatusAndComparator(t *testing.T) {
	// A status the evaluator understands but the validator rejects would make a
	// goal unreadable after the monitor settled it.
	for _, status := range []GoalStatus{
		GoalStatusActive, GoalStatusPaused, GoalStatusAchieved, GoalStatusMissed, GoalStatusArchived,
	} {
		goal := validGoal()
		goal.Status = status
		if err := goal.Validate(); err != nil {
			t.Fatalf("status %q: %v", status, err)
		}
	}
	for _, comparator := range []Comparator{ComparatorGTE, ComparatorLTE} {
		goal := validGoal()
		goal.Comparator = comparator
		if err := goal.Validate(); err != nil {
			t.Fatalf("comparator %q: %v", comparator, err)
		}
	}
}

func TestValidateRejectsGoalsItCannotEvaluate(t *testing.T) {
	// Each of these would fail on every tick, forever. The point of validating is
	// that the failure lands on whoever wrote the goal instead.
	cases := []struct {
		name   string
		field  string
		mutate func(*Goal)
	}{
		{"no product", "product", func(g *Goal) { g.Product = "  " }},
		{"no title", "title", func(g *Goal) { g.Title = "" }},
		{"title too long", "title", func(g *Goal) { g.Title = strings.Repeat("a", MaxTitleLength+1) }},
		{"source text too long", "sourceText", func(g *Goal) { g.SourceText = strings.Repeat("a", MaxSourceTextLength+1) }},
		{"no metric", "metricKey", func(g *Goal) { g.MetricKey = "\t" }},
		{"no comparator", "comparator", func(g *Goal) { g.Comparator = "" }},
		{"unknown comparator", "comparator", func(g *Goal) { g.Comparator = "approximately" }},
		{"nan target", "targetValue", func(g *Goal) { g.TargetValue = math.NaN() }},
		{"infinite target", "targetValue", func(g *Goal) { g.TargetValue = math.Inf(1) }},
		{"nan baseline", "baselineValue", func(g *Goal) { nan := math.NaN(); g.BaselineValue = &nan }},
		{"no period start", "periodStart", func(g *Goal) { g.PeriodStart = time.Time{} }},
		{"no period end", "periodEnd", func(g *Goal) { g.PeriodEnd = time.Time{} }},
		{"period ends before it starts", "periodEnd", func(g *Goal) {
			g.PeriodEnd = g.PeriodStart.Add(-time.Hour)
		}},
		{"period of zero length", "periodEnd", func(g *Goal) { g.PeriodEnd = g.PeriodStart }},
		{"period too short to pace", "periodEnd", func(g *Goal) {
			g.PeriodEnd = g.PeriodStart.Add(MinPeriodLength - time.Second)
		}},
		{"period beyond any horizon", "periodEnd", func(g *Goal) {
			g.PeriodEnd = g.PeriodStart.Add(MaxPeriodLength + 24*time.Hour)
		}},
		{"no status", "status", func(g *Goal) { g.Status = "" }},
		{"unknown status", "status", func(g *Goal) { g.Status = "probably fine" }},
		{"negative tolerance", "toleranceRatio", func(g *Goal) { g.ToleranceRatio = -0.01 }},
		{"tolerance so wide nothing is late", "toleranceRatio", func(g *Goal) {
			g.ToleranceRatio = MaxToleranceRatio + 0.01
		}},
		{"nan tolerance", "toleranceRatio", func(g *Goal) { g.ToleranceRatio = math.NaN() }},
		{"negative cooldown", "triggerCooldown", func(g *Goal) { g.TriggerCooldown = -time.Second }},
		{"cooldown below the floor", "triggerCooldown", func(g *Goal) {
			g.TriggerCooldown = MinTriggerCooldown - time.Second
		}},
		{"negative trigger budget", "maxTriggersPerPeriod", func(g *Goal) { g.MaxTriggersPerPeriod = -1 }},
	}

	for _, c := range cases {
		goal := validGoal()
		c.mutate(&goal)

		err := goal.Validate()
		if err == nil {
			t.Fatalf("%s: expected a validation error", c.name)
		}
		var errs ValidationErrors
		if !errors.As(err, &errs) {
			t.Fatalf("%s: expected ValidationErrors, got %T", c.name, err)
		}
		if _, ok := errs.Fields()[c.field]; !ok {
			t.Fatalf("%s: expected the error to name %q, got %v", c.name, c.field, errs.Fields())
		}
	}
}

func TestValidateAcceptsAGoalWithNoCooldownBecauseZeroMeansNone(t *testing.T) {
	// Zero is a deliberate choice an operator can make on an existing goal; only
	// a positive value below the floor is a mistake.
	goal := validGoal()
	goal.TriggerCooldown = 0
	goal.MaxTriggersPerPeriod = 0

	if err := goal.Validate(); err != nil {
		t.Fatalf("expected zero limits to validate, got %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	// One round trip per mistake is a bad API. A caller fixing a form wants the
	// whole list.
	goal := validGoal()
	goal.Product = ""
	goal.Title = ""
	goal.MetricKey = ""

	err := goal.Validate()
	var errs ValidationErrors
	if !errors.As(err, &errs) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	if len(errs) != 3 {
		t.Fatalf("expected 3 problems, got %d: %v", len(errs), errs)
	}
	if got := errs.Fields(); len(got) != 3 {
		t.Fatalf("expected 3 fields, got %v", got)
	}
	if msg := errs.Error(); !strings.Contains(msg, "product") || !strings.Contains(msg, "metricKey") {
		t.Fatalf("expected the message to list the fields, got %q", msg)
	}
}

func TestValidationErrorsKeepsTheFirstMessageForARepeatedField(t *testing.T) {
	// The first message for a field is the specific one; a later consequence of the
	// same mistake must not overwrite it.
	errs := ValidationErrors{
		{Field: "periodEnd", Message: "must be after periodStart"},
		{Field: "periodEnd", Message: "is too far away"},
	}
	if got := errs.Fields()["periodEnd"]; got != "must be after periodStart" {
		t.Fatalf("expected the first message, got %q", got)
	}
}

func TestValidationErrorFormatsAsFieldAndMessage(t *testing.T) {
	err := ValidationError{Field: "targetValue", Message: "must be a finite number"}
	if got := err.Error(); got != "targetValue: must be a finite number" {
		t.Fatalf("unexpected message %q", got)
	}
}

func TestWithDefaultsFillsTheSafetyLimitsRatherThanLeavingThemOff(t *testing.T) {
	// Zero cooldown and zero trigger budget both mean "no limit" to the evaluator,
	// so a goal created with blank fields would be the most aggressive goal in the
	// registry. That is the opposite of what leaving a field blank means.
	goal := Goal{
		Product:     "acme",
		Title:       "Reach 100M MRR",
		MetricKey:   "mrr.total",
		TargetValue: 100_000_000,
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	filled := goal.WithDefaults()

	if filled.Status != GoalStatusActive {
		t.Fatalf("expected an active goal, got %q", filled.Status)
	}
	if filled.Comparator != ComparatorGTE {
		t.Fatalf("expected the growth comparator, got %q", filled.Comparator)
	}
	if filled.ToleranceRatio != DefaultToleranceRatio {
		t.Fatalf("expected the default tolerance, got %v", filled.ToleranceRatio)
	}
	if filled.TriggerCooldown != DefaultTriggerCooldown {
		t.Fatalf("expected the default cooldown, got %v", filled.TriggerCooldown)
	}
	if filled.MaxTriggersPerPeriod != DefaultMaxTriggersPerPeriod {
		t.Fatalf("expected the default trigger budget, got %d", filled.MaxTriggersPerPeriod)
	}
	if err := filled.Validate(); err != nil {
		t.Fatalf("expected the defaults to produce a valid goal, got %v", err)
	}
}

func TestWithDefaultsTrimsTextAndLeavesDeliberateValuesAlone(t *testing.T) {
	goal := validGoal()
	goal.Product = "  acme  "
	goal.Title = "  Reach 100M MRR\n"
	goal.SourceText = "  push it  "
	goal.MetricKey = " mrr.total "
	goal.BotID = " bot-1 "
	goal.ChannelID = " chan-1 "
	goal.Comparator = ComparatorLTE
	goal.ToleranceRatio = 0.2
	goal.TriggerCooldown = 30 * time.Minute
	goal.MaxTriggersPerPeriod = 2

	filled := goal.WithDefaults()

	for field, got := range map[string]string{
		"product":   filled.Product,
		"title":     filled.Title,
		"metricKey": filled.MetricKey,
		"botId":     filled.BotID,
		"channelId": filled.ChannelID,
	} {
		if got != strings.TrimSpace(got) {
			t.Fatalf("%s was not trimmed: %q", field, got)
		}
	}
	if filled.Title != "Reach 100M MRR" || filled.SourceText != "push it" {
		t.Fatalf("unexpected trimming: %q / %q", filled.Title, filled.SourceText)
	}
	if filled.Comparator != ComparatorLTE || filled.ToleranceRatio != 0.2 ||
		filled.TriggerCooldown != 30*time.Minute || filled.MaxTriggersPerPeriod != 2 {
		t.Fatal("expected explicit values to survive defaulting")
	}
}

func TestWithDefaultsDoesNotMutateItsReceiver(t *testing.T) {
	// Every value type in this package is copied rather than modified in place;
	// a defaulting helper that edited the original would be a silent side effect
	// in the middle of a validation path.
	goal := Goal{Product: "  acme  "}

	filled := goal.WithDefaults()

	if goal.Product != "  acme  " || goal.Status != "" {
		t.Fatalf("receiver was mutated: %+v", goal)
	}
	if filled.Product != "acme" || filled.Status != GoalStatusActive {
		t.Fatalf("copy was not defaulted: %+v", filled)
	}
}
