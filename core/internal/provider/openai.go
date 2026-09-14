package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAI calls the Chat Completions API.
//
// Chat Completions rather than the Responses API: it is the endpoint every
// OpenAI-compatible gateway implements, and a self-hosted deployment routing model
// calls through one of those is a case worth keeping working.
type OpenAI struct {
	completions *openai.ChatCompletionService
	// maxOutputTokens is the ceiling one reply may reach, whatever allowance the
	// ladder passed in. See defaultMaxOutputTokens.
	maxOutputTokens int64
}

// NewOpenAI builds a client. The API key lives inside the SDK client and nowhere
// else, so there is no field for a log line to reach.
func NewOpenAI(opts Options) (*OpenAI, error) {
	if err := opts.validate(NameOpenAI); err != nil {
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

	client := openai.NewClient(requestOptions...)
	return &OpenAI{completions: &client.Chat.Completions, maxOutputTokens: opts.MaxOutputTokens}, nil
}

// Name identifies this provider in the transcript and the spend ledger.
func (o *OpenAI) Name() string { return NameOpenAI }

// Complete makes one Chat Completions call.
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}

	messages, err := openAIMessages(req)
	if err != nil {
		return Response{}, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(req.Model),
		Messages: messages,
		Tools:    openAITools(req.Tools),
		// MaxCompletionTokens, not the deprecated MaxTokens: the reasoning models
		// reject the older field outright, and this one means the same thing
		// everywhere else.
		MaxCompletionTokens: param.NewOpt(min(req.MaxTokens, o.maxOutputTokens)),
	}

	completion, err := o.completions.New(ctx, params)
	if err != nil {
		return Response{}, openAIFailure(err)
	}
	return openAIResponse(completion)
}

// openAIMessages flattens the neutral turns into the message list.
//
// One neutral user turn can become several messages here: a tool result is its own
// message with its own role, and the API requires each one to follow the assistant
// turn that asked for it. So results are emitted first and any accompanying text
// after them, which keeps the pairing the API checks intact.
func openAIMessages(req Request) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1)

	if system := strings.TrimSpace(req.System); system != "" {
		// The system role, not the newer developer role: the API translates system
		// for the models that prefer developer, while the models that predate
		// developer reject it. The translation goes one way only.
		out = append(out, openai.SystemMessage(system))
	}

	for i, message := range req.Messages {
		if message.Role == RoleAssistant {
			out = append(out, openAIAssistantMessage(message))
			continue
		}

		for _, result := range message.ToolResults {
			content := result.Content
			if strings.TrimSpace(content) == "" {
				// A command that printed nothing succeeded, and the model has to be
				// able to tell that from one that failed.
				content = "(no output)"
			}
			if result.IsError {
				// A tool message has no error flag here, unlike Anthropic's tool
				// result block. Without the prefix a stack trace arrives as ordinary
				// output and the model is invited to read it as data.
				content = "error: " + content
			}
			out = append(out, openai.ToolMessage(content, result.CallID))
		}

		if message.Text != "" {
			out = append(out, openai.UserMessage(message.Text))
		} else if len(message.ToolResults) == 0 {
			return nil, fmt.Errorf("provider %s: message %d has no content", NameOpenAI, i)
		}
	}
	return out, nil
}

// openAIAssistantMessage replays a turn the model produced, tool calls included.
//
// The helper constructors cover a plain assistant reply but not one that called a
// tool, so this builds the parameter directly. Replaying the calls matters: they are
// what the tool messages after them refer to, and an assistant turn stripped of them
// leaves results answering nothing.
func openAIAssistantMessage(message Message) openai.ChatCompletionMessageParamUnion {
	assistant := openai.ChatCompletionAssistantMessageParam{}
	if message.Text != "" {
		assistant.Content.OfString = param.NewOpt(message.Text)
	}

	for _, call := range message.ToolCalls {
		arguments := string(call.Input)
		if strings.TrimSpace(arguments) == "" {
			// The field is a JSON string and a required one. An empty one is not
			// valid JSON, and a tool with no arguments still has an empty object.
			arguments = "{}"
		}
		assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: call.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      call.Name,
					Arguments: arguments,
				},
			},
		})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
}

// openAITools describes the offered tools.
//
// The JSON Schema goes across whole, unlike Anthropic's split parameter. Strict mode
// is deliberately not set: it demands every property be required and
// additionalProperties be false, which is a rewrite of a schema that arrived from an
// MCP server or an OpenAPI document, and rewriting a third party's contract to fit
// one vendor's flag is how a tool starts being called with arguments it never
// declared.
func openAITools(tools []ToolSchema) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		definition := shared.FunctionDefinitionParam{Name: tool.Name}
		if tool.Description != "" {
			definition.Description = param.NewOpt(tool.Description)
		}
		if len(tool.InputSchema) > 0 {
			definition.Parameters = shared.FunctionParameters(tool.InputSchema)
		}
		out = append(out, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{Function: definition},
		})
	}
	return out
}

// openAIResponse reads the reply.
func openAIResponse(completion *openai.ChatCompletion) (Response, error) {
	if len(completion.Choices) == 0 {
		// A 200 with nothing in it. There is no reply to act on and no usage to
		// bill, so it is a failed call rather than an empty answer — and it is
		// classified as transient, because the alternative reading is that a
		// well-formed request produced a well-formed nothing.
		return Response{}, failure(NameOpenAI, 0, "no_choices", errors.New("the reply carried no choices"))
	}

	choice := completion.Choices[0]
	response := Response{
		Text:  choice.Message.Content,
		Model: completion.Model,
		Usage: Usage{
			TokensIn:  completion.Usage.PromptTokens,
			TokensOut: completion.Usage.CompletionTokens,
		},
	}

	for _, call := range choice.Message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			// A custom tool call. The registry only knows function tools, so passing
			// this on would produce a call for a tool no grant covers.
			continue
		}
		arguments := call.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: []byte(arguments),
		})
	}

	if refusal := choice.Message.Refusal; refusal != "" {
		// The refusal is the reply. Leaving Text empty would record a run that
		// stopped with nothing said, when in fact the model said why.
		response.Text = refusal
		response.Finish = FinishRefused
		return response, nil
	}

	response.Finish = openAIFinish(choice.FinishReason, len(response.ToolCalls) > 0)
	return response, nil
}

// openAIFinish maps the vendor's finish reason onto ours.
func openAIFinish(reason string, hasToolCalls bool) Finish {
	// As with Anthropic: a reply carrying tool calls is a request to act, whatever
	// the label says, because ignoring the calls would report a run complete while
	// the work it asked for was never done.
	if hasToolCalls {
		return FinishToolUse
	}

	switch reason {
	case "stop":
		return FinishAnswered
	case "tool_calls", "function_call":
		// Labelled as calling a tool with no call in the message. Nothing can be run
		// and nothing was answered, so this is not success.
		return FinishUnusable
	case "length":
		return FinishTruncated
	case "content_filter":
		return FinishRefused
	default:
		// Including the empty string, which is what a truncated or non-conforming
		// reply from a gateway looks like.
		return FinishUnusable
	}
}

// openAIFailure classifies a failed call.
//
// The vendor's error code survives; its response body does not. See Error.Cause for
// why.
func openAIFailure(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		// Code is the specific one — insufficient_quota, invalid_api_key — and Type
		// is the family it belongs to. The specific one is what changes what to do
		// about it, so it wins when both are present.
		kind := apiErr.Code
		if kind == "" {
			kind = apiErr.Type
		}
		return failure(NameOpenAI, apiErr.StatusCode, kind, nil)
	}
	return failure(NameOpenAI, 0, "", err)
}
