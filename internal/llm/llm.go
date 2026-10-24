// Package llm calls the model providers behind built-in agents: Anthropic,
// and OpenAI or any OpenAI-compatible endpoint such as Ollama
// (docs/adr/0009-built-in-agents.md). It only does single-turn text
// completion over a transcript; the agent runner decides what to send.
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	ooption "github.com/openai/openai-go/v3/option"
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
type Message struct {
	Role string
	Text string
}

type Request struct {
	System    string
	Messages  []Message
	MaxTokens int
}

type Response struct {
	Text                      string
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
		if m.Role == "assistant" {
			params.Messages[i] = anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text))
		} else {
			params.Messages[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text))
		}
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
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text = append(text, b.Text)
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
		if m.Role == "assistant" {
			params.Messages = append(params.Messages, openai.AssistantMessage(m.Text))
		} else {
			params.Messages = append(params.Messages, openai.UserMessage(m.Text))
		}
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
	return out, nil
}
