package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/vegard/say-v2/internal/core"
)

// Realtime delivery reads the store, not the bus. The bus only says "room R
// has new events"; a follower then reads everything after its cursor. So
// replay after a reconnect, catching up after a missed notification and
// live delivery are one code path, and order is always seq order.

func subject(roomID string) string { return "room." + roomID }

// roomsSubject is where a principal hears that its set of rooms changed.
func roomsSubject(principalID string) string { return "principal." + principalID + ".rooms" }

// notify announces new events in room. A failure only delays delivery
// until the next notification or resync, so it is logged, not returned.
func (s *Server) notify(ctx context.Context, roomID string) {
	s.publish(ctx, subject(roomID))
}

// notifyRooms tells principal's sockets to reread its room list.
func (s *Server) notifyRooms(ctx context.Context, principalID string) {
	s.publish(ctx, roomsSubject(principalID))
}

func (s *Server) publish(ctx context.Context, subject string) {
	if err := s.bus.Publish(context.WithoutCancel(ctx), subject, nil); err != nil {
		slog.Warn("bus publish failed", "subject", subject, "err", err)
	}
}

// follow calls deliver with every event of room after cursor, in seq
// order, until ctx ends or deliver fails. It wakes on bus notifications and
// every resync interval, so a lost notification costs latency, not events.
func (s *Server) follow(ctx context.Context, roomID string, cursor int64, deliver func(core.Event) error) error {
	wake := make(chan struct{}, 1)
	unsubscribe, err := s.bus.Subscribe(subject(roomID), func(string, []byte) {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return err
	}
	defer unsubscribe()
	resync := time.NewTicker(s.resync)
	defer resync.Stop()
	for {
		events, err := s.store.EventsAfter(ctx, roomID, cursor, maxPage)
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := deliver(e); err != nil {
				return err
			}
			cursor = e.Seq
		}
		if len(events) == maxPage {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-resync.C:
		}
	}
}

// poll returns up to limit events after cursor, waiting up to wait for the
// first one to arrive.
func (s *Server) poll(ctx context.Context, roomID string, cursor int64, limit int, wait time.Duration) ([]core.Event, error) {
	events, err := s.store.EventsAfter(ctx, roomID, cursor, limit)
	if err != nil || len(events) > 0 || wait <= 0 {
		return events, err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	errGot := errors.New("got events")
	err = s.follow(ctx, roomID, cursor, func(core.Event) error { return errGot })
	if errors.Is(err, context.DeadlineExceeded) {
		return []core.Event{}, nil
	}
	if !errors.Is(err, errGot) {
		return nil, err
	}
	return s.store.EventsAfter(ctx, roomID, cursor, limit)
}

// stream serves a room's events as Server-Sent Events. Each event's SSE id
// is its seq, so an EventSource resumes from Last-Event-ID by itself;
// ?after=N sets the starting cursor explicitly.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, me core.Principal) {
	id := r.PathValue("id")
	if _, err := s.member(r.Context(), me, id); err != nil {
		writeError(w, err)
		return
	}
	from := r.URL.Query().Get("after")
	if from == "" {
		from = r.Header.Get("Last-Event-ID")
	}
	cursor, err := intParam(from, 0, 0, math.MaxInt64)
	if err != nil {
		writeError(w, errorf(http.StatusBadRequest, "bad_request", "cursor: %v", err))
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex // the heartbeat and follow both write
	write := func(f func() error) error {
		mu.Lock()
		defer mu.Unlock()
		rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := f(); err != nil {
			return err
		}
		return rc.Flush()
	}
	go func() {
		t := time.NewTicker(s.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if write(func() error { _, err := fmt.Fprint(w, ": ping\n\n"); return err }) != nil {
					cancel()
					return
				}
			}
		}
	}()
	err = s.follow(ctx, id, cursor, func(e core.Event) error {
		data, err := json.Marshal(toEvent(e))
		if err != nil {
			return err
		}
		return write(func() error {
			_, err := fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n", e.Seq, data)
			return err
		})
	})
	if err != nil && ctx.Err() == nil {
		slog.Error("event stream failed", "room", id, "err", err)
	}
}

// writeTimeout drops a realtime client that stops reading.
const writeTimeout = 15 * time.Second

// envelope is one WebSocket frame (protocol/envelope.schema.json).
type envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Status  string          `json:"status,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// outboundBuffer is how many frames a WebSocket may fall behind before it
// is closed; the client then reconnects and resumes from its cursors.
const outboundBuffer = 256

// maxSubscriptions bounds the rooms one WebSocket follows.
const maxSubscriptions = 1000

// ws serves the client and connector WebSocket. Requests are envelopes
// with a client-chosen id; the reply echoes the id and type with a status.
// Events arrive as pushes of type "event" without a status, and a push of
// type "rooms" says the caller's room list changed.
func (s *Server) ws(w http.ResponseWriter, r *http.Request, me core.Principal) {
	// Accept checks that the Origin matches the Host, so another site cannot
	// open a socket with the user's cookie.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxBody)
	c := &wsConn{
		s:    s,
		me:   me,
		conn: conn,
		out:  make(chan envelope, outboundBuffer),
		subs: map[string]context.CancelFunc{},
	}
	ctx, cancel := context.WithCancel(r.Context())
	c.cancel = cancel
	defer cancel()
	// Like room notifications this is only a hint: a client rereads its
	// room list on each push and after every reconnect.
	unsubscribe, err := s.bus.Subscribe(roomsSubject(me.ID), func(string, []byte) {
		c.push(envelope{ID: core.NewID(), Type: "rooms"})
	})
	if err != nil {
		slog.Error("websocket rooms subscription failed", "err", err)
		conn.Close(websocket.StatusInternalError, "")
		return
	}
	defer unsubscribe()
	go c.writeLoop(ctx)
	go c.pingLoop(ctx)
	err = c.readLoop(ctx)
	switch {
	case websocket.CloseStatus(err) != -1:
	case errors.Is(err, errTooSlow):
		conn.Close(websocket.StatusPolicyViolation, "too slow; reconnect and resume from your cursors")
	case ctx.Err() != nil:
		conn.Close(websocket.StatusGoingAway, "server shutting down")
	default:
		conn.Close(websocket.StatusInternalError, "")
	}
}

var errTooSlow = errors.New("client too slow")

type wsConn struct {
	s      *Server
	me     core.Principal
	conn   *websocket.Conn
	out    chan envelope
	cancel context.CancelFunc

	mu   sync.Mutex
	subs map[string]context.CancelFunc // room id -> stop following
	err  error                         // why the connection is closing
}

func (c *wsConn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-c.out:
			data, _ := json.Marshal(env)
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// pingLoop drops a peer that stops answering pings. It runs apart from
// writeLoop because a ping waits for its pong.
func (c *wsConn) pingLoop(ctx context.Context) {
	t := time.NewTicker(c.s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// push queues a frame. A client that has fallen outboundBuffer frames
// behind is disconnected rather than buffered without bound.
func (c *wsConn) push(env envelope) error {
	select {
	case c.out <- env:
		return nil
	default:
		c.fail(errTooSlow)
		return errTooSlow
	}
}

// fail records why the connection ends and stops it.
func (c *wsConn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *wsConn) readLoop(ctx context.Context) error {
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.err != nil {
				return c.err
			}
			return err
		}
		var req envelope
		if err := json.Unmarshal(data, &req); err != nil || req.ID == "" || req.Type == "" {
			c.push(envelope{ID: "0", Type: "error", Status: "error", Payload: errPayload(
				errorf(http.StatusBadRequest, "bad_request", "frames are JSON envelopes with an id and a type"))})
			continue
		}
		payload, err := c.handle(ctx, req)
		reply := envelope{ID: req.ID, Type: req.Type, Status: "ok", Payload: payload}
		if err != nil {
			reply.Status, reply.Payload = "error", errPayload(err)
		}
		c.push(reply)
	}
}

func (c *wsConn) handle(ctx context.Context, req envelope) (json.RawMessage, error) {
	switch req.Type {
	case "ping":
		return nil, nil
	case "subscribe":
		var p struct {
			RoomID string `json:"room_id"`
			After  int64  `json:"after"`
		}
		if err := decodePayload(req.Payload, &p); err != nil {
			return nil, err
		}
		return nil, c.subscribe(ctx, p.RoomID, p.After)
	case "unsubscribe":
		var p struct {
			RoomID string `json:"room_id"`
		}
		if err := decodePayload(req.Payload, &p); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if stop, ok := c.subs[p.RoomID]; ok {
			stop()
			delete(c.subs, p.RoomID)
		}
		c.mu.Unlock()
		return nil, nil
	case "send":
		var p struct {
			RoomID         string `json:"room_id"`
			IdempotencyKey string `json:"idempotency_key"`
			sendMessage
		}
		if err := decodePayload(req.Payload, &p); err != nil {
			return nil, err
		}
		e, _, err := c.s.send(ctx, c.me, p.RoomID, p.sendMessage, p.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		return json.Marshal(toEvent(e))
	default:
		return nil, errorf(http.StatusBadRequest, "bad_request", "unknown type %q", req.Type)
	}
}

// subscribe starts pushing room's events after cursor. Subscribing again to
// a room restarts it from the new cursor.
func (c *wsConn) subscribe(ctx context.Context, roomID string, cursor int64) error {
	if cursor < 0 {
		return errorf(http.StatusBadRequest, "bad_request", "after must not be negative")
	}
	if _, err := c.s.member(ctx, c.me, roomID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if stop, ok := c.subs[roomID]; ok {
		stop()
	} else if len(c.subs) >= maxSubscriptions {
		return errorf(http.StatusUnprocessableEntity, "too_many", "at most %d subscriptions per connection", maxSubscriptions)
	}
	sctx, stop := context.WithCancel(ctx)
	c.subs[roomID] = stop
	go func() {
		err := c.s.follow(sctx, roomID, cursor, func(e core.Event) error {
			data, err := json.Marshal(toEvent(e))
			if err != nil {
				return err
			}
			return c.push(envelope{ID: e.ID, Type: "event", Payload: data})
		})
		if err != nil && sctx.Err() == nil && !errors.Is(err, errTooSlow) {
			slog.Error("websocket follow failed", "room", roomID, "err", err)
			c.fail(err)
		}
	}()
	return nil
}

func decodePayload(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "invalid payload: %v", err)
	}
	return nil
}

// errPayload is the payload of a reply with status "error": the same
// {code, message} as an HTTP error body, plus the HTTP status it maps to.
func errPayload(err error) json.RawMessage {
	var ae *apiError
	if !errors.As(err, &ae) {
		slog.Error("websocket request failed", "err", err)
		ae = errorf(http.StatusInternalServerError, "internal", "internal error")
	}
	data, _ := json.Marshal(map[string]any{"code": ae.Code, "message": ae.Message, "status": ae.Status})
	return data
}
