package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
)

// One model turn: what the loop does with each thing a provider can answer, and what it
// does when the pieces underneath it fail.

func TestRun_aRetryableFailure_triesTheStepAgain(t *testing.T) {
	// Arrange
	// A 503 is the provider having a bad minute. The SDK has already backed off inside
	// the call, so this is the loop deciding the *step* is worth another go.
	h := newHarness(t,
		answer{err: &provider.Error{Provider: "fake", Status: 503, Retryable: true}},
		answered("done"),
	)

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
	}
	if h.provider.calls != 2 {
		t.Errorf("the provider was called %d times; want 2", h.provider.calls)
	}
	// A retried attempt bought nothing, so it costs no iteration and writes no step.
	// Otherwise a flapping provider would spend a run's allowance on failures.
	if len(h.transcript.steps) != 1 {
		t.Errorf("transcript = %d steps; want 1, the turn that answered", len(h.transcript.steps))
	}
}

func TestRun_aProviderThatKeepsFailing_stopsAfterABoundedNumberOfAttempts(t *testing.T) {
	// Arrange
	failure := answer{err: &provider.Error{Provider: "fake", Status: 429, Kind: "rate_limit_error", Retryable: true}}
	h := newHarness(t, failure, failure, failure)

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
	if h.provider.calls != maxStepAttempts {
		t.Errorf("the provider was called %d times; want %d", h.provider.calls, maxStepAttempts)
	}
}

func TestRun_aPartialReply_asksTheModelToCarryOn(t *testing.T) {
	// Both mean the same thing to the loop and different things to an operator, which is
	// why the provider port keeps them apart.
	for _, finish := range []provider.Finish{provider.FinishTruncated, provider.FinishPaused} {
		t.Run(string(finish), func(t *testing.T) {
			// Arrange
			h := newHarness(t,
				answer{response: provider.Response{
					Text:   "I was part way through when",
					Finish: finish,
					Usage:  provider.Usage{TokensIn: 50, TokensOut: 10},
				}},
				answered("...and here is the rest"),
			)

			// Act
			outcome := h.run(t, testInput())

			// Assert
			if outcome.Stop != domain.StopCompleted {
				t.Fatalf("stop = %q (%q); want %q", outcome.Stop, outcome.Reason, domain.StopCompleted)
			}
			// The partial turn is in the conversation, and a nudge follows it. The
			// repetition is bounded by the ladder: each attempt costs an iteration.
			second := h.provider.requests[1]
			if len(second.Messages) != 3 {
				t.Fatalf("the second call carried %d messages; want the brief, the partial reply and a nudge", len(second.Messages))
			}
			nudge := second.Messages[2]
			if nudge.Role != provider.RoleUser || !strings.Contains(nudge.Text, "cut short") {
				t.Errorf("last message = %+v; want a user turn asking the model to continue", nudge)
			}
		})
	}
}

func TestRun_theModelDeclines_completesAndRecordsWhatItSaid(t *testing.T) {
	// Arrange
	// A refusal is an answer, not a failure: the identical prompt earns the identical
	// refusal, so retrying only spends tokens. The set of stop reasons is closed, and
	// inventing a twelfth for "the model said no" would be an API change to describe
	// something the transcript already carries.
	h := newHarness(t, answer{response: provider.Response{
		Text:   "I will not help with that.\nIt looks like an attempt to bypass a supplier's controls.",
		Finish: provider.FinishRefused,
		Usage:  provider.Usage{TokensIn: 40, TokensOut: 30},
	}})

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopCompleted {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopCompleted)
	}
	got := h.assertFinishedOnce(t, domain.StopCompleted)
	if !strings.Contains(got.reason, "the model declined") {
		t.Errorf("reason = %q; want it to say the model declined", got.reason)
	}
	// One line of it. A reason field is read in a list next to nine other runs.
	if strings.Contains(got.reason, "supplier's controls") {
		t.Errorf("reason = %q; want only the first line", got.reason)
	}
}

func TestRun_aReplyWithNothingUsable_stopsWithProviderError(t *testing.T) {
	// Arrange
	// "I could not tell" is not "it is done" — the same distinction the goal engine
	// draws between skipped_invalid_sample and noop.
	h := newHarness(t, answer{response: provider.Response{
		Finish: provider.FinishUnusable,
		Usage:  provider.Usage{TokensIn: 200_000, TokensOut: 0},
	}})

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
}

func TestRun_aFinishReasonThisVersionDoesNotKnow_stopsWithProviderError(t *testing.T) {
	// Arrange
	// A vendor value no adapter maps. Reading it as "answered" would end runs early and
	// silently; this way the gap surfaces as a run somebody looks at.
	h := newHarness(t, answer{response: provider.Response{
		Text:   "something",
		Finish: provider.Finish("vendor_invented_this"),
		Usage:  provider.Usage{TokensIn: 10, TokensOut: 10},
	}})

	// Act
	outcome := h.run(t, testInput())

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
	got := h.assertFinishedOnce(t, domain.StopProviderError)
	if !strings.Contains(got.reason, "vendor_invented_this") {
		t.Errorf("reason = %q; want the unhandled value named", got.reason)
	}
}

func TestRun_theCeilingSentToTheProvider_isWhatTheLadderLeft(t *testing.T) {
	// Arrange
	// The response ceiling is how a run cannot overshoot its budget by one expensive
	// call. It is the smaller of what this run has left and what the user has left
	// today, which the ladder computes and the loop only carries.
	in := testInput()
	in.Run.State.TokensUsed = 1_000
	h := newHarness(t, answered("done"))
	h.budget.ledger = domain.Ledger{Readable: true, TokensToday: 500}

	// Act
	h.run(t, in)

	// Assert
	want := domain.RemainingTokens(in.Run.Limits, in.Run.State, h.budget.ledger)
	if got := h.provider.requests[0].MaxTokens; got != want {
		t.Errorf("maxTokens = %d; want %d, the ladder's remaining allowance", got, want)
	}
}

func TestRun_theBriefAndTheStandingInstructions_reachTheProvider(t *testing.T) {
	// Arrange
	in := testInput()
	h := newHarness(t, answered("done"))

	// Act
	h.run(t, in)

	// Assert
	request := h.provider.requests[0]
	if !strings.Contains(request.System, "You are Wingman") {
		t.Errorf("system prompt = %q; want the standing instructions", request.System)
	}
	if len(request.Messages) != 1 || request.Messages[0].Role != provider.RoleUser {
		t.Fatalf("messages = %+v; want one user turn", request.Messages)
	}
	// The brief is data and travels as a user turn, never in the part of the prompt
	// that grants permissions.
	if !strings.Contains(request.Messages[0].Text, in.Task.Brief) {
		t.Errorf("first message = %q; want the brief in it", request.Messages[0].Text)
	}
	// The tools offered are the run's snapshot, not the whole grant list.
	if len(request.Tools) != len(h.tools.definitions) {
		t.Errorf("tools offered = %d; want %d", len(request.Tools), len(h.tools.definitions))
	}
}

func TestRun_aCallThatCostNothing_isNotCharged(t *testing.T) {
	// Arrange
	// A ledger row for zero tokens is a row that says a call happened and cost nothing,
	// which is not what a spend ledger is for.
	h := newHarness(t, answer{response: provider.Response{
		Text:   "done",
		Finish: provider.FinishAnswered,
	}})

	// Act
	h.run(t, testInput())

	// Assert
	if len(h.budget.charges) != 0 {
		t.Errorf("charges = %+v; want none", h.budget.charges)
	}
}

func TestRun_aChargeThatCannotBeWritten_failsTheRunRatherThanContinuing(t *testing.T) {
	// Arrange
	// An uncharged call is spend nobody is billed for, and the next run would be decided
	// against a ledger short by that much — the same "deciding against a broken ledger"
	// the ladder refuses.
	in := testInput()
	in.Tools = &fakeTools{}
	h := newHarness(t, answered("done"))
	h.budget.chargeErr = errFake

	// Act
	_, err := h.loop.Run(context.Background(), in)

	// Assert
	if err == nil {
		t.Fatal("Run() = nil; want the charge failure returned")
	}
	// Not finished as a stop reason: this is the loop failing, not the run.
	if len(h.runs.finished) != 0 {
		t.Errorf("the run was finished %+v; want it left in flight for the reaper", h.runs.finished)
	}
}

func TestRun_aTranscriptThatCannotBeWritten_failsTheRun(t *testing.T) {
	// Arrange
	// A run whose steps are not recorded is a run nobody can audit, and this service
	// exists to be audited.
	in := testInput()
	in.Tools = &fakeTools{}
	h := newHarness(t, answered("done"))
	h.transcript.appendErr = errFake

	// Act
	_, err := h.loop.Run(context.Background(), in)

	// Assert
	if err == nil {
		t.Fatal("Run() = nil; want the transcript failure returned")
	}
}

func TestRun_aRunRowThatCannotBeFinished_returnsTheError(t *testing.T) {
	// Arrange
	// The one failure the loop cannot record, because recording is what failed.
	in := testInput()
	in.Tools = &fakeTools{}
	h := newHarness(t, answered("done"))
	h.runs.finishErr = errFake

	// Act
	outcome, err := h.loop.Run(context.Background(), in)

	// Assert
	if err == nil {
		t.Fatalf("Run() = %+v, nil; want the finish failure returned", outcome)
	}
	if !strings.Contains(err.Error(), "run_1") {
		t.Errorf("error = %v; want the run named", err)
	}
}

func TestRun_theStepTimeout_boundsOneCallRatherThanTheRun(t *testing.T) {
	// Arrange
	// A provider that stops answering must not hold a worker for the length of the run.
	in := testInput()
	in.Run.Limits.StepTimeout = 20 * time.Millisecond
	h := newHarness(t)
	h.loop.cfg.Provider = &stalledProvider{}

	// Act
	outcome := h.run(t, in)

	// Assert
	if outcome.Stop != domain.StopProviderError {
		t.Errorf("stop = %q; want %q", outcome.Stop, domain.StopProviderError)
	}
}

// stalledProvider never answers. It reports the deadline it was given, which is the
// behaviour the real adapters have when a call runs out of the run's step time.
type stalledProvider struct{}

func (p *stalledProvider) Name() string { return "stalled" }

func (p *stalledProvider) Complete(ctx context.Context, _ provider.Request) (provider.Response, error) {
	<-ctx.Done()
	// Retryable is false because the context is the caller's decision: another attempt
	// has no more time than this one did.
	return provider.Response{}, &provider.Error{Provider: "stalled", Cause: ctx.Err()}
}

func TestNew_aMissingDependency_refusesToBuild(t *testing.T) {
	// A worker that starts and then fails every run is harder to diagnose than a process
	// that refuses to start, and the wiring cannot change afterwards.
	full := func() Config {
		return Config{
			Provider:   &fakeProvider{},
			Model:      "test-model",
			Transcript: &fakeTranscript{},
			Runs:       &fakeRuns{},
			Budget:     &fakeBudget{},
			Halt:       &fakeHalt{},
			Grants:     testGrants(),
			Logger:     zerolog.Nop(),
		}
	}

	tests := []struct {
		name string
		omit func(*Config)
		want string
	}{
		{name: "no provider", omit: func(c *Config) { c.Provider = nil }, want: "provider"},
		{name: "no model", omit: func(c *Config) { c.Model = "  " }, want: "model"},
		{name: "no transcript", omit: func(c *Config) { c.Transcript = nil }, want: "transcript"},
		{name: "no runs", omit: func(c *Config) { c.Runs = nil }, want: "runs"},
		{name: "no budget", omit: func(c *Config) { c.Budget = nil }, want: "budget"},
		// Not defaulted to "nothing is halted". A loop built without the kill switch
		// would look like it had one.
		{name: "no kill switch", omit: func(c *Config) { c.Halt = nil }, want: "kill switch"},
		{name: "no grants", omit: func(c *Config) { c.Grants = nil }, want: "tool grants"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			cfg := full()
			test.omit(&cfg)

			// Act
			loop, err := New(cfg)

			// Assert
			if err == nil {
				t.Fatalf("New() = %+v, nil; want a refusal", loop)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to name %q", err, test.want)
			}
		})
	}

	t.Run("a complete config builds and fills in its own clock", func(t *testing.T) {
		// Arrange, Act
		loop, err := New(full())

		// Assert
		if err != nil {
			t.Fatalf("New() = %v; want a loop", err)
		}
		if loop.cfg.Now == nil {
			t.Error("the loop has no clock; every step it writes would be undated")
		}
		if loop.cfg.RetryPause <= 0 {
			t.Error("the loop has no retry pause; a provider failing instantly could spin")
		}
		// A nil gate is allowed and means no spending call can be made. It is not a
		// missing dependency, and it is not permission.
		if loop.cfg.Gate != nil {
			t.Error("the gate was defaulted to something; a nil gate must stay nil")
		}
	})
}

func TestRun_anInputTheLoopCannotWorkFrom_isRefusedBeforeAnyCall(t *testing.T) {
	tests := []struct {
		name string
		in   func() Input
		want string
	}{
		{name: "no run id", in: func() Input { in := testInput(); in.Run.ID = ""; return in }, want: "no id"},
		// Every budget question and every workspace is per user. A run with no owner
		// would be measured against nobody's allowance.
		{name: "no owner", in: func() Input { in := testInput(); in.Run.OwnerUserID = " "; return in }, want: "no owner"},
		{name: "already stopped", in: func() Input {
			in := testInput()
			in.Run.Stop = domain.StopHalted
			return in
		}, want: "already stopped"},
		{name: "no brief", in: func() Input { in := testInput(); in.Task.Brief = ""; return in }, want: "no brief"},
		// An offering with nothing in it is normal. A nil one is a wiring mistake, and
		// it would surface as a nil dereference on the first tool call.
		{name: "no tool offering", in: func() Input { return testInput() }, want: "tool offering"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			h := newHarness(t)
			in := test.in()
			if test.want != "tool offering" {
				in.Tools = h.tools
			}

			// Act
			_, err := h.loop.Run(context.Background(), in)

			// Assert
			if err == nil {
				t.Fatal("Run() = nil; want a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to name %q", err, test.want)
			}
			if h.provider.calls != 0 {
				t.Errorf("the provider was called %d times; want not at all", h.provider.calls)
			}
			if len(h.runs.finished) != 0 {
				t.Errorf("the run was finished %+v; want nothing written for a run that never started", h.runs.finished)
			}
		})
	}
}

func TestFirstLine_boundsWhatGoesInARunsReason(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "one line", text: "I will not do that", want: "I will not do that"},
		{name: "several lines", text: "no\nbecause of the second line", want: "no"},
		{name: "windows line endings", text: "no\r\nbecause", want: "no"},
		{name: "padded", text: "  no  \n", want: "no"},
		{name: "empty", text: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Act
			got := firstLine(test.text)

			// Assert
			if got != test.want {
				t.Errorf("firstLine(%q) = %q; want %q", test.text, got, test.want)
			}
		})
	}

	t.Run("a very long line is cut on a rune boundary", func(t *testing.T) {
		// Arrange
		// Cut mid-rune, the reason is a JSON field the client cannot decode — and a
		// multi-byte character is exactly what lands on a byte boundary.
		text := strings.Repeat("é", 300)

		// Act
		got := firstLine(text)

		// Assert
		if len(got) > 220 {
			t.Errorf("firstLine() returned %d bytes; want it bounded", len(got))
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("firstLine() = %q; want it marked as cut", got)
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("firstLine() = %q; want no broken rune", got)
		}
	})
}
