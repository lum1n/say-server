package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// rpcError is an error signal-cli answered a call with.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("signal-cli: %s (%d)", e.Message, e.Code) }

// wire is one JSON-RPC message in either direction.
type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpc speaks JSON-RPC with signal-cli over its stdin and stdout, one
// message per line. A single reader routes responses to their callers and
// queues notifications, so a call never waits on whoever consumes the
// notifications: the core's Receive can wait on a Send in progress.
type rpc struct {
	w io.Writer

	writeMu sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[string]chan wire
	queue   []wire // notifications not taken yet
	wake    chan struct{}
	done    chan struct{} // closed when the reader stops
	err     error         // why it stopped
}

func newRPC(r io.Reader, w io.Writer) *rpc {
	c := &rpc{w: w, pending: map[string]chan wire{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go c.read(r)
	return c
}

// call sends a request and decodes its result into out, if out is not nil.
func (c *rpc) call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := strconv.Itoa(c.next)
	ch := make(chan wire, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	line, err := json.Marshal(wire{JSONRPC: "2.0", ID: json.RawMessage(strconv.Quote(id)), Method: method, Params: raw})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.w.Write(append(line, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("signal-cli %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out == nil || len(m.Result) == 0 || string(m.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(m.Result, out); err != nil {
			return fmt.Errorf("signal-cli %s: %w", method, err)
		}
		return nil
	}
}

// notifications returns the queued notifications, waiting for at least one
// until ctx ends or the reader stops.
func (c *rpc) notifications(ctx context.Context) ([]wire, error) {
	for {
		c.mu.Lock()
		q := c.queue
		c.queue = nil
		c.mu.Unlock()
		if len(q) > 0 {
			return q, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, c.closedErr()
		case <-c.wake:
		}
	}
}

func (c *rpc) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	// Envelopes with long texts or many group members can be large.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			// Not JSON-RPC; signal-cli logs go to stderr, but be lenient.
			continue
		}
		var m wire
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if len(m.ID) == 0 || string(m.ID) == "null" {
			if m.Method != "" {
				c.mu.Lock()
				c.queue = append(c.queue, m)
				c.mu.Unlock()
				select {
				case c.wake <- struct{}{}:
				default:
				}
			}
			continue
		}
		id := strings.Trim(string(m.ID), `"`)
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default: // a second answer to the same id
			}
		}
	}
	c.mu.Lock()
	c.err = sc.Err()
	c.mu.Unlock()
	close(c.done)
}

// errClosed is the error of calls after signal-cli's output ended, which
// is when it exits.
var errClosed = errors.New("signal-cli closed its output")

func (c *rpc) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("%w: %w", errClosed, c.err)
	}
	return errClosed
}
