package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/ribdsp/wingman/core/internal/domain"
	"github.com/ribdsp/wingman/core/internal/provider"
	"github.com/ribdsp/wingman/core/internal/tool"
)

// runToolCalls executes what one model turn asked for.
//
// Every call the model made gets a result, including the ones that were refused. Both
// vendors reject the following turn if a call it made has no answer, so "refused" has
// to be said in the conversation rather than by omission — which is also the better
// outcome: a model told why it could not do something can say so in its reply, while
// a model told nothing tries again.
//
// The kill switch is not re-read between the calls of one batch. It is read before
// every iteration, and the one class of call that could do lasting damage in the
// meantime — a spend — goes through the goal engine's gate, whose first row is the
// kill switch. A batch of reads and writes is bounded by the batch.
func (l *Loop) runToolCalls(
	ctx context.Context,
	in Input,
	messages *[]provider.Message,
	state *domain.RunState,
	response provider.Response,
	log zerolog.Logger,
) (turn, error) {
	if len(response.ToolCalls) == 0 {
		// The adapter said tool_use and gave nothing to call. Continuing would send the
		// same conversation again and get the same answer.
		return turn{
			providerFailed: true,
			reason:         l.cfg.Provider.Name() + ": the response asked for a tool and named none",
		}, nil
	}

	attended := in.Task.Source.Attended()
	results := make([]provider.ToolResult, 0, len(response.ToolCalls))

	for _, call := range response.ToolCalls {
		if state.ToolCalls >= in.Run.Limits.MaxToolCalls {
			// The ladder checks this between iterations. It cannot check it inside one:
			// a single turn may ask for fifty calls, and running them all before the
			// next consultation would put the cap thirty calls behind the run.
			return turn{
				stop: domain.StopToolCallCap,
				reason: fmt.Sprintf("the run reached its limit of %d tool calls part-way through a turn",
					in.Run.Limits.MaxToolCalls),
			}, nil
		}

		outcome, err := l.callTool(ctx, in, state, call, attended, log)
		if err != nil {
			return turn{}, err
		}
		if outcome.stop != "" {
			return turn{stop: outcome.stop, reason: outcome.reason}, nil
		}
		results = append(results, provider.ToolResult{
			CallID:  call.ID,
			Content: outcome.content,
			IsError: outcome.isError,
		})
	}

	*messages = append(*messages, provider.Message{
		Role:        provider.RoleUser,
		ToolResults: results,
	})
	return turn{}, nil
}

// callOutcome is what one tool call produced: text for the model, or a reason to end
// the run.
type callOutcome struct {
	content string
	isError bool
	stop    domain.StopReason
	reason  string
}

// callTool gates one call and, if it survives the gate, runs it.
//
// The gate is domain.ClassifyTool against the operator's grants — not the offering.
// The offering already excludes anything ungranted, so a name that fails here is one
// the model invented or one withdrawn since the run started, and both of those end the
// run: the model was shown exactly what it could use, so a call outside that set is
// not a mistake to negotiate over. The cost is that one hallucinated name ends a run,
// and that is the intended trade — an agent that is told "no, try again" learns to
// keep guessing.
func (l *Loop) callTool(
	ctx context.Context,
	in Input,
	state *domain.RunState,
	call provider.ToolCall,
	attended bool,
	log zerolog.Logger,
) (callOutcome, error) {
	verdict := l.cfg.Grants.Classify(call.Name, attended)

	if verdict.Stop != "" {
		// Recorded before the run ends, so the transcript says what it tried to call.
		if err := l.recordTool(ctx, in, state, call.Name, "", verdict.Reason); err != nil {
			return callOutcome{}, err
		}
		log.Warn().Str("tool", call.Name).Msg("a tool call was denied")
		return callOutcome{stop: verdict.Stop, reason: verdict.Reason}, nil
	}

	if verdict.NeedsApproval {
		outcome, err := l.approve(ctx, in, state, call, verdict, log)
		if err != nil || outcome.stop != "" || outcome.content != "" {
			return outcome, err
		}
		// Approved. Fall through to the call itself.
	}

	return l.execute(ctx, in, state, call, log)
}

// approve handles a call that is legitimate but not permitted as it stands.
//
// Which way it goes depends on the grant, not on this function's opinion. A spending
// tool has a contract the operator declared and is filed with the goal engine, whose
// ladder is the only thing in the system that decides money. An unattended write has
// no contract and no gate denominated in anything a write can be measured in — the
// goal engine's policies are amounts in a currency, and filing a write there as an
// amount of nothing would be refused for having no amount, in an audit row that blames
// the number instead of the situation. So it is refused for this run and the model is
// told, which leaves a run able to finish and report what it did manage.
//
// An empty callOutcome means the call may proceed.
func (l *Loop) approve(
	ctx context.Context,
	in Input,
	state *domain.RunState,
	call provider.ToolCall,
	verdict domain.ToolVerdict,
	log zerolog.Logger,
) (callOutcome, error) {
	grant, _ := l.cfg.Grants.Get(call.Name)
	if grant.Spend == nil {
		note := verdict.Reason + ", so it was not called. Say what you would have done and why it needs a person."
		if err := l.recordTool(ctx, in, state, call.Name, "", note); err != nil {
			return callOutcome{}, err
		}
		log.Info().Str("tool", call.Name).Msg("a write was refused: this run is unattended")
		return callOutcome{content: note, isError: true}, nil
	}

	amount, err := spendAmount(call.Input, grant.Spend.AmountArgument)
	if err != nil {
		// The model's own arguments, so this is a mistake it can correct — a missing
		// or unparseable amount is fed back rather than ending the run.
		note := fmt.Sprintf("this call spends money and its %q argument %s. Give the amount as a number.",
			grant.Spend.AmountArgument, err)
		if err := l.recordTool(ctx, in, state, call.Name, "", note); err != nil {
			return callOutcome{}, err
		}
		return callOutcome{content: note, isError: true}, nil
	}

	if l.cfg.Gate == nil {
		// No goal engine configured. The thing that decides money is not there, which
		// means nothing may spend it — not that this call may go ahead.
		note := "spending is not available on this instance: no approval gate is configured, so the call was not made."
		if err := l.recordTool(ctx, in, state, call.Name, "", note); err != nil {
			return callOutcome{}, err
		}
		log.Warn().Str("tool", call.Name).Msg("a spend was refused: no approval gate is configured")
		return callOutcome{content: note, isError: true}, nil
	}

	decision, gateErr := l.cfg.Gate.Request(ctx, SpendRequest{
		ActionType: grant.Spend.ActionType,
		Amount:     amount,
		Currency:   grant.Spend.Currency,
		RunID:      in.Run.ID,
		ToolName:   call.Name,
		// Derived from the run and this call, so a retried step files the same request
		// rather than a second one against the same daily cap.
		IdempotencyKey: in.Run.ID + ":" + call.ID,
	})
	if gateErr != nil {
		// Fail closed and end the run. The gate being unreachable also makes the kill
		// switch unreadable, so the next iteration would halt anyway; stopping here
		// says which of the two it was.
		reason := "the spending gate could not be reached, so the call was refused"
		if err := l.recordTool(ctx, in, state, call.Name, "", reason); err != nil {
			return callOutcome{}, err
		}
		log.Error().Err(gateErr).Str("tool", call.Name).Msg("the spending gate could not be reached")
		return callOutcome{stop: domain.StopToolDenied, reason: reason}, nil
	}

	gate := domain.DecideAfterGate(decision.Outcome, decision.Reason)
	switch gate.Action {
	case domain.GateProceed:
		log.Info().Str("tool", call.Name).Str("actionType", grant.Spend.ActionType).
			Msg("a spend was auto-approved by the goal engine")
		return callOutcome{}, nil

	case domain.GateReport:
		if err := l.recordTool(ctx, in, state, call.Name, "", gate.Reason); err != nil {
			return callOutcome{}, err
		}
		return callOutcome{content: gate.Reason, isError: true}, nil

	default:
		if err := l.recordTool(ctx, in, state, call.Name, "", gate.Reason); err != nil {
			return callOutcome{}, err
		}
		log.Warn().Str("tool", call.Name).Msg("a spend was refused by the goal engine")
		return callOutcome{stop: domain.StopToolDenied, reason: gate.Reason}, nil
	}
}

// execute runs a permitted call and records what it produced.
//
// A tool that fails is not a failure of the run. The output is handed back with
// IsError set, because working around a command that did not work is the sort of thing
// the model is there for — and because the alternative, ending the run, would make one
// missing file lose everything the run had already established.
func (l *Loop) execute(
	ctx context.Context,
	in Input,
	state *domain.RunState,
	call provider.ToolCall,
	log zerolog.Logger,
) (callOutcome, error) {
	// The run's own sandbox deadline, not the step deadline: a command is a different
	// kind of wait from a model call, and the operator sets them separately.
	callCtx, cancel := context.WithTimeout(ctx, in.Run.Limits.SandboxTimeout)
	defer cancel()

	result, callErr := in.Tools.Call(callCtx, tool.Invocation{
		Name:      call.Name,
		Input:     call.Input,
		Workspace: in.Workspace,
	})
	if callErr != nil {
		// The error is written to the transcript and handed to the model. Runners are
		// ours and are built not to echo a script or a key into their errors; see
		// sandbox.runProcess, which names the binary and nothing else.
		note := callErr.Error()
		if err := l.recordTool(ctx, in, state, call.Name, "", note); err != nil {
			return callOutcome{}, err
		}
		log.Warn().Err(callErr).Str("tool", call.Name).Msg("a tool call failed")
		return callOutcome{content: note, isError: true}, nil
	}

	content := result.Content
	if result.Truncated {
		// Said in the transcript as well as to the model, so a reply built on a
		// half-read file can be recognised as one later.
		content = strings.TrimRight(content, "\n") + "\n[output truncated]"
	}

	stepErr := ""
	if result.IsError {
		// The tool ran and reported a failure. Kept apart from the content so the
		// transcript can be read for what went wrong without parsing output.
		stepErr = "the tool reported a failure"
	}
	if err := l.recordTool(ctx, in, state, call.Name, content, stepErr); err != nil {
		return callOutcome{}, err
	}
	return callOutcome{content: content, isError: result.IsError}, nil
}

// recordTool appends one tool step. Tool steps bill no tokens: a sandbox does not
// charge for them, and charging the model call that asked for one twice is what
// repository.AppendStep refuses.
func (l *Loop) recordTool(ctx context.Context, in Input, state *domain.RunState, name, content, stepErr string) error {
	return l.record(ctx, in, state, domain.Step{
		Kind:     domain.StepKindTool,
		ToolName: name,
		Content:  content,
		Err:      stepErr,
	})
}

// spendAmount reads the amount a spending call would move out of the model's own
// arguments.
//
// Read, not trusted: it is handed to the goal engine, whose caps are what decide. What
// this function guarantees is only that a number reached the gate — an absent or
// unparseable one must not arrive there as a zero, because zero is below every
// threshold an operator would write.
func spendAmount(input json.RawMessage, field string) (float64, error) {
	if len(input) == 0 {
		return 0, fmt.Errorf("is missing: the call carried no arguments")
	}
	var arguments map[string]any
	if err := json.Unmarshal(input, &arguments); err != nil {
		return 0, fmt.Errorf("could not be read: the arguments are not a JSON object")
	}

	raw, present := arguments[field]
	if !present {
		return 0, fmt.Errorf("is missing")
	}

	var amount float64
	switch value := raw.(type) {
	case float64:
		amount = value
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, fmt.Errorf("is not a number")
		}
		amount = parsed
	case string:
		// Accepted because a model asked for a number will sometimes send "1500.00".
		// Refusing that would turn a formatting habit into a run that cannot pay
		// anything, and the gate re-checks the value either way.
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0, fmt.Errorf("is not a number")
		}
		amount = parsed
	default:
		return 0, fmt.Errorf("is not a number")
	}

	if amount <= 0 {
		// The gate refuses a non-positive amount too. Stopping here means the model is
		// told what to fix, rather than the audit log carrying a denial for an amount
		// nobody meant to send.
		return 0, fmt.Errorf("is %v, and a spend has to be a positive amount", amount)
	}
	return amount, nil
}

// toolSchemas is the offering as the provider port wants it.
func toolSchemas(tools Tools) []provider.ToolSchema {
	definitions := tools.Definitions()
	schemas := make([]provider.ToolSchema, 0, len(definitions))
	for _, definition := range definitions {
		schemas = append(schemas, provider.ToolSchema{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: definition.InputSchema,
		})
	}
	return schemas
}
