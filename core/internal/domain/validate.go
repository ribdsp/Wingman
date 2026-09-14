package domain

import (
	"fmt"
	"strings"
	"time"
)

// Safety floors, defaults and ceilings applied to an agent run.
//
// These bound how much one run can do before a human sees it. A run with no
// iteration cap and no token allowance is a loop that spends until something else
// breaks — and unlike a goal that wakes an agent too often, a runaway loop bills
// by the second.
//
// Every default is restrictive on purpose. An operator who raises one is making a
// decision; an operator who leaves a field blank is not.
const (
	// DefaultMaxIterations is how many times a run may ask the model when it does
	// not say. Fifteen is enough for a real multi-step task and short enough that
	// a loop talking to itself is stopped inside a minute or two.
	DefaultMaxIterations = 15
	// MinMaxIterations is one: a run that may not ask the model even once is not
	// a run, it is a misconfiguration.
	MinMaxIterations = 1
	// MaxMaxIterations stops an operator from writing down a number that is
	// effectively no limit at all.
	MaxMaxIterations = 200

	// DefaultMaxToolCalls bounds side effects rather than thought. A run can
	// reach its iteration cap having done nothing; reaching this one means it
	// acted, forty times.
	DefaultMaxToolCalls = 40
	MinMaxToolCalls     = 1
	MaxMaxToolCalls     = 500

	// DefaultMaxTokensPerRun is one run's allowance.
	DefaultMaxTokensPerRun int64 = 250_000
	// MinMaxTokensPerRun is small but not pointless: below about a thousand
	// tokens a run cannot complete a single useful exchange, so a lower figure is
	// a typo rather than a tight budget.
	MinMaxTokensPerRun int64 = 1_000
	MaxMaxTokensPerRun int64 = 10_000_000

	// DefaultMaxTokensPerUserDay is one caller's allowance across every run
	// today. Switching it off needs NoUserDailyCap, written out.
	DefaultMaxTokensPerUserDay int64 = 2_000_000
	MinMaxTokensPerUserDay     int64 = 1_000

	// DefaultStepTimeout bounds one model call. Long enough for a slow provider
	// answering a large prompt, short enough that a hung connection does not hold
	// a worker for the afternoon.
	DefaultStepTimeout = 3 * time.Minute
	MinStepTimeout     = 5 * time.Second
	MaxStepTimeout     = 15 * time.Minute

	// DefaultSandboxTimeout bounds one tool execution.
	DefaultSandboxTimeout = 90 * time.Second
	MinSandboxTimeout     = time.Second
	MaxSandboxTimeout     = 30 * time.Minute
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

// ValidationErrors is every problem found at once, so a caller fixes them in one
// pass rather than one round trip per field.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return "invalid: " + strings.Join(parts, "; ")
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

// Validate reports every out-of-range limit.
//
// A zero is not an error here — it means "not set", and WithDefaults fills it. What
// is rejected is a value written down deliberately that would defeat its own
// purpose: a negative cap, a timeout too short to complete anything, a ceiling so
// high it is not a ceiling.
func (l RunLimits) Validate() error {
	var errs ValidationErrors
	add := func(field, message string) {
		errs = append(errs, ValidationError{Field: field, Message: message})
	}

	checkInt := func(field string, value, min, max int) {
		switch {
		case value < 0:
			add(field, "cannot be negative")
		case value > 0 && value < min:
			add(field, fmt.Sprintf("must be at least %d", min))
		case value > max:
			add(field, fmt.Sprintf("must be at most %d", max))
		}
	}
	checkInt("maxIterations", l.MaxIterations, MinMaxIterations, MaxMaxIterations)
	checkInt("maxToolCalls", l.MaxToolCalls, MinMaxToolCalls, MaxMaxToolCalls)

	switch {
	case l.MaxTokensPerRun < 0:
		add("maxTokensPerRun", "cannot be negative")
	case l.MaxTokensPerRun > 0 && l.MaxTokensPerRun < MinMaxTokensPerRun:
		add("maxTokensPerRun", fmt.Sprintf("must be at least %d", MinMaxTokensPerRun))
	case l.MaxTokensPerRun > MaxMaxTokensPerRun:
		add("maxTokensPerRun", fmt.Sprintf("must be at most %d", MaxMaxTokensPerRun))
	}

	// NoUserDailyCap is the one negative value that means something. Any other
	// negative is a mistake, and reading it as "unlimited" would be the most
	// expensive possible interpretation of a typo.
	switch {
	case l.MaxTokensPerUserDay == NoUserDailyCap:
	case l.MaxTokensPerUserDay < 0:
		add("maxTokensPerUserDay", fmt.Sprintf("cannot be negative; use %d to remove the cap", NoUserDailyCap))
	case l.MaxTokensPerUserDay > 0 && l.MaxTokensPerUserDay < MinMaxTokensPerUserDay:
		add("maxTokensPerUserDay", fmt.Sprintf("must be at least %d", MinMaxTokensPerUserDay))
	}

	checkDuration := func(field string, value, min, max time.Duration) {
		switch {
		case value < 0:
			add(field, "cannot be negative")
		case value > 0 && value < min:
			add(field, fmt.Sprintf("must be at least %s", min))
		case value > max:
			add(field, fmt.Sprintf("must be at most %s", max))
		}
	}
	checkDuration("stepTimeout", l.StepTimeout, MinStepTimeout, MaxStepTimeout)
	checkDuration("sandboxTimeout", l.SandboxTimeout, MinSandboxTimeout, MaxSandboxTimeout)

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// WithDefaults returns a copy with every unset limit filled in.
//
// This is the only place a zero becomes a working number. Decide treats a zero as
// a stop, so a run that reaches the ladder without passing through here halts
// immediately — which is the failure mode to want if the wiring is ever wrong.
func (l RunLimits) WithDefaults() RunLimits {
	out := l
	if out.MaxIterations == 0 {
		out.MaxIterations = DefaultMaxIterations
	}
	if out.MaxToolCalls == 0 {
		out.MaxToolCalls = DefaultMaxToolCalls
	}
	if out.MaxTokensPerRun == 0 {
		out.MaxTokensPerRun = DefaultMaxTokensPerRun
	}
	if out.MaxTokensPerUserDay == 0 {
		out.MaxTokensPerUserDay = DefaultMaxTokensPerUserDay
	}
	if out.StepTimeout == 0 {
		out.StepTimeout = DefaultStepTimeout
	}
	if out.SandboxTimeout == 0 {
		out.SandboxTimeout = DefaultSandboxTimeout
	}
	return out
}
