package domain

import (
	"errors"
	"testing"
	"time"
)

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	if err == nil {
		t.Fatal("expected validation to fail, got nil")
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) {
		t.Fatalf("error is %T, not ValidationErrors; a handler could not render per-field messages", err)
	}
	return errs.Fields()
}

func TestRunLimits_allDefaults_areValid(t *testing.T) {
	// Arrange
	limits := RunLimits{}.WithDefaults()

	// Act
	err := limits.Validate()

	// Assert
	// A default that its own validator rejects would make every unconfigured
	// install refuse to boot.
	if err != nil {
		t.Fatalf("the defaults do not pass validation: %v", err)
	}
}

func TestRunLimits_allZero_isValidBecauseZeroMeansUnset(t *testing.T) {
	// Arrange
	limits := RunLimits{}

	// Act
	err := limits.Validate()

	// Assert
	// Zero is "not set", which WithDefaults fills. Rejecting it here would force
	// every caller to spell out six limits it has no opinion about.
	if err != nil {
		t.Fatalf("an empty RunLimits was rejected: %v", err)
	}
}

func TestRunLimits_negativeValues_areRejected(t *testing.T) {
	// Arrange
	limits := RunLimits{
		MaxIterations:   -1,
		MaxToolCalls:    -1,
		MaxTokensPerRun: -1,
		StepTimeout:     -time.Second,
		SandboxTimeout:  -time.Second,
	}

	// Act
	fields := fieldsOf(t, limits.Validate())

	// Assert
	for _, field := range []string{"maxIterations", "maxToolCalls", "maxTokensPerRun", "stepTimeout", "sandboxTimeout"} {
		if _, found := fields[field]; !found {
			t.Errorf("%s accepted a negative value", field)
		}
	}
}

func TestRunLimits_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	limits := RunLimits{
		MaxIterations:   MaxMaxIterations + 1,
		MaxToolCalls:    MaxMaxToolCalls + 1,
		MaxTokensPerRun: MinMaxTokensPerRun - 1,
		StepTimeout:     MinStepTimeout - time.Second,
	}

	// Act
	fields := fieldsOf(t, limits.Validate())

	// Assert
	// One round trip per mistake is how a caller gives up on an API.
	if len(fields) != 4 {
		t.Errorf("reported %d problems (%v); want all 4", len(fields), fields)
	}
}

func TestRunLimits_belowFloorButNonZero_isRejected(t *testing.T) {
	// Arrange
	limits := RunLimits{StepTimeout: time.Millisecond}

	// Act
	fields := fieldsOf(t, limits.Validate())

	// Assert
	// A one-millisecond step timeout is a typo, not a tight budget: no provider
	// answers in that time, so every run would fail identically and mysteriously.
	if _, found := fields["stepTimeout"]; !found {
		t.Errorf("a 1ms step timeout was accepted; got %v", fields)
	}
}

func TestRunLimits_noUserDailyCap_isTheOnlyLegalNegative(t *testing.T) {
	// Arrange
	removed := RunLimits{MaxTokensPerUserDay: NoUserDailyCap}
	mistyped := RunLimits{MaxTokensPerUserDay: -500}

	// Act
	removedErr := removed.Validate()
	mistypedFields := fieldsOf(t, mistyped.Validate())

	// Assert
	if removedErr != nil {
		t.Errorf("NoUserDailyCap was rejected: %v", removedErr)
	}
	// Reading any other negative as "unlimited" would be the most expensive
	// possible interpretation of a typo.
	if _, found := mistypedFields["maxTokensPerUserDay"]; !found {
		t.Errorf("-500 was accepted as a daily cap; got %v", mistypedFields)
	}
}

func TestRunLimits_withDefaults_leavesAnExplicitValueAlone(t *testing.T) {
	// Arrange
	limits := RunLimits{MaxIterations: 2, MaxTokensPerRun: 5_000, MaxTokensPerUserDay: NoUserDailyCap}

	// Act
	got := limits.WithDefaults()

	// Assert
	if got.MaxIterations != 2 {
		t.Errorf("maxIterations = %d; an explicit 2 was overwritten", got.MaxIterations)
	}
	if got.MaxTokensPerRun != 5_000 {
		t.Errorf("maxTokensPerRun = %d; an explicit 5000 was overwritten", got.MaxTokensPerRun)
	}
	// The removal must survive: NoUserDailyCap is not zero, so it is not unset.
	if got.MaxTokensPerUserDay != NoUserDailyCap {
		t.Errorf("maxTokensPerUserDay = %d; a removed cap was silently restored to a limit", got.MaxTokensPerUserDay)
	}
	if got.MaxToolCalls != DefaultMaxToolCalls {
		t.Errorf("maxToolCalls = %d; want the default %d", got.MaxToolCalls, DefaultMaxToolCalls)
	}
}

func TestRunLimits_withDefaults_doesNotMutateTheReceiver(t *testing.T) {
	// Arrange
	limits := RunLimits{}

	// Act
	_ = limits.WithDefaults()

	// Assert
	if limits.MaxIterations != 0 {
		t.Errorf("the receiver was mutated: maxIterations = %d", limits.MaxIterations)
	}
}

func TestValidationErrors_fields_keepsTheFirstMessagePerField(t *testing.T) {
	// Arrange
	errs := ValidationErrors{
		{Field: "brief", Message: "is required"},
		{Field: "brief", Message: "must be at most 16000 characters"},
	}

	// Act
	fields := errs.Fields()

	// Assert
	// The first message is the specific one; a later consequence of the same
	// mistake must not overwrite it.
	if fields["brief"] != "is required" {
		t.Errorf("brief = %q; want the first message", fields["brief"])
	}
}

func TestValidationErrors_error_namesEveryField(t *testing.T) {
	// Arrange
	errs := ValidationErrors{
		{Field: "brief", Message: "is required"},
		{Field: "source", Message: "is required"},
	}

	// Act
	got := errs.Error()

	// Assert
	want := "invalid: brief: is required; source: is required"
	if got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}
}
