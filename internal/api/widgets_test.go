package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store"
)

// signIdentity makes an HS256 identity token the way a host site would.
func signIdentity(secret string, claims map[string]any) string {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	payload := enc.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head + "." + payload))
	return head + "." + payload + "." + enc.EncodeToString(mac.Sum(nil))
}

type wireSession struct {
	Principal wirePrincipal
	Room      wireRoom
	Token     string
}

func (e *env) session(key, token, identity string) (int, wireSession) {
	e.t.Helper()
	var s wireSession
	code := e.do("POST", "/embed/"+key+"/session", token, map[string]string{"identity": identity}, &s)
	return code, s
}

func TestWidgetAdmin(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	agent, _ := e.user("Helper", core.PrincipalAgent)

	ok := map[string]any{"name": "Support", "origins": []string{"https://Shop.example/"}, "team": []string{ada.ID}}
	if code := e.do("POST", "/widgets", adaTok, ok, nil); code != 403 {
		t.Fatalf("non-admin create = %d", code)
	}
	for name, body := range map[string]map[string]any{
		"no origins":        {"name": "x", "team": []string{ada.ID}},
		"path in origin":    {"name": "x", "origins": []string{"https://a.example/shop"}, "team": []string{ada.ID}},
		"nobody answers":    {"name": "x", "origins": []string{"https://a.example"}},
		"agent in team":     {"name": "x", "origins": []string{"https://a.example"}, "team": []string{agent.ID}},
		"person as agent":   {"name": "x", "origins": []string{"https://a.example"}, "agent_id": ada.ID},
		"no name":           {"origins": []string{"https://a.example"}, "team": []string{ada.ID}},
		"wildcard origin":   {"name": "x", "origins": []string{"https://*.a.example"}, "team": []string{ada.ID}},
		"javascript scheme": {"name": "x", "origins": []string{"javascript:alert(1)"}, "team": []string{ada.ID}},
	} {
		if code := e.do("POST", "/widgets", root, body, nil); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
	var w wireWidget
	if code := e.do("POST", "/widgets", root, ok, &w); code != 201 {
		t.Fatalf("create = %d", code)
	}
	if !strings.HasPrefix(w.PublicKey, "pk_") || !strings.HasPrefix(w.Secret, "sk_") || !slices.Equal(w.Origins, []string{"https://shop.example"}) {
		t.Fatalf("created = %+v", w)
	}
	var w2 wireWidget
	if code := e.do("PATCH", "/widgets/"+w.ID, root, map[string]any{"agent_id": agent.ID, "rotate_secret": true}, &w2); code != 200 {
		t.Fatalf("update = %d", code)
	}
	if w2.Secret == w.Secret || w2.PublicKey != w.PublicKey || w2.AgentID != agent.ID || len(w2.Team) != 1 || w2.Name != "Support" {
		t.Fatalf("updated = %+v", w2)
	}
	var list struct{ Widgets []wireWidget }
	if code := e.do("GET", "/widgets", root, nil, &list); code != 200 || len(list.Widgets) != 1 || list.Widgets[0].Secret != w2.Secret {
		t.Fatalf("list = %d %+v", code, list)
	}

	// The public config, and the frame-ancestors of the embed page.
	var cfg struct {
		Name, Greeting string
		Origins        []string
	}
	if code := e.do("GET", "/embed/"+w.PublicKey, "", nil, &cfg); code != 200 || cfg.Name != "Support" || len(cfg.Origins) != 1 {
		t.Fatalf("config = %d %+v", code, cfg)
	}
	if code := e.do("GET", "/embed/pk_nope", "", nil, &cfg); code != 404 {
		t.Fatalf("unknown config = %d", code)
	}
	if _, _, h := get(t, e.ts.URL+"/embed/"+w.PublicKey); h.Get("Content-Security-Policy") != "frame-ancestors 'self' https://shop.example" {
		t.Fatalf("embed CSP = %q", h.Get("Content-Security-Policy"))
	}
	if _, _, h := get(t, e.ts.URL+"/embed/pk_nope"); h.Get("Content-Security-Policy") != "frame-ancestors 'self'" {
		t.Fatalf("unknown embed CSP = %q", h.Get("Content-Security-Policy"))
	}
	if _, _, h := get(t, e.ts.URL+"/rooms/x"); h.Get("Content-Security-Policy") != "frame-ancestors 'self'" {
		t.Fatalf("app CSP = %q", h.Get("Content-Security-Policy"))
	}
}

// wakeBeforeScopes is a store that, just before a scope change, wakes the
// room's followers and gives them time to read it, as a resync or a
// message from elsewhere landing at that moment would. A handoff that
// ran after the append would let the bot see a team member's message
// while its agent is still the responder.
type wakeBeforeScopes struct {
	store.Store
	wake func(ctx context.Context, roomID string)
}

func (s *wakeBeforeScopes) SetMemberScopes(ctx context.Context, roomID, principalID string, scopes []string) error {
	if s.wake != nil {
		s.wake(ctx, roomID)
		time.Sleep(100 * time.Millisecond)
	}
	return s.Store.SetMemberScopes(ctx, roomID, principalID, scopes)
}

func TestWidgetVisitors(t *testing.T) {
	race := &wakeBeforeScopes{}
	e := newEnvWith(t, func(st store.Store) store.Store { race.Store = st; return race })
	race.wake = e.srv.notify
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	agent, agentTok := e.user("Helper", core.PrincipalAgent)
	var w wireWidget
	e.do("POST", "/widgets", root, map[string]any{
		"name": "Support", "origins": []string{"https://shop.example"},
		"team": []string{ada.ID}, "agent_id": agent.ID,
	}, &w)

	// An anonymous visitor, then the same one again with its token.
	code, s := e.session(w.PublicKey, "", "")
	if code != 201 || s.Principal.Kind != core.PrincipalGuest || s.Room.Kind != core.RoomWidgetThread || s.Token == "" {
		t.Fatalf("first session = %d %+v", code, s)
	}
	if code, again := e.session(w.PublicKey, s.Token, ""); code != 200 || again.Token != s.Token || again.Room.ID != s.Room.ID {
		t.Fatalf("resumed = %d %+v", code, again)
	}
	if code, _ := e.session("pk_nope", "", ""); code != 404 {
		t.Fatalf("unknown widget = %d", code)
	}

	// The thread has the visitor, the team as admins and the agent as
	// responder; members carry names, which guests cannot look up.
	var ms struct{ Members []wireMember }
	if code := e.do("GET", "/rooms/"+s.Room.ID+"/members", s.Token, nil, &ms); code != 200 || len(ms.Members) != 3 {
		t.Fatalf("members = %d %+v", code, ms)
	}
	for _, m := range ms.Members {
		switch m.PrincipalID {
		case ada.ID:
			if m.Role != core.RoleAdmin || m.DisplayName != "Ada" || m.Kind != core.PrincipalHuman {
				t.Errorf("team member = %+v", m)
			}
		case agent.ID:
			if !slices.Contains(m.Scopes, core.ScopeResponder) {
				t.Errorf("agent = %+v", m)
			}
		}
	}
	var rooms struct{ Rooms []wireRoom }
	if e.do("GET", "/rooms", adaTok, nil, &rooms); len(rooms.Rooms) != 1 || !strings.HasPrefix(rooms.Rooms[0].Name, "Support · Visitor ") {
		t.Fatalf("Ada's rooms = %+v", rooms)
	}

	// Guests chat in their thread and nowhere else. The agent's bot is
	// asked to answer them.
	bot := dialBot(t, e, agentTok)
	bot.hello()
	e.say(s.Token, s.Room.ID, "hello?")
	bot.event("hello?", core.TriggerResponder)
	other := e.room(adaTok, "Staff")
	if code := e.do("GET", "/rooms/"+other.ID+"/events", s.Token, nil, nil); code != 404 {
		t.Fatalf("guest reads another room = %d", code)
	}
	if code := e.do("POST", "/rooms", s.Token, map[string]string{"kind": "group", "name": "mine"}, nil); code != 403 {
		t.Fatalf("guest creates a room = %d", code)
	}
	if code := e.do("GET", "/principals", s.Token, nil, nil); code != 403 {
		t.Fatalf("guest lists principals = %d", code)
	}

	// The agent answers until Ada writes; then it only answers mentions.
	var reply wireEvent
	e.do("POST", "/rooms/"+s.Room.ID+"/messages", agentTok, textMsg("I can help"), &reply)
	responder := func() bool {
		m, err := e.store.GetMember(context.Background(), s.Room.ID, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(m.Scopes, core.ScopeResponder)
	}
	if !responder() {
		t.Fatal("an agent reply handed the thread over")
	}
	e.say(adaTok, s.Room.ID, "Ada here")
	// The thread is handed over before the bot sees Ada's message.
	bot.event("I can help", "")
	bot.event("Ada here", "")
	if responder() {
		t.Fatal("a team reply did not hand the thread over")
	}

	// A second visitor asks for a person.
	_, s2 := e.session(w.PublicKey, "", "")
	if s2.Room.ID == s.Room.ID || s2.Principal.ID == s.Principal.ID {
		t.Fatal("two anonymous visitors share a thread")
	}
	if code := e.do("POST", "/rooms/"+s2.Room.ID+"/handoff", s2.Token, nil, nil); code != 204 {
		t.Fatalf("handoff = %d", code)
	}
	m, _ := e.store.GetMember(context.Background(), s2.Room.ID, agent.ID)
	if slices.Contains(m.Scopes, core.ScopeResponder) {
		t.Fatal("handoff kept the responder")
	}
	if code := e.do("POST", "/rooms/"+other.ID+"/handoff", adaTok, nil, nil); code != 403 {
		t.Fatalf("handoff outside a widget thread = %d", code)
	}
	// Another visitor's token is not this visitor.
	if code, s3 := e.session(w.PublicKey, s2.Token, ""); code != 200 || s3.Principal.ID != s2.Principal.ID {
		t.Fatalf("s2 resume = %d %+v", code, s3)
	}
	// A token that is not a visitor's starts a new visitor.
	if code, s4 := e.session(w.PublicKey, adaTok, ""); code != 201 || s4.Principal.ID == ada.ID {
		t.Fatalf("session with Ada's token = %d %+v", code, s4)
	}
}

func TestWidgetInbox(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	_, bobTok := e.user("Bob", core.PrincipalHuman)
	agent, agentTok := e.user("Helper", core.PrincipalAgent)
	var w wireWidget
	e.do("POST", "/widgets", root, map[string]any{
		"name": "Support", "origins": []string{"https://shop.example"},
		"team": []string{ada.ID}, "agent_id": agent.ID,
	}, &w)
	inbox := func(token string) []wireWidgetThread {
		t.Helper()
		var out struct{ Threads []wireWidgetThread }
		if code := e.do("GET", "/widget-threads", token, nil, &out); code != 200 {
			t.Fatalf("inbox = %d", code)
		}
		return out.Threads
	}

	// Cat asks and the agent answers; then Dan asks for a person.
	_, cat := e.session(w.PublicKey, "", "")
	e.do("POST", "/rooms/"+cat.Room.ID+"/messages", cat.Token, textMsg("hi"), nil)
	e.do("POST", "/rooms/"+cat.Room.ID+"/messages", agentTok, textMsg("hello"), nil)
	time.Sleep(2 * time.Millisecond) // the inbox orders by message time
	_, dan := e.session(w.PublicKey, "", signIdentity(w.Secret, map[string]any{"sub": "u1", "name": "Dan", "exp": time.Now().Add(time.Hour).Unix()}))
	e.do("POST", "/rooms/"+dan.Room.ID+"/messages", dan.Token, textMsg("a person, please"), nil)
	e.do("POST", "/rooms/"+dan.Room.ID+"/handoff", dan.Token, nil, nil)

	got := inbox(adaTok)
	if len(got) != 2 || got[0].RoomID != dan.Room.ID || got[1].RoomID != cat.Room.ID {
		t.Fatalf("inbox = %+v; want Dan, then Cat", got)
	}
	d, c := got[0], got[1]
	if d.State != threadHandedOver || !d.Waiting || !d.Verified || d.VisitorName != "Dan" || d.VisitorID != dan.Principal.ID ||
		d.WidgetID != w.ID || d.WidgetName != "Support" || d.LastMessageAt == nil {
		t.Fatalf("Dan's thread = %+v", d)
	}
	if c.State != threadOpen || c.Waiting || c.Verified {
		t.Fatalf("Cat's thread = %+v", c)
	}
	// Ada answers Dan.
	e.do("POST", "/rooms/"+dan.Room.ID+"/messages", adaTok, textMsg("I'm here"), nil)
	if got := inbox(adaTok); got[0].RoomID != dan.Room.ID || got[0].Waiting {
		t.Fatalf("after Ada answered = %+v", got[0])
	}
	// The agent is in both threads; Bob, not on the team, sees none, and
	// visitors have no inbox.
	if got := inbox(agentTok); len(got) != 2 {
		t.Fatalf("agent's inbox = %+v", got)
	}
	if got := inbox(bobTok); len(got) != 0 {
		t.Fatalf("Bob's inbox = %+v", got)
	}
	if code := e.do("GET", "/widget-threads", cat.Token, nil, nil); code != 403 {
		t.Fatalf("visitor's inbox = %d", code)
	}
	var one struct{ Threads []wireWidgetThread }
	if code := e.do("GET", "/widget-threads?limit=1", adaTok, nil, &one); code != 200 || len(one.Threads) != 1 {
		t.Fatalf("limit 1 = %d %+v", code, one)
	}
}

func TestWidgetThreadState(t *testing.T) {
	now := time.Now()
	visitor, agent := "guest", "agent"
	for _, tc := range []struct {
		name    string
		answers bool
		sender  string
		ago     time.Duration
		want    string
	}{
		{"agent answering", true, agent, time.Minute, threadOpen},
		{"handed over", false, agent, time.Minute, threadHandedOver},
		{"quiet with the agent", true, agent, threadIdleAfter, threadIdle},
		{"quiet after a person answered", false, agent, time.Hour, threadIdle},
		{"visitor left waiting on the agent", true, visitor, time.Hour, threadIdle},
		{"visitor left waiting on a person", false, visitor, time.Hour, threadHandedOver},
		{"no message yet", true, "", time.Hour, threadIdle},
	} {
		th := store.WidgetThread{
			Visitor:      store.Visitor{PrincipalID: visitor, CreatedAt: now.Add(-tc.ago)},
			LastSenderID: tc.sender,
			AgentAnswers: tc.answers,
		}
		if tc.sender != "" {
			th.LastMessageAt = now.Add(-tc.ago)
		}
		got := toWidgetThread(th, now)
		if got.State != tc.want {
			t.Errorf("%s: state %q, want %q", tc.name, got.State, tc.want)
		}
		// Every thread but a waiting visitor's goes idle on its own.
		wantIdleAt := now.Add(-tc.ago).Add(threadIdleAfter).UTC()
		if tc.name == "visitor left waiting on a person" {
			if got.IdleAt != nil {
				t.Errorf("%s: idle at %v, want never", tc.name, got.IdleAt)
			}
		} else if got.IdleAt == nil || !got.IdleAt.Equal(wantIdleAt) {
			t.Errorf("%s: idle at %v, want %v", tc.name, got.IdleAt, wantIdleAt)
		}
	}
}

func TestWidgetInboxPush(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	var w wireWidget
	e.do("POST", "/widgets", root, map[string]any{
		"name": "Support", "origins": []string{"https://shop.example"}, "team": []string{ada.ID},
	}, &w)
	_, cat := e.session(w.PublicKey, "", "")
	c := dialWS(t, e, adaTok)
	visitor := dialWS(t, e, cat.Token)

	// Ada follows no room, yet hears that the thread has a new message.
	e.say(cat.Token, cat.Room.ID, "hello?")
	if env := wsRead(t, c); env.Type != "inbox" || env.Status != "" || env.ID == "" {
		t.Fatalf("after the visitor wrote = %+v, want an inbox push", env)
	}
	e.say(adaTok, cat.Room.ID, "hi!")
	if env := wsRead(t, c); env.Type != "inbox" {
		t.Fatalf("after Ada answered = %+v, want an inbox push", env)
	}
	// Other rooms say nothing to the inbox, and visitors have none.
	r := e.room(adaTok, "general")
	if env := wsRead(t, c); env.Type != "rooms" {
		t.Fatalf("after create = %+v, want a rooms push", env)
	}
	e.say(adaTok, r.ID, "elsewhere")
	for _, conn := range []*websocket.Conn{c, visitor} {
		wsSend(t, conn, "1", "ping", nil)
		if env := wsRead(t, conn); env.ID != "1" || env.Type != "ping" {
			t.Fatalf("got %+v, want only the ping reply", env)
		}
	}
}

func TestWidgetOffAndDeleted(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	var w wireWidget
	if code := e.do("POST", "/widgets", root, map[string]any{
		"name": "Support", "origins": []string{"https://shop.example"}, "team": []string{ada.ID},
	}, &w); code != 201 || !w.Enabled {
		t.Fatalf("create = %d %+v", code, w)
	}
	_, s := e.session(w.PublicKey, "", "")
	e.say(s.Token, s.Room.ID, "hello?")

	// Switched off, the widget is gone for visitors, and its visitors
	// cannot chat; switched on again, they can.
	if code := e.do("PATCH", "/widgets/"+w.ID, adaTok, map[string]any{"enabled": false}, nil); code != 403 {
		t.Fatalf("non-admin switch off = %d", code)
	}
	if code := e.do("PATCH", "/widgets/"+w.ID, root, map[string]any{"enabled": false}, &w); code != 200 || w.Enabled {
		t.Fatalf("switch off = %d %+v", code, w)
	}
	if code := e.do("GET", "/embed/"+w.PublicKey, "", nil, nil); code != 404 {
		t.Fatalf("config of a widget that is off = %d", code)
	}
	if code, _ := e.session(w.PublicKey, "", ""); code != 404 {
		t.Fatalf("new session on a widget that is off = %d", code)
	}
	if code := e.do("POST", "/rooms/"+s.Room.ID+"/messages", s.Token, textMsg("still there?"), nil); code != 403 {
		t.Fatalf("visitor sends while off = %d", code)
	}
	if _, _, h := get(t, e.ts.URL+"/embed/"+w.PublicKey); h.Get("Content-Security-Policy") != "frame-ancestors 'self' https://shop.example" {
		t.Fatalf("embed CSP while off = %q", h.Get("Content-Security-Policy"))
	}
	e.do("PATCH", "/widgets/"+w.ID, root, map[string]any{"enabled": true}, nil)
	if code, again := e.session(w.PublicKey, s.Token, ""); code != 200 || again.Room.ID != s.Room.ID {
		t.Fatalf("resume after switching on = %d %+v", code, again)
	}

	// Deleted, its visitors are signed out and the team keeps the threads.
	if code := e.do("DELETE", "/widgets/"+w.ID, adaTok, nil, nil); code != 403 {
		t.Fatalf("non-admin delete = %d", code)
	}
	if code := e.do("DELETE", "/widgets/"+w.ID, root, nil, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code := e.do("DELETE", "/widgets/"+w.ID, root, nil, nil); code != 404 {
		t.Fatalf("delete again = %d", code)
	}
	var list struct{ Widgets []wireWidget }
	if e.do("GET", "/widgets", root, nil, &list); len(list.Widgets) != 0 {
		t.Fatalf("widgets after delete = %+v", list.Widgets)
	}
	if code := e.do("GET", "/rooms/"+s.Room.ID+"/events", s.Token, nil, nil); code != 401 {
		t.Fatalf("visitor after delete = %d", code)
	}
	if code, _ := e.session(w.PublicKey, s.Token, ""); code != 404 {
		t.Fatalf("session after delete = %d", code)
	}
	var events struct{ Events []wireEvent }
	if code := e.do("GET", "/rooms/"+s.Room.ID+"/events", adaTok, nil, &events); code != 200 || len(events.Events) == 0 {
		t.Fatalf("Ada reads the thread after delete = %d %+v", code, events)
	}
}

func TestWidgetIdentity(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	var w wireWidget
	e.do("POST", "/widgets", root, map[string]any{"name": "Help", "origins": []string{"https://shop.example"}, "team": []string{ada.ID}}, &w)

	exp := time.Now().Add(time.Hour).Unix()
	id := signIdentity(w.Secret, map[string]any{"sub": "user-42", "name": "Grace", "exp": exp})
	code, s := e.session(w.PublicKey, "", id)
	if code != 201 || s.Principal.DisplayName != "Grace" || s.Room.Name != "Help · Grace" {
		t.Fatalf("verified = %d %+v", code, s)
	}
	// Another device: same subject, same visitor and thread, a new token.
	code, s2 := e.session(w.PublicKey, "", id)
	if code != 200 || s2.Principal.ID != s.Principal.ID || s2.Room.ID != s.Room.ID || s2.Token == s.Token {
		t.Fatalf("second device = %d %+v", code, s2)
	}
	// An anonymous visitor who signs in as a subject that already has a
	// thread gets that thread. The anonymous thread stays with the team,
	// unmerged, and the inbox says where the visitor went.
	_, anon := e.session(w.PublicKey, "", "")
	e.say(anon.Token, anon.Room.ID, "hello?")
	if code, s3 := e.session(w.PublicKey, anon.Token, id); code != 200 || s3.Principal.ID != s.Principal.ID {
		t.Fatalf("sign-in from anonymous = %d %+v", code, s3)
	}
	var grace struct{ Events []wireEvent }
	if code := e.do("GET", "/rooms/"+s.Room.ID+"/events", s.Token, nil, &grace); code != 200 || len(grace.Events) != 0 {
		t.Fatalf("Grace's thread after sign-in = %d %+v", code, grace)
	}
	linked := func() wireWidgetThread {
		t.Helper()
		var out struct{ Threads []wireWidgetThread }
		e.do("GET", "/widget-threads", adaTok, nil, &out)
		for _, th := range out.Threads {
			if th.RoomID == anon.Room.ID {
				return th
			}
		}
		t.Fatalf("anonymous thread not in the inbox: %+v", out.Threads)
		return wireWidgetThread{}
	}
	if th := linked(); th.SignedInAs == nil || *th.SignedInAs != (wireSignedInAs{VisitorID: s.Principal.ID, RoomID: s.Room.ID}) ||
		th.Verified || th.Waiting {
		t.Fatalf("anonymous thread = %+v", th)
	}
	defer func() {
		// Erasing Grace leaves the anonymous thread, linked to nobody.
		if code := e.do("DELETE", "/visitors/"+s.Principal.ID, root, nil, nil); code != 204 {
			t.Fatalf("delete Grace = %d", code)
		}
		if th := linked(); th.SignedInAs != nil {
			t.Fatalf("anonymous thread after erasing Grace = %+v", th)
		}
	}()

	// An anonymous visitor who signs in as a new subject keeps their
	// thread, now under the host's name. The anonymous token is not
	// reused and stops working.
	_, anon = e.session(w.PublicKey, "", "")
	e.say(anon.Token, anon.Room.ID, "where is my order?")
	linus := signIdentity(w.Secret, map[string]any{"sub": "user-9", "name": "Linus", "exp": exp})
	code, s5 := e.session(w.PublicKey, anon.Token, linus)
	if code != 200 || s5.Principal.ID != anon.Principal.ID || s5.Room.ID != anon.Room.ID || s5.Token == anon.Token ||
		s5.Principal.DisplayName != "Linus" || s5.Room.Name != "Help · Linus" {
		t.Fatalf("sign-in keeps the thread = %d %+v", code, s5)
	}
	if code, _ := e.session(w.PublicKey, anon.Token, ""); code != 201 {
		t.Fatalf("old anonymous token = %d, want a new visitor", code)
	}
	var log struct{ Events []wireEvent }
	if code := e.do("GET", "/rooms/"+s5.Room.ID+"/events", s5.Token, nil, &log); code != 200 || len(log.Events) != 1 {
		t.Fatalf("thread after sign-in = %d %+v", code, log)
	}
	// Another device signed in as the same subject finds that thread.
	if code, s6 := e.session(w.PublicKey, "", linus); code != 200 || s6.Room.ID != anon.Room.ID {
		t.Fatalf("other device = %d %+v", code, s6)
	}
	// Grace's token with someone else's identity is not Grace.
	other := signIdentity(w.Secret, map[string]any{"sub": "user-7", "exp": exp})
	if code, s4 := e.session(w.PublicKey, s.Token, other); code != 201 || s4.Principal.ID == s.Principal.ID {
		t.Fatalf("other identity = %d %+v", code, s4)
	}

	for name, tok := range map[string]string{
		"wrong secret": signIdentity("sk_wrong", map[string]any{"sub": "u", "exp": exp}),
		"expired":      signIdentity(w.Secret, map[string]any{"sub": "u", "exp": time.Now().Add(-time.Hour).Unix()}),
		"no exp":       signIdentity(w.Secret, map[string]any{"sub": "u"}),
		"exp too far":  signIdentity(w.Secret, map[string]any{"sub": "u", "exp": time.Now().Add(48 * time.Hour).Unix()}),
		"no sub":       signIdentity(w.Secret, map[string]any{"exp": exp}),
		"not yet":      signIdentity(w.Secret, map[string]any{"sub": "u", "exp": exp, "nbf": time.Now().Add(30 * time.Minute).Unix()}),
		"alg none":     "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u","exp":9999999999}`)) + ".",
		"garbage":      "nope",
	} {
		var body json.RawMessage
		if code := e.do("POST", "/embed/"+w.PublicKey+"/session", "", map[string]string{"identity": tok}, &body); code != 401 || errCode(body) != "bad_identity" {
			t.Errorf("%s = %d %s", name, code, body)
		}
	}
}

// The widget's iframe sends its token as a WebSocket subprotocol.
func TestWebSocketTokenProtocol(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("Ada", core.PrincipalHuman)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/api/v1/ws"
	conn, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{WSProtocol, TokenProtocol + tok}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != WSProtocol || res.Header.Get("Sec-WebSocket-Protocol") != WSProtocol {
		t.Fatalf("subprotocol = %q", conn.Subprotocol())
	}
	wsSend(t, conn, "1", "ping", nil)
	if env := wsRead(t, conn); env.Status != "ok" {
		t.Fatalf("ping = %+v", env)
	}
	_, res, err = websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{WSProtocol, TokenProtocol + "say_wrong"}})
	if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %v %v", err, res)
	}
}

func TestDeleteVisitor(t *testing.T) {
	e := newEnv(t)
	_, root := e.admin("Root")
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	var w, w2 wireWidget
	e.do("POST", "/widgets", root, map[string]any{"name": "Support", "origins": []string{"https://shop.example"}, "team": []string{ada.ID}}, &w)
	e.do("POST", "/widgets", root, map[string]any{"name": "Sales", "origins": []string{"https://shop.example"}, "team": []string{ada.ID}}, &w2)

	// A visitor deletes its own chat.
	_, s := e.session(w.PublicKey, "", "")
	e.say(s.Token, s.Room.ID, "please forget me")
	if code := e.do("DELETE", "/embed/"+w2.PublicKey+"/session", s.Token, nil, nil); code != 403 {
		t.Fatalf("delete through another widget = %d", code)
	}
	if code := e.do("DELETE", "/embed/"+w.PublicKey+"/session", adaTok, nil, nil); code != 403 {
		t.Fatalf("a person deletes a widget session = %d", code)
	}
	if code := e.do("DELETE", "/embed/"+w.PublicKey+"/session", s.Token, nil, nil); code != 204 {
		t.Fatalf("visitor deletes its chat = %d", code)
	}
	if code := e.do("GET", "/rooms/"+s.Room.ID+"/events", adaTok, nil, nil); code != 404 {
		t.Fatalf("Ada reads the deleted thread = %d", code)
	}
	if code := e.do("GET", "/me", s.Token, nil, nil); code != 401 {
		t.Fatalf("deleted visitor's token = %d", code)
	}
	// The same browser starts over as a new visitor.
	if code, again := e.session(w.PublicKey, s.Token, ""); code != 201 || again.Principal.ID == s.Principal.ID {
		t.Fatalf("session after delete = %d %+v", code, again)
	}

	// An admin deletes a verified visitor, who comes back as a new one.
	id := signIdentity(w.Secret, map[string]any{"sub": "user-1", "exp": time.Now().Add(time.Hour).Unix()})
	_, v := e.session(w.PublicKey, "", id)
	e.say(v.Token, v.Room.ID, "hi")
	if code := e.do("DELETE", "/visitors/"+v.Principal.ID, adaTok, nil, nil); code != 403 {
		t.Fatalf("non-admin deletes a visitor = %d", code)
	}
	if code := e.do("DELETE", "/visitors/"+ada.ID, root, nil, nil); code != 404 {
		t.Fatalf("delete a person as a visitor = %d", code)
	}
	if code := e.do("DELETE", "/visitors/"+v.Principal.ID, root, nil, nil); code != 204 {
		t.Fatalf("admin deletes a visitor = %d", code)
	}
	if code := e.do("DELETE", "/visitors/"+v.Principal.ID, root, nil, nil); code != 404 {
		t.Fatalf("delete twice = %d", code)
	}
	if code, again := e.session(w.PublicKey, "", id); code != 201 || again.Principal.ID == v.Principal.ID || again.Room.ID == v.Room.ID {
		t.Fatalf("verified visitor after delete = %d %+v", code, again)
	}
	var rooms struct{ Rooms []wireRoom }
	if e.do("GET", "/rooms", adaTok, nil, &rooms); len(rooms.Rooms) != 2 {
		t.Fatalf("Ada's rooms = %+v; want the two new threads", rooms)
	}
}
