package bridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/api"
	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

func TestMain(m *testing.M) {
	bridge.Fast()
	bridge.Register(bridge.Network{
		Name:         "fake",
		Title:        "Fake",
		Modes:        []string{store.ModeAccount, store.ModeBot},
		Capabilities: []connector.Capability{connector.CapReplies},
		Fields: []bridge.Field{
			{Key: "server", Label: "Server", Kind: "text", Required: true},
			{Key: "password", Label: "Password", Kind: "secret"},
		},
		Open: func(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
			return fakes.open(b, h), nil
		},
		LogOut: func(ctx context.Context, b bridge.Info) error {
			return fakes.logOut(b)
		},
	})
	bridge.Register(bridge.Network{
		Name:       "fake-idempotent",
		Title:      "Fake that takes repeats",
		Modes:      []string{store.ModeAccount},
		Fields:     []bridge.Field{{Key: "server", Label: "Server", Kind: "text", Required: true}},
		Idempotent: true,
		Open: func(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
			return fakes.open(b, h), nil
		},
	})
	os.Exit(m.Run())
}

// fakes holds the connection of every running fake bridge.
var fakes = &fakeNet{conns: map[string]*fakeConn{}}

type fakeNet struct {
	mu    sync.Mutex
	conns map[string]*fakeConn
	// ask makes new connections ask for a code before they connect.
	ask bool
	// saveOnStop makes new connections save their state, slowly, as they
	// stop.
	saveOnStop bool
	// logouts counts the logouts of each bridge; logoutErr fails them.
	logouts   map[string]int
	logoutErr error
}

func (n *fakeNet) logOut(b bridge.Info) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c := n.conns[b.ID]; c != nil && c.isRunning() {
		return errors.New("logged out while the connection runs")
	}
	if n.logouts == nil {
		n.logouts = map[string]int{}
	}
	n.logouts[b.ID]++
	return n.logoutErr
}

func (n *fakeNet) loggedOut(id string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.logouts[id]
}

func (n *fakeNet) open(b bridge.Info, h bridge.Host) *fakeConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := &fakeConn{info: b, h: h, in: make(chan bridge.Incoming, 16), ask: n.ask, saveOnStop: n.saveOnStop}
	n.conns[b.ID] = c
	return c
}

func (n *fakeNet) conn(t *testing.T, id string) *fakeConn {
	t.Helper()
	var c *fakeConn
	eventually(t, "the bridge runs", func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		c = n.conns[id]
		return c != nil && c.isRunning()
	})
	return c
}

type fakeConn struct {
	info bridge.Info
	h    bridge.Host
	in   chan bridge.Incoming
	ask  bool

	saveOnStop bool

	mu      sync.Mutex
	running bool
	answer  string
	sent    []bridge.Outgoing
	edits   []string // remote id: text
	sendErr error
	// echo makes every send come back as an incoming message, as
	// networks do with their own user's messages.
	echo bool
}

func (c *fakeConn) Run(ctx context.Context) error {
	if c.ask {
		a, err := c.h.Ask(ctx, bridge.Prompt{Kind: "code", Text: "Type the code"})
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.answer = a
		c.mu.Unlock()
	}
	c.h.Connected(ctx)
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			if c.saveOnStop {
				time.Sleep(200 * time.Millisecond)
				c.h.SaveState(context.WithoutCancel(ctx), json.RawMessage(`{"stopped":true}`))
			}
			return ctx.Err()
		case m := <-c.in:
			if err := c.h.Receive(ctx, m); err != nil {
				return err
			}
		}
	}
}

func (c *fakeConn) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *fakeConn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return "", c.sendErr
	}
	c.sent = append(c.sent, m)
	id := fmt.Sprintf("r%d", len(c.sent))
	if c.echo {
		// Delivered while the send is still under way, as a network's
		// own receive loop would.
		c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: m.Chat}, ID: id, Author: bridge.Author{ID: "me", Self: true}, Text: m.Text}
	}
	return id, nil
}

func (c *fakeConn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, remoteID+": "+m.Text)
	return nil
}

func (c *fakeConn) sends() []bridge.Outgoing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bridge.Outgoing(nil), c.sent...)
}

func (c *fakeConn) set(f func(*fakeConn)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

type env struct {
	t     *testing.T
	store store.Store
	bus   bus.Bus
	srv   *api.Server
	http  *httptest.Server
	ada   core.Principal
	token string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewInProc()
	e := &env{t: t, store: st, bus: b}
	e.srv = api.New(api.Config{Version: "test", Store: st, Bus: b})
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(func() {
		e.http.Close()
		b.Close()
		st.Close()
	})
	e.ada = core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Ada", Admin: true}
	if err := st.CreatePrincipal(ctx, e.ada); err != nil {
		t.Fatal(err)
	}
	if e.token, err = st.CreateToken(ctx, e.ada.ID, "test", time.Time{}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) runner(holder string) (stop func()) {
	return e.runnerWith(holder, e.store)
}

// runnerWith runs a node's bridges on st.
func (e *env) runnerWith(holder string, st store.Store) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r := bridge.New(bridge.Config{Store: st, Bus: e.bus, Chat: e.srv, Holder: holder})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	e.t.Cleanup(stop)
	return stop
}

// call does an API request as Ada and decodes the answer into out.
func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	var rd *strings.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = strings.NewReader(string(data))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

type wireBridge struct {
	ID         string         `json:"id"`
	State      string         `json:"state"`
	Error      string         `json:"error"`
	Config     map[string]any `json:"config"`
	SecretsSet []string       `json:"secrets_set"`
	Prompt     *bridge.Prompt `json:"prompt"`
}

func (e *env) addBridge(mode string) wireBridge {
	e.t.Helper()
	var b wireBridge
	body := map[string]any{"network": "fake", "mode": mode, "config": map[string]any{"server": "fake.example", "password": "hunter2"}}
	if code := e.call("POST", "/api/v1/bridges", body, &b); code != http.StatusCreated {
		e.t.Fatalf("create bridge: %d", code)
	}
	return b
}

func (e *env) bridge(id string) wireBridge {
	e.t.Helper()
	var b wireBridge
	if code := e.call("GET", "/api/v1/bridges/"+id, nil, &b); code != http.StatusOK {
		e.t.Fatalf("get bridge: %d", code)
	}
	return b
}

// portal waits for the room of a remote chat.
func (e *env) portal(bridgeID, chat string) store.BridgeLink {
	e.t.Helper()
	var l store.BridgeLink
	eventually(e.t, "the chat's room exists", func() bool {
		var err error
		l, err = e.store.BridgeLinkByRemote(context.Background(), bridgeID, chat)
		return err == nil
	})
	return l
}

func (e *env) log(roomID string) []core.Event {
	e.t.Helper()
	evs, err := e.store.EventsAfter(context.Background(), roomID, 0, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *env) delivery(linkID, eventID string) string {
	m, err := e.store.GetBridgeMessage(context.Background(), linkID, eventID)
	if err != nil {
		return ""
	}
	return m.State
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

func TestAccountChatsBecomeRooms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeAccount)
	if b.Config["password"] != nil || len(b.SecretsSet) != 1 || b.Config["server"] != "fake.example" {
		t.Fatalf("the secret is shown: %+v", b)
	}
	e.runner("n1")
	c := fakes.conn(t, b.ID)
	eventually(t, "the bridge is live", func() bool { return e.bridge(b.ID).State == "live" })

	old := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "dm-bob", Name: "Bob", Direct: true}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi Ada", Time: old}
	l := e.portal(b.ID, "dm-bob")
	room, err := e.store.GetRoom(ctx, l.RoomID)
	if err != nil || room.Kind != core.RoomDM || room.Name != "Bob" {
		t.Fatalf("portal = %+v, %v", room, err)
	}
	if m, err := e.store.GetMember(ctx, room.ID, e.ada.ID); err != nil || m.Role != core.RoleOwner {
		t.Fatalf("owner membership = %+v, %v", m, err)
	}
	eventually(t, "Bob's message is in the room", func() bool { return len(e.log(room.ID)) == 1 })
	first := e.log(room.ID)[0]
	bob, _ := e.store.GetPrincipal(ctx, first.SenderID)
	if bob.Kind != core.PrincipalGhost || bob.DisplayName != "Bob" || first.Prose() != "hi Ada" || !first.CreatedAt.Equal(old) {
		t.Fatalf("first message = %+v from %+v", first, bob)
	}

	// The same remote message again is ignored.
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "dm-bob"}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi Ada"}
	// Ada writing from her phone shows as Ada and is not sent back.
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "dm-bob"}, ID: "m2", Author: bridge.Author{ID: "ada-remote", Self: true}, Text: "from my phone"}
	eventually(t, "Ada's phone message is in", func() bool { return len(e.log(room.ID)) == 2 })
	if got := e.log(room.ID)[1]; got.SenderID != e.ada.ID || got.Prose() != "from my phone" {
		t.Fatalf("own message = %+v", got)
	}

	// Ada writes in say: it goes out as her, and the network's echo of it
	// is not taken in twice.
	c.set(func(c *fakeConn) { c.echo = true })
	sent, _, err := e.srv.SendText(ctx, e.ada, room.ID, "hello from say", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the message is sent", func() bool { return e.delivery(l.ID, sent.ID) == store.BridgeSent })
	if s := c.sends(); len(s) != 1 || s[0].Text != "hello from say" || s[0].Chat != "dm-bob" || s[0].Sender != "Ada" {
		t.Fatalf("sends = %+v", s)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.log(room.ID)); n != 3 {
		t.Fatalf("the echo was taken in: %d events", n)
	}
	var page struct {
		Deliveries []struct {
			EventID string `json:"event_id"`
			State   string `json:"state"`
		} `json:"deliveries"`
	}
	e.call("GET", "/api/v1/rooms/"+room.ID+"/events", nil, &page)
	if len(page.Deliveries) != 1 || page.Deliveries[0].EventID != sent.ID || page.Deliveries[0].State != "sent" {
		t.Fatalf("deliveries = %+v", page.Deliveries)
	}

	// Someone else in the room can't write as Ada.
	cleo := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "Cleo"}
	e.store.CreatePrincipal(ctx, cleo)
	e.store.AddMember(ctx, core.Membership{RoomID: room.ID, PrincipalID: cleo.ID, Role: core.RoleMember})
	other, _, err := e.srv.SendText(ctx, cleo, room.ID, "I am not Ada", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Edits go out too.
	if _, err := e.srv.EditAs(ctx, e.ada, room.ID, sent.ID, []core.Part{core.TextPart("hello, edited")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the edit is sent", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.edits) == 1 && c.edits[0] == "r1: hello, edited"
	})
	if s := c.sends(); len(s) != 1 || e.delivery(l.ID, other.ID) != "" {
		t.Fatalf("someone else's message went out: %+v", s)
	}

	// An edit from the network changes the message, and isn't sent back.
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "dm-bob"}, ID: "m1", Edit: true, Text: "hi Ada!"}
	eventually(t, "Bob's edit is in", func() bool {
		m, _ := e.store.GetEvent(ctx, first.ID)
		return m.Prose() == "hi Ada!"
	})
	time.Sleep(100 * time.Millisecond)
	c.mu.Lock()
	edits := len(c.edits)
	c.mu.Unlock()
	if edits != 1 {
		t.Fatalf("Bob's edit was sent back")
	}

	// The room shows the bridge.
	var r struct {
		Bridges []struct {
			Network, State, RemoteName string
		} `json:"bridges"`
	}
	e.call("GET", "/api/v1/rooms/"+room.ID, nil, &r)
	if len(r.Bridges) != 1 || r.Bridges[0].Network != "fake" || r.Bridges[0].State != "live" {
		t.Fatalf("room bridges = %+v", r.Bridges)
	}
}

func TestReplies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeAccount)
	e.runner("n1")
	c := fakes.conn(t, b.ID)
	chat := bridge.RemoteChat{ID: "dm-bob", Name: "Bob", Direct: true}
	bob := bridge.Author{ID: "bob", Name: "Bob"}
	c.in <- bridge.Incoming{Chat: chat, ID: "m1", Author: bob, Text: "lunch?"}
	l := e.portal(b.ID, "dm-bob")
	eventually(t, "Bob's message is in", func() bool { return len(e.log(l.RoomID)) == 1 })
	first := e.log(l.RoomID)[0]

	// A reply from the network is a reply in say.
	c.in <- bridge.Incoming{Chat: chat, ID: "m2", Author: bob, Text: "or dinner?", ReplyTo: "m1", Quote: "lunch?"}
	// A reply to a message say never saw keeps its quote as text.
	c.in <- bridge.Incoming{Chat: chat, ID: "m3", Author: bob, Text: "still?", ReplyTo: "m0", Quote: "coffee?"}
	eventually(t, "the replies are in", func() bool { return len(e.log(l.RoomID)) == 3 })
	log := e.log(l.RoomID)
	if log[1].ReplyTo != first.ID || log[1].Prose() != "or dinner?" {
		t.Fatalf("reply = %+v", log[1])
	}
	if log[2].ReplyTo != "" || log[2].Prose() != "> coffee?\nstill?" {
		t.Fatalf("reply to an unknown message = %+v", log[2])
	}

	// A reply in say names the remote message it answers.
	sent, err := e.srv.SendAs(ctx, e.ada, l.RoomID, "", []core.Part{core.TextPart("yes")}, log[1].ID, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reply is sent", func() bool { return e.delivery(l.ID, sent.ID) == store.BridgeSent })
	if s := c.sends(); len(s) != 1 || s[0].ReplyTo != "m2" {
		t.Fatalf("sends = %+v", s)
	}

	// The room says what its bridges carry.
	var r struct {
		Bridges []struct{ Capabilities []string } `json:"bridges"`
	}
	e.call("GET", "/api/v1/rooms/"+l.RoomID, nil, &r)
	if len(r.Bridges) != 1 || !slices.Equal(r.Bridges[0].Capabilities, []string{"replies"}) {
		t.Fatalf("room bridges = %+v", r.Bridges)
	}
}

func TestUnsureSendsAreNeverRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeAccount)
	stop := e.runner("n1")
	c := fakes.conn(t, b.ID)
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "g", Name: "Group"}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi"}
	l := e.portal(b.ID, "g")

	c.set(func(c *fakeConn) { c.sendErr = errors.New("timeout") })
	a, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "lost?", "", "", false)
	eventually(t, "the send is unknown", func() bool { return e.delivery(l.ID, a.ID) == store.BridgeUnknown })
	c.set(func(c *fakeConn) { c.sendErr = bridge.Rejected(errors.New("no such chat")) })
	f, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "refused", "", "", false)
	eventually(t, "the send failed", func() bool { return e.delivery(l.ID, f.ID) == store.BridgeFailed })

	// Offline: messages wait, and only recent ones go out on return.
	stop()
	c.set(func(c *fakeConn) { c.sendErr = nil })
	if got := e.bridge(b.ID).State; got != "offline" {
		t.Fatalf("stopped bridge is %q", got)
	}
	recent, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "while away", "", "", false)
	stale, err := e.srv.SendAs(ctx, e.ada, l.RoomID, core.NewID(), []core.Part{core.TextPart("long ago")}, "", "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.runner("n2")
	c2 := fakes.conn(t, b.ID)
	eventually(t, "the recent message is sent", func() bool { return e.delivery(l.ID, recent.ID) == store.BridgeSent })
	eventually(t, "the old one expired", func() bool { return e.delivery(l.ID, stale.ID) == store.BridgeExpired })
	for _, s := range c2.sends() {
		if s.Text != "while away" {
			t.Fatalf("resent %q", s.Text)
		}
	}
	if e.delivery(l.ID, a.ID) != store.BridgeUnknown {
		t.Fatal("the unknown send changed")
	}
}

func TestIdempotentSendsAreRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var b wireBridge
	body := map[string]any{"network": "fake-idempotent", "mode": store.ModeAccount, "config": map[string]any{"server": "fake.example"}}
	if code := e.call("POST", "/api/v1/bridges", body, &b); code != http.StatusCreated {
		t.Fatalf("create bridge: %d", code)
	}
	stop := e.runner("n1")
	c := fakes.conn(t, b.ID)
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "g", Name: "Group"}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi"}
	l := e.portal(b.ID, "g")

	// A send with an unknown outcome stays sending and holds up the ones
	// after it.
	c.set(func(c *fakeConn) { c.sendErr = errors.New("timeout") })
	a, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "first", "", "", false)
	eventually(t, "the send failed", func() bool {
		m, err := e.store.GetBridgeMessage(ctx, l.ID, a.ID)
		return err == nil && m.State == store.BridgeSending && m.Error != ""
	})
	next, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "second", "", "", false)
	time.Sleep(100 * time.Millisecond)
	if e.delivery(l.ID, next.ID) != "" {
		t.Fatal("a later message went first")
	}

	// The node stops; another takes over, and old messages do not expire.
	stop()
	if e.delivery(l.ID, a.ID) != store.BridgeSending {
		t.Fatalf("the stopped send is %q", e.delivery(l.ID, a.ID))
	}
	stale, err := e.srv.SendAs(ctx, e.ada, l.RoomID, core.NewID(), []core.Part{core.TextPart("long ago")}, "", "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.runner("n2")
	c2 := fakes.conn(t, b.ID)
	eventually(t, "all are sent", func() bool { return e.delivery(l.ID, stale.ID) == store.BridgeSent })
	var texts []string
	for _, s := range c2.sends() {
		texts = append(texts, s.Text)
		if len(s.Parts) != 1 || s.Parts[0].Kind != core.PartText {
			t.Fatalf("parts = %+v", s.Parts)
		}
	}
	if strings.Join(texts, ", ") != "first, second, long ago" {
		t.Fatalf("sent %v", texts)
	}

	// Parts come in as they are.
	card := core.Part{Kind: core.PartCard, Data: []byte(`{"title":"Plan"}`)}
	c2.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "g"}, ID: "m2", Author: bridge.Author{ID: "bob", Name: "Bob"}, Parts: []core.Part{core.TextPart("see"), card}}
	eventually(t, "the card is in", func() bool {
		log := e.log(l.RoomID)
		last := log[len(log)-1]
		return len(last.Parts) == 2 && last.Parts[1].Kind == core.PartCard
	})
}

func TestLoginPrompt(t *testing.T) {
	e := newEnv(t)
	fakes.mu.Lock()
	fakes.ask = true
	fakes.mu.Unlock()
	defer func() {
		fakes.mu.Lock()
		fakes.ask = false
		fakes.mu.Unlock()
	}()
	b := e.addBridge(store.ModeAccount)
	e.runner("n1")
	eventually(t, "the bridge asks", func() bool { return e.bridge(b.ID).State == "login" })
	if p := e.bridge(b.ID).Prompt; p == nil || p.Kind != "code" {
		t.Fatalf("prompt = %+v", p)
	}
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/answer", map[string]string{"value": "12345"}, nil); code != http.StatusNoContent {
		t.Fatalf("answer: %d", code)
	}
	c := fakes.conn(t, b.ID)
	c.mu.Lock()
	answer := c.answer
	c.mu.Unlock()
	if answer != "12345" {
		t.Fatalf("answer = %q", answer)
	}
	eventually(t, "the bridge is live", func() bool { return e.bridge(b.ID).State == "live" })
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/answer", map[string]string{"value": "again"}, nil); code != http.StatusConflict {
		t.Fatalf("answer without a prompt: %d", code)
	}
}

// A screen code shows the login step on another screen, without a token,
// until it runs out; it shows nothing else about the bridge.
func TestScreenCode(t *testing.T) {
	e := newEnv(t)
	fakes.mu.Lock()
	fakes.ask = true
	fakes.mu.Unlock()
	defer func() {
		fakes.mu.Lock()
		fakes.ask = false
		fakes.mu.Unlock()
	}()
	b := e.addBridge(store.ModeAccount)
	e.runner("n1")
	eventually(t, "the bridge asks", func() bool { return e.bridge(b.ID).State == "login" })

	var made struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/screen", nil, &made); code != http.StatusCreated || len(made.Code) != 11 {
		t.Fatalf("screen: %d %+v", code, made)
	}
	if left := time.Until(made.ExpiresAt); left < 9*time.Minute || left > 10*time.Minute {
		t.Fatalf("expires in %s", left)
	}
	screen := func(code string) (int, map[string]any) {
		res, err := http.Get(e.http.URL + "/api/v1/screens/" + code)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	// Twice, typed loosely the second time: the page polls it.
	for _, code := range []string{made.Code, strings.ToLower(strings.ReplaceAll(made.Code, "-", " "))} {
		status, out := screen(code)
		prompt, _ := out["prompt"].(map[string]any)
		if status != http.StatusOK || out["state"] != "login" || out["network"] != "fake" || prompt["kind"] != "code" {
			t.Fatalf("screen %q: %d %v", code, status, out)
		}
		for _, secret := range []string{"id", "owner_id", "config", "error"} {
			if _, ok := out[secret]; ok {
				t.Fatalf("screen shows %s: %v", secret, out)
			}
		}
	}
	if status, _ := screen("AAAAA-AAAAA"); status != http.StatusNotFound {
		t.Fatalf("wrong code: %d", status)
	}

	// Only its owner may make one.
	bo := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Bo"}
	if err := e.store.CreatePrincipal(context.Background(), bo); err != nil {
		t.Fatal(err)
	}
	token, _ := e.store.CreateToken(context.Background(), bo.ID, "test", time.Time{})
	req, _ := http.NewRequest("POST", e.http.URL+"/api/v1/bridges/"+b.ID+"/screen", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("someone else's bridge: %d", res.StatusCode)
	}

	// A code that ran out shows nothing.
	old, err := e.store.CreateBridgeScreen(context.Background(), b.ID, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := screen(old); status != http.StatusNotFound {
		t.Fatalf("expired code: %d", status)
	}

	// Deleting the bridge takes its codes with it.
	if err := e.store.DeleteBridge(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBotRelaysRooms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeBot)
	var created struct{ ID string }
	e.call("POST", "/api/v1/rooms", map[string]string{"kind": "group", "name": "Team"}, &created)
	before, _, _ := e.srv.SendText(ctx, e.ada, created.ID, "history", "", "", false)
	var link struct{ ID string }
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/links", map[string]string{"room_id": created.ID, "remote_id": "#team"}, &link); code != http.StatusCreated {
		t.Fatalf("link: %d", code)
	}
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/links", map[string]string{"room_id": created.ID, "remote_id": "#team"}, nil); code != http.StatusConflict {
		t.Fatalf("second link: %d", code)
	}
	e.runner("n1")
	c := fakes.conn(t, b.ID)

	// Unlinked channels are ignored; linked ones come in as ghosts.
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "#other"}, ID: "x1", Author: bridge.Author{ID: "zed", Name: "zed"}, Text: "elsewhere"}
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "#team"}, ID: "x2", Author: bridge.Author{ID: "zed", Name: "zed"}, Text: "hey team"}
	eventually(t, "zed's message is in", func() bool { return len(e.log(created.ID)) == 2 })

	msg, _, _ := e.srv.SendText(ctx, e.ada, created.ID, "hi zed", "", "", false)
	eventually(t, "Ada's message is relayed", func() bool { return e.delivery(link.ID, msg.ID) == store.BridgeSent })
	time.Sleep(100 * time.Millisecond)
	s := c.sends()
	if len(s) != 1 || s[0].Text != "hi zed" || s[0].Sender != "Ada" {
		t.Fatalf("sends = %+v; history (%s) and zed's own message must not go out", s, before.ID)
	}
	if _, err := e.store.BridgeLinkByRemote(ctx, b.ID, "#other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a bot made a room for an unlinked channel: %v", err)
	}
	if code := e.call("DELETE", "/api/v1/bridges/"+b.ID+"/links/"+link.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("unlink: %d", code)
	}
}

func TestBridgeSettings(t *testing.T) {
	e := newEnv(t)
	var b wireBridge
	if code := e.call("POST", "/api/v1/bridges", map[string]any{"network": "fake", "config": map[string]any{}}, &b); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing required setting: %d", code)
	}
	if code := e.call("POST", "/api/v1/bridges", map[string]any{"network": "nope"}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown network: %d", code)
	}
	b = e.addBridge(store.ModeAccount)
	// A patch without the secret keeps it; an empty string clears a field.
	var got wireBridge
	e.call("PATCH", "/api/v1/bridges/"+b.ID, map[string]any{"name": "Mine", "config": map[string]any{"server": "other.example"}}, &got)
	if got.Config["server"] != "other.example" || len(got.SecretsSet) != 1 {
		t.Fatalf("patched = %+v", got)
	}
	e.call("PATCH", "/api/v1/bridges/"+b.ID, map[string]any{"config": map[string]any{"password": ""}}, &got)
	if len(got.SecretsSet) != 0 {
		t.Fatalf("secret not cleared: %+v", got)
	}
	e.call("PATCH", "/api/v1/bridges/"+b.ID, map[string]any{"enabled": false}, &got)
	if got.State != "disabled" {
		t.Fatalf("state = %q", got.State)
	}

	// Someone else sees neither the bridge nor its settings.
	bo := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Bo"}
	e.store.CreatePrincipal(context.Background(), bo)
	tok, _ := e.store.CreateToken(context.Background(), bo.ID, "t", time.Time{})
	e.token = tok
	var list struct{ Bridges []wireBridge }
	e.call("GET", "/api/v1/bridges", nil, &list)
	if len(list.Bridges) != 0 {
		t.Fatalf("Bo sees %+v", list.Bridges)
	}
	if code := e.call("GET", "/api/v1/bridges/"+b.ID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("Bo reads Ada's bridge: %d", code)
	}
	if code := e.call("POST", "/api/v1/bridges", map[string]any{"network": "fake", "mode": "bot", "config": map[string]any{"server": "x"}}, nil); code != http.StatusForbidden {
		t.Fatalf("Bo adds a bot: %d", code)
	}
}

// crashedStore is a node's store that never hears it stop: its lease is
// left to expire, as when the node is killed.
type crashedStore struct{ store.Store }

func (crashedStore) ReleaseLease(context.Context, string, string) error { return nil }

func (crashedStore) SetBridgeSeen(context.Context, string, time.Time) error { return nil }

func TestTakeoverAfterCrashSendsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeAccount)
	crash := e.runnerWith("n1", crashedStore{e.store})
	c1 := fakes.conn(t, b.ID)
	e.runner("n2")
	c1.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "g", Name: "Group"}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi"}
	l := e.portal(b.ID, "g")
	first, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "one", "", "", false)
	eventually(t, "n1 sends", func() bool { return e.delivery(l.ID, first.ID) == store.BridgeSent })

	crash()
	during, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "two", "", "", false)
	var c2 *fakeConn
	eventually(t, "n2 takes over once the lease expires", func() bool {
		fakes.mu.Lock()
		defer fakes.mu.Unlock()
		c2 = fakes.conns[b.ID]
		return c2 != c1 && c2.isRunning()
	})
	after, _, _ := e.srv.SendText(ctx, e.ada, l.RoomID, "three", "", "", false)
	eventually(t, "n2 sends", func() bool {
		return e.delivery(l.ID, during.ID) == store.BridgeSent && e.delivery(l.ID, after.ID) == store.BridgeSent
	})
	time.Sleep(300 * time.Millisecond)
	var texts []string
	for _, s := range append(c1.sends(), c2.sends()...) {
		texts = append(texts, s.Text)
	}
	if strings.Join(texts, ",") != "one,two,three" {
		t.Fatalf("sends = %v", texts)
	}
}

// The lease is held until the connection has stopped, so the next node
// gets what it saved on the way out.
func TestHandoverWaitsForStop(t *testing.T) {
	e := newEnv(t)
	fakes.mu.Lock()
	fakes.saveOnStop = true
	fakes.mu.Unlock()
	defer func() {
		fakes.mu.Lock()
		fakes.saveOnStop = false
		fakes.mu.Unlock()
	}()
	b := e.addBridge(store.ModeAccount)
	stop1 := e.runner("n1")
	c1 := fakes.conn(t, b.ID)
	e.runner("n2")
	stop1()
	var c2 *fakeConn
	eventually(t, "n2 takes over", func() bool {
		fakes.mu.Lock()
		defer fakes.mu.Unlock()
		c2 = fakes.conns[b.ID]
		return c2 != c1 && c2.isRunning()
	})
	if string(c2.info.State) != `{"stopped":true}` {
		t.Fatalf("n2 started with state %s", c2.info.State)
	}
}

func TestDeleteBridge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.addBridge(store.ModeAccount)
	e.runner("n1")
	c := fakes.conn(t, b.ID)
	c.in <- bridge.Incoming{Chat: bridge.RemoteChat{ID: "g", Name: "Group"}, ID: "m1", Author: bridge.Author{ID: "bob", Name: "Bob"}, Text: "hi"}
	l := e.portal(b.ID, "g")
	eventually(t, "Bob's message is in", func() bool { return len(e.log(l.RoomID)) == 1 })

	var got wireBridge
	if code := e.call("DELETE", "/api/v1/bridges/"+b.ID, nil, &got); code != http.StatusAccepted || got.State != "deleting" {
		t.Fatalf("delete: %d %+v", code, got)
	}
	eventually(t, "the bridge is gone", func() bool {
		return e.call("GET", "/api/v1/bridges/"+b.ID, nil, nil) == http.StatusNotFound
	})
	if c.isRunning() {
		t.Fatal("the connection still runs")
	}
	if n := fakes.loggedOut(b.ID); n != 1 {
		t.Fatalf("logged out %d times", n)
	}
	// The portal stays, unlinked, with its log.
	if _, err := e.store.GetRoom(ctx, l.RoomID); err != nil {
		t.Fatalf("portal room: %v", err)
	}
	if links, _ := e.store.RoomBridgeLinks(ctx, l.RoomID); len(links) != 0 {
		t.Fatalf("portal still linked: %+v", links)
	}
	if evs := e.log(l.RoomID); len(evs) != 1 || !strings.Contains(string(evs[0].Parts[0].Data), `"hi"`) {
		t.Fatalf("portal log = %+v", evs)
	}
	if code := e.call("DELETE", "/api/v1/bridges/"+b.ID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestDeleteBridgeWhileOff(t *testing.T) {
	e := newEnv(t)
	b := e.addBridge(store.ModeBot)
	e.call("PATCH", "/api/v1/bridges/"+b.ID, map[string]any{"enabled": false}, nil)
	if code := e.call("DELETE", "/api/v1/bridges/"+b.ID, nil, nil); code != http.StatusAccepted {
		t.Fatalf("delete: %d", code)
	}
	// Until a node removes it, it can't be changed or linked.
	if code := e.call("PATCH", "/api/v1/bridges/"+b.ID, map[string]any{"enabled": true}, nil); code != http.StatusConflict {
		t.Fatalf("patch while deleting: %d", code)
	}
	var created struct{ ID string }
	e.call("POST", "/api/v1/rooms", map[string]string{"kind": "group", "name": "Team"}, &created)
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/links", map[string]string{"room_id": created.ID, "remote_id": "#team"}, nil); code != http.StatusConflict {
		t.Fatalf("link while deleting: %d", code)
	}
	if code := e.call("DELETE", "/api/v1/bridges/"+b.ID, nil, nil); code != http.StatusAccepted {
		t.Fatalf("delete again: %d", code)
	}
	e.runner("n1")
	eventually(t, "the bridge is gone", func() bool {
		return e.call("GET", "/api/v1/bridges/"+b.ID, nil, nil) == http.StatusNotFound
	})
	if n := fakes.loggedOut(b.ID); n != 1 {
		t.Fatalf("logged out %d times", n)
	}
}

func TestDeleteBridgeLogoutFails(t *testing.T) {
	e := newEnv(t)
	fakes.mu.Lock()
	fakes.logoutErr = errors.New("network down")
	fakes.mu.Unlock()
	defer func() {
		fakes.mu.Lock()
		fakes.logoutErr = nil
		fakes.mu.Unlock()
	}()
	b := e.addBridge(store.ModeAccount)
	e.runner("n1")
	fakes.conn(t, b.ID)
	e.call("DELETE", "/api/v1/bridges/"+b.ID, nil, nil)
	eventually(t, "the failure shows", func() bool {
		got := e.bridge(b.ID)
		return got.State == "deleting" && strings.Contains(got.Error, "network down")
	})
	// It is tried again, then given up on: the bridge goes anyway.
	eventually(t, "the bridge is gone", func() bool {
		return e.call("GET", "/api/v1/bridges/"+b.ID, nil, nil) == http.StatusNotFound
	})
	if n := fakes.loggedOut(b.ID); n < 2 {
		t.Fatalf("logged out %d times, want retries", n)
	}
}
