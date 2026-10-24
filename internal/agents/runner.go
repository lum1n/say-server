// Package agents runs built-in agents: agent principals whose replies the
// server writes itself by calling a model provider
// (docs/adr/0009-built-in-agents.md).
//
// Every node runs a Runner. Each enabled agent is held by one node at a
// time through a lease in the store, and the holder follows the agent's
// rooms and replies when a message calls for it. Replies go through the same
// send path as every other client, so pauses, hop limits and budgets apply
// to them as to external agents.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/vegard/say-v2/internal/bus"
	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/llm"
	"github.com/vegard/say-v2/internal/store"
)

// Chat is how a built-in agent takes part, the same way as any client.
type Chat interface {
	// SendParts sends a message from me in reply to causedBy. A repeated
	// idempotencyKey returns the earlier message, with created false. A
	// streaming message may start without parts; StreamParts replaces its
	// parts and EndParts makes it final, with parts or, when they are nil,
	// with what it streamed.
	SendParts(ctx context.Context, me core.Principal, roomID string, parts []core.Part, causedBy, idempotencyKey string, streaming bool) (e core.Event, created bool, err error)
	StreamParts(ctx context.Context, me core.Principal, roomID, eventID string, parts []core.Part) error
	EndParts(ctx context.Context, me core.Principal, roomID, eventID string, parts []core.Part) error
	// AgentPolicy is the agent policy in effect in room.
	AgentPolicy(room core.Room) core.AgentPolicy
}

type Config struct {
	Store store.Store
	Bus   bus.Bus
	Chat  Chat
	// Holder names this node in leases. It must differ between nodes.
	Holder string
	// NewProvider makes an agent's model client; nil is llm.New.
	NewProvider func(llm.Config) (llm.Provider, error)
	// Version is the server's, told to the agents' MCP servers.
	Version string
}

// Timings; tests shorten them.
var (
	// scanEvery is how often a runner renews its leases and looks for
	// agents to take over. A lease outlives three missed renewals.
	scanEvery = 10 * time.Second
	leaseTTL  = 30 * time.Second
	// resync is how often an agent rereads its rooms without a bus
	// notification.
	resync = 30 * time.Second
	// maxAge is the oldest message an agent still answers, so an agent
	// that was down or just joined does not answer stale questions.
	maxAge = 5 * time.Minute
	// replyTimeout bounds one model call.
	replyTimeout = 2 * time.Minute
)

type Runner struct {
	c       Config
	running map[string]*running // by principal id
}

type running struct {
	updated time.Time // the settings it runs with
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(c Config) *Runner {
	if c.NewProvider == nil {
		c.NewProvider = llm.New
	}
	return &Runner{c: c, running: map[string]*running{}}
}

func leaseName(principalID string) string { return "agent:" + principalID }

// Run runs the agents this node holds until ctx ends, then stops them and
// releases their leases.
func (r *Runner) Run(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	unsubscribe, err := r.c.Bus.Subscribe(bus.BuiltinAgentsSubject, func(string, []byte) {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer unsubscribe()
	tick := time.NewTicker(scanEvery)
	defer tick.Stop()
	for {
		r.scan(ctx)
		select {
		case <-ctx.Done():
			for id := range r.running {
				r.stop(id)
			}
			return nil
		case <-wake:
		case <-tick.C:
		}
	}
}

// scan renews or takes the leases of enabled agents, starts the agents it
// holds and stops the ones it lost or whose settings changed.
func (r *Runner) scan(ctx context.Context) {
	all, err := r.c.Store.BuiltinAgents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("built-in agents: list failed", "err", err)
		}
		return
	}
	held := map[string]store.BuiltinAgent{}
	for _, a := range all {
		if !a.Enabled {
			continue
		}
		_, ok, err := r.c.Store.AcquireLease(ctx, leaseName(a.PrincipalID), r.c.Holder, leaseTTL)
		if err != nil && ctx.Err() == nil {
			slog.Warn("built-in agents: lease failed", "agent", a.PrincipalID, "err", err)
		}
		if ok {
			held[a.PrincipalID] = a
		}
	}
	for id, run := range r.running {
		if a, ok := held[id]; !ok || !a.UpdatedAt.Equal(run.updated) {
			r.stop(id)
		}
	}
	for id, a := range held {
		if _, ok := r.running[id]; !ok {
			r.start(ctx, a)
		}
	}
}

func (r *Runner) start(ctx context.Context, a store.BuiltinAgent) {
	me, err := r.c.Store.GetPrincipal(ctx, a.PrincipalID)
	if err != nil {
		slog.Warn("built-in agents: no principal", "agent", a.PrincipalID, "err", err)
		return
	}
	provider, err := r.c.NewProvider(llm.Config{Provider: a.Provider, BaseURL: a.BaseURL, APIKey: a.APIKey, Model: a.Model})
	if err != nil {
		r.c.Store.SetBuiltinAgentError(ctx, a.PrincipalID, err.Error())
		return
	}
	actx, cancel := context.WithCancel(ctx)
	run := &running{updated: a.UpdatedAt, cancel: cancel, done: make(chan struct{})}
	r.running[a.PrincipalID] = run
	w := &worker{c: r.c, cfg: a, me: me, llm: provider, lastErr: a.LastError}
	go func() {
		defer close(run.done)
		w.run(actx)
	}()
	slog.Info("built-in agent started", "agent", me.DisplayName, "id", me.ID)
}

// stop stops an agent and releases its lease, so another node may take it
// over at once.
func (r *Runner) stop(id string) {
	run := r.running[id]
	run.cancel()
	<-run.done
	delete(r.running, id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.c.Store.ReleaseLease(ctx, leaseName(id), r.c.Holder); err != nil {
		slog.Warn("built-in agents: release failed", "agent", id, "err", err)
	}
}

// worker follows one agent's rooms.
type worker struct {
	c       Config
	cfg     store.BuiltinAgent
	me      core.Principal
	llm     llm.Provider
	lastErr string

	mu      sync.Mutex
	dirty   map[string]bool // rooms with news
	refresh bool            // the room list changed
	wake    chan struct{}

	rooms map[string]func() // room id → unsubscribe
	names map[string]string // principal id → display name
}

func (w *worker) run(ctx context.Context) {
	w.dirty = map[string]bool{}
	w.wake = make(chan struct{}, 1)
	w.rooms = map[string]func(){}
	w.names = map[string]string{}
	defer func() {
		for _, unsubscribe := range w.rooms {
			unsubscribe()
		}
	}()
	unsubscribe, err := w.c.Bus.Subscribe(bus.RoomsSubject(w.me.ID), func(string, []byte) {
		w.mark(func() { w.refresh = true })
	})
	if err != nil {
		slog.Warn("built-in agent: subscribe failed", "agent", w.me.ID, "err", err)
		return
	}
	defer unsubscribe()
	w.endStreams(ctx)
	w.refresh = true
	tick := time.NewTicker(resync)
	defer tick.Stop()
	for {
		w.mu.Lock()
		refresh, dirty := w.refresh, w.dirty
		w.refresh, w.dirty = false, map[string]bool{}
		w.mu.Unlock()
		if refresh {
			w.syncRooms(ctx)
		}
		for roomID := range dirty {
			if _, ok := w.rooms[roomID]; ok {
				w.process(ctx, roomID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-tick.C:
			w.mark(func() {
				w.refresh = true
				for id := range w.rooms {
					w.dirty[id] = true
				}
			})
		}
	}
}

// endStreams ends the agent's replies that a node stopped writing, by
// crashing or losing the agent's lease, with what they have. Tool calls
// still running then never finished.
func (w *worker) endStreams(ctx context.Context) {
	streaming, err := w.c.Store.StreamingFrom(ctx, w.me.ID)
	if err != nil {
		w.warn(ctx, "list streaming messages", err)
		return
	}
	for _, e := range streaming {
		var parts []core.Part
		for i, p := range e.Parts {
			var c core.ToolCall
			if p.Kind != core.PartToolCall || json.Unmarshal(p.Data, &c) != nil || c.Status != core.ToolRunning {
				continue
			}
			if parts == nil {
				parts = slices.Clone(e.Parts)
			}
			c.Status, c.Error = core.ToolFailed, "the reply was cut off"
			parts[i] = c.Part()
		}
		if err := w.c.Chat.EndParts(ctx, w.me, e.RoomID, e.ID, parts); err != nil {
			w.warn(ctx, "end a stream", err)
		}
	}
}

// mark changes the pending work under the lock and wakes the loop.
func (w *worker) mark(f func()) {
	w.mu.Lock()
	f()
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// syncRooms follows the rooms the agent is in and stops following the
// ones it left.
func (w *worker) syncRooms(ctx context.Context) {
	rooms, err := w.c.Store.RoomsFor(ctx, w.me.ID)
	if err != nil {
		w.warn(ctx, "list rooms", err)
		return
	}
	in := map[string]bool{}
	for _, room := range rooms {
		in[room.ID] = true
		if _, ok := w.rooms[room.ID]; ok {
			continue
		}
		if err := w.startCursor(ctx, room.ID); err != nil {
			w.warn(ctx, "read room", err)
			continue
		}
		id := room.ID
		unsubscribe, err := w.c.Bus.Subscribe(bus.RoomSubject(id), func(string, []byte) {
			w.mark(func() { w.dirty[id] = true })
		})
		if err != nil {
			w.warn(ctx, "subscribe", err)
			continue
		}
		w.rooms[id] = unsubscribe
		w.mark(func() { w.dirty[id] = true })
	}
	for id, unsubscribe := range w.rooms {
		if !in[id] {
			unsubscribe()
			delete(w.rooms, id)
		}
	}
}

// startCursor gives a room the agent has never read a cursor a little way
// back, so a question asked just before the agent was added still gets an
// answer; maxAge keeps it from answering old ones.
func (w *worker) startCursor(ctx context.Context, roomID string) error {
	_, err := w.c.Store.AgentCursor(ctx, w.me.ID, roomID)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	last, err := w.c.Store.EventsBefore(ctx, roomID, 1<<62, 1)
	if err != nil {
		return err
	}
	var seq int64
	if len(last) > 0 {
		seq = max(0, last[0].Seq-lookBack)
	}
	return w.c.Store.SetAgentCursor(ctx, w.me.ID, roomID, seq)
}

// lookBack is how many events before joining a new agent reads.
const lookBack = 20

// process reads a room past the agent's cursor and answers the latest
// message that calls for an answer. Several such messages arriving
// together get one reply, to the last of them.
func (w *worker) process(ctx context.Context, roomID string) {
	cursor, err := w.c.Store.AgentCursor(ctx, w.me.ID, roomID)
	if err != nil {
		w.warn(ctx, "read cursor", err)
		return
	}
	room, err := w.c.Store.GetRoom(ctx, roomID)
	if err != nil {
		w.warn(ctx, "read room", err)
		return
	}
	m, err := w.c.Store.GetMember(ctx, roomID, w.me.ID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		w.warn(ctx, "read membership", err)
		return
	}
	var trigger *core.Event
	var through int64
	for {
		events, err := w.c.Store.EventsAfter(ctx, roomID, cursor, 200)
		if err != nil {
			w.warn(ctx, "read events", err)
			return
		}
		for _, e := range events {
			msg, ok, err := w.message(ctx, e)
			if err != nil {
				w.warn(ctx, "read message", err)
				return
			}
			if ok && w.calls(room, m, msg, e.CreatedAt) {
				trigger, through = &msg, e.Seq
			}
			cursor = e.Seq
		}
		if len(events) < 200 {
			break
		}
	}
	if err := w.c.Store.SetAgentCursor(ctx, w.me.ID, roomID, cursor); err != nil {
		w.warn(ctx, "save cursor", err)
		return
	}
	if trigger != nil {
		w.reply(ctx, room, *trigger, through)
	}
}

// message returns the complete message e brings: e itself when it is a
// final message, or the message whose stream e ended. A streaming message
// waits for its end, and other edits bring nothing to answer. Reading
// both a message that is already final and the edit that ended it is
// harmless: the edit commits with the message's last change, so both are
// in the same pass and the agent answers once.
func (w *worker) message(ctx context.Context, e core.Event) (core.Event, bool, error) {
	switch {
	case e.Type == core.EventMessage:
		return e, e.Stream != core.StreamStreaming, nil
	case e.Type == core.EventEdit && e.Stream == core.StreamFinal:
		msg, err := w.c.Store.GetEvent(ctx, e.Edits)
		if errors.Is(err, store.ErrNotFound) {
			return core.Event{}, false, nil
		}
		return msg, err == nil, err
	}
	return core.Event{}, false, nil
}

// calls reports whether msg, complete at at, asks the agent for an answer:
// it is a recent message from someone else, and it is in a DM, the agent
// is a designated responder in the room, or it mentions the agent.
func (w *worker) calls(room core.Room, m core.Membership, msg core.Event, at time.Time) bool {
	if msg.SenderID == w.me.ID || time.Since(at) > maxAge {
		return false
	}
	if m.Role == core.RoleObserver {
		return false
	}
	if room.Kind == core.RoomDM {
		return true
	}
	for _, s := range m.Scopes {
		if s == core.ScopeResponder {
			return true
		}
	}
	return core.Mentions(msg.Text(), w.me.DisplayName)
}
