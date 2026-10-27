package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// DefaultHookLimit is how many messages a minute a bridge's hook takes on
// one node, when Config.HookLimit is zero.
const DefaultHookLimit = 60

// hookLimiter bounds the messages each bridge's hook takes, so a leaked
// secret can't flood its rooms. A bridge may post its limit at once, then
// its limit a minute. Each node counts on its own. Only requests with the
// right secret count, so a stranger can't use up a bridge's allowance;
// the map is bounded by the number of bridges that take hooks.
type hookLimiter struct {
	perMinute int
	mu        sync.Mutex
	bridges   map[string]*rate.Limiter
}

func newHookLimiter(perMinute int) *hookLimiter {
	if perMinute <= 0 {
		perMinute = DefaultHookLimit
	}
	return &hookLimiter{perMinute: perMinute, bridges: map[string]*rate.Limiter{}}
}

// take counts a message for bridgeID. When the bridge has none left, it
// takes nothing and returns how long until one is.
func (h *hookLimiter) take(bridgeID string, now time.Time) (wait time.Duration, ok bool) {
	h.mu.Lock()
	l := h.bridges[bridgeID]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(float64(h.perMinute)/60), h.perMinute)
		h.bridges[bridgeID] = l
	}
	h.mu.Unlock()
	r := l.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return d, false
	}
	return 0, true
}

// reset gives bridgeID a full allowance, as when its secret changes.
func (h *hookLimiter) reset(bridgeID string) {
	h.mu.Lock()
	delete(h.bridges, bridgeID)
	h.mu.Unlock()
}
