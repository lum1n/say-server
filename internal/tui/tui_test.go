package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/vegard/say-v2/say"
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
