package irc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/lum1n/say-server/internal/bridge"
)

// TestErgo runs the bridge against a real server:
//
//	docker run -d --name say-test-irc-ergo -p 127.0.0.1:18962:6667 ghcr.io/ergochat/ergo:stable
//	SAY_TEST_IRC=127.0.0.1:18962 go test ./internal/bridge/irc/ -run Ergo
func TestErgo(t *testing.T) {
	addr := os.Getenv("SAY_TEST_IRC")
	if addr == "" {
		t.Skip("SAY_TEST_IRC is not set")
	}
	n := rand.IntN(100000)
	me, other, channel := fmt.Sprintf("say%d", n), fmt.Sprintf("bob%d", n), fmt.Sprintf("#saytest%d", n)
	cfg, _ := json.Marshal(map[string]any{"server": addr, "nick": me, "channels": channel})

	run := func(h *host) (*conn, context.CancelFunc) {
		bc, err := open(bridge.Info{ID: "b", Mode: "account", Config: cfg}, h)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- bc.Run(ctx) }()
		select {
		case <-h.connected:
		case err := <-done:
			t.Fatalf("Run: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("not connected")
		}
		return bc.(*conn), func() {
			cancel()
			<-done
		}
	}
	h := newHost()
	c, stop := run(h)

	// bob is a plain client.
	got := make(chan ircmsg.Message, 10)
	joined := make(chan struct{}, 1)
	bob := &ircevent.Connection{Server: addr, Nick: other, RequestCaps: []string{"server-time", "message-tags"}, Log: log.New(io.Discard, "", 0)}
	bob.AddCallback("PRIVMSG", func(m ircmsg.Message) { got <- m })
	bob.AddCallback("JOIN", func(m ircmsg.Message) { joined <- struct{}{} })
	if err := bob.Connect(); err != nil {
		t.Fatal(err)
	}
	defer bob.Quit()
	bob.Join(channel)
	<-joined
	receive := func() ircmsg.Message {
		t.Helper()
		select {
		case m := <-got:
			return m
		case <-time.After(10 * time.Second):
			t.Fatal("bob got nothing")
			return ircmsg.Message{}
		}
	}
	ctx := context.Background()

	// In a channel, both ways.
	bob.Privmsg(channel, "hello from bob")
	in := h.next(t)
	if in.Chat.ID != channel || in.Author.Name != other || in.Text != "hello from bob" {
		t.Errorf("received %+v", in)
	}
	id, err := c.Send(ctx, bridge.Outgoing{Chat: channel, EventID: "e1", Text: "hello from say"})
	if err != nil {
		t.Fatal(err)
	}
	m := receive()
	if _, msgid := m.GetTag("msgid"); m.Params[1] != "hello from say" || msgid != id {
		t.Errorf("bob got %v %q (msgid %q), Send returned %q", m.Params, m.Params[1], msgid, id)
	}

	// Privately, both ways.
	bob.Privmsg(me, "psst")
	in = h.next(t)
	if in.Chat != (bridge.RemoteChat{ID: other, Name: other, Direct: true}) || in.Text != "psst" {
		t.Errorf("received %+v", in)
	}
	pid, err := c.Send(ctx, bridge.Outgoing{Chat: other, EventID: "e2", Text: "psst back"})
	if err != nil {
		t.Fatal(err)
	}
	m = receive()
	if _, msgid := m.GetTag("msgid"); m.Params[0] != other || m.Params[1] != "psst back" || msgid != pid {
		t.Errorf("bob got %v (msgid %q), Send returned %q", m.Params, msgid, pid)
	}
	h.none(t)
	stop()

	// Connecting again reads the channel's history back, with the ids
	// seen the first time, so the core knows every line.
	h2 := newHost()
	_, stop = run(h2)
	defer stop()
	ids := map[string]bridge.Incoming{}
	for {
		select {
		case in := <-h2.recv:
			ids[in.ID] = in
			continue
		case <-time.After(2 * time.Second):
		}
		break
	}
	if in, ok := ids[id]; !ok || !in.Author.Self || in.Chat.ID != channel {
		t.Errorf("own line in history: %+v (all: %+v)", in, ids)
	}
	t.Logf("history after reconnect: %d lines", len(ids))
}
