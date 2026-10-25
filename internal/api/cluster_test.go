package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lum1n/say-v2/internal/bus"
	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store"
)

// Two nodes share a store and a NATS cluster bus. A message sent through
// one reaches a stream on the other through the bus, not the resync.
func TestClusterDelivery(t *testing.T) {
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	node := func(peers ...string) (*env, *bus.NATS) {
		b, err := bus.NewNATS(bus.NATSConfig{ClusterAddr: "127.0.0.1:0", Peers: peers, Secret: "s"})
		if err != nil {
			t.Fatal(err)
		}
		s := New(Config{Version: "test", Store: st, Bus: b})
		s.resync = time.Hour
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(func() {
			ts.Close()
			b.Close()
		})
		return &env{t: t, srv: s, ts: ts, store: st}, b
	}
	a, busA := node()
	b, busB := node(busA.ClusterAddr())

	// Wait until a publish on a's bus reaches b's.
	got := make(chan struct{}, 1)
	unsubscribe, _ := busB.Subscribe("ping", func(string, []byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for ready := false; !ready; {
		if time.Now().After(deadline) {
			t.Fatal("cluster did not form")
		}
		busA.Publish(context.Background(), "ping", nil)
		select {
		case <-got:
			ready = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	unsubscribe()

	_, ada := a.user("Ada", core.PrincipalHuman)
	r := a.room(ada, "general")
	a.say(ada, r.ID, "before")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", b.ts.URL+"/api/v1/rooms/"+r.ID+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ada)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("stream on b = %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	// After the replay the follower only rereads the store when woken.
	if ev := readSSE(t, sc); partText(ev.data) != "before" {
		t.Fatalf("replay on b = %+v", ev)
	}

	a.say(ada, r.ID, "hello from a")
	if ev := readSSE(t, sc); partText(ev.data) != "hello from a" {
		t.Fatalf("b got %+v", ev)
	}
}
