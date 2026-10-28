package api

import (
	"testing"
	"time"
)

// A bridge's hook takes its limit at once, then one message every
// 60/limit seconds; each bridge counts on its own, and reset refills it.
func TestHookLimiter(t *testing.T) {
	h := newHookLimiter(3)
	now := time.Now()
	for i := range 3 {
		if _, ok := h.take("a", now); !ok {
			t.Fatalf("message %d of the burst was refused", i+1)
		}
	}
	wait, ok := h.take("a", now)
	if ok || wait <= 0 || wait > 20*time.Second {
		t.Fatalf("over the limit: wait %s, ok %v", wait, ok)
	}
	// A refused message takes nothing, so asking again doesn't push the
	// wait further out.
	if again, ok := h.take("a", now); ok || again != wait {
		t.Fatalf("asking again: wait %s (was %s), ok %v", again, wait, ok)
	}
	if _, ok := h.take("a", now.Add(wait)); !ok {
		t.Fatalf("refused after waiting %s", wait)
	}
	if _, ok := h.take("a", now.Add(wait)); ok {
		t.Fatal("took two after one came back")
	}

	if _, ok := h.take("b", now); !ok {
		t.Fatal("another bridge was refused")
	}

	h.reset("a")
	for i := range 3 {
		if _, ok := h.take("a", now.Add(wait)); !ok {
			t.Fatalf("message %d after a reset was refused", i+1)
		}
	}
}

func TestHookLimiterDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		if h := newHookLimiter(n); h.perMinute != DefaultHookLimit {
			t.Fatalf("newHookLimiter(%d).perMinute = %d", n, h.perMinute)
		}
	}
	h := newHookLimiter(0)
	now := time.Now()
	for i := range DefaultHookLimit {
		if _, ok := h.take("a", now); !ok {
			t.Fatalf("message %d of the default burst was refused", i+1)
		}
	}
	if _, ok := h.take("a", now); ok {
		t.Fatal("took more than the default burst")
	}
}
