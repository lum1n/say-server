package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/llm"
	"github.com/lum1n/say-server/internal/store"
)

// weatherServer is an MCP server with one tool, forecast, that only
// answers requests carrying auth. It counts the requests it refused.
func weatherServer(t *testing.T, auth string) (url string, refused *atomic.Int32) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "wx", Version: "test"}, nil)
	type args struct {
		City string `json:"city" jsonschema:"the city"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "forecast", Description: "Weather for a city"},
		func(_ context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "rain in " + a.City}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	refused = &atomic.Int32{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			refused.Add(1)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	return hs.URL, refused
}

// lastResults are the tool results in the request's last turn.
func lastResults(req llm.Request) []llm.ToolResult {
	return req.Messages[len(req.Messages)-1].ToolResults
}

func TestToolCalls(t *testing.T) {
	e := newEnv(t)
	url, refused := weatherServer(t, "Bearer wx-token")
	e.llm.script = func(req llm.Request) llm.Response {
		res := lastResults(req)
		if res == nil {
			return llm.Response{Text: "Let me look.", ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "wx__forecast", Input: json.RawMessage(`{"city":"Oslo"}`)},
				{ID: "c2", Name: "wx__forecast", Input: json.RawMessage(`{"city":`)},
				{ID: "c3", Name: "nope__x"},
				{ID: "c4", Name: "wx__forecast"},
			}}
		}
		var b strings.Builder
		for _, r := range res {
			fmt.Fprintf(&b, "%s error=%v %s; ", r.CallID, r.IsError, r.Content)
		}
		return llm.Response{Text: b.String()}
	}
	e.runner("node-a")
	bot := e.agent("Bot", 0, store.MCPServer{Name: "wx", URL: url, Authorization: "Bearer wx-token"})
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	e.say(r, "@Bot weather in Oslo?")
	evs := e.waitFor(r, "the reply", func(evs []core.Event) bool { return len(evs) == 2 })
	got := evs[1].Text()
	for _, want := range []string{
		"c1 error=false rain in Oslo;",
		"c2 error=true the arguments are not valid JSON;",
		`c3 error=true there is no tool called "nope__x";`,
		`c4 error=true validating "arguments"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reply %q lacks %q", got, want)
		}
	}
	// The reply shows the first round's text, its calls and how they went,
	// then the answer.
	var shown []string
	for _, p := range evs[1].Parts {
		if p.Kind != core.PartToolCall {
			shown = append(shown, string(p.Kind))
			continue
		}
		var c core.ToolCall
		if err := json.Unmarshal(p.Data, &c); err != nil {
			t.Fatal(err)
		}
		shown = append(shown, fmt.Sprintf("%s %s %s %s", c.ID, c.Name, string(c.Input), c.Status))
		if (c.Status == core.ToolFailed) != (c.Error != "") {
			t.Errorf("call %s: status %s, error %q", c.ID, c.Status, c.Error)
		}
	}
	if want := []string{
		"text",
		`c1 wx__forecast {"city":"Oslo"} done`,
		"c2 wx__forecast  failed",
		"c3 nope__x  failed",
		"c4 wx__forecast  failed",
		"text",
	}; strings.Join(shown, "\n") != strings.Join(want, "\n") {
		t.Fatalf("parts:\n%s\nwant:\n%s", strings.Join(shown, "\n"), strings.Join(want, "\n"))
	}
	if e.llm.count() != 2 {
		t.Fatalf("model calls = %d, want 2", e.llm.count())
	}
	first := e.llm.calls[0]
	if len(first.Tools) != 1 || first.Tools[0].Name != "wx__forecast" || first.Tools[0].Description != "Weather for a city" {
		t.Fatalf("tools = %+v", first.Tools)
	}
	if schema := first.Tools[0].InputSchema; schema["type"] != "object" || schema["properties"].(map[string]any)["city"] == nil {
		t.Fatalf("schema = %v", schema)
	}
	// The second call carries the first's tool calls, then their results.
	second := e.llm.last()
	if n := len(second.Messages); second.Messages[n-2].Role != "assistant" || len(second.Messages[n-2].ToolCalls) != 4 || second.Messages[n-2].Text != "Let me look." {
		t.Fatalf("transcript = %+v", second.Messages)
	}
	if refused.Load() != 0 {
		t.Fatalf("the server refused %d requests", refused.Load())
	}
	if a, _ := e.store.GetBuiltinAgent(context.Background(), bot.ID); a.LastError != "" {
		t.Fatalf("last error = %q", a.LastError)
	}
}

// An agent whose tool server is down still answers, without its tools,
// and the admin sees why.
func TestToolServerDown(t *testing.T) {
	e := newEnv(t)
	url, _ := weatherServer(t, "Bearer right")
	e.runner("node-a")
	bot := e.agent("Bot", 0, store.MCPServer{Name: "wx", URL: url, Authorization: "Bearer wrong"})
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	e.say(r, "@Bot hello?")
	e.waitFor(r, "the reply", func(evs []core.Event) bool { return len(evs) == 2 })
	if tools := e.llm.last().Tools; len(tools) != 0 {
		t.Fatalf("tools = %+v", tools)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		a, _ := e.store.GetBuiltinAgent(context.Background(), bot.ID)
		if strings.Contains(a.LastError, `tools from "wx"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last error = %q", a.LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A model that keeps calling tools gets cut off without an answer; the
// reply shows only the calls it made.
func TestToolRoundLimit(t *testing.T) {
	defer func(n int) { maxToolRounds = n }(maxToolRounds)
	maxToolRounds = 3
	e := newEnv(t)
	url, _ := weatherServer(t, "")
	e.llm.script = func(llm.Request) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "c", Name: "wx__forecast", Input: json.RawMessage(`{"city":"Oslo"}`)}}}
	}
	e.runner("node-a")
	bot := e.agent("Bot", 0, store.MCPServer{Name: "wx", URL: url})
	r := e.room(core.RoomGroup, bot)
	time.Sleep(200 * time.Millisecond)

	e.say(r, "@Bot loop")
	evs := e.quiet(r)
	if len(evs) != 2 || evs[1].Text() != "[used wx__forecast]\n[used wx__forecast]" {
		t.Fatalf("events: %s", dump(evs))
	}
	if n := e.llm.count(); n != 3 {
		t.Fatalf("model calls = %d, want 3", n)
	}
	if a, _ := e.store.GetBuiltinAgent(context.Background(), bot.ID); !strings.Contains(a.LastError, "after 3 rounds") {
		t.Fatalf("last error = %q", a.LastError)
	}
}

func TestToolName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"wx", "forecast"}:             "wx__forecast",
		{"my server", "a.b/c"}:         "my_server__a_b_c",
		{"s", strings.Repeat("x", 80)}: "s__" + strings.Repeat("x", 61),
	} {
		if got := toolName(in[0], in[1]); got != want {
			t.Errorf("toolName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
