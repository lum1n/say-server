package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/lum1n/say-v2/internal/bridge"
)

const (
	userToken = "xoxp-test"
	appToken  = "xapp-test"
)

// fakeSlack is the Web API and a Socket Mode endpoint.
type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server
	// frames are written to the open socket.
	frames chan any

	mu        sync.Mutex
	convs     []channel
	users     map[string]string
	history   map[string][]message // oldest first
	oldest    map[string]string    // last oldest asked per chat
	posted    []url.Values
	updated   []url.Values
	acks      []string
	sockets   int
	postErr   string
	limitPost int // 429s to give chat.postMessage first
	// convGate, if set, holds users.conversations until it is closed.
	convGate chan struct{}
	// failHistory makes conversations.history fail with HTTP 500.
	failHistory bool
	next        int64
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{
		t:       t,
		frames:  make(chan any, 16),
		users:   map[string]string{"UME": "Me", "U2": "Bob", "U3": "Cy"},
		history: map[string][]message{},
		oldest:  map[string]string{},
		next:    time.Now().Unix(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/{method}", f.api)
	mux.HandleFunc("/socket", f.socket)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (f *fakeSlack) api(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	r.ParseForm()
	want := userToken
	if method == "apps.connections.open" {
		want = appToken
	}
	if r.Header.Get("Authorization") != "Bearer "+want {
		f.reply(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	f.mu.Lock()
	gate := f.convGate
	f.mu.Unlock()
	if gate != nil && method == "users.conversations" {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "auth.test":
		f.reply(w, map[string]any{"ok": true, "user_id": "UME"})
	case "apps.connections.open":
		f.reply(w, map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/socket"})
	case "users.conversations":
		f.reply(w, map[string]any{"ok": true, "channels": f.convs})
	case "conversations.info":
		for _, c := range f.convs {
			if c.ID == r.Form.Get("channel") {
				f.reply(w, map[string]any{"ok": true, "channel": c})
				return
			}
		}
		f.reply(w, map[string]any{"ok": false, "error": "channel_not_found"})
	case "users.info":
		n, ok := f.users[r.Form.Get("user")]
		if !ok {
			f.reply(w, map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		f.reply(w, map[string]any{"ok": true, "user": map[string]any{"name": strings.ToLower(n), "profile": map[string]any{"display_name": n}}})
	case "conversations.history":
		if f.failHistory {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ch, oldest := r.Form.Get("channel"), r.Form.Get("oldest")
		f.oldest[ch] = oldest
		var limit int
		fmt.Sscan(r.Form.Get("limit"), &limit)
		var out []message
		for _, m := range f.history[ch] {
			if tsLess(oldest, m.Ts) {
				out = append(out, m)
			}
		}
		sort.Slice(out, func(i, j int) bool { return tsLess(out[j].Ts, out[i].Ts) })
		if len(out) > limit {
			out = out[:limit]
		}
		f.reply(w, map[string]any{"ok": true, "messages": out, "has_more": false})
	case "chat.postMessage":
		if f.limitPost > 0 {
			f.limitPost--
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if f.postErr != "" {
			f.reply(w, map[string]any{"ok": false, "error": f.postErr})
			return
		}
		f.posted = append(f.posted, r.Form)
		ts := f.ts()
		// Slack sends the user's own message back as an event.
		f.frames <- event("echo-"+ts, message{Type: "message", Channel: r.Form.Get("channel"), User: "UME", Text: r.Form.Get("text"), Ts: ts})
		f.reply(w, map[string]any{"ok": true, "channel": r.Form.Get("channel"), "ts": ts})
	case "chat.update":
		f.updated = append(f.updated, r.Form)
		f.reply(w, map[string]any{"ok": true})
	default:
		f.reply(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

// ts returns a fresh message ts.
func (f *fakeSlack) ts() string {
	f.next++
	return fmt.Sprintf("%d.000100", f.next)
}

func (f *fakeSlack) socket(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	f.mu.Lock()
	f.sockets++
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if err := wsjson.Read(ctx, ws, &ack); err != nil {
				return
			}
			f.mu.Lock()
			f.acks = append(f.acks, ack.EnvelopeID)
			f.mu.Unlock()
		}
	}()
	wsjson.Write(ctx, ws, map[string]any{"type": "hello"})
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-f.frames:
			if err := wsjson.Write(ctx, ws, fr); err != nil {
				return
			}
			if m, ok := fr.(map[string]any); ok && m["type"] == "disconnect" {
				// The client goes on a new connection; this one
				// lingers until it is closed.
				<-ctx.Done()
				return
			}
		}
	}
}

// event wraps a message event as Socket Mode does.
func event(id string, m message) map[string]any {
	return map[string]any{
		"envelope_id": id,
		"type":        "events_api",
		"payload":     map[string]any{"api_app_id": "AOWN", "event": m},
	}
}

func (f *fakeSlack) with(fn func(f *fakeSlack)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeHost is the core.
type fakeHost struct {
	mu        sync.Mutex
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

func (h *fakeHost) Chats(ctx context.Context) ([]string, error) { return nil, nil }

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

func newConn(t *testing.T, f *fakeSlack, h bridge.Host, st string) *conn {
	t.Helper()
	cfg := fmt.Sprintf(`{"user_token":%q,"app_token":%q}`, userToken, appToken)
	bc, err := open(bridge.Info{ID: "b1", Config: json.RawMessage(cfg), State: json.RawMessage(st)}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	c.api = f.srv.URL + "/api/"
	c.saveEvery = 50 * time.Millisecond
	return c
}

// run runs c until the test ends and returns a stop that waits for it.
func run(t *testing.T, c *conn) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Error("Run did not return")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() })
	return stop
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

func ago(d time.Duration) string { return formatTs(time.Now().Add(-d)) }

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func TestRun(t *testing.T) {
	f := newFakeSlack(t)
	f.convs = []channel{
		{ID: "D1", IsIM: true, User: "U2"},
		{ID: "C1", Name: "general", IsMember: yes()},
		{ID: "C2", Name: "random", IsMember: no()},
		{ID: "G1", Name: "mpdm-me--bob--cy-1", IsMPIM: true},
	}
	t1, t2, t3 := ago(2*time.Hour), ago(time.Hour), ago(30*time.Minute)
	f.history["D1"] = []message{
		{Type: "message", User: "U2", Text: "too old", Ts: ago(40 * 24 * time.Hour)},
		{Type: "message", User: "U2", Text: "hi", Ts: t1},
		{Type: "message", User: "UME", Text: "hello from my phone", Ts: t2},
	}
	f.history["C1"] = []message{
		{Type: "message", Subtype: "channel_join", User: "U3", Text: "<@U3> has joined", Ts: t1},
		{Type: "message", User: "U3", Text: "<@U2> see <#C1|general> &amp; <https://x.test|this>", Ts: t3,
			Files: []file{{Name: "a.png", Permalink: "https://files.test/a"}}},
	}
	f.history["C2"] = []message{{Type: "message", User: "U2", Text: "not mine", Ts: t1}}
	h := &fakeHost{}
	c := newConn(t, f, h, "")
	run(t, c)

	eventually(t, "the backfill arrives", func() bool { return len(h.received()) == 3 })
	got := h.received()
	byID := map[string]bridge.Incoming{}
	for _, m := range got {
		byID[m.Chat.ID+"/"+m.ID] = m
	}
	hi, phone, gen := byID["D1/"+t1], byID["D1/"+t2], byID["C1/"+t3]
	if hi.Text != "hi" || hi.Author != (bridge.Author{ID: "U2", Name: "Bob"}) || hi.Chat != (bridge.RemoteChat{ID: "D1", Name: "Bob", Direct: true}) {
		t.Errorf("hi = %+v", hi)
	}
	if !phone.Author.Self || phone.Text != "hello from my phone" {
		t.Errorf("own message = %+v", phone)
	}
	if !tsTime(t1).Equal(hi.Time) {
		t.Errorf("time = %v, want %v", hi.Time, tsTime(t1))
	}
	if want := "@Bob see #general & this (https://x.test)\n[file: a.png] https://files.test/a"; gen.Text != want {
		t.Errorf("text = %q, want %q", gen.Text, want)
	}
	if gen.Chat.Name != "general" || gen.Chat.Direct {
		t.Errorf("chat = %+v", gen.Chat)
	}
	// Oldest first within a chat.
	var d1 []string
	for _, m := range got {
		if m.Chat.ID == "D1" {
			d1 = append(d1, m.ID)
		}
	}
	if len(d1) != 2 || d1[0] != t1 {
		t.Errorf("D1 order = %v", d1)
	}
	f.with(func(f *fakeSlack) {
		if _, asked := f.oldest["C2"]; asked {
			t.Error("fetched a channel the user is not in")
		}
	})
	h.mu.Lock()
	if h.connected != 1 {
		t.Errorf("Connected %d times", h.connected)
	}
	h.mu.Unlock()

	// Live events.
	live := f.ts()
	f.frames <- event("e1", message{Type: "message", Channel: "G1", User: "U3", Text: "a &lt;b&gt;", Ts: live})
	f.frames <- event("e2", message{Type: "message", Subtype: "message_deleted", Channel: "C1", Ts: f.ts(), Hidden: true})
	f.frames <- event("e3", message{Type: "message", Subtype: "bot_message", Channel: "C1", BotID: "BOWN", AppID: "AOWN", Text: "ours", Ts: f.ts()})
	f.frames <- event("e4", message{Type: "message", Subtype: "message_changed", Channel: "G1", Hidden: true, Ts: f.ts(),
		Message:         &message{Type: "message", User: "U3", Text: "a &lt;b&gt;, edited", Ts: live},
		PreviousMessage: &message{Type: "message", User: "U3", Text: "a &lt;b&gt;", Ts: live}})
	botTs := f.ts()
	f.frames <- event("e5", message{Type: "message", Subtype: "bot_message", Channel: "C1", BotID: "B9", Username: "CI", Text: "build ok", Ts: botTs})

	eventually(t, "live events arrive", func() bool { return len(h.received()) == 6 })
	got = h.received()[3:]
	if m := got[0]; m.ID != live || m.Text != "a <b>" || m.Chat.Name != "me, bob, cy" || m.Chat.Direct || m.Author.Name != "Cy" {
		t.Errorf("live = %+v", m)
	}
	if m := got[1]; !m.Edit || m.ID != live || m.Text != "a <b>, edited" {
		t.Errorf("edit = %+v", m)
	}
	if m := got[2]; m.ID != botTs || m.Author != (bridge.Author{ID: "B9", Name: "CI"}) || m.Text != "build ok" {
		t.Errorf("bot = %+v", m)
	}
	f.with(func(f *fakeSlack) {
		if strings.Join(f.acks, ",") != "e1,e2,e3,e4,e5" {
			t.Errorf("acks = %v", f.acks)
		}
	})

	// Sending posts as the user and returns the ts the echo carries.
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "C1", Text: "x < y & z", Sender: "Me"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the echo arrives", func() bool { return len(h.received()) == 7 })
	echo := h.received()[6]
	if echo.ID != id || !echo.Author.Self || echo.Chat.ID != "C1" || echo.Text != "x < y & z" {
		t.Errorf("echo = %+v, sent id %s", echo, id)
	}
	f.with(func(f *fakeSlack) {
		if p := f.posted[0]; p.Get("text") != "x &lt; y &amp; z" || p.Get("channel") != "C1" {
			t.Errorf("posted %v", p)
		}
	})

	if err := c.Edit(context.Background(), bridge.Outgoing{Chat: "C1", Text: "x > y"}, id); err != nil {
		t.Fatal(err)
	}
	f.with(func(f *fakeSlack) {
		if len(f.updated) != 1 || f.updated[0].Get("ts") != id || f.updated[0].Get("text") != "x &gt; y" {
			t.Errorf("updated %v", f.updated)
		}
	})

	// The newest message per chat is kept for catching up.
	eventually(t, "the state is saved", func() bool {
		var st state
		json.Unmarshal([]byte(h.lastState()), &st)
		return st.Last["C1"] == id && st.Last["D1"] == t2 && st.Last["G1"] == live
	})
}

func TestResume(t *testing.T) {
	f := newFakeSlack(t)
	f.convs = []channel{{ID: "C1", Name: "general", IsMember: yes()}, {ID: "D1", IsIM: true, User: "U2"}}
	seen, missed := ago(3*time.Hour), ago(time.Hour)
	f.history["C1"] = []message{
		{Type: "message", User: "U2", Text: "seen", Ts: seen},
		{Type: "message", User: "U2", Text: "missed", Ts: missed},
	}
	f.history["D1"] = []message{{Type: "message", User: "U2", Text: "new chat", Ts: ago(2 * time.Hour)}}
	h := &fakeHost{}
	c := newConn(t, f, h, fmt.Sprintf(`{"last":{"C1":%q}}`, seen))
	run(t, c)

	eventually(t, "the catch-up arrives", func() bool { return len(h.received()) == 2 })
	got := h.received()
	// Known chats come first.
	if got[0].ID != missed || got[0].Text != "missed" || got[1].Text != "new chat" {
		t.Errorf("got %+v", got)
	}
	f.with(func(f *fakeSlack) {
		if f.oldest["C1"] != seen {
			t.Errorf("C1 fetched from %s, want %s", f.oldest["C1"], seen)
		}
		if !tsLess(f.oldest["D1"], ago(29*24*time.Hour)) {
			t.Errorf("D1 fetched from %s, want 30 days back", f.oldest["D1"])
		}
	})
}

func TestReconnect(t *testing.T) {
	f := newFakeSlack(t)
	h := &fakeHost{}
	c := newConn(t, f, h, "")
	stop := run(t, c)
	eventually(t, "the socket opens", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.sockets == 1
	})
	f.frames <- map[string]any{"type": "disconnect", "reason": "refresh_requested"}
	eventually(t, "a new socket opens", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.sockets == 2
	})
	ts := f.ts()
	f.frames <- event("e1", message{Type: "message", Channel: "C9", User: "U2", Text: "still here", Ts: ts})
	eventually(t, "the message arrives", func() bool { return len(h.received()) == 1 })
	if m := h.received()[0]; m.ID != ts || m.Chat.Name != "C9" {
		t.Errorf("got %+v", m)
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
}

func TestSocketTurnedOff(t *testing.T) {
	f := newFakeSlack(t)
	c := newConn(t, f, &fakeHost{}, "")
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	f.frames <- map[string]any{"type": "disconnect", "reason": "link_disabled"}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Socket Mode") {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestBadToken(t *testing.T) {
	f := newFakeSlack(t)
	c := newConn(t, f, &fakeHost{}, "")
	c.cfg.UserToken = "xoxp-wrong"
	err := c.Run(context.Background())
	if ae, ok := errors.AsType[*apiError](err); !ok || ae.Code != "invalid_auth" {
		t.Errorf("Run = %v", err)
	}
}

func TestSendErrors(t *testing.T) {
	f := newFakeSlack(t)
	c := newConn(t, f, &fakeHost{}, "")
	ctx := context.Background()

	f.with(func(f *fakeSlack) { f.postErr = "channel_not_found" })
	if _, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "x"}); !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("refused send = %v, want rejected", err)
	}

	// A 429 is waited out.
	f.with(func(f *fakeSlack) { f.postErr, f.limitPost = "", 1 })
	if id, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "x"}); err != nil || id == "" {
		t.Errorf("rate limited send = %q, %v", id, err)
	}
	// Running out of time while rate limited: it was never posted.
	f.with(func(f *fakeSlack) { f.limitPost = 1 })
	sctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Send(sctx, bridge.Outgoing{Chat: "C1", Text: "x"}); !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("rate limited past deadline = %v, want rejected", err)
	}

	// No answer: the outcome is unknown.
	f.srv.Close()
	if _, err := c.Send(ctx, bridge.Outgoing{Chat: "C1", Text: "x"}); err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Errorf("send to a dead server = %v, want an unknown error", err)
	}
}

func TestOpen(t *testing.T) {
	for _, cfg := range []string{
		`{"user_token":"xoxc-123","app_token":"xapp-1"}`,
		`{"user_token":"xoxp-1"}`,
		`{"user_token":"bot","app_token":"xapp-1"}`,
	} {
		if _, err := open(bridge.Info{Config: json.RawMessage(cfg)}, &fakeHost{}); err == nil {
			t.Errorf("open(%s) succeeded", cfg)
		}
	}
	n, ok := bridge.Lookup("slack")
	if !ok || !n.Supports("account") || n.Supports("bot") {
		t.Errorf("registered network = %+v", n)
	}
}

func TestPlainText(t *testing.T) {
	n := names{
		user:    func(id string) string { return map[string]string{"U1": "Ada"}[id] },
		channel: func(id string) string { return "dev" },
	}
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"a &lt; b &amp;&amp; c &gt; d", "a < b && c > d"},
		{"hi <@U1>", "hi @Ada"},
		{"hi <@U1|ada>", "hi @ada"},
		{"in <#C1>", "in #dev"},
		{"<!here> <!subteam^S1|@team>", "@here @team"},
		{"<https://a.test>", "https://a.test"},
		{"<https://a.test?x=1&amp;y=2|docs>", "docs (https://a.test?x=1&y=2)"},
		{"<mailto:a@b.test|a@b.test>", "a@b.test"},
		{"unclosed < bracket", "unclosed < bracket"},
	} {
		if got := plainText(tc.in, nil, n); got != tc.want {
			t.Errorf("plainText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A live message arriving before a chat is caught up must not move the
// saved marker past the messages still missing, or they are lost if
// catching up fails.
func TestCatchUpSurvivesLiveMessages(t *testing.T) {
	f := newFakeSlack(t)
	f.convs = []channel{{ID: "C1", Name: "general", IsMember: yes()}}
	seen, missed := ago(3*time.Hour), ago(time.Hour)
	f.history["C1"] = []message{
		{Type: "message", User: "U2", Text: "seen", Ts: seen},
		{Type: "message", User: "U2", Text: "missed", Ts: missed},
	}
	f.convGate = make(chan struct{})
	h := &fakeHost{}
	c := newConn(t, f, h, fmt.Sprintf(`{"last":{"C1":%q}}`, seen))
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()

	// A live message comes in before catching up starts, and then
	// catching up fails.
	var live string
	f.with(func(f *fakeSlack) {
		live = f.ts()
		f.history["C1"] = append(f.history["C1"], message{Type: "message", User: "U2", Text: "live", Ts: live})
	})
	f.frames <- event("e1", message{Type: "message", Channel: "C1", User: "U2", Text: "live", Ts: live})
	eventually(t, "the live message arrives", func() bool { return len(h.received()) == 1 })
	time.Sleep(3 * c.saveEvery) // let a periodic save happen
	f.with(func(f *fakeSlack) { f.failHistory = true })
	close(f.convGate)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded although history failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	var st state
	json.Unmarshal([]byte(h.lastState()), &st)
	if st.Last["C1"] != seen {
		t.Fatalf("saved C1 at %s, want %s (before the gap)", st.Last["C1"], seen)
	}

	// The next run still fetches the missed message, from the marker
	// before the gap.
	f.with(func(f *fakeSlack) { f.failHistory, f.convGate = false, nil })
	h2 := &fakeHost{}
	run(t, newConn(t, f, h2, h.lastState()))
	eventually(t, "the missed message arrives", func() bool {
		for _, m := range h2.received() {
			if m.ID == missed {
				return true
			}
		}
		return false
	})
	// Once caught up, the newest message is saved.
	eventually(t, "the state moves on", func() bool {
		var st state
		json.Unmarshal([]byte(h2.lastState()), &st)
		return st.Last["C1"] == live
	})
}
