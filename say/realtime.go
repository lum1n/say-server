package say

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Push is something the server sent without being asked: a new event in a
// followed room ("event"), a streaming message as it now is ("stream"), or
// a hint that the room list may have changed ("rooms").
type Push struct {
	Type  string
	Event *Event
}

// Realtime follows rooms over the WebSocket. It keeps each room's cursor,
// and when the socket drops it reconnects with backoff and resubscribes
// from the cursors, so no event is missed or delivered twice. After a
// reconnect it pushes "rooms", since room changes may have been missed.
type Realtime struct {
	c      *Client
	ctx    context.Context
	cancel context.CancelFunc
	pushes chan Push

	mu      sync.Mutex
	conn    *websocket.Conn
	cursors map[string]int64
	nextID  atomic.Int64
}

// Realtime opens the client's WebSocket. Pushes come on Pushes until ctx
// ends or Close is called.
func (c *Client) Realtime(ctx context.Context) (*Realtime, error) {
	ctx, cancel := context.WithCancel(ctx)
	r := &Realtime{c: c, ctx: ctx, cancel: cancel, pushes: make(chan Push, 256), cursors: map[string]int64{}}
	conn, err := r.dial()
	if err != nil {
		cancel()
		return nil, err
	}
	go r.run(conn)
	return r, nil
}

func (r *Realtime) dial() (*websocket.Conn, error) {
	u := r.c.BaseURL + "/api/v1/ws"
	u = "ws" + strings.TrimPrefix(u, "http")
	conn, _, err := websocket.Dial(r.ctx, u, &websocket.DialOptions{
		HTTPClient: r.c.HTTP,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + r.c.Token}},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// Pushes delivers events in order per room. It is closed when the
// Realtime stops.
func (r *Realtime) Pushes() <-chan Push { return r.pushes }

// Subscribe follows roomID from after, or from where it already is.
func (r *Realtime) Subscribe(roomID string, after int64) {
	r.mu.Lock()
	after = max(after, r.cursors[roomID])
	r.cursors[roomID] = after
	conn := r.conn
	r.mu.Unlock()
	r.request(conn, "subscribe", map[string]any{"room_id": roomID, "after": after})
}

func (r *Realtime) Unsubscribe(roomID string) {
	r.mu.Lock()
	delete(r.cursors, roomID)
	conn := r.conn
	r.mu.Unlock()
	r.request(conn, "unsubscribe", map[string]any{"room_id": roomID})
}

func (r *Realtime) Close() { r.cancel() }

// request sends an envelope on conn; without one, the subscriptions are
// sent again on reconnect. r.mu must not be held: the read loop needs it
// to take the server's reply, and an in-memory connection has no buffer
// for that reply to wait in.
func (r *Realtime) request(conn *websocket.Conn, typ string, payload any) {
	if conn == nil {
		return
	}
	p, _ := json.Marshal(payload)
	data, _ := json.Marshal(map[string]any{"id": "r" + strconv.FormatInt(r.nextID.Add(1), 10), "type": typ, "payload": json.RawMessage(p)})
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	conn.Write(ctx, websocket.MessageText, data)
}

func (r *Realtime) run(conn *websocket.Conn) {
	defer close(r.pushes)
	backoff := 500 * time.Millisecond
	first := true
	for {
		r.mu.Lock()
		r.conn = conn
		cursors := maps.Clone(r.cursors)
		r.mu.Unlock()
		go func() {
			for room, after := range cursors {
				r.request(conn, "subscribe", map[string]any{"room_id": room, "after": after})
			}
		}()
		if !first && !r.push(Push{Type: "rooms"}) {
			conn.CloseNow()
			return
		}
		first = false
		r.read(conn)
		r.mu.Lock()
		r.conn = nil
		r.mu.Unlock()
		conn.CloseNow()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			var err error
			if conn, err = r.dial(); err == nil {
				backoff = 500 * time.Millisecond
				break
			}
		}
	}
}

// read delivers pushes until the socket fails.
func (r *Realtime) read(conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(r.ctx)
		if err != nil {
			return
		}
		var env struct {
			Type    string          `json:"type"`
			Status  string          `json:"status"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &env) != nil || env.Status != "" {
			continue
		}
		switch env.Type {
		case "rooms":
			if !r.push(Push{Type: "rooms"}) {
				return
			}
		case "event", "stream":
			var e Event
			if json.Unmarshal(env.Payload, &e) != nil {
				continue
			}
			r.mu.Lock()
			cursor, followed := r.cursors[e.RoomID]
			fresh := env.Type == "stream" || e.Seq > cursor
			if followed && env.Type == "event" && fresh {
				r.cursors[e.RoomID] = e.Seq
			}
			r.mu.Unlock()
			// A resubscribe can overlap events already delivered.
			if followed && fresh && !r.push(Push{Type: env.Type, Event: &e}) {
				return
			}
		}
	}
}

func (r *Realtime) push(p Push) bool {
	select {
	case r.pushes <- p:
		return true
	case <-r.ctx.Done():
		return false
	}
}
