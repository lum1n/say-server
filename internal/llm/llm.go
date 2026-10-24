// Package llm calls the model providers behind built-in agents: Anthropic,
// and OpenAI or any OpenAI-compatible endpoint such as Ollama
// (docs/adr/0009-built-in-agents.md). It completes a transcript, in which
// the model may call tools; the agent runner decides what to send and runs
// the tools.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	ooption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai" // also any OpenAI-compatible base URL
)

// DefaultModel is the model a new agent of each provider gets.
var DefaultModel = map[string]string{
	ProviderAnthropic: "claude-opus-5",
	ProviderOpenAI:    "gpt-5",
}

// Config selects and authenticates a provider. An empty APIKey uses the
// server's ANTHROPIC_API_KEY or OPENAI_API_KEY; an empty BaseURL the
// provider's own API.
type Config struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

// Message is one turn of the transcript. Role is "user" or "assistant".
// An assistant turn may call tools; the user turn after it carries their
// results.
type Message struct {
	Role        string
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Tool is a function the model may call. InputSchema is a JSON Schema
// object.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is the model asking for a tool. Input should be a JSON object,
// but a model may write broken JSON.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult answers the ToolCall with the same ID.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Response is the model's turn. When it has ToolCalls the model waits for
// their results; Text is then whatever it said alongside them.
type Response struct {
	Text                      string
	ToolCalls                 []ToolCall
	InputTokens, OutputTokens int64
}

// Provider completes a transcript.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// New returns the provider c names.
func New(c Config) (Provider, error) {
	switch c.Provider {
	case ProviderAnthropic:
		var opts []aoption.RequestOption
		if c.APIKey != "" {
			opts = append(opts, aoption.WithAPIKey(c.APIKey))
		}
		if c.BaseURL != "" {
			opts = append(opts, aoption.WithBaseURL(c.BaseURL))
		}
		return &anthropicProvider{client: anthropic.NewClient(opts...), model: c.Model}, nil
	case ProviderOpenAI:
		var opts []ooption.RequestOption
		if c.APIKey != "" {
			opts = append(opts, ooption.WithAPIKey(c.APIKey))
		} else if c.BaseURL != "" {
			// Local endpoints such as Ollama take no key, but the SDK
			// refuses to send a request without one.
			opts = append(opts, ooption.WithAPIKey("none"))
		}
		if c.BaseURL != "" {
			opts = append(opts, ooption.WithBaseURL(c.BaseURL))
		}
		return &openaiProvider{client: openai.NewClient(opts...), model: c.Model, compatible: c.BaseURL != ""}, nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q", c.Provider)
	}
}

// ErrRefused is returned when the model declines to answer.
var ErrRefused = errors.New("llm: the model refused to answer")

type anthropicProvider struct {
	client anthropic.Client
	model  string
}

func (p *anthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  make([]anthropic.MessageParam, len(req.Messages)),
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for i, m := range req.Messages {
		var blocks []anthropic.ContentBlockParamUnion
		for _, r := range m.ToolResults {
			blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
		}
		if m.Text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Text))
		}
		for _, c := range m.ToolCalls {
			blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, c.Input, c.Name))
		}
		if m.Role == "assistant" {
			params.Messages[i] = anthropic.NewAssistantMessage(blocks...)
		} else {
			params.Messages[i] = anthropic.NewUserMessage(blocks...)
		}
	}
	for _, t := range req.Tools {
		schema := anthropic.ToolInputSchemaParam{Properties: t.InputSchema["properties"], ExtraFields: map[string]any{}}
		for k, v := range t.InputSchema {
			switch k {
			case "properties", "type":
			case "required":
				schema.Required = stringList(v)
			default:
				schema.ExtraFields[k] = v
			}
		}
		tool := anthropic.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tool.Description = anthropic.String(t.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, err
	}
	out := Response{InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}
	if msg.StopReason == anthropic.StopReasonRefusal {
		return out, ErrRefused
	}
	var text []string
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text = append(text, b.Text)
		case anthropic.ToolUseBlock:
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	out.Text = strings.TrimSpace(strings.Join(text, ""))
	return out, nil
}

type openaiProvider struct {
	client openai.Client
	model  string
	// compatible is set for third-party endpoints, which may not know
	// max_completion_tokens yet.
	compatible bool
}

func (p *openaiProvider) Complete(ctx context.Context, req Request) (Response, error) {
	params := openai.ChatCompletionNewParams{Model: p.model}
	if req.System != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		for _, r := range m.ToolResults {
			content := r.Content
			if r.IsError {
				content = "Error: " + content
			}
			params.Messages = append(params.Messages, openai.ToolMessage(content, r.CallID))
		}
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var a openai.ChatCompletionAssistantMessageParam
			if m.Text != "" {
				a.Content.OfString = openai.String(m.Text)
			}
			for _, c := range m.ToolCalls {
				a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID:       c.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: c.Name, Arguments: string(c.Input)},
					},
				})
			}
			params.Messages = append(params.Messages, openai.ChatCompletionMessageParamUnion{OfAssistant: &a})
		case m.Role == "assistant":
			params.Messages = append(params.Messages, openai.AssistantMessage(m.Text))
		case m.Text != "":
			params.Messages = append(params.Messages, openai.UserMessage(m.Text))
		}
	}
	for _, t := range req.Tools {
		f := shared.FunctionDefinitionParam{Name: t.Name, Parameters: shared.FunctionParameters(t.InputSchema)}
		if t.Description != "" {
			f.Description = openai.String(t.Description)
		}
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(f))
	}
	if p.compatible {
		params.MaxTokens = openai.Int(int64(req.MaxTokens))
	} else {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}
	c, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, err
	}
	out := Response{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
	if len(c.Choices) == 0 {
		return out, errors.New("llm: the response has no choices")
	}
	msg := c.Choices[0].Message
	if msg.Refusal != "" {
		return out, ErrRefused
	}
	out.Text = strings.TrimSpace(msg.Content)
	for _, c := range msg.ToolCalls {
		if c.Type != "function" {
			continue
		}
		// Input may not be valid JSON: OpenAI models write it as a string.
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Input: json.RawMessage(c.Function.Arguments)})
	}
	return out, nil
}

// stringList reads a JSON Schema "required" list.
func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		var out []string
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
