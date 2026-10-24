package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store/db"
)

// sqlStore is the Store for both engines. wrap adapts a connection or
// transaction to the engine's placeholder syntax.
type sqlStore struct {
	db   *sql.DB
	wrap func(db.DBTX) db.DBTX
}

func (s *sqlStore) q() *db.Queries { return db.New(s.wrap(s.db)) }

// tx runs fn in a transaction and commits it if fn returns nil.
func (s *sqlStore) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(db.New(s.wrap(tx))); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqlStore) Close() error { return s.db.Close() }

func (s *sqlStore) CreatePrincipal(ctx context.Context, p core.Principal) error {
	return s.q().CreatePrincipal(ctx, db.CreatePrincipalParams{
		ID:          p.ID,
		Kind:        string(p.Kind),
		DisplayName: p.DisplayName,
		CreatedAt:   millis(p.CreatedAt),
	})
}

func (s *sqlStore) GetPrincipal(ctx context.Context, id string) (core.Principal, error) {
	row, err := s.q().GetPrincipal(ctx, id)
	if err != nil {
		return core.Principal{}, notFound(err)
	}
	return core.Principal{
		ID:          row.ID,
		Kind:        core.PrincipalKind(row.Kind),
		DisplayName: row.DisplayName,
		CreatedAt:   time.UnixMilli(row.CreatedAt),
	}, nil
}

func (s *sqlStore) CreateRoom(ctx context.Context, r core.Room, owner core.Membership) error {
	if owner.RoomID != r.ID {
		return fmt.Errorf("store: owner membership is for room %q, not %q", owner.RoomID, r.ID)
	}
	return s.tx(ctx, func(q *db.Queries) error {
		err := q.CreateRoom(ctx, db.CreateRoomParams{
			ID:        r.ID,
			Kind:      string(r.Kind),
			Name:      r.Name,
			CreatedAt: millis(r.CreatedAt),
		})
		if err != nil {
			return err
		}
		return addMember(ctx, q, owner)
	})
}

func (s *sqlStore) GetRoom(ctx context.Context, id string) (core.Room, error) {
	row, err := s.q().GetRoom(ctx, id)
	if err != nil {
		return core.Room{}, notFound(err)
	}
	return room(row.ID, row.Kind, row.Name, row.CreatedAt), nil
}

func (s *sqlStore) RoomsFor(ctx context.Context, principalID string) ([]core.Room, error) {
	rows, err := s.q().RoomsFor(ctx, principalID)
	if err != nil {
		return nil, err
	}
	rooms := make([]core.Room, len(rows))
	for i, row := range rows {
		rooms[i] = room(row.ID, row.Kind, row.Name, row.CreatedAt)
	}
	return rooms, nil
}

func room(id, kind, name string, createdAt int64) core.Room {
	return core.Room{ID: id, Kind: core.RoomKind(kind), Name: name, CreatedAt: time.UnixMilli(createdAt)}
}

func (s *sqlStore) AddMember(ctx context.Context, m core.Membership) error {
	return addMember(ctx, s.q(), m)
}

func addMember(ctx context.Context, q *db.Queries, m core.Membership) error {
	scopes, err := json.Marshal(nonNil(m.Scopes))
	if err != nil {
		return err
	}
	return q.AddMember(ctx, db.AddMemberParams{
		RoomID:      m.RoomID,
		PrincipalID: m.PrincipalID,
		Role:        string(m.Role),
		Scopes:      string(scopes),
	})
}

func (s *sqlStore) GetMember(ctx context.Context, roomID, principalID string) (core.Membership, error) {
	row, err := s.q().GetMember(ctx, db.GetMemberParams{RoomID: roomID, PrincipalID: principalID})
	if err != nil {
		return core.Membership{}, notFound(err)
	}
	return membership(row)
}

func (s *sqlStore) Members(ctx context.Context, roomID string) ([]core.Membership, error) {
	rows, err := s.q().Members(ctx, roomID)
	if err != nil {
		return nil, err
	}
	ms := make([]core.Membership, len(rows))
	for i, row := range rows {
		if ms[i], err = membership(row); err != nil {
			return nil, err
		}
	}
	return ms, nil
}

func membership(row db.Membership) (core.Membership, error) {
	m := core.Membership{RoomID: row.RoomID, PrincipalID: row.PrincipalID, Role: core.Role(row.Role)}
	if err := json.Unmarshal([]byte(row.Scopes), &m.Scopes); err != nil {
		return core.Membership{}, fmt.Errorf("store: membership scopes: %w", err)
	}
	return m, nil
}

func (s *sqlStore) AppendEvent(ctx context.Context, e core.Event, idempotencyKey string) (core.Event, error) {
	if e.ID == "" {
		e.ID = core.NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	e.CreatedAt = e.CreatedAt.Truncate(time.Millisecond)
	parts, err := encodeParts(e.Parts)
	if err != nil {
		return core.Event{}, err
	}
	var existing *core.Event
	err = s.tx(ctx, func(q *db.Queries) error {
		// Bumping the room's seq first locks the room row, so a concurrent
		// append with the same key waits here and then sees this one's
		// committed event in the lookup below.
		seq, err := q.BumpSeq(ctx, e.RoomID)
		if err != nil {
			return notFound(err)
		}
		if idempotencyKey != "" {
			row, err := q.EventByIdempotencyKey(ctx, db.EventByIdempotencyKeyParams{
				RoomID:         e.RoomID,
				SenderID:       e.SenderID,
				IdempotencyKey: idempotencyKey,
			})
			if err == nil {
				ev, err := event(row)
				if err != nil {
					return err
				}
				existing = &ev
				return errRollback
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		e.Seq = seq
		err = q.InsertEvent(ctx, db.InsertEventParams{
			ID:             e.ID,
			RoomID:         e.RoomID,
			Seq:            e.Seq,
			Type:           string(e.Type),
			SenderID:       e.SenderID,
			CausedBy:       e.Causation.CausedBy,
			HopCount:       int32(e.Causation.HopCount),
			Parts:          parts,
			Stream:         string(e.Stream),
			IdempotencyKey: idempotencyKey,
			CreatedAt:      millis(e.CreatedAt),
		})
		if err != nil {
			return err
		}
		return q.InsertOutbox(ctx, db.InsertOutboxParams{
			EventID:   e.ID,
			RoomID:    e.RoomID,
			Seq:       e.Seq,
			CreatedAt: millis(e.CreatedAt),
		})
	})
	if existing != nil {
		return *existing, nil
	}
	if err != nil {
		return core.Event{}, err
	}
	return e, nil
}

// errRollback makes tx roll back without the caller treating it as a
// failure.
var errRollback = errors.New("store: rollback")

func (s *sqlStore) GetEvent(ctx context.Context, id string) (core.Event, error) {
	row, err := s.q().GetEvent(ctx, id)
	if err != nil {
		return core.Event{}, notFound(err)
	}
	return event(row)
}

func (s *sqlStore) EventsAfter(ctx context.Context, roomID string, cursor int64, limit int) ([]core.Event, error) {
	rows, err := s.q().EventsAfter(ctx, db.EventsAfterParams{RoomID: roomID, Seq: cursor, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	events := make([]core.Event, len(rows))
	for i, row := range rows {
		if events[i], err = event(row); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *sqlStore) PendingOutbox(ctx context.Context, limit int) ([]OutboxEntry, error) {
	rows, err := s.q().PendingOutbox(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	entries := make([]OutboxEntry, len(rows))
	for i, row := range rows {
		entries[i] = OutboxEntry{EventID: row.EventID, RoomID: row.RoomID, Seq: row.Seq}
	}
	return entries, nil
}

func (s *sqlStore) AckOutbox(ctx context.Context, eventID string) error {
	return s.q().DeleteOutbox(ctx, eventID)
}

func event(row db.Event) (core.Event, error) {
	parts, err := decodeParts(row.Parts)
	if err != nil {
		return core.Event{}, err
	}
	return core.Event{
		ID:        row.ID,
		RoomID:    row.RoomID,
		Seq:       row.Seq,
		Type:      core.EventType(row.Type),
		SenderID:  row.SenderID,
		Causation: core.Causation{CausedBy: row.CausedBy, HopCount: int(row.HopCount)},
		Parts:     parts,
		Stream:    core.StreamState(row.Stream),
		CreatedAt: time.UnixMilli(row.CreatedAt),
	}, nil
}

type jsonPart struct {
	Kind core.PartKind   `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

func encodeParts(parts []core.Part) (string, error) {
	js := make([]jsonPart, len(parts))
	for i, p := range parts {
		js[i] = jsonPart{Kind: p.Kind, Data: p.Data}
	}
	b, err := json.Marshal(js)
	if err != nil {
		return "", fmt.Errorf("store: encode parts: %w", err)
	}
	return string(b), nil
}

func decodeParts(s string) ([]core.Part, error) {
	var js []jsonPart
	if err := json.Unmarshal([]byte(s), &js); err != nil {
		return nil, fmt.Errorf("store: decode parts: %w", err)
	}
	if len(js) == 0 {
		return nil, nil
	}
	parts := make([]core.Part, len(js))
	for i, p := range js {
		parts[i] = core.Part{Kind: p.Kind, Data: []byte(p.Data)}
	}
	return parts, nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UnixMilli()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
