package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/lum1n/say-v2/internal/bus"
	"github.com/lum1n/say-v2/internal/store"
)

// ErrNotLinked is a hooked message for a chat no room is linked to.
var ErrNotLinked = errors.New("not linked")

// ReceiveHooked takes in the messages a request to bridge b's hook URL
// carried (Network.Hook). It runs on whichever node the request reached:
// receiving needs the store, not the connection. In bot mode every
// message must be for a linked chat, or none is taken.
func ReceiveHooked(ctx context.Context, c Config, b store.Bridge, msgs []Incoming) error {
	n, ok := Lookup(b.Network)
	if !ok {
		return fmt.Errorf("bridge: no network %q", b.Network)
	}
	owner, err := c.Store.GetPrincipal(ctx, b.OwnerID)
	if err != nil {
		return err
	}
	if b.Mode == store.ModeBot {
		for _, m := range msgs {
			_, err := c.Store.BridgeLinkByRemote(ctx, b.ID, m.Chat.ID)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: no room is linked to chat %q", ErrNotLinked, m.Chat.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	// An instance that is not running: it follows no rooms, and the node
	// running the bridge learns of new links over the bus.
	i := &instance{c: c, b: b, net: n, owner: owner, locks: map[string]*sync.Mutex{}}
	for _, m := range msgs {
		if err := i.Receive(ctx, m); err != nil {
			return err
		}
	}
	if b.Mode == store.ModeAccount {
		c.Bus.Publish(ctx, bus.BridgesSubject, nil)
	}
	return nil
}
