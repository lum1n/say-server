package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lum1n/say-server/internal/core"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) mcp(token string) *mcp.ClientSession {
	e.t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             e.ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and decodes its structured result into out. It returns
// the error text when the tool fails.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		return res.Content[0].(*mcp.TextContent).Text
	}
	if out != nil {
		data, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s: %v in %s", tool, err, data)
		}
	}
	return ""
}

func TestMCPAgentTakesPart(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	room := e.room(ada, "lab")
	e.join(ada, room.ID, bot.ID, core.RoleMember)
	cs := e.mcp(botTok)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "create_room,list_rooms,read,send,wait_for_messages" {
		t.Fatalf("tools = %s", got)
	}

	var rooms mcpListRoomsOut
	call(t, cs, "list_rooms", nil, &rooms)
	if rooms.You.ID != bot.ID || len(rooms.Rooms) != 1 || rooms.Rooms[0].ID != room.ID {
		t.Fatalf("list_rooms = %+v", rooms)
	}

	hello := e.say(ada, room.ID, "hello bot")
	var read mcpMessagesOut
	call(t, cs, "read", map[string]any{"room_id": room.ID}, &read)
	if len(read.Messages) != 1 || read.Messages[0].Text != "hello bot" || read.Messages[0].SenderName != "ada" || read.Cursor != hello.Seq {
		t.Fatalf("read = %+v", read)
	}

	var sent mcpMessage
	call(t, cs, "send", map[string]any{"room_id": room.ID, "text": "hi ada", "reply_to": hello.ID}, &sent)
	if sent.ReplyTo != hello.ID || sent.HopCount != 1 || sent.SenderKind != "agent" {
		t.Fatalf("send = %+v", sent)
	}

	// The bot's own reply does not end a wait; ada's next message does.
	done := make(chan mcpMessagesOut)
	go func() {
		var out mcpMessagesOut
		call(t, cs, "wait_for_messages", map[string]any{"room_id": room.ID, "after": read.Cursor, "timeout_seconds": 10}, &out)
		done <- out
	}()
	time.Sleep(100 * time.Millisecond)
	next := e.say(ada, room.ID, "thanks")
	select {
	case got := <-done:
		if len(got.Messages) != 2 || got.Messages[0].Text != "hi ada" || got.Messages[1].Text != "thanks" || got.Cursor != next.Seq {
			t.Fatalf("wait = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait_for_messages did not return")
	}

	// Only its own message arrives: the wait times out empty but moves the
	// cursor past it.
	call(t, cs, "send", map[string]any{"room_id": room.ID, "text": "anything else?"}, &sent)
	var idle mcpMessagesOut
	call(t, cs, "wait_for_messages", map[string]any{"room_id": room.ID, "after": next.Seq, "timeout_seconds": 1}, &idle)
	if len(idle.Messages) != 0 || idle.Cursor != sent.Seq {
		t.Fatalf("idle wait = %+v", idle)
	}

	var created mcpRoom
	call(t, cs, "create_room", map[string]any{"name": "bots"}, &created)
	if created.Kind != core.RoomGroup || created.Name != "bots" {
		t.Fatalf("create_room = %+v", created)
	}
}

func TestMCPLimits(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	room := e.room(ada, "lab")
	other := e.room(ada, "private")
	e.join(ada, room.ID, bot.ID, core.RoleMember)
	cs := e.mcp(botTok)

	if msg := call(t, cs, "read", map[string]any{"room_id": other.ID}, nil); msg != "no such room" {
		t.Errorf("read of a room it is not in = %q", msg)
	}
	// The test server allows 2 automated hops.
	cause := e.say(ada, room.ID, "go")
	for i := range 3 {
		var sent mcpMessage
		msg := call(t, cs, "send", map[string]any{"room_id": room.ID, "text": "again", "reply_to": cause.ID}, &sent)
		if i < 2 && msg != "" {
			t.Fatalf("hop %d: %s", i+1, msg)
		}
		if i == 2 && !strings.Contains(msg, "limit of 2") {
			t.Fatalf("hop 3 = %q, want the hop limit", msg)
		}
		cause.ID = sent.ID
	}
}

func TestMCPNeedsAgentToken(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("ada", core.PrincipalHuman)
	for _, tc := range []struct{ name, auth string }{{"none", ""}, {"human", "Bearer " + ada}, {"bogus", "Bearer say_nope"}} {
		req, _ := http.NewRequest("POST", e.ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		// A signed-in browser's cookie does not count either.
		req.AddCookie(&http.Cookie{Name: TokenCookie, Value: ada})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, res.StatusCode)
		}
	}
}
