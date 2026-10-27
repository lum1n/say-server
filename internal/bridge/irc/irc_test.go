package irc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircmsg"

	"github.com/lum1n/say-server/internal/bridge"
)

// fakeServer speaks enough IRC for the bridge: CAP, registration, labeled
// JOIN and PRIVMSG with echoes, and CHATHISTORY.
type fakeServer struct {
	t  *testing.T
	ln net.Listener

	caps    []string        // offered
	taken   map[string]bool // nicks in use
	banned  map[string]bool // channels that refuse a JOIN
	reject  map[string]bool // targets that refuse a PRIVMSG
	history []line
	targets []string // CHATHISTORY TARGETS answers, nick or channel
	account string   // logged in to, with 900, if set
	// afterJoin, if set, is written to the client after each JOIN reply.
	afterJoin string
	failAfter bool // CHATHISTORY AFTER fails

	got chan ircmsg.Message // everything the client sent

	mu   sync.Mutex
	c    net.Conn
	nick string
	n    int
}

// line is a message in the fake server's history.
type line struct {
	at     time.Time
	id     string
	source string
	target string
	text   string
}

var allCaps = []string{"server-time", "message-tags", "batch", "labeled-response", "echo-message", "draft/chathistory"}

func newServer(t *testing.T, caps []string) *fakeServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, caps: caps, taken: map[string]bool{}, banned: map[string]bool{}, reject: map[string]bool{}, got: make(chan ircmsg.Message, 1000)}
	t.Cleanup(func() { ln.Close(); s.drop() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

// drop closes the client's connection, as a server going away.
func (s *fakeServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		s.c.Close()
	}
}

// write sends a raw line to the client.
func (s *fakeServer) write(raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		fmt.Fprintf(s.c, "%s\r\n", raw)
	}
}

func (s *fakeServer) out(tags map[string]string, source, command string, params ...string) {
	m := ircmsg.MakeMessage(tags, source, command, params...)
	b, err := m.Line()
	if err != nil {
		s.t.Errorf("fake server: %v", err)
		return
	}
	s.write(strings.TrimRight(b, "\r\n"))
}

func (s *fakeServer) serve(c net.Conn) {
	s.mu.Lock()
	s.c = c
	s.mu.Unlock()
	defer c.Close()
	acked := map[string]bool{}
	var nick string
	var user, capStarted, capEnded, registered bool
	welcome := func() {
		if registered || nick == "" || !user || (capStarted && !capEnded) {
			return
		}
		registered = true
		s.mu.Lock()
		s.nick = nick
		s.mu.Unlock()
		if s.account != "" {
			s.out(nil, "srv", "900", nick, nick+"!u@h", s.account, "You are now logged in")
		}
		s.out(nil, "srv", "001", nick, "Welcome")
		s.out(nil, "srv", "005", nick, "CHANTYPES=#", "STATUSMSG=@+", "are supported")
		s.out(nil, "srv", "422", nick, "MOTD File is missing")
	}
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		m, err := ircmsg.ParseLine(sc.Text())
		if err != nil {
			continue
		}
		s.got <- m
		var tags map[string]string
		if ok, l := m.GetTag("label"); ok && acked["labeled-response"] {
			tags = map[string]string{"label": l}
		}
		me := nick + "!u@h"
		switch m.Command {
		case "CAP":
			switch m.Params[0] {
			case "LS":
				capStarted = true
				s.out(nil, "srv", "CAP", "*", "LS", strings.Join(s.caps, " "))
			case "REQ":
				name := m.Params[1]
				off := strings.HasPrefix(name, "-")
				if slices.Contains(s.caps, strings.TrimPrefix(name, "-")) {
					acked[strings.TrimPrefix(name, "-")] = !off
					s.out(nil, "srv", "CAP", "*", "ACK", name)
				} else {
					s.out(nil, "srv", "CAP", "*", "NAK", name)
				}
			case "END":
				capEnded = true
				welcome()
			}
		case "NICK":
			if !registered && s.taken[m.Params[0]] {
				s.out(nil, "srv", "433", "*", m.Params[0], "Nickname is already in use")
				continue
			}
			nick = m.Params[0]
			welcome()
		case "USER":
			user = true
			welcome()
		case "PING":
			s.out(nil, "srv", "PONG", "srv", m.Params[0])
		case "JOIN":
			if s.banned[m.Params[0]] {
				s.out(tags, "srv", "474", nick, m.Params[0], "Cannot join channel (+b)")
				continue
			}
			s.out(tags, me, "JOIN", m.Params[0])
			if s.afterJoin != "" {
				s.write(s.afterJoin)
			}
		case "PRIVMSG":
			switch {
			case s.reject[m.Params[0]]:
				s.out(tags, "srv", "404", nick, m.Params[0], "Cannot send to channel")
			case acked["echo-message"]:
				s.mu.Lock()
				s.n++
				id := fmt.Sprintf("m%d", s.n)
				s.mu.Unlock()
				if tags == nil {
					tags = map[string]string{}
				}
				at := time.Now().UTC().Truncate(time.Millisecond)
				tags["msgid"] = id
				tags["time"] = at.Format(tsFormat)
				// What is sent is in history too, as on a real server.
				s.mu.Lock()
				s.history = append(s.history, line{at: at, id: id, source: me, target: m.Params[0], text: m.Params[1]})
				s.mu.Unlock()
				s.out(tags, me, "PRIVMSG", m.Params[0], m.Params[1])
			case tags != nil:
				s.out(tags, "srv", "ACK")
			}
		case "CHATHISTORY":
			s.chathistory(tags, nick, m.Params)
		case "QUIT":
			return
		}
	}
}

func (s *fakeServer) chathistory(tags map[string]string, nick string, p []string) {
	if p[0] == "TARGETS" {
		s.out(withTag(tags, "", ""), "srv", "BATCH", "+t", "draft/chathistory-targets")
		for _, t := range s.targets {
			s.out(map[string]string{"batch": "t"}, "srv", "CHATHISTORY", "TARGETS", t, time.Now().UTC().Format(tsFormat))
		}
		s.out(nil, "srv", "BATCH", "-t")
		return
	}
	target := fold(p[1])
	var after time.Time
	if p[0] == "AFTER" && s.failAfter {
		s.out(tags, "srv", "FAIL", "CHATHISTORY", "MESSAGE_ERROR", "AFTER", "no history")
		return
	}
	if p[0] == "AFTER" {
		after, _ = time.Parse(tsFormat, strings.TrimPrefix(p[2], "timestamp="))
	}
	s.out(withTag(tags, "", ""), "srv", "BATCH", "+h", "chathistory", p[1])
	s.mu.Lock()
	history := slices.Clone(s.history)
	s.mu.Unlock()
	for _, l := range history {
		// A private chat's history holds lines both ways.
		chat := fold(l.target)
		if chat == fold(nick) {
			chat = fold(strings.Split(l.source, "!")[0])
		}
		if chat != target || (!after.IsZero() && !l.at.After(after)) {
			continue
		}
		s.out(map[string]string{"batch": "h", "time": l.at.UTC().Format(tsFormat), "msgid": l.id}, l.source, "PRIVMSG", l.target, l.text)
	}
	s.out(nil, "srv", "BATCH", "-h")
}

func withTag(tags map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for a, b := range tags {
		out[a] = b
	}
	if k != "" {
		out[k] = v
	}
	return out
}

// sent waits for the client to send a line with the command, and
// returns it.
func (s *fakeServer) sent(t *testing.T, command string) ircmsg.Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-s.got:
			if m.Command == command {
				return m
			}
		case <-timeout:
			t.Fatalf("the client sent no %s", command)
		}
	}
}

// host is a fake bridge.Host.
type host struct {
	chats     []string
	connected chan struct{}
	once      sync.Once
	recv      chan bridge.Incoming
	// hold, while set, makes Receive wait until it is closed.
	hold chan struct{}

	mu     sync.Mutex
	states []json.RawMessage
}

func newHost() *host {
	return &host{connected: make(chan struct{}), recv: make(chan bridge.Incoming, 100)}
}

func (h *host) Receive(ctx context.Context, m bridge.Incoming) error {
	h.mu.Lock()
	hold := h.hold
	h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.recv <- m
	return nil
}

func (h *host) Ask(ctx context.Context, p bridge.Prompt) (string, error) {
	return "", errors.New("nothing to ask")
}

func (h *host) SaveState(ctx context.Context, state json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.states = append(h.states, state)
	return nil
}

func (h *host) Chats(ctx context.Context) ([]string, error) { return h.chats, nil }

func (h *host) Connected(ctx context.Context) { h.once.Do(func() { close(h.connected) }) }

func (h *host) next(t *testing.T) bridge.Incoming {
	t.Helper()
	select {
	case m := <-h.recv:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
		return bridge.Incoming{}
	}
}

func (h *host) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-h.recv:
		t.Fatalf("received %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// start opens a bridge on the server and runs it until the test ends.
func start(t *testing.T, s *fakeServer, h *host, cfg map[string]any, st json.RawMessage) (*conn, chan error) {
	t.Helper()
	cfg["server"] = s.addr()
	if cfg["nick"] == nil {
		cfg["nick"] = "me"
	}
	raw, _ := json.Marshal(cfg)
	bc, err := open(bridge.Info{ID: "b1", Mode: "account", Config: raw, State: st}, h)
	if err != nil {
		t.Fatal(err)
	}
	c := bc.(*conn)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
	select {
	case <-h.connected:
	case err := <-done:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	return c, done
}

func TestLoginJoinAndBackfill(t *testing.T) {
	s := newServer(t, allCaps)
	s.banned["#banned"] = true
	now := time.Now()
	s.history = []line{
		{at: now.Add(-40 * 24 * time.Hour), id: "old", source: "bob!u@h", target: "#a", text: "too old"},
		{at: now.Add(-3 * time.Minute), id: "a1", source: "bob!u@h", target: "#a", text: "hi \x02all\x02"},
		{at: now.Add(-2 * time.Minute), id: "a2", source: "me!u@h", target: "#a", text: "hello bob"},
		{at: now.Add(-1 * time.Minute), id: "d1", source: "alice!u@h", target: "me", text: "psst"},
	}
	s.targets = []string{"alice", "#notjoined"}
	h := newHost()
	h.chats = []string{"#c", "alice", "#A"}
	start(t, s, h, map[string]any{"channels": "#a, b ,#banned", "server_password": "sesame"}, nil)

	if m := s.sent(t, "PASS"); m.Params[0] != "sesame" {
		t.Errorf("PASS %v", m.Params)
	}
	var joins []string
	for range 4 {
		joins = append(joins, s.sent(t, "JOIN").Params[0])
	}
	if want := []string{"#a", "#b", "#banned", "#c"}; !slices.Equal(joins, want) {
		t.Errorf("joined %v, want %v", joins, want)
	}

	m := h.next(t)
	if m.ID != "a1" || m.Text != "hi all" || m.Chat != (bridge.RemoteChat{ID: "#a", Name: "#a"}) || m.Author != (bridge.Author{ID: "bob", Name: "bob"}) {
		t.Errorf("first: %+v", m)
	}
	if !m.Time.Equal(s.history[1].at.Truncate(time.Millisecond)) {
		t.Errorf("time %v, want %v", m.Time, s.history[1].at)
	}
	if m := h.next(t); m.ID != "a2" || !m.Author.Self {
		t.Errorf("own line: %+v", m)
	}
	if m := h.next(t); m.ID != "d1" || m.Chat != (bridge.RemoteChat{ID: "alice", Name: "alice", Direct: true}) || m.Text != "psst" {
		t.Errorf("direct: %+v", m)
	}
	h.none(t)
}

func TestReceive(t *testing.T) {
	s := newServer(t, allCaps)
	h := newHost()
	start(t, s, h, map[string]any{}, nil)

	s.write("@time=2026-10-01T10:00:00.000Z;msgid=x1;+draft/reply=x0 :bob!u@h PRIVMSG #a :\x0304red\x03 text")
	s.write(":bob!u@h PRIVMSG #a :\x01ACTION waves\x01")
	s.write(":bob!u@h PRIVMSG me :\x01VERSION\x01")
	s.write(":Carol!u@h PRIVMSG me :hey there")
	s.write("@msgid=x5 :me!u@h PRIVMSG Dave :from my phone")
	s.write("@msgid=x6 :bob!u@h PRIVMSG @#A :ops only")
	s.write(":srv NOTICE me :server notice")

	m := h.next(t)
	want := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	if m.ID != "x1" || m.Text != "red text" || !m.Time.Equal(want) || m.ReplyTo != "x0" {
		t.Errorf("channel line: %+v", m)
	}
	m = h.next(t)
	if m.Text != "* bob waves" || !strings.HasPrefix(m.ID, "h:") {
		t.Errorf("action: %+v", m)
	}
	m = h.next(t)
	if m.Chat != (bridge.RemoteChat{ID: "carol", Name: "Carol", Direct: true}) || m.Author != (bridge.Author{ID: "carol", Name: "Carol"}) {
		t.Errorf("private: %+v", m)
	}
	m = h.next(t)
	if m.Chat != (bridge.RemoteChat{ID: "dave", Name: "Dave", Direct: true}) || !m.Author.Self || m.Text != "from my phone" {
		t.Errorf("own private line: %+v", m)
	}
	m = h.next(t)
	if m.Chat.ID != "#a" || m.ID != "x6" {
		t.Errorf("status message: %+v", m)
	}
	h.none(t)
}

func TestSend(t *testing.T) {
	s := newServer(t, allCaps)
	s.reject["#closed"] = true
	h := newHost()
	c, _ := start(t, s, h, map[string]any{}, nil)
	ctx := context.Background()

	// Receive is held, as the core holds it while a send on the chat is
	// under way: the send must still complete.
	hold := make(chan struct{})
	h.mu.Lock()
	h.hold = hold
	h.mu.Unlock()
	s.write("@msgid=in1 :bob!u@h PRIVMSG #a :incoming meanwhile")
	time.Sleep(50 * time.Millisecond)

	id, err := c.Send(ctx, bridge.Outgoing{Chat: "#a", EventID: "e1", Text: "hello\n\nworld"})
	if err != nil {
		t.Fatal(err)
	}
	m1, m2 := s.sent(t, "PRIVMSG"), s.sent(t, "PRIVMSG")
	if m1.Params[1] != "hello" || m2.Params[1] != "world" {
		t.Errorf("sent %q, %q", m1.Params[1], m2.Params[1])
	}
	if id != "m1" {
		t.Errorf("id %q, want the echo's msgid m1", id)
	}
	close(hold)
	if m := h.next(t); m.ID != "in1" {
		t.Errorf("received %+v", m)
	}
	// The echoes of what say sent are not received.
	h.none(t)

	long := strings.Repeat("ø", 300)
	if _, err := c.Send(ctx, bridge.Outgoing{Chat: "bob", EventID: "e2", Text: long}); err != nil {
		t.Fatal(err)
	}
	a, b := s.sent(t, "PRIVMSG").Params[1], s.sent(t, "PRIVMSG").Params[1]
	if len(a) > maxLine || a+b != long {
		t.Errorf("split into %d and %d bytes", len(a), len(b))
	}

	_, err = c.Send(ctx, bridge.Outgoing{Chat: "#closed", EventID: "e3", Text: "no"})
	if !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("refused send: %v", err)
	}
}

func TestNoCaps(t *testing.T) {
	s := newServer(t, nil)
	s.history = []line{{at: time.Now(), id: "a1", source: "bob!u@h", target: "#a", text: "hi"}}
	h := newHost()
	c, _ := start(t, s, h, map[string]any{"channels": "#a"}, nil)
	s.sent(t, "JOIN")

	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "#a", EventID: "e1", Text: "hi"})
	if err != nil || id != "say:e1" {
		t.Errorf("Send = %q, %v", id, err)
	}
	s.sent(t, "PRIVMSG")
	// The same line later gets the same id, from its time, sender and
	// text.
	l := "@time=2026-10-01T10:00:00.000Z :bob!u@h PRIVMSG #a :same"
	s.write(l)
	s.write(l)
	m1, m2 := h.next(t), h.next(t)
	if m1.ID != m2.ID || m1.ID == "" {
		t.Errorf("ids %q and %q", m1.ID, m2.ID)
	}
	for {
		select {
		case m := <-s.got:
			if m.Command == "CHATHISTORY" {
				t.Fatal("asked for history the server does not have")
			}
			continue
		default:
		}
		break
	}
}

func TestNickTaken(t *testing.T) {
	s := newServer(t, allCaps)
	s.taken["me"] = true
	h := newHost()
	start(t, s, h, map[string]any{}, nil)

	s.mu.Lock()
	nick := s.nick
	s.mu.Unlock()
	if nick != "me_0" {
		t.Fatalf("registered as %q", nick)
	}
	s.write(":bob!u@h PRIVMSG me_0 :hi")
	s.write("@msgid=y :me_0!u@h PRIVMSG bob :from my phone")
	if m := h.next(t); m.Chat.ID != "bob" || m.Author.Self {
		t.Errorf("%+v", m)
	}
	if m := h.next(t); m.Chat.ID != "bob" || !m.Author.Self {
		t.Errorf("%+v", m)
	}
}

func TestResumeAndState(t *testing.T) {
	s := newServer(t, allCaps)
	seen := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	s.history = []line{
		{at: seen.Add(-time.Minute), id: "before", source: "bob!u@h", target: "#a", text: "seen already"},
		{at: seen.Add(time.Minute), id: "after", source: "bob!u@h", target: "#a", text: "missed"},
	}
	h := newHost()
	st, _ := json.Marshal(state{Seen: map[string]time.Time{"#a": seen}})
	c, done := start(t, s, h, map[string]any{"channels": "#a"}, st)

	m := s.sent(t, "CHATHISTORY")
	if want := []string{"TARGETS"}; m.Params[0] != want[0] {
		t.Errorf("first CHATHISTORY %v", m.Params)
	}
	m = s.sent(t, "CHATHISTORY")
	if want := []string{"AFTER", "#a", "timestamp=" + seen.Format(tsFormat), "100"}; !slices.Equal(m.Params, want) {
		t.Errorf("CHATHISTORY %v, want %v", m.Params, want)
	}
	if m := h.next(t); m.ID != "after" {
		t.Errorf("%+v", m)
	}
	h.none(t)

	// The server goes away: Run ends with an error, and the state is
	// saved with the newer time.
	s.drop()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil after a disconnect")
		}
		done <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a disconnect")
	}
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "#a", EventID: "e", Text: "x"}); !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("Send while disconnected: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		t.Fatal("no state saved")
	}
	var got state
	json.Unmarshal(h.states[len(h.states)-1], &got)
	if want := s.history[1].at; !got.Seen["#a"].Equal(want) {
		t.Errorf("saved %v, want %v", got.Seen["#a"], want)
	}
}

func TestSendNotConnected(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"server": "127.0.0.1:1", "nick": "me"})
	c, err := open(bridge.Info{Config: raw}, newHost())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), bridge.Outgoing{Chat: "#a", Text: "x"}); !errors.Is(err, bridge.ErrRejected) {
		t.Errorf("Send: %v", err)
	}
}

func TestSplit(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want []string
	}{
		{"a\r\nb\n\n c", 10, []string{"a", "b", " c"}},
		{"aaaa bbbb cc", 10, []string{"aaaa bbbb", "cc"}},
		{"abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		{"ææææ", 5, []string{"ææ", "ææ"}},
		{"  \n", 5, nil},
	} {
		if got := split(c.in, c.max); !slices.Equal(got, c.want) {
			t.Errorf("split(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// lastState returns the state the bridge saved last.
func (h *host) lastState(t *testing.T) (json.RawMessage, state) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		t.Fatal("no state saved")
	}
	raw := h.states[len(h.states)-1]
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return raw, st
}

// stopped waits for Run to end after the server went away.
func stopped(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		done <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a disconnect")
	}
}

// A message sent as several lines is not received again from history
// after a reconnect, also on a new connection from saved state.
func TestMultiLineSendNotReplayed(t *testing.T) {
	s := newServer(t, allCaps)
	h := newHost()
	c, done := start(t, s, h, map[string]any{"channels": "#a"}, nil)
	s.write("@msgid=in1;time=" + time.Now().UTC().Format(tsFormat) + " :bob!u@h PRIVMSG #a :before")
	if m := h.next(t); m.ID != "in1" {
		t.Fatalf("%+v", m)
	}
	id, err := c.Send(context.Background(), bridge.Outgoing{Chat: "#a", EventID: "e1", Text: "one\ntwo\nthree"})
	if err != nil {
		t.Fatal(err)
	}
	s.drop()
	stopped(t, done)
	raw, st := h.lastState(t)
	if len(st.Sent["#a"]) != 3 || st.Sent["#a"][0] != id {
		t.Errorf("saved sent ids %v, Send returned %q", st.Sent["#a"], id)
	}

	h2 := newHost()
	start(t, s, h2, map[string]any{"channels": "#a"}, raw)
	for {
		if m := s.sent(t, "CHATHISTORY"); m.Params[0] == "AFTER" {
			break
		}
	}
	h2.none(t)

	// Even reading from before the send, the sent lines are known: only
	// the other line of the time comes in.
	s.mu.Lock()
	s.history = append([]line{{at: time.Now().Add(-30 * time.Minute), id: "in0", source: "bob!u@h", target: "#a", text: "earlier"}}, s.history...)
	s.mu.Unlock()
	st.Seen["#a"] = time.Now().Add(-time.Hour)
	raw, _ = json.Marshal(st)
	h3 := newHost()
	start(t, s, h3, map[string]any{"channels": "#a"}, raw)
	if m := h3.next(t); m.ID != "in0" {
		t.Errorf("%+v", m)
	}
	h3.none(t)
}

// A live line during the catch-up neither moves where the catch-up reads
// from nor gets its time saved before the catch-up is in.
func TestCatchUpFromSnapshot(t *testing.T) {
	s := newServer(t, allCaps)
	seen := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	s.history = []line{{at: seen.Add(time.Minute), id: "missed", source: "bob!u@h", target: "#a", text: "while away"}}
	s.afterJoin = "@msgid=live;time=" + time.Now().UTC().Format(tsFormat) + " :bob!u@h PRIVMSG #a :live"
	h := newHost()
	st, _ := json.Marshal(state{Seen: map[string]time.Time{"#a": seen}})
	start(t, s, h, map[string]any{"channels": "#a"}, st)

	for {
		m := s.sent(t, "CHATHISTORY")
		if m.Params[0] != "AFTER" {
			continue
		}
		if want := "timestamp=" + seen.Format(tsFormat); m.Params[2] != want {
			t.Errorf("AFTER %v, want %s", m.Params, want)
		}
		break
	}
	ids := map[string]bool{h.next(t).ID: true, h.next(t).ID: true}
	if !ids["missed"] || !ids["live"] {
		t.Errorf("received %v", ids)
	}
}

func TestFailedCatchUpKeepsMarker(t *testing.T) {
	s := newServer(t, allCaps)
	s.failAfter = true
	seen := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	h := newHost()
	st, _ := json.Marshal(state{Seen: map[string]time.Time{"#a": seen, "#gone": seen}})
	_, done := start(t, s, h, map[string]any{"channels": "#a"}, st)
	s.write("@msgid=live;time=" + time.Now().UTC().Format(tsFormat) + " :bob!u@h PRIVMSG #a :live")
	if m := h.next(t); m.ID != "live" {
		t.Fatalf("%+v", m)
	}
	s.drop()
	stopped(t, done)
	_, got := h.lastState(t)
	if !got.Seen["#a"].Equal(seen) {
		t.Errorf("saved %v for #a, want the time before the run, %v", got.Seen["#a"], seen)
	}
}

func TestSelfAfterNickCollision(t *testing.T) {
	s := newServer(t, allCaps)
	s.taken["me"] = true
	s.history = []line{{at: time.Now().Add(-time.Minute), id: "old", source: "me!u@h", target: "#a", text: "under my own nick"}}
	h := newHost()
	start(t, s, h, map[string]any{"channels": "#a"}, nil)
	if m := h.next(t); m.ID != "old" || !m.Author.Self {
		t.Errorf("%+v", m)
	}
	s.write("@msgid=x :Me!u@h PRIVMSG me_0 :to myself")
	h.none(t)
}

func TestSelfByAccount(t *testing.T) {
	s := newServer(t, append(slices.Clone(allCaps), "account-tag"))
	s.account = "vegard"
	h := newHost()
	start(t, s, h, map[string]any{}, nil)

	s.write("@msgid=a;account=Vegard :laptop!u@h PRIVMSG #a :from my laptop")
	s.write("@msgid=b;account=mallory :me!u@h PRIVMSG #a :not me")
	s.write("@msgid=c :bob!u@h PRIVMSG #a :no account")
	for _, want := range []struct {
		id   string
		self bool
	}{{"a", true}, {"b", false}, {"c", false}} {
		if m := h.next(t); m.ID != want.id || m.Author.Self != want.self {
			t.Errorf("%s: %+v", want.id, m)
		}
	}
}
