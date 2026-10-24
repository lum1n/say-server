package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

func (e *env) patchAgents(token, roomID string, agents map[string]any) (wireRoom, int) {
	e.t.Helper()
	var r wireRoom
	code := e.do("PATCH", "/rooms/"+roomID, token, map[string]any{"agents": agents}, &r)
	return r, code
}

// send posts text and returns the response, whose body is closed.
func (e *env) send(token, roomID, text string) (*http.Response, apiError) {
	e.t.Helper()
	b, _ := json.Marshal(textMsg(text))
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/rooms/"+roomID+"/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct{ Error apiError }
	json.NewDecoder(res.Body).Decode(&body)
	return res, body.Error
}

func TestRoomShowsAgentPolicy(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	r := e.room(ada, "general")
	want := wireAgentPolicy{MaxHopCount: 2, TurnLimit: DefaultTurnLimit, TurnWindowSeconds: 600, RateLimit: DefaultRateLimit}
	if r.Agents != want {
		t.Fatalf("new room = %+v", r.Agents)
	}
	got, code := e.patchAgents(ada, r.ID, map[string]any{"max_hop_count": 5, "turn_window_seconds": 3600})
	if code != 200 || got.Agents.MaxHopCount != 5 || got.Agents.TurnWindowSeconds != 3600 || got.Agents.TurnLimit != DefaultTurnLimit {
		t.Fatalf("patch = %d %+v", code, got.Agents)
	}
	var listed struct{ Rooms []wireRoom }
	e.do("GET", "/rooms", ada, nil, &listed)
	if len(listed.Rooms) != 1 || listed.Rooms[0].Agents != got.Agents {
		t.Fatalf("list = %+v", listed)
	}
	// Zero goes back to the server default.
	if got, _ := e.patchAgents(ada, r.ID, map[string]any{"max_hop_count": 0}); got.Agents.MaxHopCount != 2 {
		t.Fatalf("reset = %+v", got.Agents)
	}
	for _, bad := range []map[string]any{
		{"max_hop_count": 65}, {"max_hop_count": -1}, {"turn_window_seconds": 30}, {"rate_limit_per_minute": 601},
	} {
		if _, code := e.patchAgents(ada, r.ID, bad); code != 422 {
			t.Fatalf("patch %v = %d", bad, code)
		}
	}
	if _, code := e.patchAgents(ada, r.ID, map[string]any{"nope": 1}); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}
}

// Only people who own or run a room change its agent policy; an agent
// cannot, even in a room it owns.
func TestAgentPolicyWhoMayChange(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	e.join(ada, r.ID, bot.ID, core.RoleAdmin)
	pause := map[string]any{"paused": true}
	if _, code := e.patchAgents(bobTok, r.ID, pause); code != 403 {
		t.Fatalf("member = %d", code)
	}
	if _, code := e.patchAgents(botTok, r.ID, pause); code != 403 {
		t.Fatalf("agent admin = %d", code)
	}
	botRoom := e.room(botTok, "bot's room")
	if _, code := e.patchAgents(botTok, botRoom.ID, pause); code != 403 {
		t.Fatalf("agent owner = %d", code)
	}

	// An instance admin may, in rooms they are in.
	root := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Root", Admin: true}
	if err := e.store.CreatePrincipal(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	rootTok, err := e.store.CreateToken(t.Context(), root.ID, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, code := e.patchAgents(rootTok, r.ID, pause); code != 404 {
		t.Fatalf("admin outside the room = %d", code)
	}
	e.join(ada, r.ID, root.ID, core.RoleMember)
	if got, code := e.patchAgents(rootTok, r.ID, pause); code != 200 || !got.Agents.Paused {
		t.Fatalf("instance admin = %d %+v", code, got.Agents)
	}
}

func TestPauseAgents(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bot.ID, core.RoleMember)
	e.patchAgents(ada, r.ID, map[string]any{"paused": true})
	if res, ae := e.send(botTok, r.ID, "hi"); res.StatusCode != 409 || ae.Code != "agents_paused" {
		t.Fatalf("paused agent = %d %+v", res.StatusCode, ae)
	}
	if res, _ := e.send(ada, r.ID, "people still talk"); res.StatusCode != 201 {
		t.Fatalf("person in paused room = %d", res.StatusCode)
	}
	e.patchAgents(ada, r.ID, map[string]any{"paused": false})
	if res, _ := e.send(botTok, r.ID, "hi"); res.StatusCode != 201 {
		t.Fatalf("unpaused agent = %d", res.StatusCode)
	}
}

// A room's own hop limit replaces the server's.
func TestRoomHopLimit(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	r := e.room(ada, "general")
	e.join(ada, r.ID, bot.ID, core.RoleMember)
	e.patchAgents(ada, r.ID, map[string]any{"max_hop_count": 1})
	last := e.say(ada, r.ID, "hi")
	for i, want := range []int{201, 422} {
		body := textMsg("reply")
		body["caused_by"] = last.ID
		var ev wireEvent
		if code := e.do("POST", "/rooms/"+r.ID+"/messages", botTok, body, &ev); code != want {
			t.Fatalf("hop %d = %d", i+1, code)
		}
		last = ev
	}
}

// Agents that start fresh chains instead of replying still run into the
// room's turn budget and their own rate limit.
func TestAgentBudgets(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	a1, a1Tok := e.user("a1", core.PrincipalAgent)
	a2, a2Tok := e.user("a2", core.PrincipalAgent)
	r := e.room(ada, "general")
	e.join(ada, r.ID, a1.ID, core.RoleMember)
	e.join(ada, r.ID, a2.ID, core.RoleMember)
	e.patchAgents(ada, r.ID, map[string]any{"turn_limit": 3, "rate_limit_per_minute": 2})

	for range 2 {
		if res, _ := e.send(a1Tok, r.ID, "x"); res.StatusCode != 201 {
			t.Fatalf("a1 = %d", res.StatusCode)
		}
	}
	res, ae := e.send(a1Tok, r.ID, "x")
	retry, _ := strconv.Atoi(res.Header.Get("Retry-After"))
	if res.StatusCode != 429 || ae.Code != "rate_limited" || retry < 1 || retry > 60 {
		t.Fatalf("a1 over its rate = %d %+v Retry-After %q", res.StatusCode, ae, res.Header.Get("Retry-After"))
	}
	if res, _ := e.send(a2Tok, r.ID, "x"); res.StatusCode != 201 {
		t.Fatalf("a2 = %d", res.StatusCode)
	}
	res, ae = e.send(a2Tok, r.ID, "x")
	if res.StatusCode != 429 || ae.Code != "agent_budget" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("room over its budget = %d %+v", res.StatusCode, ae)
	}
	if res, _ := e.send(ada, r.ID, "people are not budgeted"); res.StatusCode != 201 {
		t.Fatalf("person = %d", res.StatusCode)
	}
}

func TestMCPSeesAgentPolicy(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("ada", core.PrincipalHuman)
	bot, botTok := e.user("bot", core.PrincipalAgent)
	r := e.room(ada, "lab")
	e.join(ada, r.ID, bot.ID, core.RoleMember)
	e.patchAgents(ada, r.ID, map[string]any{"paused": true, "max_hop_count": 4})
	cs := e.mcp(botTok)
	var rooms mcpListRoomsOut
	call(t, cs, "list_rooms", nil, &rooms)
	if len(rooms.Rooms) != 1 || !rooms.Rooms[0].AgentsPaused || rooms.Rooms[0].MaxHopCount != 4 {
		t.Fatalf("list_rooms = %+v", rooms)
	}
	if msg := call(t, cs, "send", map[string]any{"room_id": r.ID, "text": "hi"}, nil); !strings.Contains(msg, "paused") {
		t.Fatalf("send in paused room = %q", msg)
	}
}
