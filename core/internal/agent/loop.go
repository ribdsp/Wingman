package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
)

// turn is what one iteration concluded, in the terms the ladder is asked in.
//
// finished and providerFailed become ContinuationInput fields on the next
// consultation. stop is the one thing the ladder does not decide: a tool call the run
// was not permitted to make, which ends the run where it stands.
type turn struct {
	finished       bool
	providerFailed bool
	stop           domain.StopReason
	reason         string
}

// iterate runs one model turn and whatever tool calls it asked for.
//
// It mutates messages and state through pointers, because the conversation and the
// counters are the run's position: a turn that returned copies would leave the caller
// deciding what to keep, and the ladder reads those counters.
func (l *Loop) iterate(
	ctx context.Context,
	in Input,
	messages *[]provider.Message,
	state *domain.RunState,
	remainingTokens int64,
	log zerolog.Logger,
) (turn, error) {
	request := provider.Request{
		Model:     l.cfg.Model,
		System:    systemPrompt(in),
		Messages:  *messages,
		Tools:     toolSchemas(in.Tools),
		MaxTokens: remainingTokens,
	}

	response, callErr := l.complete(ctx, in, request)
	if callErr != nil {
		// Recorded as a model step even though it bought nothing. The step costs an
		// iteration the run will not get to use — it is about to stop as
		// provider_error — and it is the only place a transcript can say which
		// provider failed and how. provider.Error.Error() is safe to write down: its
		// Cause is a transport failure only, never a vendor response body.
		step := domain.Step{
			Kind:    domain.StepKindModel,
			Content: "",
			Err:     callErr.Error(),
		}
		if err := l.record(ctx, in, state, step); err != nil {
			return turn{}, err
		}
		log.Error().Err(callErr).Msg("the model call failed")
		return turn{providerFailed: true, reason: callErr.Error()}, nil
	}

	// Recorded before the tool calls run, so a run killed between the two has the
	// model's own reasoning in the transcript rather than a gap.
	step := domain.Step{
		Kind:      domain.StepKindModel,
		TokensIn:  response.Usage.TokensIn,
		TokensOut: response.Usage.TokensOut,
		Content:   response.Text,
	}
	if err := l.record(ctx, in, state, step); err != nil {
		return turn{}, err
	}
	if err := l.charge(ctx, in, response, log); err != nil {
		return turn{}, err
	}

	*messages = append(*messages, provider.Message{
		Role:      provider.RoleAssistant,
		Text:      response.Text,
		ToolCalls: response.ToolCalls,
	})

	switch response.Finish {
	case provider.FinishAnswered:
		return turn{finished: true}, nil

	case provider.FinishRefused:
		// The model declined. Recorded as a finished run rather than a failure,
		// because a refusal is an answer: the text is in the transcript, and the set
		// of stop reasons is closed — inventing a twelfth for "the model said no"
		// would be an API change to describe something the transcript already says.
		log.Info().Msg("the model declined the task")
		return turn{finished: true, reason: "the model declined: " + firstLine(response.Text)}, nil

	case provider.FinishUnusable:
		// The vendor answered, but not with anything a loop can act on. Treated as a
		// provider failure rather than as a finished run: "I could not tell" is not
		// "it is done", which is the same distinction the goal engine draws between
		// skipped_invalid_sample and noop.
		return turn{providerFailed: true, reason: l.cfg.Provider.Name() + ": the response carried nothing usable"}, nil

	case provider.FinishTruncated, provider.FinishPaused:
		// A partial reply. The assistant turn is already in the conversation, so the
		// nudge is enough to have the model carry on from it. The repetition is
		// bounded by the ladder: each attempt costs an iteration and its tokens.
		*messages = append(*messages, provider.Message{
			Role: provider.RoleUser,
			Text: "Your previous message was cut short. Continue from where it stopped. Do not repeat what you already said.",
		})
		return turn{}, nil

	case provider.FinishToolUse:
		return l.runToolCalls(ctx, in, messages, state, response, log)

	default:
		// A vendor reason no adapter maps. Reading it as "answered" would end runs
		// early and silently; reading it as a provider failure surfaces the gap.
		return turn{
			providerFailed: true,
			reason:         fmt.Sprintf("%s: finish reason %q is one this version does not handle", l.cfg.Provider.Name(), response.Finish),
		}, nil
	}
}

// complete makes one model call, retrying only what is worth retrying.
//
// The per-step deadline comes from the run's own limits, so a provider that stops
// answering cannot hold a worker for the length of the run. A retried attempt costs
// no iteration and no tool call — it bought nothing — but it does cost wall clock,
// which is what maxStepAttempts bounds.
func (l *Loop) complete(ctx context.Context, in Input, request provider.Request) (provider.Response, error) {
	var lastErr error

	for attempt := 1; attempt <= maxStepAttempts; attempt++ {
		stepCtx, cancel := context.WithTimeout(ctx, in.Run.Limits.StepTimeout)
		response, err := l.cfg.Provider.Complete(stepCtx, request)
		cancel()

		if err == nil {
			return response, nil
		}
		lastErr = err

		if !provider.Retryable(err) {
			// Includes a cancelled or timed-out call: another attempt has no more time
			// than the last one did. provider.Retryable answers false for an error it
			// does not recognise, which is the careful reading — an unknown failure
			// retried is a request nobody can account for.
			return provider.Response{}, err
		}
		if attempt == maxStepAttempts {
			break
		}
		if err := pause(ctx, l.cfg.RetryPause); err != nil {
			// The run was cancelled while waiting. The last provider error is what
			// happened; the ladder will read the cancellation on its next pass.
			return provider.Response{}, lastErr
		}
	}
	return provider.Response{}, lastErr
}

// charge books a model call against the user's daily allowance.
//
// It is separate from the transcript step on purpose. The step is what the run spent,
// which bounds this run; the charge is what the user spent, which bounds every run
// they start today. A call that bought nothing is not charged.
func (l *Loop) charge(ctx context.Context, in Input, response provider.Response, log zerolog.Logger) error {
	if response.Usage.Total() == 0 {
		return nil
	}
	model := response.Model
	if model == "" {
		// What answered, not what was asked for — but a vendor that did not say falls
		// back to what we asked for rather than leaving the ledger row anonymous.
		model = l.cfg.Model
	}

	err := l.cfg.Budget.Charge(ctx, Charge{
		UserID:    in.Run.OwnerUserID,
		RunID:     in.Run.ID,
		Provider:  l.cfg.Provider.Name(),
		Model:     model,
		TokensIn:  response.Usage.TokensIn,
		TokensOut: response.Usage.TokensOut,
		At:        l.cfg.Now(),
	})
	if err != nil {
		// Returned, not logged and swallowed. An uncharged call is spend nobody is
		// billed for, and the next run would be decided against a ledger that is
		// short by this much — which is exactly the "deciding against a broken
		// ledger" the ladder refuses to do.
		log.Error().Err(err).Msg("the model call could not be charged to the user's budget")
		return fmt.Errorf("charge run %s: %w", in.Run.ID, err)
	}
	return nil
}

// pause waits, unless the run is going away.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// firstLine is the opening line of a model's text, bounded, for a run's reason field.
func firstLine(text string) string {
	const maxReasonLength = 200
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	if len(text) <= maxReasonLength {
		return text
	}
	// Cut back to a rune boundary: the reason is rendered as JSON, and a broken rune
	// there is a field the client cannot decode.
	cut := text[:maxReasonLength]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
