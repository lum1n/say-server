package bridge

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

// Receive takes in a message from the network. A Conn must not call it
// from the goroutine that completes its Sends: it waits for a send in
// progress on the same chat.
func (i *instance) Receive(ctx context.Context, in Incoming) error {
	if in.Chat.ID == "" || in.ID == "" {
		return errors.New("bridge: an incoming message needs a chat and an id")
	}
	if in.Author.Self && i.b.Mode == store.ModeBot {
		// The bot's own relays.
		return nil
	}
	l, ok, err := i.link(ctx, in.Chat, !in.Edit)
	if err != nil || !ok {
		return err
	}
	defer i.lock(l.ID)()
	if in.Edit {
		return i.receiveEdit(ctx, l, in)
	}
	if in.Author.Self && i.net.Idempotent {
		if err := i.checkPending(ctx, l, in.ID); err != nil {
			return err
		}
	}
	eventID := core.NewID()
	ok, err = i.c.Store.StartBridgeMessage(ctx, store.BridgeMessage{
		LinkID: l.ID, EventID: eventID, RemoteID: in.ID, Direction: store.BridgeIn, State: store.BridgeSent, CreatedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	if !ok {
		// Seen before, or the echo of a message say sent. A row left by
		// a crash before its event was written is finished now.
		row, err := i.c.Store.BridgeMessageByRemote(ctx, l.ID, in.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil || row.Direction != store.BridgeIn {
			return err
		}
		if _, err := i.c.Store.GetEvent(ctx, row.EventID); !errors.Is(err, store.ErrNotFound) {
			return err
		}
		eventID = row.EventID
	}
	as, err := i.author(ctx, l, in.Author)
	if err != nil {
		return err
	}
	at := in.Time
	if at.IsZero() || at.After(time.Now()) {
		at = time.Now()
	}
	replyTo, err := i.replyTarget(ctx, l, in.ReplyTo)
	if err != nil {
		return err
	}
	if replyTo == "" && in.Quote != "" && len(in.Parts) == 0 {
		in.Text = "> " + in.Quote + "\n" + in.Text
	}
	_, err = i.c.Chat.SendAs(ctx, as, l.RoomID, eventID, in.content(), replyTo, "bridge:"+l.ID+":"+in.ID, at)
	return err
}

// replyTarget returns the message in l's room that has the remote id
// remoteID, or "" if say has not seen it: a reply to a message from before
// the bridge came is taken in as a plain message.
func (i *instance) replyTarget(ctx context.Context, l store.BridgeLink, remoteID string) (string, error) {
	if remoteID == "" {
		return "", nil
	}
	row, err := i.c.Store.BridgeMessageByRemote(ctx, l.ID, remoteID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	e, err := i.c.Store.GetEvent(ctx, row.EventID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (e.RoomID != l.RoomID || e.Type != core.EventMessage) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

// checkPending returns ErrPending if the own message remoteID is new to
// the link while a send to it is unfinished. A send whose answer was lost
// is not matched to its remote id until it is tried again, and its echo
// would be taken in as another message. Unfinished sends hold the link's
// cursor, so they are the ones after it.
func (i *instance) checkPending(ctx context.Context, l store.BridgeLink, remoteID string) error {
	_, err := i.c.Store.BridgeMessageByRemote(ctx, l.ID, remoteID)
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	cur, err := i.c.Store.GetBridgeLink(ctx, l.ID)
	if err != nil {
		return err
	}
	out, err := i.c.Store.OutgoingBridgeMessages(ctx, l.RoomID, cur.Cursor+1, math.MaxInt64)
	if err != nil {
		return err
	}
	for _, m := range out {
		if m.LinkID == l.ID && unfinished(m) {
			return ErrPending
		}
	}
	return nil
}

// samePart reports whether a message already has parts.
func samePart(a, b []core.Part) bool {
	return slices.EqualFunc(a, b, func(x, y core.Part) bool {
		return x.Kind == y.Kind && bytes.Equal(x.Data, y.Data)
	})
}

// receiveEdit changes a bridged message to its new content.
func (i *instance) receiveEdit(ctx context.Context, l store.BridgeLink, in Incoming) error {
	row, err := i.c.Store.BridgeMessageByRemote(ctx, l.ID, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	msg, err := i.c.Store.GetEvent(ctx, row.EventID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	parts := in.content()
	if len(in.Parts) == 0 && msg.Prose() == in.Text || samePart(msg.Parts, parts) {
		return nil
	}
	as, err := i.c.Store.GetPrincipal(ctx, msg.SenderID)
	if err != nil {
		return err
	}
	edit, err := i.c.Chat.EditAs(ctx, as, l.RoomID, msg.ID, parts)
	if errors.Is(err, store.ErrNotEditable) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = i.c.Store.StartBridgeMessage(ctx, store.BridgeMessage{
		LinkID: l.ID, EventID: edit.ID, Direction: store.BridgeIn, State: store.BridgeSent, CreatedAt: time.Now(),
	})
	return err
}

// link returns the link of a remote chat. In account mode a chat seen for
// the first time becomes a room of the owner, if create; a bot ignores
// chats no room is linked to.
func (i *instance) link(ctx context.Context, chat RemoteChat, create bool) (store.BridgeLink, bool, error) {
	l, err := i.c.Store.BridgeLinkByRemote(ctx, i.b.ID, chat.ID)
	if err == nil {
		return l, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.BridgeLink{}, false, err
	}
	if !create || i.b.Mode != store.ModeAccount {
		return store.BridgeLink{}, false, nil
	}
	now := time.Now()
	room := core.Room{ID: core.NewID(), Kind: core.RoomGroup, Name: chat.Name, CreatedAt: now}
	if chat.Direct {
		room.Kind = core.RoomDM
	}
	if room.Name == "" {
		room.Name = chat.ID
	}
	l = store.BridgeLink{ID: core.NewID(), BridgeID: i.b.ID, RoomID: room.ID, RemoteID: chat.ID, RemoteName: chat.Name, CreatedAt: now}
	owner := core.Membership{RoomID: room.ID, PrincipalID: i.b.OwnerID, Role: core.RoleOwner}
	err = i.c.Store.CreatePortal(ctx, room, []core.Membership{owner}, l)
	if errors.Is(err, store.ErrLinkExists) {
		l, err = i.c.Store.BridgeLinkByRemote(ctx, i.b.ID, chat.ID)
		return l, err == nil, err
	}
	if err != nil {
		return store.BridgeLink{}, false, err
	}
	i.follow(l)
	i.c.Chat.RoomsChanged(ctx, i.b.OwnerID)
	return l, true, nil
}

// author returns who sends an incoming message: the owner for their own
// messages, else the remote user's ghost, made a member of the room.
func (i *instance) author(ctx context.Context, l store.BridgeLink, a Author) (core.Principal, error) {
	if a.Self {
		return i.owner, nil
	}
	if a.ID == "" {
		return core.Principal{}, errors.New("bridge: an incoming message needs an author")
	}
	g, err := i.c.Store.EnsureGhost(ctx, i.b.ID, a.ID, a.Name)
	if err != nil {
		return core.Principal{}, err
	}
	_, err = i.c.Store.GetMember(ctx, l.RoomID, g.ID)
	if errors.Is(err, store.ErrNotFound) {
		err = i.c.Store.AddMember(ctx, core.Membership{RoomID: l.RoomID, PrincipalID: g.ID, Role: core.RoleMember})
	}
	return g, err
}
