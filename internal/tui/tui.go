// Package tui is say's terminal client (`say tui`): the room list beside
// the open room, with live delivery. It speaks the say API through a
// say.Client, so it runs the same on an embedded node and on a remote
// server.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/lum1n/say-v2/say"
)

const (
	listWidth = 26
	pageSize  = 100
)

var (
	dim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold     = lipgloss.NewStyle().Bold(true)
	selected = lipgloss.NewStyle().Reverse(true)
	human    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	agent    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	self     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	border   = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, true, false, false).BorderForeground(lipgloss.Color("8"))
)

// Model is the TUI's state.
type Model struct {
	c  *say.Client
	rt *say.Realtime
	// Where is shown in the header: "local" or the server's address.
	where string

	me     say.Principal
	people map[string]say.Principal
	rooms  []say.Room
	logs   map[string][]say.Event // messages only, with edits applied
	loaded map[string]bool        // followed on the socket
	unread map[string]int
	sel    int    // highlighted in the list
	open   string // the open room's id

	focusList bool
	input     textinput.Model
	vp        viewport.Model
	atBottom  bool
	width     int
	height    int
	status    string
}

// New makes the TUI for client c, whose realtime connection is rt.
func New(c *say.Client, rt *say.Realtime, where string) *Model {
	in := textinput.New()
	in.Placeholder = "Message, or /new NAME, /invite NAME, /quit"
	in.Prompt = "› "
	in.Focus()
	vp := viewport.New(0, 0)
	// Only paging keys: the rest would scroll while typing.
	vp.KeyMap = viewport.KeyMap{
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
	}
	vp.MouseWheelEnabled = true
	return &Model{
		c: c, rt: rt, where: where,
		people: map[string]say.Principal{}, logs: map[string][]say.Event{},
		loaded: map[string]bool{}, unread: map[string]int{},
		input: in, vp: vp, atBottom: true,
	}
}

// Messages from commands.
type (
	meMsg     say.Principal
	roomsMsg  []say.Room
	peopleMsg []say.Principal
	pageMsg   struct {
		room   string
		events []say.Event
	}
	pushMsg  say.Push
	closeMsg struct{}
	errMsg   struct{ err error }
)

func (m *Model) call(f func(ctx context.Context) (tea.Msg, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		msg, err := f(ctx)
		if err != nil {
			return errMsg{err}
		}
		return msg
	}
}

func (m *Model) loadRooms() tea.Cmd {
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		rs, err := m.c.Rooms(ctx)
		return roomsMsg(rs), err
	})
}

func (m *Model) loadPeople() tea.Cmd {
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		ps, err := m.c.Principals(ctx)
		return peopleMsg(ps), err
	})
}

func (m *Model) loadPage(room string) tea.Cmd {
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		evs, err := m.c.Events(ctx, room, say.EventsQuery{Limit: pageSize})
		return pageMsg{room, evs}, err
	})
}

func (m *Model) waitPush() tea.Cmd {
	return func() tea.Msg {
		p, ok := <-m.rt.Pushes()
		if !ok {
			return closeMsg{}
		}
		return pushMsg(p)
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.call(func(ctx context.Context) (tea.Msg, error) {
			p, err := m.c.Me(ctx)
			return meMsg(p), err
		}),
		m.loadRooms(), m.loadPeople(), m.waitPush(), textinput.Blink,
	)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
	case meMsg:
		m.me = say.Principal(msg)
	case peopleMsg:
		for _, p := range msg {
			m.people[p.ID] = p
		}
		m.render()
	case roomsMsg:
		m.rooms = msg
		for _, r := range m.rooms {
			if !m.loaded[r.ID] {
				m.loaded[r.ID] = true
				cmds = append(cmds, m.loadPage(r.ID))
			}
		}
		if m.open == "" && len(m.rooms) > 0 {
			m.openRoom(0)
		}
	case pageMsg:
		log := m.logs[msg.room]
		for _, e := range msg.events {
			if e.Type == "message" {
				log = apply(log, e)
			}
		}
		m.logs[msg.room] = log
		var last int64
		if n := len(msg.events); n > 0 {
			last = msg.events[n-1].Seq
		}
		m.rt.Subscribe(msg.room, last)
		if msg.room == m.open {
			m.render()
		}
	case pushMsg:
		cmds = append(cmds, m.waitPush(), m.push(say.Push(msg)))
	case closeMsg:
		m.status = "disconnected"
	case errMsg:
		m.status = msg.err.Error()
	case tea.KeyMsg:
		if cmd, done := m.key(msg); done {
			return m, cmd
		} else if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	if _, ok := msg.(tea.KeyMsg); !ok || !m.focusList {
		m.vp, cmd = m.vp.Update(msg)
		cmds = append(cmds, cmd)
	}
	m.atBottom = m.vp.AtBottom()
	return m, tea.Batch(cmds...)
}

// push applies something the server pushed.
func (m *Model) push(p say.Push) tea.Cmd {
	switch p.Type {
	case "rooms":
		return tea.Batch(m.loadRooms(), m.loadPeople())
	case "event", "stream":
		e := *p.Event
		m.logs[e.RoomID] = apply(m.logs[e.RoomID], e)
		if p.Type == "event" && e.Type == "message" && e.SenderID != m.me.ID && e.RoomID != m.open {
			m.unread[e.RoomID]++
		}
		if e.RoomID == m.open {
			m.render()
		}
		if _, known := m.people[e.SenderID]; !known {
			return m.loadPeople()
		}
	}
	return nil
}

// apply adds e to a room's messages: a message by id, an edit to the
// message it changes. Other events are not shown.
func apply(log []say.Event, e say.Event) []say.Event {
	switch e.Type {
	case "edit":
		for i := range log {
			if log[i].ID != e.Edits {
				continue
			}
			log[i].Parts = e.Parts
			if e.Stream == "final" {
				log[i].Stream = "final"
			} else {
				t := e.CreatedAt
				log[i].EditedAt = &t
			}
		}
	case "message":
		for i := range log {
			if log[i].ID == e.ID {
				log[i] = e
				return log
			}
		}
		log = append(log, e)
	}
	return log
}

// key handles a key press; done means Update should return cmd at once.
func (m *Model) key(k tea.KeyMsg) (cmd tea.Cmd, done bool) {
	switch k.String() {
	case "ctrl+c":
		return tea.Quit, true
	case "tab":
		m.focusList = !m.focusList
		if m.focusList {
			m.input.Blur()
		} else {
			m.input.Focus()
		}
		return nil, true
	case "ctrl+n", "ctrl+down":
		m.openRoom(m.index() + 1)
		return nil, true
	case "ctrl+p", "ctrl+up":
		m.openRoom(m.index() - 1)
		return nil, true
	}
	if m.focusList {
		switch k.String() {
		case "up", "k":
			m.sel = max(m.sel-1, 0)
		case "down", "j":
			m.sel = min(m.sel+1, len(m.rooms)-1)
		case "enter":
			m.openRoom(m.sel)
			m.focusList = false
			m.input.Focus()
		case "q", "esc":
			return tea.Quit, true
		}
		return nil, true
	}
	if k.String() == "enter" {
		text := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		if text == "" {
			return nil, true
		}
		return m.submit(text), true
	}
	return nil, false
}

func (m *Model) index() int {
	return slices.IndexFunc(m.rooms, func(r say.Room) bool { return r.ID == m.open })
}

func (m *Model) openRoom(i int) {
	if len(m.rooms) == 0 {
		return
	}
	i = (i + len(m.rooms)) % len(m.rooms)
	m.sel = i
	m.open = m.rooms[i].ID
	delete(m.unread, m.open)
	m.atBottom = true
	m.render()
}

// submit sends a message or runs a command.
func (m *Model) submit(text string) tea.Cmd {
	m.status = ""
	cmd, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	switch {
	case cmd == "/quit":
		return tea.Quit
	case cmd == "/new":
		if arg == "" {
			m.status = "usage: /new NAME"
			return nil
		}
		return m.call(func(ctx context.Context) (tea.Msg, error) {
			_, err := m.c.CreateRoom(ctx, "group", arg)
			if err != nil {
				return nil, err
			}
			rs, err := m.c.Rooms(ctx)
			return roomsMsg(rs), err
		})
	case cmd == "/invite":
		room := m.open
		if room == "" || arg == "" {
			m.status = "usage: /invite NAME, in a room"
			return nil
		}
		// A fresh directory: the person may be newer than ours.
		return m.call(func(ctx context.Context) (tea.Msg, error) {
			ps, err := m.c.Principals(ctx)
			if err != nil {
				return nil, err
			}
			var who []say.Principal
			for _, p := range ps {
				if strings.EqualFold(p.DisplayName, arg) {
					who = append(who, p)
				}
			}
			switch {
			case len(who) == 0:
				return nil, fmt.Errorf("nobody is called %q", arg)
			case len(who) > 1:
				return nil, fmt.Errorf("%d people are called %q", len(who), arg)
			}
			if err := m.c.AddMember(ctx, room, who[0].ID); err != nil {
				return nil, err
			}
			return peopleMsg(ps), nil
		})
	case strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, "//"):
		m.status = "unknown command " + cmd + " (/new, /invite, /quit; // starts a message with /)"
		return nil
	}
	if m.open == "" {
		m.status = "no room yet: /new NAME makes one"
		return nil
	}
	text = strings.TrimPrefix(text, "/")
	room := m.open
	m.atBottom = true
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		_, err := m.c.Send(ctx, room, []say.Part{say.TextPart(text)}, "")
		return nil, err
	})
}

func (m *Model) layout() {
	w := max(m.width-listWidth-1, 10)
	m.vp.Width = w
	m.vp.Height = max(m.height-3, 1)
	m.input.Width = w - 3
	m.render()
}

// render fills the viewport with the open room's messages.
func (m *Model) render() {
	if m.vp.Width == 0 {
		return
	}
	var b strings.Builder
	var prev say.Event
	for i, e := range m.logs[m.open] {
		if i == 0 || e.SenderID != prev.SenderID || e.CreatedAt.Sub(prev.CreatedAt) > 5*time.Minute {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(m.name(e.SenderID) + " " + dim.Render(e.CreatedAt.Local().Format("15:04")) + "\n")
		}
		prev = e
		b.WriteString(lipgloss.NewStyle().Width(m.vp.Width).Render(body(e)) + "\n")
	}
	if len(m.logs[m.open]) == 0 && m.open != "" {
		b.WriteString(dim.Render("No messages yet. Say something."))
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if m.atBottom {
		m.vp.GotoBottom()
	}
}

func (m *Model) name(id string) string {
	p, ok := m.people[id]
	switch {
	case id == m.me.ID:
		return self.Render(m.me.DisplayName)
	case !ok:
		return human.Render("…")
	case p.Kind == "agent":
		return agent.Render(p.DisplayName) + dim.Render(" agent")
	}
	return human.Render(p.DisplayName)
}

// body is a message's text and the tools it used.
func body(e say.Event) string {
	var lines []string
	for _, p := range e.Parts {
		switch p.Kind {
		case "text":
			var d struct{ Text string }
			json.Unmarshal(p.Data, &d)
			lines = append(lines, d.Text)
		case "tool-call":
			var d struct{ Name, Status string }
			json.Unmarshal(p.Data, &d)
			verb := map[string]string{"running": "using", "failed": "failed:"}[d.Status]
			if verb == "" {
				verb = "used"
			}
			lines = append(lines, dim.Render("⚙ "+verb+" "+d.Name))
		}
	}
	s := strings.Join(lines, "\n")
	if e.Stream == "streaming" {
		s += dim.Render("▍")
	}
	if e.EditedAt != nil {
		s += dim.Render(" (edited)")
	}
	return s
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	var list strings.Builder
	list.WriteString(bold.Render("say") + "\n")
	for _, w := range strings.Split(m.where, " · ") {
		list.WriteString(dim.Render(truncate(w, listWidth-1)) + "\n")
	}
	list.WriteString("\n")
	for i, r := range m.rooms {
		name := r.Name
		if name == "" {
			name = "Untitled room"
		}
		if r.Kind == "channel" {
			name = "# " + name
		}
		if n := m.unread[r.ID]; n > 0 {
			name += fmt.Sprintf(" (%d)", n)
		}
		name = truncate(name, listWidth-2)
		switch {
		case m.focusList && i == m.sel:
			name = selected.Render(name)
		case r.ID == m.open:
			name = bold.Render(name)
		}
		list.WriteString(" " + name + "\n")
	}
	if len(m.rooms) == 0 {
		list.WriteString(dim.Render(" No rooms yet"))
	}
	left := border.Width(listWidth).Height(m.height).Render(list.String())

	title := "…"
	if i := m.index(); i >= 0 {
		title = m.rooms[i].Name
	}
	status := dim.Render(truncate("tab rooms · ctrl+n/p next/prev · pgup/pgdn scroll · ctrl+c quit", m.vp.Width))
	if m.status != "" {
		status = errStyle.Render(truncate(m.status, m.vp.Width))
	}
	right := lipgloss.JoinVertical(lipgloss.Left,
		bold.Render(truncate(title, m.vp.Width)),
		m.vp.View(),
		m.input.View(),
		status,
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func truncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > n {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
