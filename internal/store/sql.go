package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	return createPrincipal(ctx, s.q(), p)
}

func createPrincipal(ctx context.Context, q *db.Queries, p core.Principal) error {
	var admin int32
	if p.Admin {
		admin = 1
	}
	return q.CreatePrincipal(ctx, db.CreatePrincipalParams{
		ID:          p.ID,
		Kind:        string(p.Kind),
		DisplayName: p.DisplayName,
		Admin:       admin,
		CreatedAt:   millis(p.CreatedAt),
	})
}

func (s *sqlStore) GetPrincipal(ctx context.Context, id string) (core.Principal, error) {
	row, err := s.q().GetPrincipal(ctx, id)
	if err != nil {
		return core.Principal{}, notFound(err)
	}
	return principal(row), nil
}

func principal(row db.Principal) core.Principal {
	return core.Principal{
		ID:          row.ID,
		Kind:        core.PrincipalKind(row.Kind),
		DisplayName: row.DisplayName,
		Admin:       row.Admin != 0,
		CreatedAt:   time.UnixMilli(row.CreatedAt),
	}
}

func (s *sqlStore) CreateUser(ctx context.Context, p core.Principal, l Login) error {
	return s.tx(ctx, func(q *db.Queries) error { return createUser(ctx, q, p, l) })
}

func createUser(ctx context.Context, q *db.Queries, p core.Principal, l Login) error {
	if err := createPrincipal(ctx, q, p); err != nil {
		return err
	}
	// ON CONFLICT DO NOTHING turns a duplicate username into zero rows on
	// both engines, instead of an engine-specific constraint error.
	n, err := q.CreateLogin(ctx, db.CreateLoginParams{
		PrincipalID:  p.ID,
		Username:     l.Username,
		PasswordHash: l.PasswordHash,
		CreatedAt:    millis(p.CreatedAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUsernameTaken
	}
	return nil
}

func (s *sqlStore) LoginByUsername(ctx context.Context, username string) (Login, error) {
	row, err := s.q().LoginByUsername(ctx, username)
	if err != nil {
		return Login{}, notFound(err)
	}
	return Login{PrincipalID: row.PrincipalID, Username: row.Username, PasswordHash: row.PasswordHash}, nil
}

func (s *sqlStore) Setup(ctx context.Context, admin core.Principal, l Login) error {
	admin.Admin = true
	return s.tx(ctx, func(q *db.Queries) error {
		// A concurrent claim on Postgres waits for this transaction and
		// then inserts nothing.
		n, err := q.ClaimSetup(ctx, admin.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrSetupDone
		}
		return createUser(ctx, q, admin, l)
	})
}

func (s *sqlStore) SetupDone(ctx context.Context) (bool, error) {
	n, err := s.q().SetupDone(ctx)
	return n > 0, err
}

// tokenPrefix marks say secrets so that secret scanners and people can tell
// them apart.
const tokenPrefix = "say_"

func (s *sqlStore) CreateToken(ctx context.Context, principalID, name string, expires time.Time) (string, error) {
	secret := tokenPrefix + rand.Text()
	var expiresAt int64
	if !expires.IsZero() {
		expiresAt = expires.UnixMilli()
	}
	err := s.q().CreateToken(ctx, db.CreateTokenParams{
		Hash:        tokenHash(secret),
		PrincipalID: principalID,
		Name:        name,
		ExpiresAt:   expiresAt,
		CreatedAt:   millis(time.Time{}),
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

func (s *sqlStore) PrincipalByToken(ctx context.Context, secret string) (core.Principal, error) {
	row, err := s.q().PrincipalByToken(ctx, db.PrincipalByTokenParams{
		Hash:      tokenHash(secret),
		ExpiresAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return core.Principal{}, notFound(err)
	}
	return principal(row), nil
}

func (s *sqlStore) RevokeToken(ctx context.Context, secret string) error {
	return s.q().DeleteToken(ctx, tokenHash(secret))
}

func tokenHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
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
	return events(rows)
}

func (s *sqlStore) EventsBefore(ctx context.Context, roomID string, before int64, limit int) ([]core.Event, error) {
	rows, err := s.q().EventsBefore(ctx, db.EventsBeforeParams{RoomID: roomID, Seq: before, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return events(rows)
}

func events(rows []db.Event) ([]core.Event, error) {
	events := make([]core.Event, len(rows))
	for i, row := range rows {
		var err error
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
