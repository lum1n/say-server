package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lum1n/say-v2/internal/core"
	"github.com/lum1n/say-v2/internal/store/db"
)

// ErrLinkExists is CreateBridgeLink's error when the bridge already links
// that remote channel.
var ErrLinkExists = errors.New("store: the remote channel is already linked")

// Bridge modes.
const (
	// ModeAccount is a person's own account on a network: its chats
	// become rooms of the owner, and the owner's messages go out as them.
	ModeAccount = "account"
	// ModeBot is a relay bot an admin runs, mirroring existing rooms.
	ModeBot = "bot"
)

// Bridge is a connection to another network (docs/adr/0014-bridges.md).
type Bridge struct {
	ID      string
	OwnerID string
	Mode    string
	Network string
	Name    string
	// Config is the network's settings as a JSON object, secrets included.
	Config json.RawMessage
	// State is what the node running the bridge keeps across restarts.
	State json.RawMessage
	// Prompt is a login step waiting for the owner, or empty.
	Prompt    json.RawMessage
	Enabled   bool
	LastError string
	// SeenAt is when the node running the bridge last renewed it.
	SeenAt    time.Time
	UpdatedAt time.Time
	CreatedAt time.Time
}

// BridgeLink mirrors a room to a remote channel. Cursor is the last seq
// of the room handled for delivery.
type BridgeLink struct {
	ID         string
	BridgeID   string
	RoomID     string
	RemoteID   string
	RemoteName string
	Cursor     int64
	CreatedAt  time.Time
}

// BridgeGhost maps a remote user of a bridge to a ghost principal.
type BridgeGhost struct {
	BridgeID     string
	RemoteUserID string
	PrincipalID  string
}

// Directions and states of a BridgeMessage.
const (
	BridgeIn  = "in"
	BridgeOut = "out"

	BridgeSending = "sending"
	BridgeSent    = "sent"
	// BridgeUnknown is a send that was under way when its node stopped.
	// Whether it arrived is unknown, so it is never retried.
	BridgeUnknown = "unknown"
	BridgeFailed  = "failed"
	// BridgeExpired was too old to send when the bridge came back.
	BridgeExpired = "expired"
)

// BridgeMessage is a message that crossed a link, in either direction.
type BridgeMessage struct {
	LinkID    string
	EventID   string
	RemoteID  string
	Direction string
	State     string
	Error     string
	UpdatedAt time.Time
	CreatedAt time.Time
}

func (s *sqlStore) CreateBridge(ctx context.Context, b Bridge) error {
	return s.q().CreateBridge(ctx, db.CreateBridgeParams{
		ID:        b.ID,
		OwnerID:   b.OwnerID,
		Mode:      b.Mode,
		Network:   b.Network,
		Name:      b.Name,
		Config:    configText(b.Config),
		Enabled:   flag(b.Enabled),
		UpdatedAt: millis(b.UpdatedAt),
		CreatedAt: millis(b.CreatedAt),
	})
}

func (s *sqlStore) UpdateBridge(ctx context.Context, b Bridge) error {
	n, err := s.q().UpdateBridge(ctx, db.UpdateBridgeParams{
		ID:        b.ID,
		Name:      b.Name,
		Config:    configText(b.Config),
		Enabled:   flag(b.Enabled),
		UpdatedAt: millis(b.UpdatedAt),
	})
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func configText(c json.RawMessage) string {
	if len(c) == 0 {
		return "{}"
	}
	return string(c)
}

func (s *sqlStore) GetBridge(ctx context.Context, id string) (Bridge, error) {
	row, err := s.q().GetBridge(ctx, id)
	if err != nil {
		return Bridge{}, notFound(err)
	}
	return bridge(row), nil
}

func (s *sqlStore) Bridges(ctx context.Context) ([]Bridge, error) {
	rows, err := s.q().ListBridges(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Bridge, len(rows))
	for i, row := range rows {
		out[i] = bridge(row)
	}
	return out, nil
}

func bridge(row db.Bridge) Bridge {
	b := Bridge{
		ID:        row.ID,
		OwnerID:   row.OwnerID,
		Mode:      row.Mode,
		Network:   row.Network,
		Name:      row.Name,
		Config:    json.RawMessage(row.Config),
		State:     json.RawMessage(row.State),
		Enabled:   row.Enabled != 0,
		LastError: row.LastError,
		SeenAt:    unmillis(row.SeenAt),
		UpdatedAt: time.UnixMilli(row.UpdatedAt),
		CreatedAt: time.UnixMilli(row.CreatedAt),
	}
	if row.Prompt != "" {
		b.Prompt = json.RawMessage(row.Prompt)
	}
	return b
}

func (s *sqlStore) SetBridgeState(ctx context.Context, id string, state json.RawMessage) error {
	return s.q().SetBridgeState(ctx, db.SetBridgeStateParams{ID: id, State: configText(state)})
}

func (s *sqlStore) SetBridgePrompt(ctx context.Context, id string, prompt json.RawMessage) error {
	return s.q().SetBridgePrompt(ctx, db.SetBridgePromptParams{ID: id, Prompt: string(prompt)})
}

func (s *sqlStore) SetBridgeError(ctx context.Context, id, msg string) error {
	return s.q().SetBridgeError(ctx, db.SetBridgeErrorParams{ID: id, LastError: msg})
}

func (s *sqlStore) SetBridgeSeen(ctx context.Context, id string, at time.Time) error {
	return s.q().SetBridgeSeen(ctx, db.SetBridgeSeenParams{ID: id, SeenAt: optMillis(at)})
}

func (s *sqlStore) CreateBridgeLink(ctx context.Context, l BridgeLink) error {
	return createBridgeLink(ctx, s.q(), l)
}

func (s *sqlStore) CreatePortal(ctx context.Context, r core.Room, members []core.Membership, l BridgeLink) error {
	if l.RoomID != r.ID {
		return fmt.Errorf("store: link is for room %q, not %q", l.RoomID, r.ID)
	}
	return s.tx(ctx, func(q *db.Queries) error {
		err := q.CreateRoom(ctx, db.CreateRoomParams{ID: r.ID, Kind: string(r.Kind), Name: r.Name, CreatedAt: millis(r.CreatedAt)})
		if err != nil {
			return err
		}
		for _, m := range members {
			if err := addMember(ctx, q, m); err != nil {
				return err
			}
		}
		return createBridgeLink(ctx, q, l)
	})
}

func createBridgeLink(ctx context.Context, q *db.Queries, l BridgeLink) error {
	n, err := q.CreateBridgeLink(ctx, db.CreateBridgeLinkParams{
		ID:         l.ID,
		BridgeID:   l.BridgeID,
		RoomID:     l.RoomID,
		RemoteID:   l.RemoteID,
		RemoteName: l.RemoteName,
		Cursor:     l.Cursor,
		CreatedAt:  millis(l.CreatedAt),
	})
	if err == nil && n == 0 {
		return ErrLinkExists
	}
	return err
}

func (s *sqlStore) DeleteBridgeLink(ctx context.Context, bridgeID, linkID string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteBridgeLinkMessages(ctx, linkID); err != nil {
			return err
		}
		n, err := q.DeleteBridgeLink(ctx, db.DeleteBridgeLinkParams{ID: linkID, BridgeID: bridgeID})
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
}

func (s *sqlStore) GetBridgeLink(ctx context.Context, id string) (BridgeLink, error) {
	row, err := s.q().GetBridgeLink(ctx, id)
	if err != nil {
		return BridgeLink{}, notFound(err)
	}
	return bridgeLink(row), nil
}

func (s *sqlStore) BridgeLinkByRemote(ctx context.Context, bridgeID, remoteID string) (BridgeLink, error) {
	row, err := s.q().BridgeLinkByRemote(ctx, db.BridgeLinkByRemoteParams{BridgeID: bridgeID, RemoteID: remoteID})
	if err != nil {
		return BridgeLink{}, notFound(err)
	}
	return bridgeLink(row), nil
}

func (s *sqlStore) BridgeLinks(ctx context.Context, bridgeID string) ([]BridgeLink, error) {
	rows, err := s.q().BridgeLinks(ctx, bridgeID)
	return bridgeLinks(rows, err)
}

func (s *sqlStore) RoomBridgeLinks(ctx context.Context, roomID string) ([]BridgeLink, error) {
	rows, err := s.q().RoomBridgeLinks(ctx, roomID)
	return bridgeLinks(rows, err)
}

func bridgeLinks(rows []db.BridgeLink, err error) ([]BridgeLink, error) {
	if err != nil {
		return nil, err
	}
	out := make([]BridgeLink, len(rows))
	for i, row := range rows {
		out[i] = bridgeLink(row)
	}
	return out, nil
}

func bridgeLink(row db.BridgeLink) BridgeLink {
	return BridgeLink{
		ID:         row.ID,
		BridgeID:   row.BridgeID,
		RoomID:     row.RoomID,
		RemoteID:   row.RemoteID,
		RemoteName: row.RemoteName,
		Cursor:     row.Cursor,
		CreatedAt:  time.UnixMilli(row.CreatedAt),
	}
}

func (s *sqlStore) SetBridgeLinkCursor(ctx context.Context, linkID string, seq int64) error {
	return s.q().SetBridgeLinkCursor(ctx, db.SetBridgeLinkCursorParams{ID: linkID, Cursor: seq})
}

func (s *sqlStore) EnsureGhost(ctx context.Context, bridgeID, remoteUserID, name string) (core.Principal, error) {
	var p core.Principal
	err := s.tx(ctx, func(q *db.Queries) error {
		g, err := q.GetBridgeGhost(ctx, db.GetBridgeGhostParams{BridgeID: bridgeID, RemoteUserID: remoteUserID})
		if err == nil {
			row, err := q.GetPrincipal(ctx, g.PrincipalID)
			if err != nil {
				return err
			}
			p = principal(row)
			if name != "" && name != p.DisplayName {
				p.DisplayName = name
				_, err = q.RenamePrincipal(ctx, db.RenamePrincipalParams{ID: p.ID, DisplayName: name})
			}
			return err
		}
		if err = notFound(err); !errors.Is(err, ErrNotFound) {
			return err
		}
		p = core.Principal{ID: core.NewID(), Kind: core.PrincipalGhost, DisplayName: name, CreatedAt: time.Now()}
		if p.DisplayName == "" {
			p.DisplayName = remoteUserID
		}
		if err := createPrincipal(ctx, q, p); err != nil {
			return err
		}
		n, err := q.CreateBridgeGhost(ctx, db.CreateBridgeGhostParams{
			BridgeID:     bridgeID,
			RemoteUserID: remoteUserID,
			PrincipalID:  p.ID,
			CreatedAt:    millis(p.CreatedAt),
		})
		if err == nil && n == 0 {
			return errGhostRace
		}
		return err
	})
	if errors.Is(err, errGhostRace) {
		// Another node made the ghost first; ours was rolled back.
		return s.EnsureGhost(ctx, bridgeID, remoteUserID, name)
	}
	return p, err
}

var errGhostRace = errors.New("store: ghost created concurrently")

func (s *sqlStore) BridgeGhostByPrincipal(ctx context.Context, principalID string) (BridgeGhost, error) {
	row, err := s.q().BridgeGhostByPrincipal(ctx, principalID)
	if err != nil {
		return BridgeGhost{}, notFound(err)
	}
	return BridgeGhost{BridgeID: row.BridgeID, RemoteUserID: row.RemoteUserID, PrincipalID: row.PrincipalID}, nil
}

func (s *sqlStore) StartBridgeMessage(ctx context.Context, m BridgeMessage) (bool, error) {
	n, err := s.q().StartBridgeMessage(ctx, db.StartBridgeMessageParams{
		LinkID:    m.LinkID,
		EventID:   m.EventID,
		RemoteID:  m.RemoteID,
		Direction: m.Direction,
		State:     m.State,
		UpdatedAt: millis(m.CreatedAt),
	})
	return n > 0, err
}

func (s *sqlStore) FinishBridgeMessage(ctx context.Context, linkID, eventID, state, remoteID, errMsg string) error {
	return s.q().FinishBridgeMessage(ctx, db.FinishBridgeMessageParams{
		LinkID:    linkID,
		EventID:   eventID,
		State:     state,
		RemoteID:  remoteID,
		Error:     errMsg,
		UpdatedAt: time.Now().UnixMilli(),
	})
}

func (s *sqlStore) BridgeMessageByRemote(ctx context.Context, linkID, remoteID string) (BridgeMessage, error) {
	row, err := s.q().BridgeMessageByRemote(ctx, db.BridgeMessageByRemoteParams{LinkID: linkID, RemoteID: remoteID})
	if err != nil {
		return BridgeMessage{}, notFound(err)
	}
	return bridgeMessage(row), nil
}

func (s *sqlStore) GetBridgeMessage(ctx context.Context, linkID, eventID string) (BridgeMessage, error) {
	row, err := s.q().GetBridgeMessage(ctx, db.GetBridgeMessageParams{LinkID: linkID, EventID: eventID})
	if err != nil {
		return BridgeMessage{}, notFound(err)
	}
	return bridgeMessage(row), nil
}

func (s *sqlStore) MarkBridgeSendsUnknown(ctx context.Context, bridgeID string) error {
	return s.q().MarkBridgeSendsUnknown(ctx, db.MarkBridgeSendsUnknownParams{BridgeID: bridgeID, UpdatedAt: time.Now().UnixMilli()})
}

func (s *sqlStore) OutgoingBridgeMessages(ctx context.Context, roomID string, fromSeq, toSeq int64) ([]BridgeMessage, error) {
	rows, err := s.q().OutgoingBridgeMessages(ctx, db.OutgoingBridgeMessagesParams{RoomID: roomID, FromSeq: fromSeq, ToSeq: toSeq})
	if err != nil {
		return nil, err
	}
	out := make([]BridgeMessage, len(rows))
	for i, row := range rows {
		out[i] = bridgeMessage(row)
	}
	return out, nil
}

func bridgeMessage(row db.BridgeMessage) BridgeMessage {
	return BridgeMessage{
		LinkID:    row.LinkID,
		EventID:   row.EventID,
		RemoteID:  row.RemoteID,
		Direction: row.Direction,
		State:     row.State,
		Error:     row.Error,
		UpdatedAt: time.UnixMilli(row.UpdatedAt),
		CreatedAt: time.UnixMilli(row.CreatedAt),
	}
}
