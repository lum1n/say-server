package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/lum1n/say-server/say"
)

func TestApply(t *testing.T) {
	msg := say.Event{ID: "m", Type: "message", Seq: 1, Stream: "streaming", Parts: []say.Part{say.TextPart("he")}}
	log := apply(nil, msg)
	msg.Parts = []say.Part{say.TextPart("hello")}
	log = apply(log, msg) // a stream frame replaces it
	log = apply(log, say.Event{ID: "e", Type: "edit", Seq: 2, Edits: "m", Stream: "final", Parts: []say.Part{say.TextPart("hello!")}})
	if len(log) != 1 || log[0].Text() != "hello!" || log[0].Stream != "final" || log[0].EditedAt != nil {
		t.Fatalf("after the stream ends: %+v", log)
	}
	log = apply(log, say.Event{ID: "e2", Type: "edit", Seq: 3, Edits: "m", Parts: []say.Part{say.TextPart("hi")}})
	if log[0].Text() != "hi" || log[0].EditedAt == nil {
		t.Fatalf("after an edit: %+v", log[0])
	}
	if log = apply(log, say.Event{ID: "r", Type: "reaction", Seq: 4}); len(log) != 1 {
		t.Fatal("a reaction was shown as a message")
	}
}

// run runs cmd and feeds its messages back into m. It must not be given
// the push wait, which blocks.
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			run(t, m, c)
		}
	case nil:
	default:
		_, next := m.Update(msg)
		run(t, m, next)
	}
}

func TestModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := say.Open(ctx, say.Config{DB: filepath.Join(t.TempDir(), "say.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	me, _ := n.LocalUser(ctx, "ada", "Ada", "tui")
	bob, _ := n.LocalUser(ctx, "bob", "Bob", "test")
	c := n.Client(me.Token)
	rt, err := c.Realtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	m := New(c, rt, "local")
	// A blinking cursor ticks forever.
	m.input.Cursor.SetMode(cursor.CursorStatic)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	// Init without its push wait, which would block.
	run(t, m, m.call(func(ctx context.Context) (tea.Msg, error) {
		p, err := c.Me(ctx)
		return meMsg(p), err
	}))
	run(t, m, m.loadRooms())

	type key = tea.KeyMsg
	typeLine := func(s string) {
		for _, r := range s {
			m.Update(key{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		_, cmd := m.Update(key{Type: tea.KeyEnter})
		run(t, m, cmd)
	}
	typeLine("hello")
	if !strings.Contains(m.status, "no room yet") {
		t.Fatalf("status = %q", m.status)
	}
	typeLine("/new Kitchen")
	if len(m.rooms) != 1 || m.open != m.rooms[0].ID || !strings.Contains(m.View(), "Kitchen") {
		t.Fatalf("after /new: rooms %+v open %q", m.rooms, m.open)
	}
	typeLine("/invite bob")
	if ms, _ := c.Members(ctx, m.open); len(ms) != 2 {
		t.Fatalf("members after /invite = %+v (status %q)", ms, m.status)
	}
	typeLine("/invite nobody")
	if !strings.Contains(m.status, "nobody is called") {
		t.Fatalf("status = %q", m.status)
	}
	typeLine("tea is ready")
	// Bob answers; the push arrives on the socket.
	n.Client(bob.Token).Send(ctx, m.open, []say.Part{say.TextPart("coming")}, "")
	for !strings.Contains(m.View(), "coming") {
		select {
		case p := <-rt.Pushes():
			run(t, m, m.push(p)) // not Update, which waits for the next push
		case <-ctx.Done():
			t.Fatalf("Bob's message never showed:\n%s", m.View())
		}
	}
	view := m.View()
	for _, want := range []string{"tea is ready", "Ada", "Bob", "coming"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
}

// open starts a TUI for user token on node n, with its rooms read.
func open(t *testing.T, ctx context.Context, n *say.Node, token string) *Model {
	t.Helper()
	c := n.Client(token)
	rt, err := c.Realtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	m := New(c, rt, "local")
	m.input.Cursor.SetMode(cursor.CursorStatic)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	run(t, m, m.call(func(ctx context.Context) (tea.Msg, error) {
		p, err := c.Me(ctx)
		return meMsg(p), err
	}))
	run(t, m, m.loadPeople())
	run(t, m, m.loadRooms())
	return m
}

func typeLine(t *testing.T, m *Model, s string) {
	t.Helper()
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(t, m, cmd)
}

// waitFor applies pushes until the view shows want.
func waitFor(t *testing.T, ctx context.Context, m *Model, want string) {
	t.Helper()
	for !strings.Contains(m.View(), want) {
		select {
		case p := <-m.rt.Pushes():
			run(t, m, m.push(p))
		case <-ctx.Done():
			t.Fatalf("%q never showed:\n%s", want, m.View())
		}
	}
}

func TestEarlierMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := say.Open(ctx, say.Config{DB: filepath.Join(t.TempDir(), "say.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	me, _ := n.LocalUser(ctx, "ada", "Ada", "tui")
	c := n.Client(me.Token)
	r, err := c.CreateRoom(ctx, "group", "Log")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2*pageSize + 10 {
		if _, err := c.Send(ctx, r.ID, []say.Part{say.TextPart(fmt.Sprintf("msg %d", i))}, ""); err != nil {
			t.Fatal(err)
		}
	}
	m := open(t, ctx, n, me.Token)
	if got := len(m.logs[r.ID]); got != pageSize || m.first[r.ID] <= 1 {
		t.Fatalf("first page: %d messages, first seq %d", got, m.first[r.ID])
	}
	pgup := func() {
		m.vp.GotoTop()
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		run(t, m, cmd)
	}
	pgup()
	if got := len(m.logs[r.ID]); got != 2*pageSize {
		t.Fatalf("after one pgup: %d messages", got)
	}
	pgup()
	log := m.logs[r.ID]
	if len(log) != 2*pageSize+10 || m.first[r.ID] != 0 || log[0].Text() != "msg 0" || log[len(log)-1].Text() != fmt.Sprintf("msg %d", 2*pageSize+9) {
		t.Fatalf("after reaching the start: %d messages, first seq %d", len(log), m.first[r.ID])
	}
	if cmd := m.loadEarlier(r.ID); cmd != nil {
		t.Fatal("read before the start of the log")
	}
}

func TestEditDMAndMembers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := say.Open(ctx, say.Config{DB: filepath.Join(t.TempDir(), "say.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	me, _ := n.LocalUser(ctx, "ada", "Ada", "tui")
	bob, _ := n.LocalUser(ctx, "bob", "Bob", "test")
	m := open(t, ctx, n, me.Token)

	typeLine(t, m, "/dm bob")
	if len(m.rooms) != 1 || m.rooms[0].Kind != "dm" || m.open != m.rooms[0].ID {
		t.Fatalf("after /dm: rooms %+v open %q (status %q)", m.rooms, m.open, m.status)
	}
	room := m.open
	if !strings.Contains(m.View(), "@ Bob") {
		t.Fatalf("the DM is not named after Bob:\n%s", m.View())
	}
	typeLine(t, m, "/dm Bob")
	if len(m.rooms) != 1 || m.open != room {
		t.Fatalf("a second /dm made another room: %+v", m.rooms)
	}
	typeLine(t, m, "/dm Ada")
	if !strings.Contains(m.status, "you") {
		t.Fatalf("status = %q", m.status)
	}

	typeLine(t, m, "helo bob")
	waitFor(t, ctx, m, "helo bob")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.editing == "" || m.input.Value() != "helo bob" {
		t.Fatalf("up did not start an edit: editing %q input %q", m.editing, m.input.Value())
	}
	m.input.SetValue("")
	typeLine(t, m, "hello bob")
	waitFor(t, ctx, m, "(edited)")
	if log := m.logs[room]; len(log) != 1 || log[0].Text() != "hello bob" || m.editing != "" {
		t.Fatalf("after the edit: %+v", log)
	}
	// Bob's messages are not ours to edit.
	n.Client(bob.Token).Send(ctx, room, []say.Part{say.TextPart("hi ada")}, "")
	waitFor(t, ctx, m, "hi ada")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "hello bob" {
		t.Fatalf("up edits %q", m.input.Value())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.editing != "" || m.input.Value() != "" {
		t.Fatal("esc did not cancel the edit")
	}

	typeLine(t, m, "/members")
	view := m.View()
	for _, want := range []string{"Members", "Ada", "owner", "Bob", "Agents", "Paused", "/agents pause"} {
		if !strings.Contains(view, want) {
			t.Errorf("members view lacks %q:\n%s", want, view)
		}
	}
	typeLine(t, m, "/agents pause")
	if !m.rooms[0].Agents.Paused || !strings.Contains(m.vp.View(), "yes") {
		t.Fatalf("after /agents pause: %+v (status %q)", m.rooms[0].Agents, m.status)
	}
	typeLine(t, m, "/agents hops 3")
	if m.rooms[0].Agents.MaxHopCount != 3 {
		t.Fatalf("after /agents hops 3: %+v (status %q)", m.rooms[0].Agents, m.status)
	}
	typeLine(t, m, "/agents fly")
	if !strings.Contains(m.status, "usage: /agents") {
		t.Fatalf("status = %q", m.status)
	}
	typeLine(t, m, "/responder Bob on")
	if !strings.Contains(m.status, "no agent called") {
		t.Fatalf("status = %q", m.status)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.info || !strings.Contains(m.View(), "hi ada") {
		t.Fatalf("esc did not go back to the messages:\n%s", m.View())
	}
}
