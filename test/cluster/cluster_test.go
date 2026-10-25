// Package cluster tests a running say cluster from the outside, over HTTP.
// It needs three or more nodes on one Postgres, all started with the same
// setup code:
//
//	make cluster-test
//
// Without SAY_CLUSTER_NODES the test is skipped. It can be rerun against the
// same cluster: once the instance is set up it signs in instead.
package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	username = "admin"
	pass     = "cluster-test-password"
	// deliveryTimeout is well under the 30 s resync, so a message that
	// arrives in time came over the cluster bus.
	deliveryTimeout = 5 * time.Second
)

func TestCluster(t *testing.T) {
	var nodes []string
	for _, n := range strings.Split(os.Getenv("SAY_CLUSTER_NODES"), ",") {
		if n = strings.TrimRight(strings.TrimSpace(n), "/"); n != "" {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		t.Skip("SAY_CLUSTER_NODES is not set")
	}
	if len(nodes) < 3 {
		t.Fatalf("need 3 nodes, got %d", len(nodes))
	}
	a, b, c := nodes[0], nodes[1], nodes[2]
	for _, n := range nodes {
		waitHealthy(t, n)
	}

	admin := signIn(t, a, os.Getenv("SAY_SETUP_CODE"))

	// A second participant, created on b, listens while the admin talks.
	var agent struct {
		Principal struct {
			ID string `json:"id"`
		} `json:"principal"`
		Token string `json:"token"`
	}
	call(t, b, "POST", "/api/v1/principals", admin, map[string]any{"kind": "agent", "display_name": "listener"}, 201, &agent)

	var room struct {
		ID string `json:"id"`
	}
	call(t, a, "POST", "/api/v1/rooms", admin, map[string]any{"kind": "group", "name": "cluster test"}, 201, &room)
	call(t, c, "POST", "/api/v1/rooms/"+room.ID+"/members", admin, map[string]any{"principal_id": agent.Principal.ID}, 201, nil)

	// A client on a sees a message posted to c.
	s := openStream(t, a, room.ID, agent.Token, "")
	send(t, c, room.ID, admin, "hello from c")
	last := s.next(t)
	if last.text != "hello from c" {
		t.Fatalf("stream on a got %q, want the message posted to c", last.text)
	}
	s.close()

	// Messages sent while the client is away are replayed, in order, when
	// it reconnects to another node with its cursor.
	missed := []string{"one", "two", "three"}
	for _, m := range missed {
		send(t, b, room.ID, admin, m)
	}
	s = openStream(t, c, room.ID, agent.Token, strconv.FormatInt(last.seq, 10))
	defer s.close()
	for i, want := range missed {
		ev := s.next(t)
		if ev.text != want || ev.seq != last.seq+int64(i)+1 {
			t.Fatalf("replay %d on c = seq %d %q, want seq %d %q", i, ev.seq, ev.text, last.seq+int64(i)+1, want)
		}
	}

	// And it keeps following live after the replay.
	send(t, a, room.ID, admin, "live again")
	if ev := s.next(t); ev.text != "live again" {
		t.Fatalf("after replay c got %q", ev.text)
	}
}

func waitHealthy(t *testing.T, node string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := http.Get(node + "/api/v1/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not healthy: %v", node, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// signIn sets up the instance through node, or signs in when that was done
// on an earlier run, and returns the session token.
func signIn(t *testing.T, node, code string) string {
	t.Helper()
	var setup struct {
		Needed bool `json:"needed"`
	}
	call(t, node, "GET", "/api/v1/setup", "", nil, 200, &setup)
	path, body, want := "/api/v1/login", map[string]any{"username": username, "password": pass}, 200
	if setup.Needed {
		if code == "" {
			t.Fatal("the instance needs setup; set SAY_SETUP_CODE to the nodes' setup code")
		}
		path, body, want = "/api/v1/setup", map[string]any{"code": code, "username": username, "password": pass}, 201
	}
	res := do(t, node, "POST", path, "", body)
	defer res.Body.Close()
	expect(t, res, want)
	for _, ck := range res.Cookies() {
		if ck.Name == "say_token" {
			return ck.Value
		}
	}
	t.Fatalf("%s set no say_token cookie", path)
	return ""
}

func send(t *testing.T, node, room, token, text string) {
	t.Helper()
	part := map[string]any{"kind": "text", "data": map[string]string{"text": text}}
	call(t, node, "POST", "/api/v1/rooms/"+room+"/messages", token, map[string]any{"parts": []any{part}}, 201, nil)
}

func call(t *testing.T, node, method, path, token string, body any, want int, out any) {
	t.Helper()
	res := do(t, node, method, path, token, body)
	defer res.Body.Close()
	expect(t, res, want)
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("%s %s%s: %v", method, node, path, err)
		}
	}
}

func do(t *testing.T, node, method, path, token string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, node+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, node, path, err)
	}
	return res
}

func expect(t *testing.T, res *http.Response, want int) {
	t.Helper()
	if res.StatusCode != want {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("%s %s = %d, want %d: %s", res.Request.Method, res.Request.URL, res.StatusCode, want, b)
	}
}

type stream struct {
	cancel context.CancelFunc
	body   io.Closer
	events chan event
}

type event struct {
	seq  int64
	text string
}

// openStream follows a room over SSE, from lastSeq when it is set.
func openStream(t *testing.T, node, room, token, lastSeq string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", node+"/api/v1/rooms/"+room+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastSeq != "" {
		req.Header.Set("Last-Event-ID", lastSeq)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		expect(t, res, 200)
	}
	s := &stream{cancel: cancel, body: res.Body, events: make(chan event, 64)}
	go s.read(res.Body)
	return s
}

func (s *stream) read(r io.Reader) {
	defer close(s.events)
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "" && data != "":
			var ev struct {
				Seq   int64 `json:"seq"`
				Parts []struct {
					Data struct {
						Text string `json:"text"`
					} `json:"data"`
				} `json:"parts"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && len(ev.Parts) > 0 {
				s.events <- event{seq: ev.Seq, text: ev.Parts[0].Data.Text}
			}
			data = ""
		}
	}
}

func (s *stream) next(t *testing.T) event {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(deliveryTimeout):
		t.Fatalf("no event within %s", deliveryTimeout)
	}
	return event{}
}

func (s *stream) close() {
	s.cancel()
	s.body.Close()
}
