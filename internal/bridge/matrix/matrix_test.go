package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	retryPause = 10 * time.Millisecond
	syncWait = 200 * time.Millisecond
}

// start opens the driver on a fake server and runs it until the test ends.
func start(t *testing.T, cfg map[string]any, saved json.RawMessage, h *fakeHost) (*conn, <-chan error) {
	t.Helper()
	data, _ := json.Marshal(cfg)
	c, err := open(bridge.Info{ID: "b1", Mode: store.ModeAccount, Config: data, State: saved}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return c.(*conn), done
}

func connected(t *testing.T, h *fakeHost, done <-chan error) {
	t.Helper()
	select {
	case <-h.connected:
	case err := <-done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
}

// seed makes a direct chat of ada and bob, a named group, and a room
// quiet for too long to backfill.
func seed(f *fakeServer) (dm string, ids map[string]string) {
	f.rooms["!dm:hs"] = &fakeRoom{members: map[string]string{"@ada:hs": "Ada", "@bob:hs": "Bob"}}
	f.rooms["!club:hs"] = &fakeRoom{name: "Book club", members: map[string]string{"@ada:hs": "Ada", "@bob:hs": "Bob", "@cy:hs": "Cy"}}
	f.rooms["!old:hs"] = &fakeRoom{name: "Old", members: map[string]string{"@ada:hs": "Ada", "@bob:hs": "Bob"}}
	now := time.Now()
	ids = map[string]string{}
	ids["old"] = f.add("!old:hs", "@bob:hs", now.Add(-40*24*time.Hour), map[string]any{"msgtype": "m.text", "body": "ancient"})
	ids["hi"] = f.add("!dm:hs", "@bob:hs", now.Add(-time.Hour), map[string]any{"msgtype": "m.text", "body": "hi"})
	ids["hey"] = f.add("!dm:hs", "@ada:hs", now.Add(-50*time.Minute), map[string]any{"msgtype": "m.text", "body": "hey"})
	ids["reply"] = f.add("!dm:hs", "@bob:hs", now.Add(-40*time.Minute), map[string]any{
		"msgtype": "m.text", "body": "> <@ada:hs> hey\n\nhow are you?",
		"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": ids["hey"]}},
	})
	ids["edit"] = f.add("!dm:hs", "@bob:hs", now.Add(-30*time.Minute), map[string]any{
		"msgtype": "m.text", "body": "* hi there",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "hi there"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": ids["hi"]},
	})
	ids["secret"] = f.addType("!dm:hs", "m.room.encrypted", "@bob:hs", now.Add(-20*time.Minute), map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "x"})
	ids["club"] = f.add("!club:hs", "@cy:hs", now.Add(-10*time.Minute), map[string]any{"msgtype": "m.image", "body": "cat.png", "url": "mxc://hs/cat"})
	return "!dm:hs", ids
}

func TestLoginBackfillAndReceive(t *testing.T) {
	f := newFakeServer(t)
	_, ids := seed(f)
	h := newFakeHost()
	_, done := start(t, map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret", "device_name": "say test"}, nil, h)
	connected(t, h, done)

	got := h.messages()
	want := []bridge.Incoming{
		{ID: ids["hi"], Author: bridge.Author{ID: "@bob:hs", Name: "Bob"}, Text: "hi"},
		{ID: ids["hey"], Author: bridge.Author{ID: "@ada:hs", Name: "Ada", Self: true}, Text: "hey"},
		{ID: ids["reply"], Author: bridge.Author{ID: "@bob:hs", Name: "Bob"}, Text: "how are you?", ReplyTo: ids["hey"]},
		{ID: ids["hi"], Author: bridge.Author{ID: "@bob:hs", Name: "Bob"}, Text: "hi there", Edit: true},
		{ID: ids["secret"], Author: bridge.Author{ID: "@bob:hs", Name: "Bob"}, Text: encryptedText},
		{ID: ids["club"], Author: bridge.Author{ID: "@cy:hs", Name: "Cy"}, Text: "[image: cat.png] mxc://hs/cat"},
	}
	if len(got) != len(want) {
		t.Fatalf("backfill: got %d messages, want %d: %+v", len(got), len(want), got)
	}
	// The club's message is the latest, so its room comes first.
	got = append(got[1:], got[0])
	for i, w := range want {
		g := got[i]
		if g.ID != w.ID || g.Author != w.Author || g.Text != w.Text || g.Edit != w.Edit || g.ReplyTo != w.ReplyTo {
			t.Errorf("message %d: got %+v, want %+v", i, g, w)
		}
		if g.Time.IsZero() || time.Since(g.Time) > 2*time.Hour {
			t.Errorf("message %d: time %v", i, g.Time)
		}
	}
	if c := got[0].Chat; c.ID != "!dm:hs" || c.Name != "Bob" || !c.Direct {
		t.Errorf("dm chat: %+v", c)
	}
	if c := got[5].Chat; c.ID != "!club:hs" || c.Name != "Book club" || c.Direct {
		t.Errorf("club chat: %+v", c)
	}

	st := h.saved()
	if st.AccessToken != "tok1" || st.DeviceID != "DEV1" || st.UserID != "@ada:hs" || st.NextBatch == "" || st.FromConfig {
		t.Errorf("state: %+v", st)
	}
	if strings.Contains(string(h.state), "secret") {
		t.Errorf("the password is in the state: %s", h.state)
	}

	// Live messages arrive.
	id := f.add("!club:hs", "@bob:hs", time.Now(), map[string]any{"msgtype": "m.emote", "body": "waves"})
	m := h.waitFor(t, "a live message", func(m bridge.Incoming) bool { return m.ID == id })
	if m.Text != "* Bob waves" || m.Chat.ID != "!club:hs" {
		t.Errorf("live: %+v", m)
	}
}

func TestSendAndEdit(t *testing.T) {
	f := newFakeServer(t)
	dm, _ := seed(f)
	h := newFakeHost()
	c, done := start(t, map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret"}, nil, h)
	connected(t, h, done)
	ctx := context.Background()

	id, err := c.Send(ctx, bridge.Outgoing{Chat: dm, EventID: "say1", Text: "from say", Sender: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	echo := h.waitFor(t, "the echo", func(m bridge.Incoming) bool { return m.ID == id })
	if !echo.Author.Self || echo.Text != "from say" {
		t.Errorf("echo: %+v", echo)
	}
	// The same say event again is the same transaction.
	again, err := c.Send(ctx, bridge.Outgoing{Chat: dm, EventID: "say1", Text: "from say"})
	if err != nil || again != id {
		t.Errorf("send again: %q, %v; want %q", again, err, id)
	}

	if err := c.Edit(ctx, bridge.Outgoing{Chat: dm, EventID: "say1", Text: "from say, edited"}, id); err != nil {
		t.Fatal(err)
	}
	edit := h.waitFor(t, "the edit's echo", func(m bridge.Incoming) bool { return m.Edit && m.ID == id })
	if edit.ID != id || edit.Text != "from say, edited" || !edit.Author.Self {
		t.Errorf("edit: %+v", edit)
	}

	_, err = c.Send(ctx, bridge.Outgoing{Chat: "!nope:hs", EventID: "say2", Text: "x"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("send to a room not joined: %v, want a rejection", err)
	}
}

func TestResume(t *testing.T) {
	f := newFakeServer(t)
	_, ids := seed(f)
	h := newFakeHost()
	cfg := map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret"}
	ctx, cancel := context.WithCancel(context.Background())
	data, _ := json.Marshal(cfg)
	c, _ := open(bridge.Info{ID: "b1", Mode: store.ModeAccount, Config: data}, h)
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	connected(t, h, done)
	cancel()
	<-done

	// While the bridge is down, a new room starts.
	f.mu.Lock()
	f.rooms["!new:hs"] = &fakeRoom{members: map[string]string{"@ada:hs": "Ada", "@dee:hs": "Dee"}}
	f.mu.Unlock()
	f.add("!dm:hs", "@bob:hs", time.Now(), map[string]any{"msgtype": "m.text", "body": "while you were out"})
	newID := f.add("!new:hs", "@dee:hs", time.Now(), map[string]any{"msgtype": "m.text", "body": "hello from dee"})

	h2 := newFakeHost()
	h2.state = h.state
	_, done2 := start(t, cfg, h.state, h2)
	connected(t, h2, done2)
	m := h2.waitFor(t, "the new room's message", func(m bridge.Incoming) bool { return m.ID == newID })
	if m.Chat.Name != "Dee" || !m.Chat.Direct || m.Author.Name != "Dee" {
		t.Errorf("new room: %+v", m)
	}
	for _, m := range h2.messages() {
		if m.ID == ids["hi"] {
			t.Errorf("an old message came again after resuming: %+v", m)
		}
	}
	if len(h2.messages()) != 2 {
		t.Errorf("after resuming: %+v", h2.messages())
	}
	f.mu.Lock()
	logins := f.logins
	f.mu.Unlock()
	if logins != 1 {
		t.Errorf("logged in %d times, want once", logins)
	}
}

func TestLoggedOutElsewhere(t *testing.T) {
	f := newFakeServer(t)
	dm, _ := seed(f)
	h := newFakeHost()
	_, done := start(t, map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret"}, nil, h)
	connected(t, h, done)
	f.mu.Lock()
	delete(f.tokens, "tok1")
	f.mu.Unlock()
	id := f.add(dm, "@bob:hs", time.Now(), map[string]any{"msgtype": "m.text", "body": "still there?"})
	h.waitFor(t, "a message after logging in again", func(m bridge.Incoming) bool { return m.ID == id })
	if st := h.saved(); st.AccessToken != "tok2" || st.DeviceID != "DEV2" {
		t.Errorf("state after logging in again: %+v", st)
	}
}

func TestAccessToken(t *testing.T) {
	f := newFakeServer(t)
	seed(f)
	f.tokens["given"] = "@ada:hs"
	h := newFakeHost()
	_, done := start(t, map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "access_token": "given"}, nil, h)
	connected(t, h, done)
	if st := h.saved(); st.AccessToken != "given" || !st.FromConfig || st.DeviceID != "TOKENDEV" {
		t.Errorf("state: %+v", st)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != 0 {
		t.Errorf("logged in with a token given")
	}
}

func TestBadAccessToken(t *testing.T) {
	f := newFakeServer(t)
	seed(f)
	data, _ := json.Marshal(map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "access_token": "stale"})
	c, err := open(bridge.Info{ID: "b1", Mode: store.ModeAccount, Config: data}, newFakeHost())
	if err != nil {
		t.Fatal(err)
	}
	err = c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Errorf("Run: %v, want an error about the access token", err)
	}
}

func TestAskForPassword(t *testing.T) {
	f := newFakeServer(t)
	seed(f)
	h := newFakeHost()
	h.answer = "secret"
	_, done := start(t, map[string]any{"homeserver": f.URL, "user_id": "@ada:hs"}, nil, h)
	connected(t, h, done)
	if len(h.asked) != 1 || h.asked[0].Kind != "password" {
		t.Errorf("asked: %+v", h.asked)
	}
}

// TestEditsOnlyByTheSender checks that an edit counts only from whoever
// sent the message: homeservers let any member send one.
func TestEditsOnlyByTheSender(t *testing.T) {
	f := newFakeServer(t)
	dm, ids := seed(f)
	edit := func(room, sender, of, text string) string {
		return f.add(room, sender, time.Now(), map[string]any{
			"msgtype": "m.text", "body": "* " + text,
			"m.new_content": map[string]any{"msgtype": "m.text", "body": text},
			"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": of},
		})
	}
	h := newFakeHost()
	cfg := map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret"}
	_, done := start(t, cfg, nil, h)
	connected(t, h, done)

	// Cy rewrites Bob's message and Ada's; then Bob edits his own.
	clubMsg := f.add("!club:hs", "@bob:hs", time.Now(), map[string]any{"msgtype": "m.text", "body": "meet at 6"})
	adaMsg := f.add("!club:hs", "@ada:hs", time.Now(), map[string]any{"msgtype": "m.text", "body": "ok"})
	edit("!club:hs", "@cy:hs", clubMsg, "meet at 9")
	edit("!club:hs", "@cy:hs", adaMsg, "no")
	edit("!club:hs", "@cy:hs", "$nosuch", "what")
	edit("!club:hs", "@bob:hs", clubMsg, "meet at 7")
	h.waitFor(t, "Bob's own edit", func(m bridge.Incoming) bool { return m.Edit && m.ID == clubMsg && m.Text == "meet at 7" })

	// A run that resumes has not seen the message edited: it asks the
	// homeserver who sent it.
	h2 := newFakeHost()
	_, done2 := start(t, cfg, h.stateJSON(), h2)
	connected(t, h2, done2)
	edit(dm, "@ada:hs", ids["hi"], "ada was here")
	edit(dm, "@bob:hs", ids["hi"], "hi again")
	h2.waitFor(t, "Bob's edit after resuming", func(m bridge.Incoming) bool { return m.Edit && m.ID == ids["hi"] && m.Text == "hi again" })

	for _, m := range append(h.messages(), h2.messages()...) {
		if m.Edit && (m.Author.ID == "@cy:hs" || m.Text == "ada was here" || m.Text == "meet at 9" || m.Text == "no") {
			t.Errorf("an edit by someone else came through: %+v", m)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.lookups, "event/"+ids["hi"]) {
		t.Errorf("the original sender was not looked up: %v", f.lookups)
	}
}

func TestStripReply(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                              "plain",
		"> <@a:hs> quoted\n> more\n\nanswer": "answer",
		"> <@a:hs> quoted\n\nline 1\nline 2": "line 1\nline 2",
	} {
		if got := stripReply(in); got != want {
			t.Errorf("stripReply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenChecksConfig(t *testing.T) {
	for _, cfg := range []string{`{"homeserver":"","user_id":"@a:hs"}`, `{"homeserver":"hs","user_id":"ada"}`} {
		if _, err := open(bridge.Info{Config: json.RawMessage(cfg)}, newFakeHost()); err == nil {
			t.Errorf("open(%s) took it", cfg)
		}
	}
}

func TestLogOut(t *testing.T) {
	f := newFakeServer(t)
	seed(f)
	cfg := map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "password": "secret"}
	data, _ := json.Marshal(cfg)
	if err := logOut(context.Background(), bridge.Info{ID: "b1", Config: data}); err != nil {
		t.Fatalf("logging out a bridge that never logged in: %v", err)
	}
	h := newFakeHost()
	_, done := start(t, cfg, nil, h)
	connected(t, h, done)
	info := bridge.Info{ID: "b1", Config: data, State: h.stateJSON()}
	if err := logOut(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, left := f.tokens["tok1"]
	f.mu.Unlock()
	if left {
		t.Fatal("say's session is still logged in")
	}
	if err := logOut(context.Background(), info); err != nil {
		t.Fatalf("logging out twice: %v", err)
	}

	// A token the person gave is theirs: it stays logged in. The first
	// session's sync may still be asking the fake server, hence the lock.
	f.mu.Lock()
	f.tokens["given"] = "@ada:hs"
	f.mu.Unlock()
	tcfg := map[string]any{"homeserver": f.URL, "user_id": "@ada:hs", "access_token": "given"}
	h2 := newFakeHost()
	_, done2 := start(t, tcfg, nil, h2)
	connected(t, h2, done2)
	tdata, _ := json.Marshal(tcfg)
	if err := logOut(context.Background(), bridge.Info{ID: "b2", Config: tdata, State: h2.stateJSON()}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, kept := f.tokens["given"]
	f.mu.Unlock()
	if !kept {
		t.Fatal("logged out the person's own token")
	}
}
