// Package bus fans out notifications between the parts of one node and, in
// cluster mode, between nodes. The store is the source of truth: the bus only
// says "room R has new events", and subscribers that miss a message catch up
// by reading from their cursor (docs/adr/0002-storage-and-scaling.md).
package bus

import "context"

// Handler receives one published message. It must not block for long.
type Handler func(subject string, data []byte)

type Bus interface {
	Publish(ctx context.Context, subject string, data []byte) error
	// Subscribe registers h for an exact subject. The returned function
	// removes the subscription.
	Subscribe(subject string, h Handler) (unsubscribe func(), err error)
	Close() error
}
