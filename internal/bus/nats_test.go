package bus

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newNode(t *testing.T, c NATSConfig) *NATS {
	t.Helper()
	if c.ClusterAddr == "" {
		c.ClusterAddr = "127.0.0.1:0"
	}
	b, err := NewNATS(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func waitPeers(t *testing.T, want int, nodes ...*NATS) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, n := range nodes {
		for n.Peers() < want {
			if time.Now().After(deadline) {
				t.Fatalf("node %s has %d peers, want %d", n.ClusterAddr(), n.Peers(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// receives reports whether a publish on from reaches a subscriber on to.
// Interest crosses routes asynchronously, so it publishes until the
// message arrives or the timeout passes.
func receives(t *testing.T, from, to *NATS, timeout time.Duration) bool {
	t.Helper()
	got := make(chan []byte, 16)
	unsubscribe, err := to.Subscribe("room.1", func(_ string, d []byte) { got <- d })
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	deadline := time.After(timeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := from.Publish(context.Background(), "room.1", []byte("x")); err != nil {
			t.Fatal(err)
		}
		select {
		case d := <-got:
			if string(d) != "x" {
				t.Fatalf("got %q", d)
			}
			return true
		case <-tick.C:
		case <-deadline:
			return false
		}
	}
}

func TestNATSCluster(t *testing.T) {
	const secret = "s3cret"
	a := newNode(t, NATSConfig{Secret: secret})
	// b and c only know a; gossip connects them to each other.
	b := newNode(t, NATSConfig{Secret: secret, Peers: []string{a.ClusterAddr()}})
	c := newNode(t, NATSConfig{Secret: secret, Peers: []string{a.ClusterAddr()}})
	waitPeers(t, 2, a, b, c)

	if !receives(t, c, b, 5*time.Second) {
		t.Fatal("c → b not delivered")
	}
	if !receives(t, a, c, 5*time.Second) {
		t.Fatal("a → c not delivered")
	}
	if !receives(t, b, b, time.Second) {
		t.Fatal("local delivery failed")
	}

	// A node with the wrong secret never joins.
	x := newNode(t, NATSConfig{Secret: "wrong", Peers: []string{a.ClusterAddr()}})
	if receives(t, x, a, 500*time.Millisecond) {
		t.Fatal("node with the wrong secret joined the cluster")
	}
	if a.Peers() != 2 {
		t.Fatalf("a has %d peers, want 2", a.Peers())
	}

	c.Close()
	if err := c.Publish(context.Background(), "room.1", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish after close = %v", err)
	}
}
