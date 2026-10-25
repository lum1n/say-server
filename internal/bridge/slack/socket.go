package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// envelope is a Socket Mode frame.
type envelope struct {
	EnvelopeID string `json:"envelope_id"`
	Type       string `json:"type"` // hello, events_api, disconnect, …
	Reason     string `json:"reason"`
	Payload    struct {
		APIAppID string          `json:"api_app_id"`
		Event    json.RawMessage `json:"event"`
	} `json:"payload"`
}

// errRefresh is Slack asking for a new connection, which it does every
// few hours.
var errRefresh = errors.New("slack: socket refresh")

// connect asks for a Socket Mode URL with the app-level token and dials it.
func (c *conn) connect(ctx context.Context) (*websocket.Conn, error) {
	var r struct {
		response
		URL string `json:"url"`
	}
	if err := c.call(ctx, "apps.connections.open", c.cfg.AppToken, nil, &r); err != nil {
		return nil, fmt.Errorf("open Socket Mode (check the app-level token and that Socket Mode is on): %w", err)
	}
	ws, _, err := websocket.Dial(ctx, r.URL, &websocket.DialOptions{HTTPClient: c.socketClient()})
	if err != nil {
		return nil, fmt.Errorf("slack: socket: %w", err)
	}
	// Events with many files or blocks outgrow the default 32 KiB.
	ws.SetReadLimit(4 << 20)
	return ws, nil
}

// socketClient is the HTTP client for dialing: the API client without its
// timeout, which would cut the long-lived connection.
func (c *conn) socketClient() *http.Client {
	cl := *c.http
	cl.Timeout = 0
	return &cl
}

// socket reads events until ctx ends or the connection fails, moving to
// a new connection when Slack asks for one.
func (c *conn) socket(ctx context.Context, ws *websocket.Conn, events chan<- message) error {
	for {
		err := c.read(ctx, ws, events)
		ws.CloseNow()
		if !errors.Is(err, errRefresh) {
			return err
		}
		if ws, err = c.connect(ctx); err != nil {
			return err
		}
	}
}

// read acks every envelope and queues the message events.
func (c *conn) read(ctx context.Context, ws *websocket.Conn, events chan<- message) error {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("slack: socket: %w", err)
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		// Unacked envelopes come again, so ack before the work.
		if env.EnvelopeID != "" {
			if err := wsjson.Write(ctx, ws, map[string]string{"envelope_id": env.EnvelopeID}); err != nil {
				return fmt.Errorf("slack: socket: %w", err)
			}
		}
		switch env.Type {
		case "disconnect":
			if env.Reason == "link_disabled" {
				return errors.New("slack: Socket Mode was turned off for the app")
			}
			return errRefresh
		case "events_api":
			var m message
			if err := json.Unmarshal(env.Payload.Event, &m); err != nil || m.Type != "message" {
				continue
			}
			if id := env.Payload.APIAppID; id != "" {
				c.mu.Lock()
				c.appID = id
				c.mu.Unlock()
			}
			select {
			case events <- m:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
