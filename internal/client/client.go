// Package client calls the say API (protocol/) as one principal, on an
// embedded node or a remote server. Package say exports it to programs;
// it lives here so that bridge networks, which say imports, can use it
// too (docs/adr/0015-sync.md).
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls the say API as one principal, on an embedded node or a
// remote server. The types mirror protocol/openapi.yaml.
type Client struct {
	// BaseURL is the server's root, like https://chat.example.com, or
	// say.BaseURL for an embedded node.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client of a remote server.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: http.DefaultClient}
}

type Principal struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"display_name"`
	Admin       bool      `json:"admin,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Room struct {
	ID        string      `json:"id"`
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Agents    AgentPolicy `json:"agents"`
	CreatedAt time.Time   `json:"created_at"`
}

// AgentPolicy is how far agents may go in a room, the server's defaults
// included.
type AgentPolicy struct {
	Paused            bool `json:"paused"`
	MaxHopCount       int  `json:"max_hop_count"`
	TurnLimit         int  `json:"turn_limit"`
	TurnWindowSeconds int  `json:"turn_window_seconds"`
	RateLimit         int  `json:"rate_limit_per_minute"`
}

// AgentPatch changes a room's agent settings. Nil fields stay as they
// are; a zero limit goes back to the server default.
type AgentPatch struct {
	Paused            *bool `json:"paused,omitempty"`
	MaxHopCount       *int  `json:"max_hop_count,omitempty"`
	TurnLimit         *int  `json:"turn_limit,omitempty"`
	TurnWindowSeconds *int  `json:"turn_window_seconds,omitempty"`
	RateLimit         *int  `json:"rate_limit_per_minute,omitempty"`
}

type Member struct {
	PrincipalID string   `json:"principal_id"`
	Role        string   `json:"role"`
	Scopes      []string `json:"scopes"`
	DisplayName string   `json:"display_name,omitempty"`
	Kind        string   `json:"kind,omitempty"`
}

// Event is one entry in a room's log. Edits are events too: Edits names
// the message an edit changes, and a message's Parts are its latest
// content.
type Event struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"room_id"`
	Seq       int64      `json:"seq"`
	Type      string     `json:"type"`
	SenderID  string     `json:"sender_id"`
	Parts     []Part     `json:"parts"`
	Stream    string     `json:"stream,omitempty"`
	Edits     string     `json:"edits,omitempty"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Part is a typed piece of a message; Data is its kind's JSON body.
type Part struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// Text joins the event's text parts.
func (e Event) Text() string {
	var b strings.Builder
	for _, p := range e.Parts {
		if p.Kind != "text" {
			continue
		}
		var d struct{ Text string }
		if json.Unmarshal(p.Data, &d) == nil {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(d.Text)
		}
	}
	return b.String()
}

// TextPart makes a text part.
func TextPart(text string) Part {
	data, _ := json.Marshal(map[string]string{"text": text})
	return Part{Kind: "text", Data: data}
}

// Error is an error answer from the API.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("say: %d %s: %s", e.Status, e.Code, e.Message)
}

// do sends a request with body as JSON and decodes the answer into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any, header ...string) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string }
		}
		json.Unmarshal(data, &e)
		return &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Me(ctx context.Context) (Principal, error) {
	var p Principal
	return p, c.do(ctx, "GET", "/me", nil, &p)
}

// Principals is the instance directory.
func (c *Client) Principals(ctx context.Context) ([]Principal, error) {
	var out struct{ Principals []Principal }
	return out.Principals, c.do(ctx, "GET", "/principals", nil, &out)
}

// Rooms are the rooms the client's principal is in, oldest first.
func (c *Client) Rooms(ctx context.Context) ([]Room, error) {
	var out struct{ Rooms []Room }
	return out.Rooms, c.do(ctx, "GET", "/rooms", nil, &out)
}

// CreateRoom makes a room of kind dm, group or channel.
func (c *Client) CreateRoom(ctx context.Context, kind, name string) (Room, error) {
	var r Room
	return r, c.do(ctx, "POST", "/rooms", map[string]string{"kind": kind, "name": name}, &r)
}

func (c *Client) Members(ctx context.Context, roomID string) ([]Member, error) {
	var out struct{ Members []Member }
	return out.Members, c.do(ctx, "GET", "/rooms/"+url.PathEscape(roomID)+"/members", nil, &out)
}

func (c *Client) AddMember(ctx context.Context, roomID, principalID string) error {
	return c.do(ctx, "POST", "/rooms/"+url.PathEscape(roomID)+"/members", map[string]string{"principal_id": principalID}, nil)
}

// UpdateAgents changes the room's agent settings and returns the room.
func (c *Client) UpdateAgents(ctx context.Context, roomID string, p AgentPatch) (Room, error) {
	var r Room
	return r, c.do(ctx, "PATCH", "/rooms/"+url.PathEscape(roomID), map[string]any{"agents": p}, &r)
}

// SetResponder makes an agent in the room answer every message, not only
// @mentions, or stops it.
func (c *Client) SetResponder(ctx context.Context, roomID, principalID string, on bool) (Member, error) {
	var m Member
	return m, c.do(ctx, "PATCH", "/rooms/"+url.PathEscape(roomID)+"/members/"+url.PathEscape(principalID),
		map[string]bool{"responder": on}, &m)
}

// EventsQuery picks a page of a room's log: forward from After, or else
// the page that ends before Before, or the latest page.
type EventsQuery struct {
	After  int64
	Before int64
	Limit  int
}

func (c *Client) Events(ctx context.Context, roomID string, q EventsQuery) ([]Event, error) {
	v := url.Values{}
	if q.After > 0 {
		v.Set("after", strconv.FormatInt(q.After, 10))
	}
	if q.Before > 0 {
		v.Set("before", strconv.FormatInt(q.Before, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out struct{ Events []Event }
	return out.Events, c.do(ctx, "GET", "/rooms/"+url.PathEscape(roomID)+"/events?"+v.Encode(), nil, &out)
}

// Send posts a message. Retrying with the same idempotencyKey is safe:
// the server returns the message the first attempt created. An empty key
// gets a random one.
func (c *Client) Send(ctx context.Context, roomID string, parts []Part, idempotencyKey string) (Event, error) {
	if idempotencyKey == "" {
		idempotencyKey = rand.Text()
	}
	var e Event
	return e, c.do(ctx, "POST", "/rooms/"+url.PathEscape(roomID)+"/messages",
		map[string]any{"parts": parts}, &e, "Idempotency-Key", idempotencyKey)
}

// Edit replaces the parts of one of the client's messages and returns the
// message as it now is.
func (c *Client) Edit(ctx context.Context, roomID, eventID string, parts []Part) (Event, error) {
	var e Event
	return e, c.do(ctx, "PATCH", "/rooms/"+url.PathEscape(roomID)+"/messages/"+url.PathEscape(eventID),
		map[string]any{"parts": parts}, &e)
}

// Device is a token for one of a person's devices, from Pair.
type Device struct {
	Principal Principal `json:"principal"`
	Token     string    `json:"token"`
}

// Pair signs in as username with password and returns a token for the
// device name that does not expire. It replaces the token an earlier Pair
// returned for the same name. The client's own token is not used.
func (c *Client) Pair(ctx context.Context, username, password, name string) (Device, error) {
	var d Device
	anon := *c
	anon.Token = ""
	return d, anon.do(ctx, "POST", "/devices", map[string]string{"username": username, "password": password, "name": name}, &d)
}

// Logout revokes the client's token.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, "POST", "/logout", nil, nil)
}
