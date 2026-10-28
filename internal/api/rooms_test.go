package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

type env struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	store store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with the server's store wrapped by wrap, so a test
// can slow or watch store calls.
func newEnvWith(t *testing.T, wrap func(store.Store) store.Store) *env {
	t.Helper()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		st = wrap(st)
	}
	b := bus.NewInProc()
	s := New(Config{Version: "test", Store: st, Bus: b, Agents: core.AgentPolicy{MaxHops: 2}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		b.Close()
		st.Close()
	})
	return &env{t: t, srv: s, ts: ts, store: st}
}

// user creates a principal and returns it with a token.
func (e *env) user(name string, kind core.PrincipalKind) (core.Principal, string) {
	e.t.Helper()
	ctx := context.Background()
	p := core.Principal{ID: core.NewID(), Kind: kind, DisplayName: name}
	if err := e.store.CreatePrincipal(ctx, p); err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.store.CreateToken(ctx, p.ID, "test", time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	return p, tok
}

// do sends a request with token and decodes a JSON response into out,
// returning the status.
func (e *env) do(method, path, token string, body any, out any, header ...string) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+"/api/v1"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		data, _ := io.ReadAll(res.Body)
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, data)
		}
	}
	return res.StatusCode
}

func (e *env) room(token, name string) wireRoom {
	e.t.Helper()
	var r wireRoom
	if code := e.do("POST", "/rooms", token, map[string]string{"kind": "group", "name": name}, &r); code != 201 {
		e.t.Fatalf("create room = %d", code)
	}
	return r
}

func (e *env) join(owner, roomID, principalID string, role core.Role) {
	e.t.Helper()
	if code := e.do("POST", "/rooms/"+roomID+"/members", owner, map[string]any{"principal_id": principalID, "role": role}, nil); code != 201 {
		e.t.Fatalf("add member = %d", code)
	}
}

func textMsg(s string) map[string]any {
	return map[string]any{"parts": []map[string]any{{"kind": "text", "data": map[string]string{"text": s}}}}
}

func (e *env) say(token, roomID, text string) wireEvent {
	e.t.Helper()
	var ev wireEvent
	if code := e.do("POST", "/rooms/"+roomID+"/messages", token, textMsg(text), &ev); code != 201 {
		e.t.Fatalf("send = %d", code)
	}
	return ev
}

func partText(ev wireEvent) string {
	var d struct{ Text string }
	json.Unmarshal(ev.Parts[0].Data, &d)
	return d.Text
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	ada, tok := e.user("Ada", core.PrincipalHuman)
	var errBody struct{ Error apiError }
	if code := e.do("GET", "/me", "", nil, &errBody); code != 401 || errBody.Error.Code != "unauthorized" {
		t.Fatalf("no token = %d %+v", code, errBody)
	}
	if code := e.do("GET", "/me", "say_wrong", nil, nil); code != 401 {
		t.Fatalf("wrong token = %d", code)
	}
	var me wirePrincipal
	if code := e.do("GET", "/me", tok, nil, &me); code != 200 || me.ID != ada.ID || me.Kind != core.PrincipalHuman {
		t.Fatalf("me = %d %+v", code, me)
	}

	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: TokenCookie, Value: tok})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("cookie auth = %d", res.StatusCode)
	}

	// A form post cannot reach a handler, so a cookie alone cannot be used
	// cross-site.
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/v1/rooms", strings.NewReader(`{"kind":"group","name":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(&http.Cookie{Name: TokenCookie, Value: tok})
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain post = %d", res.StatusCode)
	}
}

func TestRoomsAndMembers(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	eve, eveTok := e.user("Eve", core.PrincipalHuman)

	r := e.room(ada, "general")
	if r.Kind != core.RoomGroup || r.Name != "general" || r.ID == "" {
		t.Fatalf("room = %+v", r)
	}
	if code := e.do("POST", "/rooms", ada, map[string]string{"kind": "widget-thread"}, nil); code != 422 {
		t.Fatalf("widget-thread room = %d", code)
	}
	if code := e.do("POST", "/rooms", ada, map[string]any{"kind": "group", "extra": 1}, nil); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}

	// Rooms you are not in look like rooms that do not exist.
	if code := e.do("GET", "/rooms/"+r.ID, eveTok, nil, nil); code != 404 {
		t.Fatalf("non-member get = %d", code)
	}
	if code := e.do("GET", "/rooms/nope", ada, nil, nil); code != 404 {
		t.Fatalf("missing room = %d", code)
	}

	e.join(ada, r.ID, bob.ID, core.RoleMember)
	if code := e.do("POST", "/rooms/"+r.ID+"/members", ada, map[string]any{"principal_id": bob.ID}, nil); code != 409 {
		t.Fatalf("duplicate member = %d", code)
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/members", bobTok, map[string]any{"principal_id": eve.ID}, nil); code != 403 {
		t.Fatalf("member adds member = %d", code)
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/members", ada, map[string]any{"principal_id": eve.ID, "role": "owner"}, nil); code != 422 {
		t.Fatalf("second owner = %d", code)
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/members", ada, map[string]any{"principal_id": "nope"}, nil); code != 404 {
		t.Fatalf("unknown principal = %d", code)
	}

	var rooms struct{ Rooms []wireRoom }
	if code := e.do("GET", "/rooms", bobTok, nil, &rooms); code != 200 || len(rooms.Rooms) != 1 || rooms.Rooms[0].ID != r.ID {
		t.Fatalf("bob's rooms = %d %+v", code, rooms)
	}
	var members struct{ Members []wireMember }
	if code := e.do("GET", "/rooms/"+r.ID+"/members", bobTok, nil, &members); code != 200 || len(members.Members) != 2 {
		t.Fatalf("members = %d %+v", code, members)
	}
}

func TestSendAndPage(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	obs, obsTok := e.user("Obs", core.PrincipalHuman)
	r := e.room(ada, "general")
	e.join(ada, r.ID, obs.ID, core.RoleObserver)

	for i := range 5 {
		ev := e.say(ada, r.ID, fmt.Sprint("m", i))
		if ev.Seq != int64(i+1) || ev.Stream != core.StreamFinal || partText(ev) != fmt.Sprint("m", i) {
			t.Fatalf("event %d = %+v", i, ev)
		}
	}

	var page struct{ Events []wireEvent }
	e.do("GET", "/rooms/"+r.ID+"/events?limit=2", ada, nil, &page)
	if len(page.Events) != 2 || page.Events[0].Seq != 4 || page.Events[1].Seq != 5 {
		t.Fatalf("latest page = %+v", page.Events)
	}
	e.do("GET", "/rooms/"+r.ID+"/events?limit=2&before=4", ada, nil, &page)
	if len(page.Events) != 2 || page.Events[0].Seq != 2 {
		t.Fatalf("before page = %+v", page.Events)
	}
	e.do("GET", "/rooms/"+r.ID+"/events?after=3", obsTok, nil, &page)
	if len(page.Events) != 2 || page.Events[0].Seq != 4 {
		t.Fatalf("after page = %+v", page.Events)
	}
	if code := e.do("GET", "/rooms/"+r.ID+"/events?limit=0", ada, nil, nil); code != 400 {
		t.Fatalf("limit=0 = %d", code)
	}

	// The same idempotency key returns the first event.
	var first, again wireEvent
	key := []string{"Idempotency-Key", "k1"}
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, textMsg("once"), &first, key...); code != 201 {
		t.Fatalf("keyed send = %d", code)
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, textMsg("twice"), &again, key...); code != 200 || again.ID != first.ID || partText(again) != "once" {
		t.Fatalf("repeat send = %d %+v", code, again)
	}

	invalid := []map[string]any{
		{"parts": []any{}},
		{"parts": []map[string]any{{"kind": "text", "data": map[string]string{"text": ""}}}},
		{"parts": []map[string]any{{"kind": "text", "data": "hi"}}},
		{"parts": []map[string]any{{"kind": "blink", "data": map[string]string{}}}},
		{"parts": []map[string]any{{"kind": "tool-call", "data": map[string]string{"status": "done"}}}},
		{"parts": []map[string]any{{"kind": "tool-call", "data": map[string]string{"name": "x", "status": "maybe"}}}},
		{"parts": []map[string]any{{"kind": "tool-call", "data": map[string]any{"name": 3, "status": "done"}}}},
		{"parts": textMsg("x")["parts"], "caused_by": "nope"},
	}
	for _, body := range invalid {
		if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, body, nil); code != 422 {
			t.Errorf("send %v = %d, want 422", body, code)
		}
	}
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", obsTok, textMsg("hi"), nil); code != 403 {
		t.Fatalf("observer send = %d", code)
	}

	// caused_by must be in the same room.
	other := e.room(ada, "other")
	elsewhere := e.say(ada, other.ID, "x")
	body := textMsg("reply")
	body["caused_by"] = elsewhere.ID
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, body, nil); code != 422 {
		t.Fatalf("cross-room cause = %d", code)
	}
}

func TestReplies(t *testing.T) {
	e := newEnv(t)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	_, ada := e.user("Ada", core.PrincipalHuman)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	q := e.say(ada, r.ID, "lunch?")

	body := textMsg("yes")
	body["reply_to"] = q.ID
	var reply wireEvent
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", bobTok, body, &reply, "Idempotency-Key", "k-reply"); code != 201 {
		t.Fatalf("reply = %d", code)
	}
	// A reply is not a cause: only automated chains have those.
	if reply.ReplyTo != q.ID || reply.Causation.CausedBy != "" || reply.ClientKey != "k-reply" {
		t.Fatalf("reply = %+v", reply)
	}
	var page struct{ Events []wireEvent }
	e.do("GET", "/rooms/"+r.ID+"/events", ada, nil, &page)
	if got := page.Events[len(page.Events)-1]; got.ReplyTo != q.ID || got.ClientKey != "k-reply" {
		t.Fatalf("listed reply = %+v", got)
	}

	// The quoted message can be fetched alone, by members only.
	var got wireEvent
	if code := e.do("GET", "/rooms/"+r.ID+"/events/"+q.ID, bobTok, nil, &got); code != 200 || got.ID != q.ID || partText(got) != "lunch?" {
		t.Fatalf("get event = %d %+v", code, got)
	}
	_, eveTok := e.user("Eve", core.PrincipalHuman)
	if code := e.do("GET", "/rooms/"+r.ID+"/events/"+q.ID, eveTok, nil, nil); code != 404 {
		t.Fatalf("get event as outsider = %d", code)
	}

	// The target must be a message in the same room.
	other := e.room(ada, "other")
	elsewhere := e.say(ada, other.ID, "x")
	if code := e.do("GET", "/rooms/"+r.ID+"/events/"+elsewhere.ID, ada, nil, nil); code != 404 {
		t.Fatalf("get event of another room = %d", code)
	}
	for _, target := range []string{elsewhere.ID, "nope"} {
		body["reply_to"] = target
		if code := e.do("POST", "/rooms/"+r.ID+"/messages", ada, body, nil); code != 422 {
			t.Fatalf("reply to %s = %d", target, code)
		}
	}
}

// Two agents replying to each other stop at MaxHops (2 in newEnv); a human
// message resets the chain.
func TestHopLimit(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	a1, a1Tok := e.user("a1", core.PrincipalAgent)
	a2, a2Tok := e.user("a2", core.PrincipalAgent)
	r := e.room(ada, "general")
	e.join(ada, r.ID, a1.ID, core.RoleMember)
	e.join(ada, r.ID, a2.ID, core.RoleMember)

	reply := func(token, cause string) (wireEvent, int) {
		body := textMsg("reply")
		body["caused_by"] = cause
		var ev wireEvent
		code := e.do("POST", "/rooms/"+r.ID+"/messages", token, body, &ev)
		return ev, code
	}
	last := e.say(ada, r.ID, "hi agents")
	for i, tok := range []string{a1Tok, a2Tok} {
		ev, code := reply(tok, last.ID)
		if code != 201 || ev.Causation.HopCount != i+1 || ev.Causation.CausedBy != last.ID {
			t.Fatalf("hop %d = %d %+v", i+1, code, ev)
		}
		last = ev
	}
	var errBody struct{ Error apiError }
	body := textMsg("reply")
	body["caused_by"] = last.ID
	if code := e.do("POST", "/rooms/"+r.ID+"/messages", a1Tok, body, &errBody); code != 422 || errBody.Error.Code != "hop_limit" {
		t.Fatalf("third hop = %d %+v", code, errBody)
	}
	if ev, code := reply(ada, last.ID); code != 201 || ev.Causation.HopCount != 0 {
		t.Fatalf("human reply = %d %+v", code, ev)
	}
}

func TestLongPoll(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	r := e.room(ada, "general")

	start := time.Now()
	var page struct{ Events []wireEvent }
	if code := e.do("GET", "/rooms/"+r.ID+"/events?after=0&wait=1", ada, nil, &page); code != 200 || len(page.Events) != 0 {
		t.Fatalf("empty poll = %d %+v", code, page)
	}
	if time.Since(start) < time.Second {
		t.Fatal("poll returned before its wait")
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		e.say(ada, r.ID, "wake up")
	}()
	start = time.Now()
	e.do("GET", "/rooms/"+r.ID+"/events?after=0&wait=10", ada, nil, &page)
	if len(page.Events) != 1 || partText(page.Events[0]) != "wake up" || time.Since(start) > 5*time.Second {
		t.Fatalf("woken poll = %+v after %v", page.Events, time.Since(start))
	}
}

type sse struct {
	id   string
	data wireEvent
}

func readSSE(t *testing.T, sc *bufio.Scanner) sse {
	t.Helper()
	var ev sse
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.id != "" {
				return ev
			}
		case strings.HasPrefix(line, "id: "):
			ev.id = line[4:]
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(line[6:]), &ev.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatalf("stream ended: %v", sc.Err())
	return ev
}

func TestSSE(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	_, eve := e.user("Eve", core.PrincipalHuman)
	r := e.room(ada, "general")
	e.say(ada, r.ID, "one")
	e.say(ada, r.ID, "two")

	open := func(token string, header ...string) *http.Response {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/v1/rooms/"+r.ID+"/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	if res := open(eve); res.StatusCode != 404 {
		t.Fatalf("non-member stream = %d", res.StatusCode)
	}

	res := open(ada, "Last-Event-ID", "1")
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	if ev := readSSE(t, sc); ev.id != "2" || partText(ev.data) != "two" {
		t.Fatalf("replay = %+v", ev)
	}
	e.say(ada, r.ID, "three")
	if ev := readSSE(t, sc); ev.id != "3" || partText(ev.data) != "three" {
		t.Fatalf("live = %+v", ev)
	}
}

func dialWS(t *testing.T, e *env, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/api/v1/ws"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func wsSend(t *testing.T, c *websocket.Conn, id, typ string, payload any) {
	t.Helper()
	p, _ := json.Marshal(payload)
	data, _ := json.Marshal(envelope{ID: id, Type: typ, Payload: p})
	if err := c.Write(context.Background(), websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func wsRead(t *testing.T, c *websocket.Conn) envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func eventOf(t *testing.T, env envelope) wireEvent {
	t.Helper()
	var ev wireEvent
	if err := json.Unmarshal(env.Payload, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestWebSocket(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	secret := e.room(ada, "secret")
	e.say(ada, r.ID, "before")

	c := dialWS(t, e, bobTok)

	wsSend(t, c, "1", "subscribe", map[string]any{"room_id": secret.ID})
	if env := wsRead(t, c); env.ID != "1" || env.Status != "error" || !strings.Contains(string(env.Payload), "not_found") {
		t.Fatalf("subscribe to foreign room = %+v", env)
	}

	wsSend(t, c, "2", "subscribe", map[string]any{"room_id": r.ID, "after": 0})
	// The reply and the replayed event race; both must arrive.
	got := map[string]envelope{}
	for range 2 {
		env := wsRead(t, c)
		got[env.Type] = env
	}
	if got["subscribe"].Status != "ok" || got["subscribe"].ID != "2" {
		t.Fatalf("subscribe reply = %+v", got["subscribe"])
	}
	if ev := eventOf(t, got["event"]); ev.Seq != 1 || partText(ev) != "before" || got["event"].Status != "" {
		t.Fatalf("replayed = %+v", got["event"])
	}

	// A send from another client arrives as a push.
	e.say(ada, r.ID, "live")
	if env := wsRead(t, c); env.Type != "event" || partText(eventOf(t, env)) != "live" {
		t.Fatalf("live push = %+v", env)
	}

	// Sending over the socket replies with the event and pushes it too.
	msg := textMsg("from ws")
	msg["room_id"] = r.ID
	msg["idempotency_key"] = "ws-1"
	wsSend(t, c, "3", "send", msg)
	got = map[string]envelope{}
	for range 2 {
		env := wsRead(t, c)
		got[env.Type] = env
	}
	if got["send"].Status != "ok" || eventOf(t, got["send"]).Seq != 3 || eventOf(t, got["event"]).Seq != 3 {
		t.Fatalf("send = %+v", got)
	}
	wsSend(t, c, "4", "send", msg)
	if env := wsRead(t, c); env.Status != "ok" || eventOf(t, env).Seq != 3 {
		t.Fatalf("repeated send = %+v", env)
	}

	wsSend(t, c, "5", "unsubscribe", map[string]any{"room_id": r.ID})
	if env := wsRead(t, c); env.ID != "5" || env.Status != "ok" {
		t.Fatalf("unsubscribe = %+v", env)
	}
	e.say(ada, r.ID, "unseen")
	wsSend(t, c, "6", "ping", nil)
	if env := wsRead(t, c); env.ID != "6" || env.Type != "ping" {
		t.Fatalf("after unsubscribe got %+v, want the ping reply", env)
	}

	wsSend(t, c, "7", "dance", nil)
	if env := wsRead(t, c); env.Status != "error" || !strings.Contains(string(env.Payload), "bad_request") {
		t.Fatalf("unknown type = %+v", env)
	}
	c.Write(context.Background(), websocket.MessageText, []byte("not json"))
	if env := wsRead(t, c); env.Status != "error" {
		t.Fatalf("bad frame = %+v", env)
	}
}

func TestWebSocketRoomsPush(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	c := dialWS(t, e, bobTok)

	// Rooms that do not involve Bob say nothing to him.
	r := e.room(ada, "general")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	if env := wsRead(t, c); env.Type != "rooms" || env.Status != "" || env.ID == "" {
		t.Fatalf("after join = %+v, want a rooms push", env)
	}
	e.room(bobTok, "mine")
	if env := wsRead(t, c); env.Type != "rooms" {
		t.Fatalf("after create = %+v, want a rooms push", env)
	}
	wsSend(t, c, "1", "ping", nil)
	if env := wsRead(t, c); env.ID != "1" || env.Type != "ping" {
		t.Fatalf("got %+v, want only the ping reply", env)
	}
}

func TestWebSocketNeedsToken(t *testing.T) {
	e := newEnv(t)
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/api/v1/ws"
	_, res, err := websocket.Dial(context.Background(), url, nil)
	if err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("dial without token: %v %v", err, res)
	}
}

// A follower that misses a bus notification still delivers the event on
// its next resync.
func TestResyncWithoutNotification(t *testing.T) {
	e := newEnv(t)
	e.srv.resync = 50 * time.Millisecond
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	r := e.room(adaTok, "general")

	c := dialWS(t, e, adaTok)
	wsSend(t, c, "1", "subscribe", map[string]any{"room_id": r.ID})
	if env := wsRead(t, c); env.Status != "ok" {
		t.Fatalf("subscribe = %+v", env)
	}
	// Written straight to the store, so nothing is published.
	_, err := e.store.AppendEvent(context.Background(), core.Event{
		RoomID: r.ID, Type: core.EventMessage, SenderID: ada.ID, Stream: core.StreamFinal,
		Parts: []core.Part{{Kind: core.PartText, Data: []byte(`{"text":"quiet"}`)}},
	}, store.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if env := wsRead(t, c); env.Type != "event" || partText(eventOf(t, env)) != "quiet" {
		t.Fatalf("resync push = %+v", env)
	}
}
