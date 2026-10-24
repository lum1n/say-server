package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

func parts(texts ...string) []map[string]any {
	out := []map[string]any{}
	for _, s := range texts {
		out = append(out, map[string]any{"kind": "text", "data": map[string]string{"text": s}})
	}
	return out
}

func TestStreamingAndEdits(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	other := e.room(ada, "other")

	var bad struct{ Error apiError }
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, map[string]any{"parts": parts(), "stream": "final"}, &bad); code != 422 {
		t.Fatalf("final message without parts = %d", code)
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, map[string]any{"parts": parts("x"), "stream": "later"}, &bad); code != 422 {
		t.Fatalf("unknown stream state = %d", code)
	}

	c := dialWS(t, e, bobTok)
	wsSend(t, c, "1", "subscribe", map[string]any{"room_id": r.ID})
	if env := wsRead(t, c); env.Status != "ok" {
		t.Fatalf("subscribe = %+v", env)
	}

	var msg wireEvent
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, map[string]any{"parts": parts(), "stream": "streaming"}, &msg); code != 201 {
		t.Fatalf("start a stream = %d", code)
	}
	if msg.Stream != core.StreamStreaming || len(msg.Parts) != 0 {
		t.Fatalf("started = %+v", msg)
	}
	if env := wsRead(t, c); env.Type != "event" || eventOf(t, env).ID != msg.ID {
		t.Fatalf("start push = %+v", env)
	}

	path := "/rooms/" + r.ID + "/messages/" + msg.ID
	var got wireEvent
	if code := e.do("PATCH", path, ada, map[string]any{"parts": parts("hel"), "stream": "streaming"}, &got); code != 200 {
		t.Fatalf("stream update = %d", code)
	}
	if got.Seq != msg.Seq || got.Stream != core.StreamStreaming || partText(got) != "hel" {
		t.Fatalf("updated = %+v", got)
	}
	// The update is a frame, not a logged event.
	if env := wsRead(t, c); env.Type != "stream" || partText(eventOf(t, env)) != "hel" || eventOf(t, env).ID != msg.ID {
		t.Fatalf("stream frame = %+v", env)
	}
	if code := e.do("PATCH", path, bobTok, map[string]any{"parts": parts("mine")}, &bad); code != 403 {
		t.Fatalf("edit of someone else's message = %d", code)
	}
	if code := e.do("PATCH", path, ada, map[string]any{}, &bad); code != 422 {
		t.Fatalf("stream update without parts = %d", code)
	}
	if code := e.do("PATCH", "/rooms/"+other.ID+"/messages/"+msg.ID, ada, map[string]any{"stream": "final"}, &bad); code != 404 {
		t.Fatalf("edit through another room = %d", code)
	}

	// Ending the stream keeps the streamed parts and logs an edit.
	if code := e.do("PATCH", path, ada, map[string]any{"stream": "final"}, &got); code != 200 {
		t.Fatalf("end = %d", code)
	}
	if got.Stream != core.StreamFinal || partText(got) != "hel" || got.EditedAt != nil {
		t.Fatalf("ended = %+v", got)
	}
	env := wsRead(t, c)
	end := eventOf(t, env)
	if env.Type != "event" || end.Type != core.EventEdit || end.Edits != msg.ID || end.Stream != core.StreamFinal || end.Seq != msg.Seq+1 {
		t.Fatalf("end push = %+v", end)
	}
	if code := e.do("PATCH", path, ada, map[string]any{"parts": parts("again"), "stream": "streaming"}, &bad); code != 409 || bad.Error.Code != "not_streaming" {
		t.Fatalf("restream = %d %+v", code, bad)
	}
	if code := e.do("PATCH", path, ada, map[string]any{"stream": "final"}, &bad); code != 422 {
		t.Fatalf("empty edit of a final message = %d", code)
	}

	// A later edit is an edit too, and marks the message edited.
	if code := e.do("PATCH", path, ada, map[string]any{"parts": parts("hello")}, &got); code != 200 {
		t.Fatalf("edit = %d", code)
	}
	if partText(got) != "hello" || got.EditedAt == nil {
		t.Fatalf("edited = %+v", got)
	}
	if ev := eventOf(t, wsRead(t, c)); ev.Type != core.EventEdit || ev.Stream != "" || partText(ev) != "hello" {
		t.Fatalf("edit push = %+v", ev)
	}
	var page struct{ Events []wireEvent }
	e.do("GET", "/rooms/"+r.ID+"/events", ada, nil, &page)
	if len(page.Events) != 3 || partText(page.Events[0]) != "hello" || page.Events[0].EditedAt == nil {
		t.Fatalf("log = %+v", page.Events)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/messages/"+end.ID, ada, map[string]any{"parts": parts("x")}, &bad); code != 403 {
		t.Fatalf("edit of an edit = %d", code)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/messages/nope", ada, map[string]any{"parts": parts("x")}, &bad); code != 404 {
		t.Fatalf("edit of a missing message = %d", code)
	}
}

func TestSSEStreamFrames(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	r := e.room(ada, "general")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/v1/rooms/"+r.ID+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ada)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	// next returns the next SSE event's name, id and data.
	next := func() (name, id string, ev wireEvent) {
		t.Helper()
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "" && name != "":
				return name, id, ev
			case strings.HasPrefix(line, "event: "):
				name = line[7:]
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(line[6:]), &ev)
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return
	}

	var msg wireEvent
	e.do("POST", "/rooms/"+r.ID+"/messages", ada, map[string]any{"parts": parts("a"), "stream": "streaming"}, &msg)
	if name, id, ev := next(); name != "event" || id != "1" || ev.ID != msg.ID {
		t.Fatalf("start = %s %s %+v", name, id, ev)
	}
	// Give the stream subscription a moment; frames are not replayed.
	time.Sleep(50 * time.Millisecond)
	e.do("PATCH", "/rooms/"+r.ID+"/messages/"+msg.ID, ada, map[string]any{"parts": parts("a b")}, nil)
	if name, id, ev := next(); name != "stream" || id != "" || partText(ev) != "a b" {
		t.Fatalf("frame = %s %q %+v", name, id, ev)
	}
	e.do("PATCH", "/rooms/"+r.ID+"/messages/"+msg.ID, ada, map[string]any{"stream": "final"}, nil)
	if name, id, ev := next(); name != "event" || id != "2" || ev.Edits != msg.ID {
		t.Fatalf("end = %s %s %+v", name, id, ev)
	}
}

func TestMCPWaitsForStreamsToEnd(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	room := e.room(ada, "lab")
	e.join(ada, room.ID, bot.ID, core.RoleMember)
	cs := e.mcp(botTok)

	done := make(chan mcpMessagesOut)
	go func() {
		var out mcpMessagesOut
		call(t, cs, "wait_for_messages", map[string]any{"room_id": room.ID, "after": 0, "timeout_seconds": 10}, &out)
		done <- out
	}()
	time.Sleep(100 * time.Millisecond)
	var msg wireEvent
	e.do("POST", "/rooms/"+room.ID+"/messages", ada, map[string]any{"parts": parts("the answer is"), "stream": "streaming"}, &msg)

	var read mcpMessagesOut
	call(t, cs, "read", map[string]any{"room_id": room.ID}, &read)
	if len(read.Messages) != 1 || !read.Messages[0].Streaming {
		t.Fatalf("read while streaming = %+v", read)
	}
	select {
	case got := <-done:
		t.Fatalf("a streaming message ended the wait: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	e.do("PATCH", "/rooms/"+room.ID+"/messages/"+msg.ID, ada, map[string]any{"parts": parts("the answer is 42"), "stream": "final"}, nil)
	select {
	case got := <-done:
		if len(got.Messages) != 1 || got.Messages[0].ID != msg.ID || got.Messages[0].Text != "the answer is 42" || got.Messages[0].Streaming || got.Cursor != msg.Seq+1 {
			t.Fatalf("wait = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the stream did not end the wait")
	}

	// Read from past the message, the edit brings it whole.
	call(t, cs, "read", map[string]any{"room_id": room.ID, "after": msg.Seq}, &read)
	if len(read.Messages) != 1 || read.Messages[0].Text != "the answer is 42" {
		t.Fatalf("read after = %+v", read)
	}
}
