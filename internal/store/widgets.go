package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store/db"
)

// ErrVisitorExists is CreateVisitor's error when the widget already has a
// visitor with that subject, created by a concurrent request.
var ErrVisitorExists = errors.New("store: the widget already has this visitor")

// Widget is a chat widget that other sites embed (docs/adr/0012-widget.md).
type Widget struct {
	ID   string
	Name string
	// PublicKey (pk_…) names the widget in the host page.
	PublicKey string
	// Secret (sk_…) signs identity tokens on the host site's server.
	Secret string
	// Origins are the host page origins allowed to embed the widget.
	Origins []string
	// AgentID is the agent that answers visitors, if any; Team are the
	// people added to every new thread.
	AgentID  string
	Team     []string
	Greeting string
	// Enabled is off for a widget switched off: it shows nothing on host
	// sites and its visitors cannot chat.
	Enabled   bool
	UpdatedAt time.Time
	CreatedAt time.Time
}

// Visitor is the guest principal behind a widget thread. Subject is the
// host site's user id of a verified visitor, empty for an anonymous one.
// SignedInAs is the verified visitor an anonymous one signed in as when
// that one already had a thread, so the two threads stay apart.
type Visitor struct {
	PrincipalID string
	WidgetID    string
	Subject     string
	RoomID      string
	SignedInAs  string
	CreatedAt   time.Time
}

// WidgetThread is a visitor's thread as its team sees it in the inbox.
type WidgetThread struct {
	Visitor
	WidgetName  string
	VisitorName string
	// LastSenderID and LastMessageAt are the thread's latest message;
	// both are zero when it has none.
	LastSenderID  string
	LastMessageAt time.Time
	// AgentAnswers is whether a member is still the thread's responder,
	// that is, the thread has not been handed over.
	AgentAnswers bool
	// SignedInRoomID is the thread of the visitor SignedInAs.
	SignedInRoomID string
}

func (s *sqlStore) CreateWidget(ctx context.Context, w Widget) error {
	origins, team, err := widgetLists(w)
	if err != nil {
		return err
	}
	return s.q().CreateWidget(ctx, db.CreateWidgetParams{
		ID:        w.ID,
		Name:      w.Name,
		PublicKey: w.PublicKey,
		Secret:    w.Secret,
		Origins:   origins,
		AgentID:   w.AgentID,
		Team:      team,
		Greeting:  w.Greeting,
		Enabled:   flag(w.Enabled),
		UpdatedAt: millis(w.UpdatedAt),
		CreatedAt: millis(w.CreatedAt),
	})
}

func (s *sqlStore) UpdateWidget(ctx context.Context, w Widget) error {
	origins, team, err := widgetLists(w)
	if err != nil {
		return err
	}
	n, err := s.q().UpdateWidget(ctx, db.UpdateWidgetParams{
		ID:        w.ID,
		Name:      w.Name,
		Secret:    w.Secret,
		Origins:   origins,
		AgentID:   w.AgentID,
		Team:      team,
		Greeting:  w.Greeting,
		Enabled:   flag(w.Enabled),
		UpdatedAt: millis(w.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) DeleteWidget(ctx context.Context, id string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteWidgetVisitorTokens(ctx, id); err != nil {
			return err
		}
		if err := q.DeleteWidgetVisitors(ctx, id); err != nil {
			return err
		}
		n, err := q.DeleteWidget(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func widgetLists(w Widget) (origins, team string, err error) {
	o, err := json.Marshal(nonNil(w.Origins))
	if err != nil {
		return "", "", err
	}
	t, err := json.Marshal(nonNil(w.Team))
	if err != nil {
		return "", "", err
	}
	return string(o), string(t), nil
}

func (s *sqlStore) GetWidget(ctx context.Context, id string) (Widget, error) {
	row, err := s.q().GetWidget(ctx, id)
	if err != nil {
		return Widget{}, notFound(err)
	}
	return widget(row)
}

func (s *sqlStore) WidgetByKey(ctx context.Context, publicKey string) (Widget, error) {
	row, err := s.q().WidgetByKey(ctx, publicKey)
	if err != nil {
		return Widget{}, notFound(err)
	}
	return widget(row)
}

func (s *sqlStore) Widgets(ctx context.Context) ([]Widget, error) {
	rows, err := s.q().ListWidgets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Widget, len(rows))
	for i, row := range rows {
		if out[i], err = widget(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func widget(row db.Widget) (Widget, error) {
	w := Widget{
		ID:        row.ID,
		Name:      row.Name,
		PublicKey: row.PublicKey,
		Secret:    row.Secret,
		AgentID:   row.AgentID,
		Greeting:  row.Greeting,
		Enabled:   row.Enabled != 0,
		UpdatedAt: unmillis(row.UpdatedAt),
		CreatedAt: unmillis(row.CreatedAt),
	}
	if err := json.Unmarshal([]byte(row.Origins), &w.Origins); err != nil {
		return Widget{}, fmt.Errorf("store: widget origins: %w", err)
	}
	if err := json.Unmarshal([]byte(row.Team), &w.Team); err != nil {
		return Widget{}, fmt.Errorf("store: widget team: %w", err)
	}
	return w, nil
}

func (s *sqlStore) CreateVisitor(ctx context.Context, guest core.Principal, room core.Room, members []core.Membership, v Visitor) error {
	if v.PrincipalID != guest.ID || v.RoomID != room.ID {
		return fmt.Errorf("store: visitor %q in room %q is not guest %q in room %q", v.PrincipalID, v.RoomID, guest.ID, room.ID)
	}
	return s.tx(ctx, func(q *db.Queries) error {
		if err := createPrincipal(ctx, q, guest); err != nil {
			return err
		}
		err := q.CreateRoom(ctx, db.CreateRoomParams{
			ID:        room.ID,
			Kind:      string(room.Kind),
			Name:      room.Name,
			CreatedAt: millis(room.CreatedAt),
		})
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.RoomID != room.ID {
				return fmt.Errorf("store: membership is for room %q, not %q", m.RoomID, room.ID)
			}
			if err := addMember(ctx, q, m); err != nil {
				return err
			}
		}
		n, err := q.CreateVisitor(ctx, db.CreateVisitorParams{
			PrincipalID: v.PrincipalID,
			WidgetID:    v.WidgetID,
			Subject:     v.Subject,
			RoomID:      v.RoomID,
			CreatedAt:   millis(v.CreatedAt),
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrVisitorExists
		}
		return nil
	})
}

func (s *sqlStore) VisitorByPrincipal(ctx context.Context, principalID string) (Visitor, error) {
	row, err := s.q().VisitorByPrincipal(ctx, principalID)
	if err != nil {
		return Visitor{}, notFound(err)
	}
	return visitor(row), nil
}

func (s *sqlStore) VisitorBySubject(ctx context.Context, widgetID, subject string) (Visitor, error) {
	row, err := s.q().VisitorBySubject(ctx, db.VisitorBySubjectParams{WidgetID: widgetID, Subject: subject})
	if err != nil {
		return Visitor{}, notFound(err)
	}
	return visitor(row), nil
}

func (s *sqlStore) VerifyVisitor(ctx context.Context, principalID, subject, displayName, roomName string) (Visitor, error) {
	var v Visitor
	err := s.tx(ctx, func(q *db.Queries) error {
		n, err := q.VerifyVisitor(ctx, db.VerifyVisitorParams{PrincipalID: principalID, Subject: subject})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrVisitorExists
		}
		row, err := q.VisitorByPrincipal(ctx, principalID)
		if err != nil {
			return err
		}
		v = visitor(row)
		if displayName != "" {
			if _, err := q.RenamePrincipal(ctx, db.RenamePrincipalParams{ID: principalID, DisplayName: displayName}); err != nil {
				return err
			}
		}
		if roomName != "" {
			if _, err := q.RenameRoom(ctx, db.RenameRoomParams{ID: v.RoomID, Name: roomName}); err != nil {
				return err
			}
		}
		return nil
	})
	return v, err
}

func (s *sqlStore) LinkVisitor(ctx context.Context, principalID, verifiedID string) error {
	n, err := s.q().LinkVisitor(ctx, db.LinkVisitorParams{PrincipalID: principalID, SignedInAs: verifiedID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) DeleteVisitor(ctx context.Context, principalID string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		row, err := q.VisitorByPrincipal(ctx, principalID)
		if err != nil {
			return notFound(err)
		}
		room := row.RoomID
		// Bumping the seq locks the room row first, so an append that is
		// running finishes before its events are deleted, and one that
		// comes later finds no room.
		if _, err := q.BumpSeq(ctx, room); err != nil {
			return notFound(err)
		}
		steps := []func(context.Context, string) error{
			q.DeleteRoomBridgeMessages, q.DeleteRoomBridgeLinks, q.DeleteRoomAgentCursors,
			q.DeleteRoomEvents, q.DeleteRoomMembers,
		}
		for _, step := range steps {
			if err := step(ctx, room); err != nil {
				return err
			}
		}
		// Anonymous threads it signed in from stay, as nobody's.
		if err := q.UnlinkVisitors(ctx, principalID); err != nil {
			return err
		}
		if err := q.DeleteVisitorRow(ctx, principalID); err != nil {
			return err
		}
		if err := q.DeleteRoom(ctx, room); err != nil {
			return err
		}
		// The rooms it was added to besides its thread.
		steps = []func(context.Context, string) error{
			q.DeleteSenderBridgeMessages, q.DeleteSenderEvents, q.DeletePrincipalMemberships,
			q.DeletePrincipalTokens, q.DeletePrincipal,
		}
		for _, step := range steps {
			if err := step(ctx, principalID); err != nil {
				return err
			}
		}
		return nil
	})
}

func visitor(row db.WidgetVisitor) Visitor {
	return Visitor{
		PrincipalID: row.PrincipalID,
		WidgetID:    row.WidgetID,
		Subject:     row.Subject,
		RoomID:      row.RoomID,
		SignedInAs:  row.SignedInAs,
		CreatedAt:   unmillis(row.CreatedAt),
	}
}

func (s *sqlStore) WidgetThreads(ctx context.Context, principalID string, limit int) ([]WidgetThread, error) {
	rows, err := s.q().WidgetThreadsFor(ctx, db.WidgetThreadsForParams{PrincipalID: principalID, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]WidgetThread, len(rows))
	for i, row := range rows {
		out[i] = WidgetThread{
			Visitor: visitor(db.WidgetVisitor{
				PrincipalID: row.PrincipalID,
				WidgetID:    row.WidgetID,
				Subject:     row.Subject,
				RoomID:      row.RoomID,
				SignedInAs:  row.SignedInAs,
				CreatedAt:   row.CreatedAt,
			}),
			WidgetName:     row.WidgetName,
			VisitorName:    row.VisitorName,
			LastSenderID:   row.LastSenderID.String,
			AgentAnswers:   row.AgentAnswers,
			SignedInRoomID: row.SignedInRoomID,
		}
		if row.LastMessageAt.Valid {
			out[i].LastMessageAt = unmillis(row.LastMessageAt.Int64)
		}
	}
	return out, nil
}
