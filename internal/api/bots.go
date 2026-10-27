package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// A bot session is a /ws socket whose agent said hello
// (docs/adr/0011-bot-api.md). The server then follows every room the
// agent is in from its stored cursors, tells it which messages ask it for
// an answer, and shows it as online while the socket is open.

// botHello is the payload of a "hello" request.
type botHello struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

const (
	maxHelloField    = 100
	maxCapabilities  = 32
	maxCapabilityLen = 32
	// triggeredMemory bounds the messages a follower remembers having
	// triggered, so a message and the edit that ended its stream, read in
	// one pass, trigger once.
	triggeredMemory = 1000
)

func (c *wsConn) hello(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if c.me.Kind != core.PrincipalAgent {
		return nil, errorf(http.StatusForbidden, "forbidden", "only agent tokens open bot sessions")
	}
	var p botHello
	if err := decodePayload(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Name) > maxHelloField || len(p.Version) > maxHelloField {
		return nil, errorf(http.StatusUnprocessableEntity, "invalid", "name and version are at most %d bytes", maxHelloField)
	}
	if len(p.Capabilities) > maxCapabilities {
		return nil, errorf(http.StatusUnprocessableEntity, "invalid", "at most %d capabilities", maxCapabilities)
	}
	for _, cap := range p.Capabilities {
		if cap == "" || len(cap) > maxCapabilityLen {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "a capability is 1 to %d bytes", maxCapabilityLen)
		}
	}
	c.mu.Lock()
	switch {
	case c.bot != nil:
		c.mu.Unlock()
		return nil, errorf(http.StatusConflict, "conflict", "this socket already said hello")
	case len(c.subs) > 0:
		c.mu.Unlock()
		return nil, errorf(http.StatusConflict, "conflict", "unsubscribe from every room before hello")
	}
	c.mu.Unlock()
	now := time.Now()
	cs := store.ConnectorSession{
		ID:           core.NewID(),
		PrincipalID:  c.me.ID,
		Name:         p.Name,
		Version:      p.Version,
		Capabilities: p.Capabilities,
		ConnectedAt:  now,
		SeenAt:       now,
	}
	if err := c.s.store.SaveConnectorSession(ctx, cs); err != nil {
		return nil, err
	}
	bot := &botSession{wake: make(chan struct{}, 1), rooms: map[string]roomTrigger{}}
	c.mu.Lock()
	c.bot = bot
	c.mu.Unlock()
	// Events follow the reply.
	c.afterReply = func() { go c.runBot(ctx, bot, cs) }
	return json.Marshal(map[string]any{"principal": toPrincipal(c.me), "session_id": cs.ID})
}

// botSession is the state of a socket that said hello.
type botSession struct {
	wake chan struct{} // the agent's room list may have changed

	// rooms caches, per room, what triggers depend on besides the
	// message, so a busy room costs no store reads per message. Every
	// change to it comes with a room list push, which clears the cache;
	// so does each heartbeat, as pushes are hints.
	mu    sync.Mutex
	rooms map[string]roomTrigger
	gen   uint64 // bumped on each clear
}

// roomTrigger is a room and the agent's membership of it.
type roomTrigger struct {
	room core.Room
	m    core.Membership
}

// poke says the agent's rooms may have changed: the room list is reread
// and cached rooms are forgotten.
func (b *botSession) poke() {
	b.forget()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *botSession) forget() {
	b.mu.Lock()
	clear(b.rooms)
	b.gen++
	b.mu.Unlock()
}

// room returns roomID and the agent's membership of it, from the cache
// or the store.
func (b *botSession) room(ctx context.Context, st store.Store, agentID, roomID string) (roomTrigger, error) {
	b.mu.Lock()
	rt, ok := b.rooms[roomID]
	gen := b.gen
	b.mu.Unlock()
	if ok {
		return rt, nil
	}
	room, err := st.GetRoom(ctx, roomID)
	if err != nil {
		return roomTrigger{}, err
	}
	m, err := st.GetMember(ctx, roomID, agentID)
	if err != nil {
		return roomTrigger{}, err
	}
	rt = roomTrigger{room: room, m: m}
	b.mu.Lock()
	// A clear during the reads may mean they are already stale.
	if b.gen == gen {
		b.rooms[roomID] = rt
	}
	b.mu.Unlock()
	return rt, nil
}

// runBot follows the agent's rooms and renews its session until the
// socket closes.
func (c *wsConn) runBot(ctx context.Context, bot *botSession, cs store.ConnectorSession) {
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := c.s.store.DeleteConnectorSession(dctx, cs.ID); err != nil {
			slog.Warn("bot session: delete failed", "session", cs.ID, "err", err)
		}
	}()
	beat := time.NewTicker(c.s.heartbeat)
	defer beat.Stop()
	for {
		c.syncBotRooms(ctx, bot)
		select {
		case <-ctx.Done():
			return
		case <-bot.wake:
		case <-beat.C:
			// Room list pushes are hints, so the list is reread and the
			// cached rooms forgotten on each heartbeat too.
			bot.forget()
			cs.SeenAt = time.Now()
			if err := c.s.store.SaveConnectorSession(ctx, cs); err != nil && ctx.Err() == nil {
				slog.Warn("bot session: renew failed", "session", cs.ID, "err", err)
			}
		}
	}
}

// syncBotRooms follows the rooms the agent is in and stops following the
// ones it left.
func (c *wsConn) syncBotRooms(ctx context.Context, bot *botSession) {
	rooms, err := c.s.store.RoomsFor(ctx, c.me.ID)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("bot session: list rooms failed", "agent", c.me.ID, "err", err)
		}
		return
	}
	in := map[string]bool{}
	for _, room := range rooms {
		in[room.ID] = true
		c.mu.Lock()
		_, ok := c.subs[room.ID]
		c.mu.Unlock()
		if ok {
			continue
		}
		cursor, err := store.StartAgentCursor(ctx, c.s.store, c.me.ID, room.ID)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("bot session: read cursor failed", "agent", c.me.ID, "room", room.ID, "err", err)
			}
			continue
		}
		c.mu.Lock()
		err = c.followRoom(ctx, room.ID, cursor, bot)
		c.mu.Unlock()
		if err != nil {
			slog.Warn("bot session: follow failed", "agent", c.me.ID, "room", room.ID, "err", err)
		}
	}
	c.mu.Lock()
	for id, stop := range c.subs {
		if !in[id] {
			stop()
			delete(c.subs, id)
		}
	}
	c.mu.Unlock()
}

// trigger returns why e asks the agent for an answer, if it does.
func (c *wsConn) trigger(ctx context.Context, bot *botSession, e core.Event, triggered map[string]bool) core.Trigger {
	msg, ok, err := store.CompleteMessage(ctx, c.s.store, e)
	if err != nil || !ok || msg.SenderID == c.me.ID || triggered[msg.ID] {
		return ""
	}
	rt, err := bot.room(ctx, c.s.store, c.me.ID, e.RoomID)
	if err != nil {
		return ""
	}
	t := core.Triggers(c.me, rt.room, rt.m, msg, e.CreatedAt, time.Now())
	if t != "" {
		if len(triggered) >= triggeredMemory {
			clear(triggered)
		}
		triggered[msg.ID] = true
	}
	return t
}

// ack stores how far the agent has handled a room.
func (c *wsConn) ack(ctx context.Context, raw json.RawMessage) error {
	c.mu.Lock()
	bot := c.bot
	c.mu.Unlock()
	if bot == nil {
		return errorf(http.StatusConflict, "conflict", "say hello before ack")
	}
	var p struct {
		RoomID string `json:"room_id"`
		Seq    int64  `json:"seq"`
	}
	if err := decodePayload(raw, &p); err != nil {
		return err
	}
	if p.Seq < 0 {
		return errorf(http.StatusBadRequest, "bad_request", "seq must not be negative")
	}
	if _, err := c.s.member(ctx, c.me, p.RoomID); err != nil {
		return err
	}
	return c.s.store.SetAgentCursor(ctx, c.me.ID, p.RoomID, p.Seq)
}

// liveSessions maps each principal with an open bot session to its
// latest one.
func (s *Server) liveSessions(ctx context.Context) (map[string]store.ConnectorSession, error) {
	sessions, err := s.store.ConnectorSessions(ctx, time.Now().Add(-s.sessionLive()))
	if err != nil {
		return nil, err
	}
	out := map[string]store.ConnectorSession{}
	for _, cs := range sessions { // oldest first
		out[cs.PrincipalID] = cs
	}
	return out, nil
}

// sessionLive is how long a session counts as live after it was last
// renewed: two missed heartbeats and some slack.
func (s *Server) sessionLive() time.Duration { return 2*s.heartbeat + 10*time.Second }
