package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Anthropic calls the Messages API.
type Anthropic struct {
	messages *anthropic.MessageService
	// maxOutputTokens is the ceiling one reply may reach, whatever allowance the
	// ladder passed in. See defaultMaxOutputTokens.
	maxOutputTokens int64
}

// NewAnthropic builds a client. It holds the API key inside the SDK client and
// nowhere else, so there is no field for a log line to reach.
func NewAnthropic(opts Options) (*Anthropic, error) {
	if err := opts.validate(NameAnthropic); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()

	requestOptions := []option.RequestOption{
		option.WithAPIKey(opts.APIKey),
		option.WithMaxRetries(opts.MaxRetries),
	}
	if opts.BaseURL != "" {
		requestOptions = append(requestOptions, option.WithBaseURL(opts.BaseURL))
	}

	client := anthropic.NewClient(requestOptions...)
	return &Anthropic{messages: &client.Messages, maxOutputTokens: opts.MaxOutputTokens}, nil
}

// Name identifies this provider in the transcript and the spend ledger.
func (a *Anthropic) Name() string { return NameAnthropic }

// Complete makes one Messages call.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}

	messages, err := anthropicMessages(req.Messages)
	if err != nil {
		return Response{}, err
	}

	params := anthropic.MessageNewParams{
		Model: anthropic.Model(req.Model),
		// The smaller of what the run may still spend and what one reply may be.
		MaxTokens: min(req.MaxTokens, a.maxOutputTokens),
		Messages:  messages,
		Tools:     anthropicTools(req.Tools),
	}
	if system := strings.TrimSpace(req.System); system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}

	message, err := a.messages.New(ctx, params)
	if err != nil {
		return Response{}, anthropicFailure(err)
	}
	return anthropicResponse(message), nil
}

// anthropicMessages turns the neutral turns into blocks.
//
// Anthropic's shape is the one the neutral types were modelled on, so this is close
// to a rename — with one substitution worth knowing about, on empty tool output.
func anthropicMessages(messages []Message) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for i, message := range messages {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(message.ToolCalls)+len(message.ToolResults))

		// Results come before text in a user turn: the API requires a tool result to
		// be the first thing in the turn that answers a tool call.
		for _, result := range message.ToolResults {
			content := result.Content
			if strings.TrimSpace(content) == "" {
				// An empty result is rejected, and "" is also not what happened. A
				// command that printed nothing succeeded, and the model has to be
				// able to tell that from a command that failed.
				content = "(no output)"
			}
			blocks = append(blocks, anthropic.NewToolResultBlock(result.CallID, content, result.IsError))
		}

		if text := message.Text; text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(text))
		}

		for _, call := range message.ToolCalls {
			input, err := decodeToolInput(call)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, input, call.Name))
		}

		if len(blocks) == 0 {
			// An empty turn is rejected by the API, and there is nothing sensible to
			// substitute: a turn with no content is a caller mistake.
			return nil, fmt.Errorf("provider %s: message %d has no content", NameAnthropic, i)
		}

		switch message.Role {
		case RoleAssistant:
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		default:
			// Validate has already refused anything that is not one of the two roles.
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out, nil
}

// decodeToolInput turns the raw arguments back into a value the SDK will encode.
//
// The raw JSON is not passed through as bytes because the SDK encodes this field as
// an arbitrary value, and a []byte reaching that encoder is a base64 string rather
// than an object. Validate has already established the bytes are valid JSON.
func decodeToolInput(call ToolCall) (any, error) {
	if len(call.Input) == 0 {
		// A tool with no arguments. The API wants an object, not null.
		return map[string]any{}, nil
	}
	var input any
	if err := json.Unmarshal(call.Input, &input); err != nil {
		return nil, fmt.Errorf("provider: arguments of tool call %s are not JSON: %w", call.ID, err)
	}
	return input, nil
}

// anthropicTools describes the offered tools.
//
// The JSON Schema is split across Properties, Required and ExtraFields because that
// is the shape of the parameter. Everything the operator wrote that is not one of
// the three named keys is carried in ExtraFields rather than dropped — a schema's
// additionalProperties or its enums are what keep a model from inventing arguments.
func anthropicTools(tools []ToolSchema) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		schema := anthropic.ToolInputSchemaParam{}
		extra := map[string]any{}
		for key, value := range tool.InputSchema {
			switch key {
			case "properties":
				schema.Properties = value
			case "required":
				schema.Required = stringSlice(value)
			case "type":
				// Always "object" here; the field is a constant in the SDK.
			default:
				extra[key] = value
			}
		}
		if len(extra) > 0 {
			schema.ExtraFields = extra
		}

		declared := anthropic.ToolParam{Name: tool.Name, InputSchema: schema}
		if tool.Description != "" {
			declared.Description = param.NewOpt(tool.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &declared})
	}
	return out
}

// stringSlice reads a JSON Schema "required" list, whatever it decoded into.
//
// It is []any after a round trip through encoding/json and []string when built in
// Go, and a required list silently dropped because of which of those it was is a
// model free to omit an argument the tool needs.
func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// anthropicResponse reads the reply.
func anthropicResponse(message *anthropic.Message) Response {
	response := Response{
		Model: message.Model,
		Usage: Usage{
			TokensIn:  message.Usage.InputTokens,
			TokensOut: message.Usage.OutputTokens,
		},
	}

	var text strings.Builder
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			input := block.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			response.ToolCalls = append(response.ToolCalls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: input,
			})
		}
		// Any other block type is ignored rather than refused. Blocks are added by
		// the vendor over time, and a run must not fail on one it has no use for.
	}
	response.Text = text.String()
	response.Finish = anthropicFinish(message.StopReason, len(response.ToolCalls) > 0)
	return response
}

// anthropicFinish maps the vendor's stop reason onto ours.
func anthropicFinish(reason anthropic.StopReason, hasToolCalls bool) Finish {
	// A reply carrying tool calls is a request to act, whatever the label says. The
	// alternative reading — trusting the label — would let a run be reported
	// complete while the work it asked for was never done.
	if hasToolCalls {
		return FinishToolUse
	}

	switch reason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return FinishAnswered
	case anthropic.StopReasonToolUse:
		// Labelled tool_use with no tool call in the content. Nothing can be run and
		// nothing was answered, so this is not success.
		return FinishUnusable
	case anthropic.StopReasonMaxTokens:
		return FinishTruncated
	case anthropic.StopReasonPauseTurn:
		return FinishPaused
	case anthropic.StopReasonRefusal:
		return FinishRefused
	default:
		// Including model_context_window_exceeded, which cannot be continued past:
		// sending the same conversation again produces the same answer.
		return FinishUnusable
	}
}

// anthropicFailure classifies a failed call.
//
// The vendor's error type survives; its response body does not. See Error.Cause for
// why.
func anthropicFailure(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return failure(NameAnthropic, apiErr.StatusCode, string(apiErr.Type()), nil)
	}
	return failure(NameAnthropic, 0, "", err)
}
