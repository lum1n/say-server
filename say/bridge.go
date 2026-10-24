package say

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// Request and Connect are the node's API as strings in and out, for
// bindings to other languages (mobile/, cmd/libsay) that cannot share Go
// types. Bodies and envelopes are the JSON of protocol/.

// Response is an HTTP answer from the node.
type Response struct {
	Status int
	Body   string
}

// Request calls the node's HTTP API in memory. path is absolute, like
// /api/v1/rooms; a non-empty body is sent as application/json.
func (n *Node) Request(ctx context.Context, method, path, token, body string) (Response, error) {
	if !strings.HasPrefix(path, "/") {
		return Response{}, errors.New("say: the path must start with /")
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, rd)
	if err != nil {
		return Response{}, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := n.client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return Response{Status: res.StatusCode, Body: string(data)}, err
}

// Conn is a raw WebSocket session with the node, as on /api/v1/ws:
// envelopes in, envelopes out. It does not reconnect; the in-memory
// connection only ends when the node closes or the client is too slow.
type Conn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

// Connect opens a WebSocket session with the node as the principal of
// token.
func (n *Node) Connect(ctx context.Context, token string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, "ws://say.internal/api/v1/ws", &websocket.DialOptions{
		HTTPClient: n.client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(4 << 20)
	cctx, cancel := context.WithCancel(n.ctx)
	return &Conn{ws: ws, ctx: cctx, cancel: cancel}, nil
}

// Send writes one envelope.
func (c *Conn) Send(ctx context.Context, envelope string) error {
	return c.ws.Write(ctx, websocket.MessageText, []byte(envelope))
}

// Recv reads the next envelope. It fails once the session has ended.
func (c *Conn) Recv(ctx context.Context) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	_, data, err := c.ws.Read(ctx)
	return string(data), err
}

// Close ends the session.
func (c *Conn) Close() error {
	c.cancel()
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
