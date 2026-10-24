package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if !reflect.DeepEqual(got, Response{Text: "4", InputTokens: 12, OutputTokens: 3}) {
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
	if !reflect.DeepEqual(got, Response{Text: "4", InputTokens: 20, OutputTokens: 1}) {
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

// withTools is a transcript in which the model called a tool and got its
// result.
var withTools = Request{
	MaxTokens: 100,
	Tools: []Tool{{
		Name:        "wx__forecast",
		Description: "Weather for a city",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"city": map[string]any{"type": "string"}},
			"required":             []any{"city"},
			"additionalProperties": false,
		},
	}},
	Messages: []Message{
		{Role: "user", Text: "ada: weather in Oslo?"},
		{Role: "assistant", Text: "Checking.", ToolCalls: []ToolCall{{ID: "call_1", Name: "wx__forecast", Input: json.RawMessage(`{"city":"Oslo"}`)}}},
		{Role: "user", ToolResults: []ToolResult{{CallID: "call_1", Content: "rain", IsError: true}}},
	},
}

func TestAnthropicTools(t *testing.T) {
	srv, body, _ := record(t, "/v1/messages", `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-test",
		"content": [
			{"type": "text", "text": "Again."},
			{"type": "tool_use", "id": "toolu_2", "name": "wx__forecast", "input": {"city": "Bergen"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 30, "output_tokens": 9}
	}`)
	p, _ := New(Config{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test"})
	got, err := p.Complete(context.Background(), withTools)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Again." || len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "toolu_2" ||
		got.ToolCalls[0].Name != "wx__forecast" || string(got.ToolCalls[0].Input) != `{"city": "Bergen"}` {
		t.Fatalf("got %+v", got)
	}
	b := *body
	tool := b["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "wx__forecast" || tool["description"] != "Weather for a city" || schema["type"] != "object" ||
		schema["required"].([]any)[0] != "city" || schema["additionalProperties"] != false || schema["properties"] == nil {
		t.Errorf("tool = %v", tool)
	}
	msgs := b["messages"].([]any)
	use := msgs[1].(map[string]any)["content"].([]any)
	if len(use) != 2 || use[1].(map[string]any)["type"] != "tool_use" || use[1].(map[string]any)["input"].(map[string]any)["city"] != "Oslo" {
		t.Errorf("assistant turn = %v", use)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "call_1" || res["is_error"] != true {
		t.Errorf("result turn = %v", res)
	}
}

func TestOpenAITools(t *testing.T) {
	srv, body, _ := record(t, "/chat/completions", `{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "llama",
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": null,
			"tool_calls": [{"id": "call_2", "type": "function", "function": {"name": "wx__forecast", "arguments": "{\"city\":\"Bergen\"}"}}]}}],
		"usage": {"prompt_tokens": 40, "completion_tokens": 8, "total_tokens": 48}
	}`)
	p, _ := New(Config{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "llama"})
	got, err := p.Complete(context.Background(), withTools)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolCall{{ID: "call_2", Name: "wx__forecast", Input: json.RawMessage(`{"city":"Bergen"}`)}}
	if got.Text != "" || !reflect.DeepEqual(got.ToolCalls, want) {
		t.Fatalf("got %+v", got)
	}
	b := *body
	fn := b["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "wx__forecast" || fn["parameters"].(map[string]any)["required"].([]any)[0] != "city" {
		t.Errorf("tool = %v", fn)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["function"].(map[string]any)["arguments"] != `{"city":"Oslo"}` {
		t.Errorf("assistant turn = %v", msgs[1])
	}
	if res := msgs[2].(map[string]any); res["role"] != "tool" || res["tool_call_id"] != "call_1" || res["content"] != "Error: rain" {
		t.Errorf("result turn = %v", res)
	}
}

func TestUnknownProvider(t *testing.T) {
	if _, err := New(Config{Provider: "nope"}); err == nil {
		t.Fatal("want an error")
	}
}

// recordStream serves one canned event stream and keeps the request body.
func recordStream(t *testing.T, path, stream string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path = %s, want %s", r.URL.Path, path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(stream))
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

func TestAnthropicStreaming(t *testing.T) {
	srv, body := recordStream(t, "/v1/messages", `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Two plus"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" two is 4."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"calc","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\": 4}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`)
	p, _ := New(Config{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "k", Model: "claude-test"})
	req := transcript
	var seen []string
	req.OnText = func(text string) { seen = append(seen, text) }
	got, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"Two plus", "Two plus two is 4."}) {
		t.Errorf("OnText saw %q", seen)
	}
	if got.Text != "Two plus two is 4." || got.InputTokens != 12 || got.OutputTokens != 9 ||
		len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "calc" || string(got.ToolCalls[0].Input) != `{"x": 4}` {
		t.Fatalf("got %+v", got)
	}
	if (*body)["stream"] != true {
		t.Errorf("stream = %v", (*body)["stream"])
	}
}

func TestOpenAIStreaming(t *testing.T) {
	srv, body := recordStream(t, "/chat/completions", `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[{"index":0,"delta":{"role":"assistant","content":"Two plus"}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[{"index":0,"delta":{"content":" two is 4. "}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":6,"total_tokens":26}}

data: [DONE]

`)
	p, _ := New(Config{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "llama"})
	req := transcript
	var seen []string
	req.OnText = func(text string) { seen = append(seen, text) }
	got, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"Two plus", "Two plus two is 4. "}) {
		t.Errorf("OnText saw %q", seen)
	}
	if !reflect.DeepEqual(got, Response{Text: "Two plus two is 4.", InputTokens: 20, OutputTokens: 6}) {
		t.Fatalf("got %+v", got)
	}
	b := *body
	if b["stream"] != true || b["stream_options"].(map[string]any)["include_usage"] != true {
		t.Errorf("body = %v", b)
	}
}
