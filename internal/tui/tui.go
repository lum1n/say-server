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
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/lum1n/say-server/say"
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
	// first is the seq of the oldest event read from a room's log, or 0
	// when nothing is before it.
	first   map[string]int64
	earlier map[string]bool // an earlier page on its way
	members map[string][]say.Member
	unread  map[string]int
	sel     int    // highlighted in the list
	open    string // the open room's id

	focusList bool
	info      bool   // the open room's members and agent settings, not its log
	editing   string // the id of the message the input edits
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
	in.Placeholder = placeholder
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
		loaded: map[string]bool{}, first: map[string]int64{}, earlier: map[string]bool{},
		members: map[string][]say.Member{}, unread: map[string]int{},
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
		older  bool // the page before what was read, not the latest
	}
	membersMsg struct {
		room    string
		members []say.Member
	}
	roomMsg say.Room // a room's settings changed
	openMsg struct { // the room list, and the room to open in it
		rooms []say.Room
		open  string
	}
	earlierErrMsg struct {
		room string
		err  error
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
		return pageMsg{room, evs, false}, err
	})
}

// loadEarlier reads the page before the oldest event read, once at a time.
func (m *Model) loadEarlier(room string) tea.Cmd {
	before := m.first[room]
	if before <= 1 || m.earlier[room] {
		return nil
	}
	m.earlier[room] = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		evs, err := m.c.Events(ctx, room, say.EventsQuery{Before: before, Limit: pageSize})
		if err != nil {
			return earlierErrMsg{room, err}
		}
		return pageMsg{room, evs, true}
	}
}

func (m *Model) loadMembers(room string) tea.Cmd {
	if room == "" {
		return nil
	}
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		ms, err := m.c.Members(ctx, room)
		return membersMsg{room, ms}, err
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
		cmds = append(cmds, m.setRooms(msg))
		if m.open == "" && len(m.rooms) > 0 {
			cmds = append(cmds, m.openRoom(0))
		}
	case openMsg:
		cmds = append(cmds, m.setRooms(msg.rooms))
		if i := slices.IndexFunc(m.rooms, func(r say.Room) bool { return r.ID == msg.open }); i >= 0 {
			cmds = append(cmds, m.openRoom(i))
		}
	case roomMsg:
		if i := slices.IndexFunc(m.rooms, func(r say.Room) bool { return r.ID == msg.ID }); i >= 0 {
			m.rooms[i] = say.Room(msg)
		}
		m.render()
	case membersMsg:
		m.members[msg.room] = msg.members
		m.render()
	case pageMsg:
		if msg.older {
			m.earlier[msg.room] = false
			m.logs[msg.room] = prepend(m.logs[msg.room], msg.events)
			m.first[msg.room] = firstSeq(msg.events)
			if msg.room == m.open && !m.info {
				// The lines added above keep what was on screen in place.
				lines, off := m.vp.TotalLineCount(), m.vp.YOffset
				m.render()
				m.vp.SetYOffset(off + m.vp.TotalLineCount() - lines)
			}
			break
		}
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
		m.first[msg.room] = firstSeq(msg.events)
		m.rt.Subscribe(msg.room, last)
		if msg.room == m.open {
			m.render()
		}
	case earlierErrMsg:
		m.earlier[msg.room] = false
		m.status = msg.err.Error()
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
		// Members may have changed too.
		return tea.Batch(m.loadRooms(), m.loadPeople(), m.loadMembers(m.open))
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

// setRooms keeps the room list, starts reading the rooms not read yet and
// reads the members of direct messages, which are named after them.
func (m *Model) setRooms(rooms []say.Room) tea.Cmd {
	m.rooms = rooms
	var cmds []tea.Cmd
	for _, r := range m.rooms {
		if !m.loaded[r.ID] {
			m.loaded[r.ID] = true
			cmds = append(cmds, m.loadPage(r.ID))
		}
		if r.Kind == "dm" && r.ID != m.open {
			cmds = append(cmds, m.loadMembers(r.ID))
		}
	}
	return tea.Batch(cmds...)
}

// firstSeq is the seq of a page's first event, or 0 if the page is the
// start of the log: a short page is.
func firstSeq(page []say.Event) int64 {
	if len(page) < pageSize {
		return 0
	}
	return page[0].Seq
}

// prepend puts an earlier page's messages before log. Their parts already
// have the page's edits applied.
func prepend(log, page []say.Event) []say.Event {
	var older []say.Event
	for _, e := range page {
		if e.Type == "message" && !slices.ContainsFunc(log, func(x say.Event) bool { return x.ID == e.ID }) {
			older = append(older, e)
		}
	}
	return append(older, log...)
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
		return m.openRoom(m.index() + 1), true
	case "ctrl+p", "ctrl+up":
		return m.openRoom(m.index() - 1), true
	}
	if m.focusList {
		switch k.String() {
		case "up", "k":
			m.sel = max(m.sel-1, 0)
		case "down", "j":
			m.sel = min(m.sel+1, len(m.rooms)-1)
		case "enter":
			cmd = m.openRoom(m.sel)
			m.focusList = false
			m.input.Focus()
			return cmd, true
		case "q", "esc":
			return tea.Quit, true
		}
		return nil, true
	}
	switch k.String() {
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		if m.editing != "" {
			return m.saveEdit(text), true
		}
		if text == "" {
			return nil, true
		}
		return m.submit(text), true
	case "esc":
		switch {
		case m.editing != "":
			m.stopEditing()
			m.input.SetValue("")
		case m.info:
			m.info = false
			m.atBottom = true
			m.render()
		}
		return nil, true
	case "up":
		if m.input.Value() == "" && m.editing == "" && !m.info {
			m.startEdit()
			return nil, true
		}
	case "pgup":
		// Scrolling up past the oldest message read reads the page before it.
		if !m.info && m.vp.AtTop() {
			return m.loadEarlier(m.open), false
		}
	}
	return nil, false
}

// startEdit puts the user's last message in the open room into the input.
func (m *Model) startEdit() {
	log := m.logs[m.open]
	for i := len(log) - 1; i >= 0; i-- {
		e := log[i]
		if e.SenderID != m.me.ID || e.Stream == "streaming" {
			continue
		}
		if slices.ContainsFunc(e.Parts, func(p say.Part) bool { return p.Kind != "text" }) {
			m.status = "only text messages can be edited here"
			return
		}
		m.editing = e.ID
		m.input.SetValue(e.Text())
		m.input.CursorEnd()
		m.status = ""
		return
	}
	m.status = "nothing of yours to edit here"
}

func (m *Model) stopEditing() {
	m.editing = ""
	m.status = ""
}

// saveEdit changes the message being edited to text. The edit arrives on
// the socket like any other.
func (m *Model) saveEdit(text string) tea.Cmd {
	id, room := m.editing, m.open
	m.stopEditing()
	if text == "" {
		m.status = "an edit needs text: esc cancels"
		m.editing = id
		return nil
	}
	if e := m.find(room, id); e != nil && e.Text() == text {
		return nil
	}
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		_, err := m.c.Edit(ctx, room, id, []say.Part{say.TextPart(text)})
		return nil, err
	})
}

func (m *Model) find(room, id string) *say.Event {
	log := m.logs[room]
	if i := slices.IndexFunc(log, func(e say.Event) bool { return e.ID == id }); i >= 0 {
		return &log[i]
	}
	return nil
}

func (m *Model) index() int {
	return slices.IndexFunc(m.rooms, func(r say.Room) bool { return r.ID == m.open })
}

func (m *Model) openRoom(i int) tea.Cmd {
	if len(m.rooms) == 0 {
		return nil
	}
	i = (i + len(m.rooms)) % len(m.rooms)
	if m.editing != "" {
		m.stopEditing()
		m.input.SetValue("")
	}
	m.sel = i
	m.open = m.rooms[i].ID
	delete(m.unread, m.open)
	m.atBottom = true
	m.render()
	return m.loadMembers(m.open)
}

// commands is what the help and the unknown command message list.
const (
	commands    = "/new, /dm, /invite, /members, /agents, /responder, /quit"
	placeholder = "Message, or /new NAME, /dm NAME, /invite NAME, /members, /quit"
)

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
	case cmd == "/dm":
		if arg == "" {
			m.status = "usage: /dm NAME"
			return nil
		}
		return m.dm(arg)
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
			who, err := named(ps, arg)
			if err != nil {
				return nil, err
			}
			if err := m.c.AddMember(ctx, room, who.ID); err != nil {
				return nil, err
			}
			ms, err := m.c.Members(ctx, room)
			return tea.BatchMsg{
				func() tea.Msg { return peopleMsg(ps) },
				func() tea.Msg { return membersMsg{room, ms} },
			}, err
		})
	case cmd == "/members":
		if m.open == "" {
			m.status = "no room yet: /new NAME makes one"
			return nil
		}
		m.info = !m.info
		m.atBottom = !m.info
		m.render()
		if m.info {
			m.vp.GotoTop()
			return m.loadMembers(m.open)
		}
		return nil
	case cmd == "/agents":
		return m.agents(arg)
	case cmd == "/responder":
		return m.responder(arg)
	case strings.HasPrefix(cmd, "/") && !strings.HasPrefix(cmd, "//"):
		m.status = "unknown command " + cmd + " (" + commands + "; // starts a message with /)"
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

// named is the one principal in ps called name.
func named(ps []say.Principal, name string) (say.Principal, error) {
	var who []say.Principal
	for _, p := range ps {
		if strings.EqualFold(p.DisplayName, name) {
			who = append(who, p)
		}
	}
	switch {
	case len(who) == 0:
		return say.Principal{}, fmt.Errorf("nobody is called %q", name)
	case len(who) > 1:
		return say.Principal{}, fmt.Errorf("%d people are called %q", len(who), name)
	}
	return who[0], nil
}

// dm opens the direct messages with the person called name, starting
// them if there are none yet.
func (m *Model) dm(name string) tea.Cmd {
	me := m.me.ID
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		ps, err := m.c.Principals(ctx)
		if err != nil {
			return nil, err
		}
		who, err := named(ps, name)
		if err != nil {
			return nil, err
		}
		if who.ID == me {
			return nil, fmt.Errorf("that is you")
		}
		rs, err := m.c.Rooms(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.Kind != "dm" {
				continue
			}
			ms, err := m.c.Members(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			if len(ms) == 2 && slices.ContainsFunc(ms, func(x say.Member) bool { return x.PrincipalID == who.ID }) {
				return openMsg{rs, r.ID}, nil
			}
		}
		r, err := m.c.CreateRoom(ctx, "dm", "")
		if err != nil {
			return nil, err
		}
		if err := m.c.AddMember(ctx, r.ID, who.ID); err != nil {
			return nil, err
		}
		rs, err = m.c.Rooms(ctx)
		return openMsg{rs, r.ID}, err
	})
}

// agents changes the open room's agent settings: /agents pause, resume,
// or a limit and its value.
func (m *Model) agents(arg string) tea.Cmd {
	room := m.open
	usage := "usage: /agents pause, resume, hops N, turns N, window MINUTES or rate N (0 is the default)"
	if room == "" {
		m.status = "no room yet: /new NAME makes one"
		return nil
	}
	var p say.AgentPatch
	f := strings.Fields(arg)
	switch {
	case len(f) == 1 && (f[0] == "pause" || f[0] == "resume"):
		paused := f[0] == "pause"
		p.Paused = &paused
	case len(f) == 2:
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 0 {
			m.status = usage
			return nil
		}
		switch f[0] {
		case "hops":
			p.MaxHopCount = &n
		case "turns":
			p.TurnLimit = &n
		case "window":
			n *= 60
			p.TurnWindowSeconds = &n
		case "rate":
			p.RateLimit = &n
		default:
			m.status = usage
			return nil
		}
	default:
		m.status = usage
		return nil
	}
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		r, err := m.c.UpdateAgents(ctx, room, p)
		return roomMsg(r), err
	})
}

// responder makes an agent in the open room answer every message, or only
// @mentions: /responder NAME on|off.
func (m *Model) responder(arg string) tea.Cmd {
	room := m.open
	i := strings.LastIndex(arg, " ")
	if room == "" || i < 0 || (arg[i+1:] != "on" && arg[i+1:] != "off") {
		m.status = "usage: /responder NAME on|off, in a room"
		return nil
	}
	name, on := strings.TrimSpace(arg[:i]), arg[i+1:] == "on"
	return m.call(func(ctx context.Context) (tea.Msg, error) {
		ms, err := m.c.Members(ctx, room)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(ms, func(x say.Member) bool { return x.Kind == "agent" && strings.EqualFold(x.DisplayName, name) })
		if i < 0 {
			return nil, fmt.Errorf("no agent called %q is in the room", name)
		}
		if _, err := m.c.SetResponder(ctx, room, ms[i].PrincipalID, on); err != nil {
			return nil, err
		}
		ms, err = m.c.Members(ctx, room)
		return membersMsg{room, ms}, err
	})
}

func (m *Model) layout() {
	w := max(m.width-listWidth-1, 10)
	m.vp.Width = w
	m.vp.Height = max(m.height-3, 1)
	m.input.Width = w - 3
	m.render()
}

// render fills the viewport with the open room's messages, or its
// members and agent settings.
func (m *Model) render() {
	if m.vp.Width == 0 {
		return
	}
	if m.info {
		m.vp.SetContent(m.roomInfo())
		return
	}
	var b strings.Builder
	switch {
	case m.earlier[m.open]:
		b.WriteString(dim.Render("Reading earlier messages…") + "\n\n")
	case m.first[m.open] > 1:
		b.WriteString(dim.Render("pgup for earlier messages") + "\n\n")
	}
	var prev say.Event
	for i, e := range m.logs[m.open] {
		if i == 0 || e.SenderID != prev.SenderID || e.CreatedAt.Sub(prev.CreatedAt) > 5*time.Minute {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(m.name(e.SenderID) + " " + dim.Render(e.CreatedAt.Local().Format("15:04")) + "\n")
		}
		prev = e
		text := body(e)
		if e.ID == m.editing {
			text = selected.Render(text)
		}
		b.WriteString(lipgloss.NewStyle().Width(m.vp.Width).Render(text) + "\n")
	}
	if len(m.logs[m.open]) == 0 && m.open != "" && m.first[m.open] <= 1 {
		b.WriteString(dim.Render("No messages yet. Say something."))
	}
	m.vp.SetContent(strings.TrimRight(b.String(), "\n"))
	if m.atBottom {
		m.vp.GotoBottom()
	}
}

// roomInfo is the open room's members and agent settings, with the
// commands that change them for those who may.
func (m *Model) roomInfo() string {
	i := m.index()
	if i < 0 {
		return ""
	}
	r := m.rooms[i]
	ms := m.members[r.ID]
	mine := slices.IndexFunc(ms, func(x say.Member) bool { return x.PrincipalID == m.me.ID })
	manage := m.me.Kind == "human" && (m.me.Admin || mine >= 0 && (ms[mine].Role == "owner" || ms[mine].Role == "admin"))
	var b strings.Builder
	b.WriteString(bold.Render("Members") + "\n")
	if ms == nil {
		b.WriteString(dim.Render(" …") + "\n")
	}
	for _, x := range ms {
		line := " " + m.name(x.PrincipalID)
		if _, known := m.people[x.PrincipalID]; !known && x.PrincipalID != m.me.ID {
			line = " " + human.Render(x.DisplayName) // guests are not in the directory
		}
		if x.Role != "member" {
			line += dim.Render(" " + x.Role)
		}
		if slices.Contains(x.Scopes, "responder") {
			line += dim.Render(" · answers everything")
		}
		b.WriteString(line + "\n")
	}
	a := r.Agents
	paused := "no"
	if a.Paused {
		paused = "yes"
	}
	b.WriteString("\n" + bold.Render("Agents") + "\n")
	rows := [][2]string{
		{"Paused", paused},
		{"Replies in a row", strconv.Itoa(a.MaxHopCount)},
		{"Messages from all agents", fmt.Sprintf("%d per %d min", a.TurnLimit, a.TurnWindowSeconds/60)},
		{"Messages per agent per minute", strconv.Itoa(a.RateLimit)},
	}
	for _, row := range rows {
		b.WriteString(fmt.Sprintf(" %-30s %s\n", row[0], row[1]))
	}
	b.WriteString("\n")
	if manage {
		b.WriteString(dim.Render(strings.Join([]string{
			"/agents pause or /agents resume",
			"/agents hops N · turns N · window MINUTES · rate N (0 is the default)",
			"/responder NAME on|off: an agent answers every message, not only @mentions",
		}, "\n")) + "\n")
	}
	if r.Kind != "dm" {
		b.WriteString(dim.Render("/invite NAME adds someone") + "\n")
	}
	b.WriteString(dim.Render("esc or /members goes back to the messages"))
	return lipgloss.NewStyle().Width(m.vp.Width).Render(b.String())
}

// roomName is what the list and the header call r. Direct messages
// without a name are called after the others in them.
func (m *Model) roomName(r say.Room) string {
	switch {
	case r.Name != "" && r.Kind == "channel":
		return "# " + r.Name
	case r.Name != "":
		return r.Name
	case r.Kind == "dm":
		var names []string
		for _, x := range m.members[r.ID] {
			if x.PrincipalID != m.me.ID {
				names = append(names, x.DisplayName)
			}
		}
		if len(names) > 0 {
			return "@ " + strings.Join(names, ", ")
		}
		return "@ …"
	}
	return "Untitled room"
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
		name := m.roomName(r)
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
		title = m.roomName(m.rooms[i])
		if m.info {
			title += " · members and agents"
		}
	}
	hint := "tab rooms · ctrl+n/p next/prev · pgup/pgdn scroll · ↑ edit · ctrl+c quit"
	if m.editing != "" {
		hint = "editing your message · enter saves · esc cancels"
	}
	status := dim.Render(truncate(hint, m.vp.Width))
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
