package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// record serves one canned JSON response and keeps the request it got.
func record(t *testing.T, path, resp string) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()
	var body map[string]any
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path = %s, want %s", r.URL.Path, path)
		}
		header = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &body, &header
}

var transcript = Request{
	System:    "be brief",
	MaxTokens: 100,
	Messages: []Message{
		{Role: "user", Text: "ada: hi"},
		{Role: "assistant", Text: "hello"},
		{Role: "user", Text: "ada: 2+2?"},
	},
}

func TestAnthropic(t *testing.T) {
	srv, body, header := record(t, "/v1/messages", `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
		"content": [{"type": "text", "text": " 4 "}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 3}
	}`)
	p, err := New(Config{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Complete(context.Background(), transcript)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Response{Text: "4", InputTokens: 12, OutputTokens: 3}) {
		t.Fatalf("got %+v", got)
	}
	if k := header.Get("X-Api-Key"); k != "k" {
		t.Errorf("x-api-key = %q", k)
	}
	b := *body
	if b["model"] != "claude-test" || b["max_tokens"] != 100.0 {
		t.Errorf("body = %v", b)
	}
	if msgs := b["messages"].([]any); len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("messages = %v", msgs)
	}
	if sys := b["system"].([]any); sys[0].(map[string]any)["text"] != "be brief" {
		t.Errorf("system = %v", sys)
	}
}

func TestAnthropicRefusal(t *testing.T) {
	srv, _, _ := record(t, "/v1/messages", `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
		"content": [], "stop_reason": "refusal",
		"usage": {"input_tokens": 5, "output_tokens": 0}
	}`)
	p, _ := New(Config{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test"})
	got, err := p.Complete(context.Background(), transcript)
	if !errors.Is(err, ErrRefused) || got.InputTokens != 5 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestOpenAICompatible(t *testing.T) {
	srv, body, header := record(t, "/chat/completions", `{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "llama",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "4"}}],
		"usage": {"prompt_tokens": 20, "completion_tokens": 1, "total_tokens": 21}
	}`)
	p, err := New(Config{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "llama"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Complete(context.Background(), transcript)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Response{Text: "4", InputTokens: 20, OutputTokens: 1}) {
		t.Fatalf("got %+v", got)
	}
	if a := header.Get("Authorization"); a != "Bearer none" {
		t.Errorf("authorization = %q", a)
	}
	b := *body
	if b["max_tokens"] != 100.0 || b["max_completion_tokens"] != nil {
		t.Errorf("a compatible endpoint should get max_tokens: %v", b)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestUnknownProvider(t *testing.T) {
	if _, err := New(Config{Provider: "nope"}); err == nil {
		t.Fatal("want an error")
	}
}
