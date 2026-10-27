package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lum1n/say-server/internal/core"
)

// botConn reads a bot socket, keeping the pushes it skips while waiting
// for a reply.
type botConn struct {
	t       *testing.T
	c       *websocket.Conn
	pending []envelope
}

func dialBot(t *testing.T, e *env, token string) *botConn {
	return &botConn{t: t, c: dialWS(t, e, token)}
}

func (b *botConn) send(id, typ string, payload any) { wsSend(b.t, b.c, id, typ, payload) }

// reply reads until the reply to request id.
func (b *botConn) reply(id string) envelope {
	b.t.Helper()
	for {
		env := wsRead(b.t, b.c)
		if env.Status != "" && env.ID == id {
			return env
		}
		b.pending = append(b.pending, env)
	}
}

// event returns the next event push, checking its text, unless text is
// "*", and its trigger.
func (b *botConn) event(text string, trigger core.Trigger) wireEvent {
	b.t.Helper()
	for {
		var env envelope
		if len(b.pending) > 0 {
			env, b.pending = b.pending[0], b.pending[1:]
		} else {
			env = wsRead(b.t, b.c)
		}
		if env.Status != "" || env.Type != "event" {
			continue
		}
		ev := eventOf(b.t, env)
		if text != "*" && partText(ev) != text || env.Trigger != trigger {
			b.t.Fatalf("event %d %q trigger %q, want %q trigger %q", ev.Seq, partText(ev), env.Trigger, text, trigger)
		}
		return ev
	}
}

func (b *botConn) hello() {
	b.t.Helper()
	b.send("h", "hello", map[string]any{"name": "echo", "version": "1.2", "capabilities": []string{"streaming"}})
	if env := b.reply("h"); env.Status != "ok" {
		b.t.Fatalf("hello reply = %+v", env)
	}
}

func wsErrCode(t *testing.T, env envelope) string {
	t.Helper()
	if env.Status != "error" {
		t.Fatalf("status = %q, want error (%s)", env.Status, env.Payload)
	}
	var p struct{ Code string }
	json.Unmarshal(env.Payload, &p)
	return p.Code
}

func TestBotSession(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bot, botTok := e.user("Echo", core.PrincipalAgent)
	lab := e.room(ada, "lab")
	e.join(ada, lab.ID, bot.ID, core.RoleMember)
	e.say(ada, lab.ID, "hi all")

	// Only agents open bot sessions.
	h := dialBot(t, e, ada)
	h.send("1", "hello", nil)
	if code := wsErrCode(t, h.reply("1")); code != "forbidden" {
		t.Fatalf("human hello: %s", code)
	}

	c := dialBot(t, e, botTok)
	c.send("a", "ack", map[string]any{"room_id": lab.ID, "seq": 1})
	if code := wsErrCode(t, c.reply("a")); code != "conflict" {
		t.Fatalf("ack before hello: %s", code)
	}
	c.hello()
	// A room the bot never acked starts core.LookBack events back.
	c.event("hi all", "")
	c.send("s", "subscribe", map[string]any{"room_id": lab.ID})
	if code := wsErrCode(t, c.reply("s")); code != "conflict" {
		t.Fatalf("subscribe on a bot session: %s", code)
	}
	c.send("h2", "hello", nil)
	if code := wsErrCode(t, c.reply("h2")); code != "conflict" {
		t.Fatalf("second hello: %s", code)
	}

	// The directory shows the bot online.
	var dir struct{ Principals []wirePrincipal }
	e.do("GET", "/principals", ada, nil, &dir)
	var conn *wireConnector
	for _, p := range dir.Principals {
		if p.ID == bot.ID {
			conn = p.Connector
		} else if p.Connector != nil {
			t.Fatalf("%s has a connector", p.DisplayName)
		}
	}
	if conn == nil || conn.Name != "echo" || conn.Version != "1.2" || len(conn.Capabilities) != 1 {
		t.Fatalf("bot connector = %+v", conn)
	}

	e.say(ada, lab.ID, "@echo ping")
	ping := c.event("@echo ping", core.TriggerMention)
	c.send("a", "ack", map[string]any{"room_id": lab.ID, "seq": ping.Seq})
	if env := c.reply("a"); env.Status != "ok" {
		t.Fatalf("ack = %+v", env)
	}

	// A reply streams over the socket and ends with an edit.
	c.send("r", "send", map[string]any{"room_id": lab.ID, "caused_by": ping.ID, "stream": "streaming", "parts": []any{}})
	reply := eventOf(t, c.reply("r"))
	if reply.Causation.HopCount != 1 || reply.Stream != core.StreamStreaming {
		t.Fatalf("reply = %+v", reply)
	}
	c.send("u", "edit", map[string]any{"room_id": lab.ID, "event_id": reply.ID, "stream": "streaming", "parts": parts("po")})
	if env := c.reply("u"); env.Status != "ok" || partText(eventOf(t, env)) != "po" {
		t.Fatalf("streaming edit = %+v", env)
	}
	c.send("f", "edit", map[string]any{"room_id": lab.ID, "event_id": reply.ID, "stream": "final", "parts": parts("pong")})
	if env := c.reply("f"); env.Status != "ok" || eventOf(t, env).Stream != core.StreamFinal {
		t.Fatalf("final edit = %+v", env)
	}
	c.event("*", "")    // the bot's own reply, as far as it got
	c.event("pong", "") // and the edit that ended it

	// Unacked events come again after a reconnect, from the stored cursor.
	c.c.Close(websocket.StatusNormalClosure, "")
	e.say(ada, lab.ID, "@Echo again")
	c = dialBot(t, e, botTok)
	c.hello()
	c.event("pong", "")
	c.event("pong", "")
	c.event("@Echo again", core.TriggerMention)

	// A room the bot is added to is followed at once; a DM triggers.
	var dm wireRoom
	e.do("POST", "/rooms", ada, map[string]any{"kind": "dm"}, &dm)
	e.join(ada, dm.ID, bot.ID, core.RoleMember)
	e.say(ada, dm.ID, "hello there")
	c.event("hello there", core.TriggerDM)

	// A stream from someone else triggers once, when it ends.
	var msg wireEvent
	e.do("POST", "/rooms/"+lab.ID+"/messages", ada, map[string]any{"stream": "streaming", "parts": parts("@echo wh")}, &msg)
	c.event("@echo wh", "")
	e.do("PATCH", "/rooms/"+lab.ID+"/messages/"+msg.ID, ada, map[string]any{"stream": "final", "parts": parts("@echo what?")}, nil)
	c.event("@echo what?", core.TriggerMention)

	// Closing the socket takes the bot offline.
	c.c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		dir.Principals = nil
		e.do("GET", "/principals", ada, nil, &dir)
		online := false
		for _, p := range dir.Principals {
			online = online || p.Connector != nil
		}
		if !online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bot still online after closing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBotSessionResponderAndObserver(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bot, botTok := e.user("Echo", core.PrincipalAgent)
	team := e.room(ada, "team")
	e.join(ada, team.ID, bot.ID, core.RoleMember)
	if code := e.do("PATCH", "/rooms/"+team.ID+"/members/"+bot.ID, ada, map[string]any{"responder": true}, nil); code != http.StatusOK {
		t.Fatalf("make responder: %d", code)
	}
	quiet := e.room(ada, "quiet")
	e.join(ada, quiet.ID, bot.ID, core.RoleObserver)

	c := dialBot(t, e, botTok)
	c.hello()
	e.say(ada, quiet.ID, "@echo can you hear me")
	c.event("@echo can you hear me", "")
	e.say(ada, team.ID, "anyone?")
	c.event("anyone?", core.TriggerResponder)

	// The session caches the membership; the room list push that comes
	// with a change makes it reread.
	if code := e.do("PATCH", "/rooms/"+team.ID+"/members/"+bot.ID, ada, map[string]any{"responder": false}, nil); code != http.StatusOK {
		t.Fatalf("unmake responder: %d", code)
	}
	e.say(ada, team.ID, "anyone now?")
	c.event("anyone now?", "")
}
