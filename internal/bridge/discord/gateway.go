package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// intents are GUILDS, GUILD_MESSAGES and MESSAGE_CONTENT: channel names,
// messages in server channels, and their text.
const intents = 1<<0 | 1<<9 | 1<<15

// frame is a Gateway payload.
type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// session is one Gateway connection.
type session struct {
	c     *conn
	ws    *websocket.Conn
	seq   atomic.Int64 // last sequence number, 0 for none
	acked atomic.Bool  // the last heartbeat was answered
}

// dial asks for the Gateway's URL and connects to it.
func (c *conn) dial(ctx context.Context) (*websocket.Conn, error) {
	u := c.gateway
	if u == "" {
		var r struct {
			URL string `json:"url"`
		}
		if err := c.do(ctx, http.MethodGet, "/gateway/bot", nil, &r); err != nil {
			if status(err) == http.StatusUnauthorized {
				return nil, fmt.Errorf("discord: the bot token was refused: %w", err)
			}
			return nil, err
		}
		u = r.URL
	}
	pu, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("discord: gateway URL: %w", err)
	}
	pu.RawQuery = url.Values{"v": {"10"}, "encoding": {"json"}}.Encode()
	// The API client's timeout would cut the long-lived connection.
	cl := *c.http
	cl.Timeout = 0
	ws, _, err := websocket.Dial(ctx, pu.String(), &websocket.DialOptions{HTTPClient: &cl})
	if err != nil {
		return nil, fmt.Errorf("discord: gateway: %w", err)
	}
	// A big server's GUILD_CREATE outgrows the default 32 KiB.
	ws.SetReadLimit(32 << 20)
	return ws, nil
}

// hello reads the first frame, which says how often to send heartbeats.
func (s *session) hello(ctx context.Context) (time.Duration, error) {
	var f frame
	if err := wsjson.Read(ctx, s.ws, &f); err != nil {
		return 0, closeError(err)
	}
	var d struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if f.Op != opHello || json.Unmarshal(f.D, &d) != nil || d.Interval <= 0 {
		return 0, fmt.Errorf("discord: gateway: expected hello, got op %d", f.Op)
	}
	return time.Duration(d.Interval) * time.Millisecond, nil
}

// identify logs the bot in.
func (s *session) identify(ctx context.Context) error {
	err := wsjson.Write(ctx, s.ws, map[string]any{
		"op": opIdentify,
		"d": map[string]any{
			"token":   s.c.cfg.BotToken,
			"intents": intents,
			"properties": map[string]string{
				"os": "linux", "browser": "say", "device": "say",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("discord: gateway: %w", err)
	}
	return nil
}

// heartbeat beats at the interval, the first after a random part of it,
// and fails when a beat goes unanswered: the connection is dead.
func (s *session) heartbeat(ctx context.Context, interval time.Duration) error {
	s.acked.Store(true)
	t := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if !s.acked.Swap(false) {
			return errors.New("discord: the gateway stopped answering heartbeats")
		}
		if err := s.beat(ctx); err != nil {
			return err
		}
		t.Reset(interval)
	}
}

// beat sends one heartbeat with the last sequence number.
func (s *session) beat(ctx context.Context) error {
	var d any
	if n := s.seq.Load(); n > 0 {
		d = n
	}
	if err := wsjson.Write(ctx, s.ws, map[string]any{"op": opHeartbeat, "d": d}); err != nil {
		return fmt.Errorf("discord: gateway: %w", err)
	}
	return nil
}

// read handles frames until the connection fails, closing ready on READY
// and queueing messages.
func (s *session) read(ctx context.Context, ready chan<- struct{}, events chan<- message) error {
	isReady := false
	for {
		_, data, err := s.ws.Read(ctx)
		if err != nil {
			return closeError(err)
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.S != nil {
			s.seq.Store(*f.S)
		}
		switch f.Op {
		case opHeartbeat:
			if err := s.beat(ctx); err != nil {
				return err
			}
		case opHeartbeatACK:
			s.acked.Store(true)
		case opReconnect:
			return errors.New("discord: the gateway asked to reconnect")
		case opInvalidSession:
			return errors.New("discord: the gateway session was invalidated")
		case opDispatch:
			switch f.T {
			case "READY":
				var d struct {
					User        struct{ ID string } `json:"user"`
					Application struct{ ID string } `json:"application"`
				}
				json.Unmarshal(f.D, &d)
				s.c.mu.Lock()
				s.c.self, s.c.appID = d.User.ID, d.Application.ID
				s.c.mu.Unlock()
				if !isReady {
					isReady = true
					close(ready)
				}
			case "GUILD_CREATE":
				var d struct {
					Channels []channel `json:"channels"`
					Threads  []channel `json:"threads"`
				}
				json.Unmarshal(f.D, &d)
				s.c.name(append(d.Channels, d.Threads...)...)
			case "CHANNEL_CREATE", "CHANNEL_UPDATE", "THREAD_CREATE", "THREAD_UPDATE":
				var ch channel
				json.Unmarshal(f.D, &ch)
				s.c.name(ch)
			case "MESSAGE_CREATE", "MESSAGE_UPDATE":
				var m message
				if err := json.Unmarshal(f.D, &m); err != nil {
					continue
				}
				m.update = f.T == "MESSAGE_UPDATE"
				select {
				case events <- m:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

// closeError explains why the gateway closed the connection.
func closeError(err error) error {
	switch websocket.CloseStatus(err) {
	case 4004:
		return errors.New("discord: the bot token was refused")
	case 4013, 4014:
		return errors.New("discord: the bot may not use the Message Content intent: turn it on under Bot in the developer portal")
	}
	return fmt.Errorf("discord: gateway: %w", err)
}
