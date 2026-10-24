package bus

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("bus: closed")

// InProc is the single-node bus. Delivery is synchronous, in subscription
// order.
type InProc struct {
	mu     sync.RWMutex
	next   int
	subs   map[string]map[int]Handler
	closed bool
}

func NewInProc() *InProc {
	return &InProc{subs: map[string]map[int]Handler{}}
}

func (b *InProc) Publish(ctx context.Context, subject string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrClosed
	}
	handlers := make([]Handler, 0, len(b.subs[subject]))
	for id := 0; id < b.next; id++ {
		if h, ok := b.subs[subject][id]; ok {
			handlers = append(handlers, h)
		}
	}
	b.mu.RUnlock()
	for _, h := range handlers {
		h(subject, data)
	}
	return nil
}

func (b *InProc) Subscribe(subject string, h Handler) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	id := b.next
	b.next++
	if b.subs[subject] == nil {
		b.subs[subject] = map[int]Handler{}
	}
	b.subs[subject][id] = h
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[subject], id)
		if len(b.subs[subject]) == 0 {
			delete(b.subs, subject)
		}
	}, nil
}

func (b *InProc) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.subs = nil
	return nil
}
