package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store"
)

// deliver sends a link's room log past its cursor to the network.
func (i *instance) deliver(ctx context.Context, linkID string) {
	defer i.lock(linkID)()
	l, err := i.c.Store.GetBridgeLink(ctx, linkID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		i.warn(ctx, "read link", err)
		return
	}
	cursor := l.Cursor
	for ctx.Err() == nil && i.isConnected() {
		events, err := i.c.Store.EventsAfter(ctx, l.RoomID, cursor, 100)
		if err != nil {
			i.warn(ctx, "read events", err)
			return
		}
		for _, e := range events {
			if err := i.deliverEvent(ctx, l, e); err != nil {
				i.warn(ctx, "deliver", err)
				break
			}
			cursor = e.Seq
		}
		if err := i.c.Store.SetBridgeLinkCursor(ctx, l.ID, cursor); err != nil {
			i.warn(ctx, "save cursor", err)
			return
		}
		if len(events) < 100 {
			return
		}
	}
}

// deliverEvent sends one event of the log, if it is something to send.
// An error leaves the cursor before the event, to try it again.
func (i *instance) deliverEvent(ctx context.Context, l store.BridgeLink, e core.Event) error {
	var msg core.Event
	switch {
	case e.Type == core.EventMessage && e.Stream == core.StreamFinal:
		msg = e
	case e.Type == core.EventEdit && e.Stream == core.StreamFinal:
		// The end of a stream: the message is complete now.
		m, err := i.c.Store.GetEvent(ctx, e.Edits)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		msg = m
	case e.Type == core.EventEdit:
		return i.deliverEdit(ctx, l, e)
	default:
		return nil
	}
	if ok, err := i.sends(ctx, msg.SenderID); !ok || err != nil {
		return err
	}
	text := msg.Prose()
	if text == "" {
		return nil
	}
	if time.Since(e.CreatedAt) > MaxDeliveryAge {
		_, err := i.start(ctx, l, msg.ID, store.BridgeExpired)
		return err
	}
	ok, err := i.start(ctx, l, msg.ID, store.BridgeSending)
	if !ok || err != nil {
		// Received from this link, or tried before.
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	remoteID, err := i.conn.Send(sctx, Outgoing{Chat: l.RemoteID, EventID: msg.ID, Text: text, Sender: i.name(ctx, msg.SenderID)})
	cancel()
	state, errMsg := store.BridgeSent, ""
	switch {
	case errors.Is(err, ErrRejected):
		state, errMsg = store.BridgeFailed, err.Error()
	case err != nil:
		state, errMsg = store.BridgeUnknown, err.Error()
	}
	if err := i.c.Store.FinishBridgeMessage(context.WithoutCancel(ctx), l.ID, msg.ID, state, remoteID, errMsg); err != nil {
		return err
	}
	i.c.Chat.DeliveryChanged(ctx, l.RoomID, msg.ID)
	return nil
}

// start records an outgoing message in state; false means the link
// already has a row for it.
func (i *instance) start(ctx context.Context, l store.BridgeLink, eventID, state string) (bool, error) {
	ok, err := i.c.Store.StartBridgeMessage(ctx, store.BridgeMessage{
		LinkID: l.ID, EventID: eventID, Direction: store.BridgeOut, State: state, CreatedAt: time.Now(),
	})
	if ok {
		i.c.Chat.DeliveryChanged(ctx, l.RoomID, eventID)
	}
	return ok, err
}

// deliverEdit changes a sent message on the network, when the network can.
func (i *instance) deliverEdit(ctx context.Context, l store.BridgeLink, e core.Event) error {
	editor, ok := i.conn.(Editor)
	if !ok {
		return nil
	}
	if ok, err := i.sends(ctx, e.SenderID); !ok || err != nil {
		return err
	}
	// An edit that came from the network has a row of its own.
	if _, err := i.c.Store.GetBridgeMessage(ctx, l.ID, e.ID); !errors.Is(err, store.ErrNotFound) {
		return err
	}
	sent, err := i.c.Store.GetBridgeMessage(ctx, l.ID, e.Edits)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sent.RemoteID == "" || time.Since(e.CreatedAt) > MaxDeliveryAge {
		return nil
	}
	text := e.Prose()
	if text == "" {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	m := Outgoing{Chat: l.RemoteID, EventID: e.Edits, Text: text, Sender: i.name(ctx, e.SenderID)}
	if err := editor.Edit(sctx, m, sent.RemoteID); err != nil {
		// An edit is not retried either; the message stays as it was.
		i.warn(ctx, "edit", err)
	}
	return nil
}

// sends reports whether messages from senderID go out on this bridge. In
// account mode only the owner's do: nobody else may write as them. A bot
// relays everyone but its own ghosts, whose messages came from there.
func (i *instance) sends(ctx context.Context, senderID string) (bool, error) {
	if i.b.Mode == store.ModeAccount {
		return senderID == i.b.OwnerID, nil
	}
	g, err := i.c.Store.BridgeGhostByPrincipal(ctx, senderID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return g.BridgeID != i.b.ID, nil
}

// name returns a principal's display name.
func (i *instance) name(ctx context.Context, id string) string {
	i.mu.Lock()
	n, ok := i.names[id]
	i.mu.Unlock()
	if ok {
		return n
	}
	p, err := i.c.Store.GetPrincipal(ctx, id)
	if err != nil {
		return "someone"
	}
	i.mu.Lock()
	i.names[id] = p.DisplayName
	i.mu.Unlock()
	return p.DisplayName
}
