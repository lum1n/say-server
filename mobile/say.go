// Package say is the say node for iOS and Android apps, bound with
// gomobile (docs/adr/0013-embedding.md):
//
//	make android   # build/say.aar, for Kotlin and Java
//	make ios       # build/Say.xcframework, for Swift (on macOS)
//
// It wraps package github.com/vegard/say-v2/say in the types gomobile can
// bind: strings, numbers, structs and callback interfaces. Requests and
// envelopes are the JSON of protocol/.
//
// Swift:
//
//	let node = try SayOpen(dir + "/say.db")
//	let me = try node.localUser("me", displayName: "Me", client: "ios")
//	let res = try node.request("GET", path: "/api/v1/rooms", token: me.token, body: "")
package say

import (
	"context"
	"errors"
	"sync/atomic"

	embedded "github.com/vegard/say-v2/say"
)

// Node is a say node running in the app.
type Node struct {
	n *embedded.Node
}

// Open opens or creates the node's SQLite database at dbPath (in the
// app's data directory) and starts the node.
func Open(dbPath string) (*Node, error) {
	if dbPath == "" {
		return nil, errors.New("say: Open needs a database path")
	}
	n, err := embedded.Open(context.Background(), embedded.Config{DB: dbPath, Version: "mobile"})
	if err != nil {
		return nil, err
	}
	return &Node{n: n}, nil
}

// Listen also serves the node on a TCP address and returns it. Use
// "127.0.0.1:0" to show the web app in a WebView.
func (n *Node) Listen(addr string) (string, error) {
	a, err := n.n.Listen(addr)
	if err != nil {
		return "", err
	}
	return a.String(), nil
}

// SetupCode is the setup wizard's code, or "" once there is an admin.
func (n *Node) SetupCode() string { return n.n.SetupCode() }

// User is a person on this node and a token to act as them.
type User struct {
	ID          string
	DisplayName string
	Admin       bool
	Token       string
}

// LocalUser returns a token for the app's user, creating them (as the
// admin, if first) when need be. Call it on every start; it replaces the
// token it gave the same client before.
func (n *Node) LocalUser(username, displayName, client string) (*User, error) {
	u, err := n.n.LocalUser(context.Background(), username, displayName, client)
	if err != nil {
		return nil, err
	}
	return &User{ID: u.ID, DisplayName: u.DisplayName, Admin: u.Admin, Token: u.Token}, nil
}

// Response is an HTTP answer: the status and the JSON body.
type Response struct {
	Status int
	Body   string
}

// Request calls the REST API, like Request("POST", "/api/v1/rooms",
// token, `{"kind":"group","name":"Tea"}`). An empty body sends none.
func (n *Node) Request(method, path, token, body string) (*Response, error) {
	res, err := n.n.Request(context.Background(), method, path, token, body)
	if err != nil {
		return nil, err
	}
	return &Response{Status: res.Status, Body: res.Body}, nil
}

// Listener gets a session's envelopes. Calls come on a background
// thread, one at a time.
type Listener interface {
	OnMessage(envelope string)
	// OnClose says the session ended; reason is empty after Close.
	OnClose(reason string)
}

// Conn is a WebSocket session with the node.
type Conn struct {
	c      *embedded.Conn
	closed atomic.Bool
}

// Connect opens a session as the principal of token. Send it envelopes
// such as {"id":"1","type":"subscribe","payload":{"room_id":"…","after":0}};
// replies and pushed events go to l.
func (n *Node) Connect(token string, l Listener) (*Conn, error) {
	c, err := n.n.Connect(context.Background(), token)
	if err != nil {
		return nil, err
	}
	conn := &Conn{c: c}
	go func() {
		for {
			msg, err := c.Recv(context.Background())
			if err != nil {
				reason := err.Error()
				if conn.closed.Load() {
					reason = ""
				}
				l.OnClose(reason)
				return
			}
			l.OnMessage(msg)
		}
	}()
	return conn, nil
}

// Send writes one envelope.
func (c *Conn) Send(envelope string) error {
	return c.c.Send(context.Background(), envelope)
}

// Close ends the session.
func (c *Conn) Close() {
	c.closed.Store(true)
	c.c.Close()
}

// Close stops the node.
func (n *Node) Close() error { return n.n.Close() }
