package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/vegard/say-v2/internal/bridge"
)

const token = "bot-token"

// call is a REST request the fake got.
type call struct {
	Method, Path, Query string
	Body                map[string]any
	Raw                 string
}

// fakeDiscord is the REST API and a Gateway.
type fakeDiscord struct {
	t   *testing.T
	srv *httptest.Server
	// frames are written to the open gateway connection.
	frames chan any

	mu        sync.Mutex
	interval  int // heartbeat interval in ms
	noAck     bool
	identify  []map[string]any
	beats     int
	calls     []call
	hooks     map[string][]map[string]any // channel: webhooks
	noHooks   map[string]bool             // channels where webhooks are forbidden
	history   map[string][]message        // channel: messages, any order
	limitExec int                         // 429s to give webhook execution first
	execErr   int                         // status for webhook execution
	next      int
}

func newFake(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{
		t:        t,
		frames:   make(chan any, 16),
		interval: 30,
		hooks:    map[string][]map[string]any{},
		noHooks:  map[string]bool{},
		history:  map[string][]message{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", f.rest)
	mux.HandleFunc("/gateway", f.gateway)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) with(fn func(f *fakeDiscord)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeDiscord) rest(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if parts[0] != "webhooks" && r.Header.Get("Authorization") != "Bot "+token {
		reply(w, 401, map[string]any{"code": 0, "message": "401: Unauthorized"})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Method: r.Method, Path: path, Query: r.URL.RawQuery, Body: body, Raw: string(raw)})
	switch {
	case r.Method == "GET" && path == "/gateway/bot":
		reply(w, 200, map[string]any{"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/gateway"})
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		after := r.URL.Query().Get("after")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var out []message
		for _, m := range f.history[parts[1]] {
			if snowLess(after, m.ID) {
				out = append(out, m)
			}
		}
		sortByID(out)
		if len(out) > limit {
			out = out[:limit]
		}
		// Discord lists newest first.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		reply(w, 200, out)
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "webhooks":
		ch := parts[1]
		if f.noHooks[ch] {
			reply(w, 403, map[string]any{"code": 50013, "message": "Missing Permissions"})
			return
		}
		if r.Method == "GET" {
			reply(w, 200, append([]map[string]any{}, f.hooks[ch]...))
			return
		}
		f.next++
		h := map[string]any{"id": fmt.Sprintf("H%d", f.next), "type": 1, "token": fmt.Sprintf("tok%d", f.next), "name": body["name"], "application_id": "APP"}
		f.hooks[ch] = append(f.hooks[ch], h)
		reply(w, 200, h)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "webhooks":
		if f.limitExec > 0 {
			f.limitExec--
			reply(w, 429, map[string]any{"message": "You are being rate limited.", "retry_after": 0.05, "global": false})
			return
		}
		if f.execErr != 0 {
			reply(w, f.execErr, map[string]any{"code": 50035, "message": "Invalid Form Body"})
			return
		}
		f.next++
		reply(w, 200, map[string]any{"id": fmt.Sprintf("M%d", f.next), "webhook_id": parts[1]})
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		f.next++
		reply(w, 200, map[string]any{"id": fmt.Sprintf("M%d", f.next)})
	case r.Method == "PATCH":
		reply(w, 200, map[string]any{"id": parts[len(parts)-1]})
	default:
		reply(w, 404, map[string]any{"code": 0, "message": "404: Not Found"})
	}
}

// called returns the calls with a method and path prefix.
func (f *fakeDiscord) called(method, prefix string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method && strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDiscord) gateway(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f.mu.Lock()
	interval := f.interval
	f.mu.Unlock()
	if r.URL.Query().Get("v") != "10" || r.URL.Query().Get("encoding") != "json" {
		f.t.Errorf("gateway query = %q", r.URL.RawQuery)
	}
	wsjson.Write(ctx, ws, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": interval}})
	var id map[string]any
	if err := wsjson.Read(ctx, ws, &id); err != nil {
		return
	}
	f.with(func(f *fakeDiscord) { f.identify = append(f.identify, id) })
	wsjson.Write(ctx, ws, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
		"user": map[string]any{"id": "BOT", "username": "saybot"}, "application": map[string]any{"id": "APP"},
	}})
	wsjson.Write(ctx, ws, map[string]any{"op": 0, "s": 2, "t": "GUILD_CREATE", "d": map[string]any{
		"channels": []map[string]any{{"id": "C1", "name": "general"}, {"id": "C2", "name": "random"}, {"id": "222", "name": "random"}},
	}})
	go func() {
		defer cancel()
		for {
			var fr frame
			if err := wsjson.Read(ctx, ws, &fr); err != nil {
				return
			}
			if fr.Op != opHeartbeat {
				continue
			}
			f.mu.Lock()
			f.beats++
			ack := !f.noAck
			f.mu.Unlock()
			if ack {
				wsjson.Write(ctx, ws, map[string]any{"op": 11})
			}
		}
	}()
	seq := 2
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-f.frames:
			if m, ok := fr.(map[string]any); ok && m["op"] == 0 {
				seq++
				m["s"] = seq
			}
			if err := wsjson.Write(ctx, ws, fr); err != nil {
				return
			}
		}
	}
}

func dispatch(t string, d any) map[string]any {
	return map[string]any{"op": 0, "t": t, "d": d}
}

// fakeHost is the core.
type fakeHost struct {
	mu        sync.Mutex
	chats     []string
	got       []bridge.Incoming
	states    []string
	connected int
}

func (h *fakeHost) Receive(ctx context.Context, m bridge.Incoming) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, m)
	return nil
}

func (h *fakeHost) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	return "", errors.New("no prompts expected")
}

func (h *fakeHost) SaveState(ctx context.Context, state json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states = append(h.states, string(state))
	return nil
}

func (h *fakeHost) Chats(ctx context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.chats...), nil
}

func (h *fakeHost) Connected(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected++
}

func (h *fakeHost) received() []bridge.Incoming {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bridge.Incoming(nil), h.got...)
}

func (h *fakeHost) lastState() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		return ""
	}
	return h.states[len(h.states)-1]
}

func newConn(t *testing.T, f *fakeDiscord, h bridge.Host, st string) *conn {
	t.Helper()
	bc, err := open(bridge.Info{ID: "b1", Mode: "bot", Config: json.RawMessage(`{"bot_token":"` + token + `"}`), State: json.RawMessage(st)}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	c.api = f.srv.URL + "/api"
	c.saveEvery = 50 * time.Millisecond
	return c
}

// run runs c until the test ends. stop ends it and waits; done is closed
// when Run returns, with its error in *err.
func run(t *testing.T, c *conn) (stop func() error, done <-chan struct{}, err *error) {
	ctx, cancel := context.WithCancel(context.Background())
	fin := make(chan struct{})
	var runErr error
	go func() {
		runErr = c.Run(ctx)
		close(fin)
	}()
	stop = func() error {
		cancel()
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
		return runErr
	}
	t.Cleanup(func() { stop() })
	return stop, fin, &runErr
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ids are snowflakes a little in the future, so they sort after the
// place a fresh channel starts from.
var idBase, _ = strconv.ParseUint(snowflake(time.Now().Add(time.Minute)), 10, 64)

func id(n int) string { return strconv.FormatUint(idBase+uint64(n), 10) }

func msg(ch, mid, author, text string) map[string]any {
	return map[string]any{
		"id": mid, "channel_id": ch, "type": 0, "content": text,
		"timestamp": "2026-10-24T12:00:00.000000+00:00",
		"author":    map[string]any{"id": author, "username": strings.ToLower(author), "global_name": author},
	}
}

func TestRunReceives(t *testing.T) {
	f := newFake(t)
	h := &fakeHost{chats: []string{"C1"}}
	c := newConn(t, f, h, "")
	run(t, c)
	eventually(t, "connected", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.connected == 1 })

	f.mu.Lock()
	ident := f.identify[0]
	f.mu.Unlock()
	if ident["op"] != float64(2) {
		t.Fatalf("identify op = %v", ident["op"])
	}
	d := ident["d"].(map[string]any)
	if d["token"] != token || d["intents"] != float64(1|1<<9|1<<15) {
		t.Fatalf("identify = %v", d)
	}

	// The bridge's webhook, made by a send.
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "from say", Sender: "Ada"}); err != nil {
		t.Fatal(err)
	}
	hook := f.called("POST", "/channels/C1/webhooks")
	if len(hook) != 1 {
		t.Fatalf("webhook creates = %d", len(hook))
	}

	m := msg("C1", id(1), "U2", "hi <@33> and <@!44>, see <#222> <:wave:123> <@55>")
	m["mentions"] = []map[string]any{{"id": "33", "username": "cy"}, {"id": "44", "username": "dee", "global_name": "Dee"}}
	m["attachments"] = []map[string]any{{"filename": "cat.png"}}
	f.frames <- dispatch("MESSAGE_CREATE", m)
	f.frames <- dispatch("MESSAGE_CREATE", msg("C9", id(2), "U2", "unlinked"))
	f.frames <- dispatch("MESSAGE_CREATE", msg("C1", id(3), "BOT", "**Ada**: as the bot"))
	own := msg("C1", id(4), "H1", "from say")
	own["webhook_id"] = "H1"
	f.frames <- dispatch("MESSAGE_CREATE", own)
	other := msg("C1", id(5), "W9", "relayed")
	other["webhook_id"] = "W9"
	other["author"] = map[string]any{"id": "W9", "username": "Other relay"}
	f.frames <- dispatch("MESSAGE_CREATE", other)
	preview := msg("C1", id(1), "U2", "link preview")
	f.frames <- dispatch("MESSAGE_UPDATE", preview)
	edit := msg("C1", id(1), "U2", "hi again")
	edit["edited_timestamp"] = "2026-10-24T12:01:00.000000+00:00"
	f.frames <- dispatch("MESSAGE_UPDATE", edit)
	reply := msg("C1", id(6), "U3", "")
	reply["type"] = 19
	reply["message_reference"] = map[string]any{"message_id": id(1)}
	reply["content"] = "yes"
	f.frames <- dispatch("MESSAGE_CREATE", reply)
	join := msg("C1", id(7), "U5", "")
	join["type"] = 7
	f.frames <- dispatch("MESSAGE_CREATE", join)
	f.frames <- dispatch("MESSAGE_CREATE", msg("C1", id(8), "U2", "last"))

	eventually(t, "the last message", func() bool {
		got := h.received()
		return len(got) > 0 && got[len(got)-1].ID == id(8)
	})
	got := h.received()
	if len(got) != 7 {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	first := got[0]
	if first.Chat.ID != "C1" || first.Chat.Name != "general" || first.Author != (bridge.Author{ID: "U2", Name: "U2"}) {
		t.Errorf("first = %+v", first)
	}
	if want := "hi @cy and @Dee, see #random :wave: <@55>\n[attachment: cat.png]"; first.Text != want {
		t.Errorf("text = %q, want %q", first.Text, want)
	}
	if first.Time.IsZero() {
		t.Error("no time")
	}
	if !got[1].Author.Self || !got[2].Author.Self {
		t.Errorf("own messages not self: %+v %+v", got[1].Author, got[2].Author)
	}
	if got[3].Author != (bridge.Author{ID: "W9", Name: "Other relay"}) {
		t.Errorf("other webhook = %+v", got[3].Author)
	}
	if !got[4].Edit || got[4].ID != id(1) || got[4].Text != "hi again" {
		t.Errorf("edit = %+v", got[4])
	}
	if got[5].ReplyTo != id(1) || got[5].Text != "yes" {
		t.Errorf("reply = %+v", got[5])
	}

	eventually(t, "heartbeats", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.beats >= 2 })
	eventually(t, "state saved", func() bool { return strings.Contains(h.lastState(), id(8)) })
	if !strings.Contains(h.lastState(), `"hooks":{"C1":"H1"}`) {
		t.Errorf("state = %s", h.lastState())
	}
}

func TestSendWebhook(t *testing.T) {
	f := newFake(t)
	c := newConn(t, f, &fakeHost{}, "")
	ctx := context.Background()
	rid, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "hello @everyone", Sender: "My Discord  Clyde"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "again", Sender: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.called("POST", "/channels/C1/webhooks")); n != 1 {
		t.Fatalf("webhook creates = %d", n)
	}
	exec := f.called("POST", "/webhooks/H1/tok1")
	if len(exec) != 2 {
		t.Fatalf("executions = %d", len(exec))
	}
	e := exec[0]
	if e.Query != "wait=true" || e.Body["content"] != "hello @everyone" || e.Body["username"] != "My D.iscord C.lyde" {
		t.Errorf("execute = %+v", e)
	}
	if !strings.Contains(e.Raw, `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("allowed_mentions = %s", e.Raw)
	}
	if rid != "M2" {
		t.Errorf("remote id = %q", rid)
	}
}

func TestSendFindsWebhook(t *testing.T) {
	f := newFake(t)
	f.hooks["C1"] = []map[string]any{
		{"id": "X", "type": 1, "token": "x", "name": "say", "application_id": "OTHER"},
		{"id": "Y", "type": 1, "token": "y", "name": "say", "application_id": "APP"},
	}
	c := newConn(t, f, &fakeHost{}, "")
	c.self, c.appID = "BOT", "APP"
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.called("POST", "/channels/C1/webhooks")); n != 0 {
		t.Errorf("webhook creates = %d", n)
	}
	if n := len(f.called("POST", "/webhooks/Y/y")); n != 1 {
		t.Errorf("executions = %d", n)
	}
}

func TestSendFallsBackToBot(t *testing.T) {
	f := newFake(t)
	f.noHooks["C1"] = true
	c := newConn(t, f, &fakeHost{}, "")
	rid, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada_B"})
	if err != nil {
		t.Fatal(err)
	}
	posts := f.called("POST", "/channels/C1/messages")
	if len(posts) != 1 || posts[0].Body["content"] != `**Ada\_B**: hi` || rid != "M1" {
		t.Fatalf("posts = %+v, id %q", posts, rid)
	}
	// The answer is remembered: no second webhook lookup.
	c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "again", Sender: "Ada"})
	if n := len(f.called("GET", "/channels/C1/webhooks")); n != 1 {
		t.Errorf("webhook lookups = %d", n)
	}
	// An edit of a bot message goes to the channel.
	if err := c.Edit(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi!", Sender: "Ada"}, rid); err != nil {
		t.Fatal(err)
	}
	if p := f.called("PATCH", "/channels/C1/messages/"+rid); len(p) != 1 || p[0].Body["content"] != "**Ada**: hi!" {
		t.Errorf("edit = %+v", p)
	}
}

func TestSendRateLimited(t *testing.T) {
	f := newFake(t)
	f.limitExec = 2
	c := newConn(t, f, &fakeHost{}, "")
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.called("POST", "/webhooks/")); n != 3 {
		t.Errorf("executions = %d", n)
	}

	// One that stays limited gives up, refused.
	f.with(func(f *fakeDiscord) { f.limitExec = 100 })
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("err = %v", err)
	}
}

func TestSendErrors(t *testing.T) {
	f := newFake(t)
	f.execErr = 400
	c := newConn(t, f, &fakeHost{}, "")
	_, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"})
	if !errors.Is(err, bridge.ErrRejected) || strings.Contains(err.Error(), "tok1") {
		t.Errorf("400: err = %v", err)
	}
	f.with(func(f *fakeDiscord) { f.execErr = 502 })
	_, err = c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"})
	if err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Errorf("502: err = %v", err)
	}
}

func TestEditWebhook(t *testing.T) {
	f := newFake(t)
	c := newConn(t, f, &fakeHost{}, "")
	ctx := context.Background()
	rid, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "hi", Sender: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(ctx, bridge.Outgoing{Chat: "C1", Text: "hi there", Sender: "Ada"}, rid); err != nil {
		t.Fatal(err)
	}
	p := f.called("PATCH", "/webhooks/H1/tok1/messages/"+rid)
	if len(p) != 1 || p[0].Body["content"] != "hi there" {
		t.Fatalf("edits = %+v", p)
	}
	if n := len(f.called("PATCH", "/channels/")); n != 0 {
		t.Errorf("channel edits = %d", n)
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", maxText+10)
	got := clip(long)
	if r := []rune(got); len(r) != maxText || r[len(r)-1] != '…' {
		t.Errorf("clip: %d runes", len(r))
	}
	if username("") != "someone" || len([]rune(username(strings.Repeat("a", 100)))) != 80 {
		t.Error("username bounds")
	}
}

func TestCatchUp(t *testing.T) {
	f := newFake(t)
	h := &fakeHost{chats: []string{"C1", "C2"}}
	c := newConn(t, f, h, "")
	stop, _, _ := run(t, c)
	eventually(t, "connected", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.connected == 1 })
	f.frames <- dispatch("MESSAGE_CREATE", msg("C1", id(10), "U2", "live"))
	eventually(t, "the live message", func() bool { return len(h.received()) == 1 })
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop: %v", err)
	}
	if n := len(f.called("GET", "/channels/C1/messages")); n != 0 {
		t.Errorf("a channel seen for the first time was backfilled")
	}
	st := h.lastState()
	if !strings.Contains(st, id(10)) || !strings.Contains(st, `"C2"`) {
		t.Fatalf("state = %s", st)
	}

	// Missed while away.
	f.with(func(f *fakeDiscord) {
		f.history["C1"] = []message{
			{ID: id(9), ChannelID: "C1", Content: ptr("old"), Author: &user{ID: "U2", Username: "bob"}},
			{ID: id(12), ChannelID: "C1", Content: ptr("second"), Author: &user{ID: "U2", Username: "bob"}},
			{ID: id(11), ChannelID: "C1", Content: ptr("first"), Author: &user{ID: "U3", Username: "cy"}},
		}
	})
	h2 := &fakeHost{chats: []string{"C1", "C2"}}
	c2 := newConn(t, f, h2, st)
	run(t, c2)
	eventually(t, "caught up", func() bool { return len(h2.received()) == 2 })
	got := h2.received()
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("caught up = %+v", got)
	}
	q := f.called("GET", "/channels/C1/messages")
	if len(q) != 1 || !strings.Contains(q[0].Query, "after="+id(10)) {
		t.Errorf("history calls = %+v", q)
	}
	eventually(t, "state saved", func() bool { return strings.Contains(h2.lastState(), id(12)) })
}

func TestCatchUpPages(t *testing.T) {
	f := newFake(t)
	for i := 1; i <= 150; i++ {
		f.history["C1"] = append(f.history["C1"], message{ID: id(i), Content: ptr(strconv.Itoa(i)), Author: &user{ID: "U2"}})
	}
	h := &fakeHost{chats: []string{"C1"}}
	c := newConn(t, f, h, `{"last":{"C1":"`+id(0)+`"}}`)
	run(t, c)
	eventually(t, "caught up", func() bool { return len(h.received()) == 150 })
	got := h.received()
	for i, m := range got {
		if m.Text != strconv.Itoa(i+1) {
			t.Fatalf("message %d = %q", i, m.Text)
		}
	}
	if n := len(f.called("GET", "/channels/C1/messages")); n != 2 {
		t.Errorf("pages = %d", n)
	}
}

func TestMissedHeartbeat(t *testing.T) {
	f := newFake(t)
	f.noAck = true
	c := newConn(t, f, &fakeHost{}, "")
	_, done, err := run(t, c)
	select {
	case <-done:
		if *err == nil || !strings.Contains((*err).Error(), "heartbeats") {
			t.Errorf("err = %v", *err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail")
	}
}

func TestReconnectAsked(t *testing.T) {
	f := newFake(t)
	h := &fakeHost{}
	c := newConn(t, f, h, "")
	_, done, err := run(t, c)
	eventually(t, "connected", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.connected == 1 })
	f.frames <- map[string]any{"op": 7, "d": nil}
	select {
	case <-done:
		if *err == nil || !strings.Contains((*err).Error(), "reconnect") {
			t.Errorf("err = %v", *err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestBadToken(t *testing.T) {
	f := newFake(t)
	bc, err := open(bridge.Info{Config: json.RawMessage(`{"bot_token":"wrong"}`)}, &fakeHost{})
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	c.api = f.srv.URL + "/api"
	err = c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "token was refused") {
		t.Errorf("err = %v", err)
	}
}

func ptr(s string) *string { return &s }
