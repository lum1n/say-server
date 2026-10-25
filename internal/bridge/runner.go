package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/lum1n/say-v2/internal/bus"
	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store"
)

// Chat is how bridges write to rooms: the same send path as every client.
type Chat interface {
	// SendAs appends a message from as with id eventID, made at at. A
	// repeated idempotencyKey returns the earlier message.
	SendAs(ctx context.Context, as core.Principal, roomID, eventID string, parts []core.Part, idempotencyKey string, at time.Time) (core.Event, error)
	// EditAs changes as's final message eventID to parts and returns the
	// edit event; an edit that changes nothing is store.ErrNotEditable.
	EditAs(ctx context.Context, as core.Principal, roomID, eventID string, parts []core.Part) (core.Event, error)
	// RoomsChanged tells principalID's clients to reread their rooms and
	// bridges.
	RoomsChanged(ctx context.Context, principalID string)
	// DeliveryChanged tells a room's clients that a message's delivery to
	// a bridge changed.
	DeliveryChanged(ctx context.Context, roomID, eventID string)
}

type Config struct {
	Store store.Store
	Bus   bus.Bus
	Chat  Chat
	// Holder names this node in leases. It must differ between nodes.
	Holder string
}

// Timings; tests shorten them.
var (
	// scanEvery is how often a runner renews its leases, marks its
	// bridges as seen and looks for bridges to take over.
	scanEvery = 10 * time.Second
	leaseTTL  = 30 * time.Second
	// LiveWithin is how recently a running bridge must have been seen to
	// count as live.
	LiveWithin = 30 * time.Second
	// resync is how often links are reread without a bus notification.
	resync = 30 * time.Second
	// MaxDeliveryAge is the oldest message still sent when a bridge
	// comes back; older ones expire.
	MaxDeliveryAge = 10 * time.Minute
	sendTimeout    = 30 * time.Second
	// retryFirst and retryMax bound the pause before Run is called again.
	retryFirst = 2 * time.Second
	retryMax   = time.Minute
)

type Runner struct {
	c       Config
	running map[string]*running // by bridge id
}

type running struct {
	updated time.Time
	inst    *instance
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(c Config) *Runner {
	return &Runner{c: c, running: map[string]*running{}}
}

func leaseName(bridgeID string) string { return "bridge:" + bridgeID }

// Run runs the bridges this node holds until ctx ends, then stops them
// and releases their leases.
func (r *Runner) Run(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	unsubscribe, err := r.c.Bus.Subscribe(bus.BridgesSubject, func(string, []byte) {
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
			for _, run := range r.running {
				run.inst.mark(func(i *instance) { i.refresh = true })
			}
		case <-tick.C:
		}
	}
}

// scan renews or takes the leases of enabled bridges, starts the ones it
// holds, stops the ones it lost or whose settings changed, and marks the
// connected ones as seen.
func (r *Runner) scan(ctx context.Context) {
	all, err := r.c.Store.Bridges(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("bridges: list failed", "err", err)
		}
		return
	}
	held := map[string]store.Bridge{}
	for _, b := range all {
		if !b.Enabled {
			continue
		}
		if _, ok := Lookup(b.Network); !ok {
			continue
		}
		_, ok, err := r.c.Store.AcquireLease(ctx, leaseName(b.ID), r.c.Holder, leaseTTL)
		if err != nil && ctx.Err() == nil {
			slog.Warn("bridges: lease failed", "bridge", b.ID, "err", err)
		}
		if ok {
			held[b.ID] = b
		}
	}
	for id, run := range r.running {
		if b, ok := held[id]; !ok || !b.UpdatedAt.Equal(run.updated) {
			r.stop(id)
		}
	}
	for id, b := range held {
		if _, ok := r.running[id]; !ok {
			r.start(ctx, b)
		}
	}
	now := time.Now()
	for id, run := range r.running {
		if run.inst.isConnected() {
			if err := r.c.Store.SetBridgeSeen(ctx, id, now); err != nil && ctx.Err() == nil {
				slog.Warn("bridges: heartbeat failed", "bridge", id, "err", err)
			}
		}
	}
}

func (r *Runner) start(ctx context.Context, b store.Bridge) {
	n, _ := Lookup(b.Network)
	owner, err := r.c.Store.GetPrincipal(ctx, b.OwnerID)
	if err != nil {
		slog.Warn("bridges: no owner", "bridge", b.ID, "err", err)
		return
	}
	inst := &instance{c: r.c, b: b, net: n, owner: owner}
	conn, err := n.Open(Info{ID: b.ID, Mode: b.Mode, Config: b.Config, State: b.State}, inst)
	if err != nil {
		r.c.Store.SetBridgeError(ctx, b.ID, err.Error())
		r.c.Chat.RoomsChanged(ctx, b.OwnerID)
		return
	}
	inst.conn = conn
	bctx, cancel := context.WithCancel(ctx)
	run := &running{updated: b.UpdatedAt, inst: inst, cancel: cancel, done: make(chan struct{})}
	r.running[b.ID] = run
	go func() {
		defer close(run.done)
		inst.run(bctx)
	}()
	slog.Info("bridge started", "bridge", b.Name, "network", b.Network, "id", b.ID)
}

// stop stops a bridge and releases its lease, so another node may take it
// over at once.
func (r *Runner) stop(id string) {
	run := r.running[id]
	run.cancel()
	<-run.done
	delete(r.running, id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.c.Store.SetBridgeSeen(ctx, id, time.Time{})
	if err := r.c.Store.ReleaseLease(ctx, leaseName(id), r.c.Holder); err != nil {
		slog.Warn("bridges: release failed", "bridge", id, "err", err)
	}
}

// instance runs one bridge: its connection, and delivery to its links.
type instance struct {
	c     Config
	b     store.Bridge
	net   Network
	owner core.Principal
	conn  Conn

	mu        sync.Mutex
	connected bool
	dirty     map[string]bool // link ids with news
	refresh   bool            // the links changed
	wake      chan struct{}
	locks     map[string]*sync.Mutex // per link
	links     map[string]linked      // by link id
	names     map[string]string      // principal id → display name
}

type linked struct {
	roomID      string
	unsubscribe func()
}

func (i *instance) run(ctx context.Context) {
	i.mu.Lock()
	i.dirty = map[string]bool{}
	i.wake = make(chan struct{}, 1)
	i.locks = map[string]*sync.Mutex{}
	i.links = map[string]linked{}
	i.names = map[string]string{}
	i.mu.Unlock()
	defer func() {
		for _, l := range i.links {
			l.unsubscribe()
		}
	}()
	// What a node that stopped was sending may or may not have arrived.
	if err := i.c.Store.MarkBridgeSendsUnknown(ctx, i.b.ID); err != nil {
		i.warn(ctx, "mark unfinished sends", err)
	}
	i.mark(func(i *instance) { i.refresh = true })
	go i.connect(ctx)
	tick := time.NewTicker(resync)
	defer tick.Stop()
	for {
		i.mu.Lock()
		refresh, dirty, connected := i.refresh, i.dirty, i.connected
		i.refresh = false
		if connected {
			i.dirty = map[string]bool{}
		}
		i.mu.Unlock()
		if refresh {
			i.syncLinks(ctx)
		}
		if connected {
			for id := range dirty {
				i.deliver(ctx, id)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-i.wake:
		case <-tick.C:
			i.mark(func(i *instance) {
				i.refresh = true
				for id := range i.links {
					i.dirty[id] = true
				}
			})
		}
	}
}

// connect runs the connection, again after a pause when it fails.
func (i *instance) connect(ctx context.Context) {
	pause := retryFirst
	for {
		started := time.Now()
		err := i.conn.Run(ctx)
		i.setConnected(ctx, false)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the connection ended")
		}
		slog.Warn("bridge: connection failed", "bridge", i.b.ID, "network", i.b.Network, "err", err)
		i.c.Store.SetBridgeError(ctx, i.b.ID, err.Error())
		i.c.Chat.RoomsChanged(ctx, i.b.OwnerID)
		if time.Since(started) > retryMax {
			pause = retryFirst
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, retryMax)
	}
}

// Connected tells the core that the connection is logged in and
// receiving; delivery starts and the bridge shows as live.
func (i *instance) Connected(ctx context.Context) {
	i.setConnected(ctx, true)
}

func (i *instance) setConnected(ctx context.Context, on bool) {
	i.mu.Lock()
	was := i.connected
	i.connected = on
	if on {
		for id := range i.links {
			i.dirty[id] = true
		}
	}
	i.mu.Unlock()
	if on == was {
		return
	}
	at := time.Time{}
	if on {
		at = time.Now()
		i.c.Store.SetBridgeError(ctx, i.b.ID, "")
	}
	i.c.Store.SetBridgeSeen(context.WithoutCancel(ctx), i.b.ID, at)
	i.c.Chat.RoomsChanged(context.WithoutCancel(ctx), i.b.OwnerID)
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (i *instance) isConnected() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.connected
}

// mark changes the pending work under the lock and wakes the loop.
func (i *instance) mark(f func(*instance)) {
	i.mu.Lock()
	if i.wake == nil {
		i.mu.Unlock()
		return
	}
	f(i)
	i.mu.Unlock()
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

// syncLinks follows the bridge's links and stops following removed ones.
func (i *instance) syncLinks(ctx context.Context) {
	links, err := i.c.Store.BridgeLinks(ctx, i.b.ID)
	if err != nil {
		i.warn(ctx, "list links", err)
		return
	}
	in := map[string]bool{}
	for _, l := range links {
		in[l.ID] = true
		i.follow(l)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, l := range i.links {
		if !in[id] {
			l.unsubscribe()
			delete(i.links, id)
		}
	}
}

// follow delivers a link's room from now on, if it does not already.
func (i *instance) follow(l store.BridgeLink) {
	i.mu.Lock()
	_, ok := i.links[l.ID]
	running := i.links != nil
	i.mu.Unlock()
	if ok || !running {
		return
	}
	id := l.ID
	unsubscribe, err := i.c.Bus.Subscribe(bus.RoomSubject(l.RoomID), func(string, []byte) {
		i.mark(func(i *instance) { i.dirty[id] = true })
	})
	if err != nil {
		slog.Warn("bridge: subscribe failed", "bridge", i.b.ID, "err", err)
		return
	}
	i.mark(func(i *instance) {
		if _, ok := i.links[id]; ok {
			unsubscribe()
			return
		}
		i.links[id] = linked{roomID: l.RoomID, unsubscribe: unsubscribe}
		i.dirty[id] = true
	})
}

// lock serializes delivery and receiving on a link, so a network's echo
// of a message being sent is only seen once the send is recorded.
func (i *instance) lock(linkID string) func() {
	i.mu.Lock()
	m := i.locks[linkID]
	if m == nil {
		m = &sync.Mutex{}
		i.locks[linkID] = m
	}
	i.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (i *instance) warn(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		slog.Warn("bridge: "+what+" failed", "bridge", i.b.ID, "err", err)
	}
}

// SaveState keeps state for the next Run.
func (i *instance) SaveState(ctx context.Context, state json.RawMessage) error {
	return i.c.Store.SetBridgeState(ctx, i.b.ID, state)
}

// Chats returns the remote chats linked to rooms.
func (i *instance) Chats(ctx context.Context) ([]string, error) {
	links, err := i.c.Store.BridgeLinks(ctx, i.b.ID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(links))
	for n, l := range links {
		out[n] = l.RemoteID
	}
	return out, nil
}

// Ask shows the owner a prompt and waits for the answer, which comes over
// the bus from whichever node the owner reached.
func (i *instance) Ask(ctx context.Context, p Prompt) (string, error) {
	answers := make(chan string, 1)
	unsubscribe, err := i.c.Bus.Subscribe(bus.BridgeAnswerSubject(i.b.ID), func(_ string, data []byte) {
		select {
		case answers <- string(data):
		default:
		}
	})
	if err != nil {
		return "", err
	}
	defer unsubscribe()
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if err := i.c.Store.SetBridgePrompt(ctx, i.b.ID, data); err != nil {
		return "", err
	}
	i.c.Chat.RoomsChanged(ctx, i.b.OwnerID)
	defer func() {
		cctx := context.WithoutCancel(ctx)
		i.c.Store.SetBridgePrompt(cctx, i.b.ID, nil)
		i.c.Chat.RoomsChanged(cctx, i.b.OwnerID)
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case a := <-answers:
		return a, nil
	}
}
