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

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store/db"
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
	return q.CreatePrincipal(ctx, db.CreatePrincipalParams{
		ID:          p.ID,
		Kind:        string(p.Kind),
		DisplayName: p.DisplayName,
		Admin:       flag(p.Admin),
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

func (s *sqlStore) Principals(ctx context.Context) ([]core.Principal, error) {
	rows, err := s.q().ListPrincipals(ctx)
	if err != nil {
		return nil, err
	}
	ps := make([]core.Principal, len(rows))
	for i, row := range rows {
		ps[i] = principal(row)
	}
	return ps, nil
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
	secret, _, err := s.IssueToken(ctx, NewToken{PrincipalID: principalID, Name: name, Expires: expires})
	return secret, err
}

// maxUserAgent bounds the client description kept with a token.
const maxUserAgent = 256

func (s *sqlStore) IssueToken(ctx context.Context, t NewToken) (string, Token, error) {
	secret := tokenPrefix + rand.Text()
	ua := t.UserAgent
	if len(ua) > maxUserAgent {
		ua = ua[:maxUserAgent]
	}
	tok := Token{ID: "tok_" + core.NewID(), Name: t.Name, UserAgent: ua, CreatedAt: time.Now().Truncate(time.Millisecond), ExpiresAt: t.Expires}
	err := s.q().CreateToken(ctx, db.CreateTokenParams{
		Hash:        tokenHash(secret),
		ID:          tok.ID,
		PrincipalID: t.PrincipalID,
		Name:        t.Name,
		UserAgent:   ua,
		ExpiresAt:   optMillis(t.Expires),
		CreatedAt:   tok.CreatedAt.UnixMilli(),
	})
	if err != nil {
		return "", Token{}, err
	}
	return secret, tok, nil
}

func (s *sqlStore) PrincipalByToken(ctx context.Context, secret string) (core.Principal, error) {
	p, _, err := s.Authenticate(ctx, secret)
	return p, err
}

// touchEvery is how stale a token's last use may get before it is
// recorded again, so that reads don't each write.
const touchEvery = time.Hour

func (s *sqlStore) Authenticate(ctx context.Context, secret string) (core.Principal, string, error) {
	now := time.Now()
	hash := tokenHash(secret)
	row, err := s.q().PrincipalByToken(ctx, db.PrincipalByTokenParams{Hash: hash, ExpiresAt: now.UnixMilli()})
	if err != nil {
		return core.Principal{}, "", notFound(err)
	}
	if now.Sub(time.UnixMilli(row.LastUsedAt)) > touchEvery {
		s.q().TouchToken(ctx, db.TouchTokenParams{Hash: hash, LastUsedAt: now.UnixMilli()})
	}
	return principal(db.Principal{
		ID: row.ID, Kind: row.Kind, DisplayName: row.DisplayName, CreatedAt: row.CreatedAt, Admin: row.Admin,
	}), row.TokenID, nil
}

func (s *sqlStore) Tokens(ctx context.Context, principalID string) ([]Token, error) {
	rows, err := s.q().TokensFor(ctx, db.TokensForParams{PrincipalID: principalID, ExpiresAt: time.Now().UnixMilli()})
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(rows))
	for _, r := range rows {
		out = append(out, Token{
			ID: r.ID, Name: r.Name, UserAgent: r.UserAgent,
			CreatedAt: time.UnixMilli(r.CreatedAt), ExpiresAt: unmillis(r.ExpiresAt), LastUsedAt: unmillis(r.LastUsedAt),
		})
	}
	return out, nil
}

func (s *sqlStore) RevokeTokenID(ctx context.Context, principalID, id string) error {
	n, err := s.q().DeleteTokenID(ctx, db.DeleteTokenIDParams{PrincipalID: principalID, ID: id})
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

func (s *sqlStore) RevokeToken(ctx context.Context, secret string) error {
	return s.q().DeleteToken(ctx, tokenHash(secret))
}

func (s *sqlStore) RevokeTokensNamed(ctx context.Context, principalID, name string) error {
	return s.q().DeleteTokensNamed(ctx, db.DeleteTokensNamedParams{PrincipalID: principalID, Name: name})
}

// Pairing codes are ten Crockford base32 characters: 50 bits, typed as
// two groups of five, and good for one use within minutes.
const (
	pairingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairingLen      = 10
)

func (s *sqlStore) CreatePairing(ctx context.Context, principalID string, expires time.Time) (string, error) {
	b := make([]byte, pairingLen)
	rand.Read(b)
	for i := range b {
		b[i] = pairingAlphabet[int(b[i])%len(pairingAlphabet)]
	}
	code := string(b)
	now := time.Now()
	err := s.tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteExpiredPairings(ctx, now.UnixMilli()); err != nil {
			return err
		}
		return q.CreatePairing(ctx, db.CreatePairingParams{
			Hash: tokenHash(code), PrincipalID: principalID, ExpiresAt: expires.UnixMilli(), CreatedAt: now.UnixMilli(),
		})
	})
	if err != nil {
		return "", err
	}
	return code[:5] + "-" + code[5:], nil
}

func (s *sqlStore) RedeemPairing(ctx context.Context, code string) (string, error) {
	id, err := s.q().RedeemPairing(ctx, db.RedeemPairingParams{Hash: tokenHash(normalizePairing(code)), ExpiresAt: time.Now().UnixMilli()})
	return id, notFound(err)
}

// normalizePairing undoes what typing a code does to it: case, dashes and
// spaces, and the letters Crockford base32 reads as digits.
func normalizePairing(code string) string {
	out := make([]byte, 0, len(code))
	for _, c := range []byte(code) {
		switch {
		case c == '-' || c == ' ':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		switch c {
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		out = append(out, c)
	}
	return string(out)
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
	return room(row), nil
}

func (s *sqlStore) RoomsFor(ctx context.Context, principalID string) ([]core.Room, error) {
	rows, err := s.q().RoomsFor(ctx, principalID)
	if err != nil {
		return nil, err
	}
	rooms := make([]core.Room, len(rows))
	for i, row := range rows {
		rooms[i] = room(row)
	}
	return rooms, nil
}

func room(row db.Room) core.Room {
	return core.Room{
		ID:   row.ID,
		Kind: core.RoomKind(row.Kind),
		Name: row.Name,
		Agents: core.AgentPolicy{
			Paused:     row.AgentsPaused != 0,
			MaxHops:    int(row.MaxHopCount),
			TurnLimit:  int(row.AgentTurnLimit),
			TurnWindow: time.Duration(row.AgentTurnWindow) * time.Millisecond,
			RateLimit:  int(row.AgentRateLimit),
		},
		CreatedAt: time.UnixMilli(row.CreatedAt),
	}
}

func (s *sqlStore) SetAgentPolicy(ctx context.Context, roomID string, p core.AgentPolicy) error {
	n, err := s.q().SetAgentPolicy(ctx, db.SetAgentPolicyParams{
		ID:              roomID,
		AgentsPaused:    flag(p.Paused),
		MaxHopCount:     int32(p.MaxHops),
		AgentTurnLimit:  int32(p.TurnLimit),
		AgentTurnWindow: p.TurnWindow.Milliseconds(),
		AgentRateLimit:  int32(p.RateLimit),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
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

func (s *sqlStore) AppendEvent(ctx context.Context, e core.Event, opts AppendOptions) (core.Event, error) {
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
		if opts.IdempotencyKey != "" {
			row, err := q.EventByIdempotencyKey(ctx, db.EventByIdempotencyKeyParams{
				RoomID:         e.RoomID,
				SenderID:       e.SenderID,
				IdempotencyKey: opts.IdempotencyKey,
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
		// A retried send above is not a new turn, so it passes even when
		// the budget has been spent since.
		if opts.Automated {
			if err := checkBudgets(ctx, q, e, opts); err != nil {
				return err
			}
		}
		e.Seq = seq
		return insertEvent(ctx, q, e, parts, opts)
	})
	if existing != nil {
		return *existing, nil
	}
	if err != nil {
		return core.Event{}, err
	}
	return e, nil
}

// insertEvent writes e, with its seq allocated.
func insertEvent(ctx context.Context, q *db.Queries, e core.Event, parts string, opts AppendOptions) error {
	return q.InsertEvent(ctx, db.InsertEventParams{
		ID:             e.ID,
		RoomID:         e.RoomID,
		Seq:            e.Seq,
		Type:           string(e.Type),
		SenderID:       e.SenderID,
		CausedBy:       e.Causation.CausedBy,
		HopCount:       int32(e.Causation.HopCount),
		Parts:          parts,
		Stream:         string(e.Stream),
		IdempotencyKey: opts.IdempotencyKey,
		Automated:      flag(opts.Automated),
		Edits:          e.Edits,
		CreatedAt:      millis(e.CreatedAt),
	})
}

func (s *sqlStore) UpdateStreaming(ctx context.Context, eventID, senderID string, parts []core.Part) error {
	data, err := encodeParts(parts)
	if err != nil {
		return err
	}
	n, err := s.q().UpdateStreaming(ctx, db.UpdateStreamingParams{ID: eventID, SenderID: senderID, Parts: data})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotStreaming
	}
	return nil
}

func (s *sqlStore) AppendEdit(ctx context.Context, e core.Event) (edit, message core.Event, err error) {
	if e.ID == "" {
		e.ID = core.NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	e.CreatedAt = e.CreatedAt.Truncate(time.Millisecond)
	e.Type = core.EventEdit
	e.Stream = ""
	err = s.tx(ctx, func(q *db.Queries) error {
		// The room's lock orders this edit against other edits of the
		// message; a concurrent streaming update finds it final.
		seq, err := q.BumpSeq(ctx, e.RoomID)
		if err != nil {
			return notFound(err)
		}
		row, err := q.GetEvent(ctx, e.Edits)
		if err != nil {
			return notFound(err)
		}
		if message, err = event(row); err != nil {
			return err
		}
		if message.RoomID != e.RoomID {
			return ErrNotFound
		}
		if message.Type != core.EventMessage || message.SenderID != e.SenderID {
			return ErrNotEditable
		}
		if e.Parts == nil {
			if message.Stream != core.StreamStreaming {
				return ErrNotEditable
			}
			e.Parts = message.Parts
		}
		if message.Stream == core.StreamStreaming {
			message.Stream, e.Stream = core.StreamFinal, core.StreamFinal
		} else {
			message.EditedAt = e.CreatedAt
		}
		message.Parts = e.Parts
		parts, err := encodeParts(e.Parts)
		if err != nil {
			return err
		}
		err = q.ReplaceMessage(ctx, db.ReplaceMessageParams{
			ID:       message.ID,
			Parts:    parts,
			Stream:   string(message.Stream),
			EditedAt: optMillis(message.EditedAt),
		})
		if err != nil {
			return err
		}
		e.Seq = seq
		return insertEvent(ctx, q, e, parts, AppendOptions{})
	})
	if err != nil {
		return core.Event{}, core.Event{}, err
	}
	return e, message, nil
}

func (s *sqlStore) StreamingFrom(ctx context.Context, senderID string) ([]core.Event, error) {
	rows, err := s.q().StreamingFrom(ctx, senderID)
	if err != nil {
		return nil, err
	}
	return events(rows)
}

// checkBudgets fails when e would be more than a budget's Max events in
// its Window: when the Max-th latest counted event is inside the window
// that ends at e.
func checkBudgets(ctx context.Context, q *db.Queries, e core.Event, opts AppendOptions) error {
	check := func(b Budget, sender bool) error {
		if b.Max <= 0 {
			return nil
		}
		var at int64
		var err error
		if sender {
			at, err = q.AutomatedFromSenderAt(ctx, db.AutomatedFromSenderAtParams{RoomID: e.RoomID, SenderID: e.SenderID, Offset: int32(b.Max - 1)})
		} else {
			at, err = q.AutomatedAt(ctx, db.AutomatedAtParams{RoomID: e.RoomID, Offset: int32(b.Max - 1)})
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if retry := time.UnixMilli(at).Add(b.Window); retry.After(e.CreatedAt) {
			return &BudgetError{Sender: sender, Budget: b, RetryAt: retry}
		}
		return nil
	}
	if err := check(opts.Room, false); err != nil {
		return err
	}
	return check(opts.Sender, true)
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
		Edits:     row.Edits,
		EditedAt:  unmillis(row.EditedAt),
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

// optMillis and unmillis store an optional time, zero as 0.
func optMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func unmillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func flag(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
