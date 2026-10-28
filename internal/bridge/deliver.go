package bridge

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// deliver sends a link's room log past its cursor to the network. After
// a failure the link waits, longer each time, before it is tried again.
func (i *instance) deliver(ctx context.Context, linkID string) {
	defer i.lock(linkID)()
	if i.waiting(linkID) {
		return
	}
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
		var failed error
		for _, e := range events {
			if failed = i.deliverEvent(ctx, l, e); failed != nil {
				break
			}
			cursor = e.Seq
		}
		if err := i.c.Store.SetBridgeLinkCursor(ctx, l.ID, cursor); err != nil {
			i.warn(ctx, "save cursor", err)
			i.backOff(linkID)
			return
		}
		if failed != nil {
			i.warn(ctx, "deliver", failed)
			i.backOff(linkID)
			return
		}
		if len(events) < 100 {
			i.mu.Lock()
			delete(i.retries, linkID)
			i.mu.Unlock()
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
	case e.Type == core.EventReaction:
		return i.deliverReaction(ctx, l, e)
	default:
		return nil
	}
	if ok, err := i.sends(ctx, msg.SenderID); !ok || err != nil {
		return err
	}
	text, files := i.content(msg)
	if text == "" && len(files) == 0 {
		return nil
	}
	retry := i.net.Idempotent
	if !retry && time.Since(e.CreatedAt) > MaxDeliveryAge {
		_, err := i.start(ctx, l, msg.ID, store.BridgeExpired)
		return err
	}
	ok, err := i.start(ctx, l, msg.ID, store.BridgeSending)
	if err != nil {
		return err
	}
	if !ok {
		// Received from this link, or tried before: again only if the
		// network takes repeats and the send did not finish.
		if !retry {
			return nil
		}
		row, err := i.c.Store.GetBridgeMessage(ctx, l.ID, msg.ID)
		if err != nil || !unfinished(row) {
			return err
		}
	}
	replyTo, err := i.remoteReply(ctx, l, msg.ReplyTo)
	if err != nil {
		return err
	}
	timeout := sendTimeout
	if len(files) > 0 {
		timeout = mediaTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	remoteID, err := i.conn.Send(sctx, Outgoing{Chat: l.RemoteID, EventID: msg.ID, Text: text, Parts: msg.Parts, Sender: i.name(ctx, msg.SenderID), ReplyTo: replyTo, Media: files})
	cancel()
	state, errMsg := store.BridgeSent, ""
	switch {
	case errors.Is(err, ErrRejected):
		state, errMsg = store.BridgeFailed, err.Error()
	case err != nil && retry:
		// Still sending: the error shows, and the cursor stays before
		// the message so that it and those after it are tried again.
		if ferr := i.c.Store.FinishBridgeMessage(context.WithoutCancel(ctx), l.ID, msg.ID, store.BridgeSending, "", err.Error()); ferr != nil {
			return ferr
		}
		i.c.Chat.DeliveryChanged(ctx, l.RoomID, msg.ID)
		return err
	case err != nil:
		state, errMsg = store.BridgeUnknown, err.Error()
	}
	if err := i.c.Store.FinishBridgeMessage(context.WithoutCancel(ctx), l.ID, msg.ID, state, remoteID, errMsg); err != nil {
		return err
	}
	i.c.Chat.DeliveryChanged(ctx, l.RoomID, msg.ID)
	return nil
}

// content is what of msg goes to the network: its prose, and its files
// where the network takes them. Elsewhere the text names them.
func (i *instance) content(msg core.Event) (string, []Media) {
	text := msg.Prose()
	as := msg.Attachments()
	if !slices.Contains(i.net.Capabilities, connector.CapAttachments) {
		for _, a := range as {
			text = strings.TrimSpace(text + "\n" + a.Placeholder())
		}
		return text, nil
	}
	files := make([]Media, len(as))
	for k, a := range as {
		files[k] = Media{
			Mime: a.Mime, Name: a.Name, Size: a.Size, Width: a.Width, Height: a.Height,
			Open: func(ctx context.Context) (io.ReadCloser, error) { return i.c.Chat.OpenMedia(ctx, a.Media) },
		}
	}
	return text, files
}

// remoteReply returns the remote id on l of the message eventID, or ""
// when it has none there, such as a message that never reached the network.
func (i *instance) remoteReply(ctx context.Context, l store.BridgeLink, eventID string) (string, error) {
	if eventID == "" {
		return "", nil
	}
	row, err := i.c.Store.GetBridgeMessage(ctx, l.ID, eventID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return row.RemoteID, err
}

// unfinished reports whether an outgoing message's send may not have
// arrived.
func unfinished(m store.BridgeMessage) bool {
	return m.Direction == store.BridgeOut && (m.State == store.BridgeSending || m.State == store.BridgeUnknown)
}

// waiting reports whether a link is waiting after a failure.
func (i *instance) waiting(linkID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return time.Now().Before(i.retries[linkID].at)
}

// backOff makes a link wait before it is delivered again, twice as long
// as last time, and marks it for delivery when the wait is over.
func (i *instance) backOff(linkID string) {
	i.mu.Lock()
	r := i.retries[linkID]
	r.pause = min(max(r.pause*2, retryFirst), retryMax)
	r.at = time.Now().Add(r.pause)
	i.retries[linkID] = r
	i.mu.Unlock()
	time.AfterFunc(r.pause, func() {
		i.mark(func(i *instance) { i.dirty[linkID] = true })
	})
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
	if sent.RemoteID == "" || !i.net.Idempotent && time.Since(e.CreatedAt) > MaxDeliveryAge {
		return nil
	}
	text := e.Prose()
	if text == "" {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	m := Outgoing{Chat: l.RemoteID, EventID: e.Edits, Text: text, Parts: e.Parts, Sender: i.name(ctx, e.SenderID)}
	if err := editor.Edit(sctx, m, sent.RemoteID); err != nil {
		if i.net.Idempotent && !errors.Is(err, ErrRejected) {
			// Tried again, as sends are; an edit is idempotent.
			return err
		}
		// An edit is not retried otherwise; the message stays as it was.
		i.warn(ctx, "edit", err)
	}
	return nil
}

// deliverReaction sends the owner's reaction to the network, in account
// mode: a bot reacts as itself, so it cannot stand for the many people of a
// room, and leaves reactions out for now.
func (i *instance) deliverReaction(ctx context.Context, l store.BridgeLink, e core.Event) error {
	reactor, ok := i.conn.(Reactor)
	if !ok || i.b.Mode != store.ModeAccount || e.Reaction == nil || e.SenderID != i.b.OwnerID {
		return nil
	}
	if !i.net.Idempotent && time.Since(e.CreatedAt) > MaxDeliveryAge {
		return nil
	}
	target, err := i.remoteReply(ctx, l, e.Reacts)
	if err != nil || target == "" {
		return err
	}
	r := Reaction{Chat: l.RemoteID, EventID: e.ID, Target: target, Key: e.Reaction.Key, Removed: e.Reaction.Removed}
	if r.Removed {
		added, err := i.c.Store.LastReactionAdd(ctx, e.Reacts, e.SenderID, r.Key)
		if err == nil {
			r.Added, err = i.remoteReply(ctx, l, added.ID)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	// Received from this link, or sent before: reactions are not retried.
	ok, err = i.c.Store.StartBridgeMessage(ctx, store.BridgeMessage{
		LinkID: l.ID, EventID: e.ID, Direction: store.BridgeOut, State: store.BridgeSending, CreatedAt: time.Now(),
	})
	if err != nil || !ok {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	remoteID, err := reactor.React(sctx, r)
	cancel()
	state, errMsg := store.BridgeSent, ""
	switch {
	case errors.Is(err, ErrRejected):
		state, errMsg = store.BridgeFailed, err.Error()
		i.warn(ctx, "reaction dropped", err)
	case err != nil:
		state, errMsg = store.BridgeUnknown, err.Error()
	}
	return i.c.Store.FinishBridgeMessage(context.WithoutCancel(ctx), l.ID, e.ID, state, remoteID, errMsg)
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
