package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

// eachEngine runs fn against a fresh SQLite store, and against a fresh
// Postgres schema when SAY_TEST_POSTGRES holds a postgres:// URL.
func eachEngine(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "say.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		fn(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("SAY_TEST_POSTGRES")
		if dsn == "" {
			t.Skip("SAY_TEST_POSTGRES not set")
		}
		fn(t, freshPostgres(t, dsn))
	})
}

func freshPostgres(t *testing.T, dsn string) Store {
	t.Helper()
	s, err := OpenPostgres(context.Background(), freshSchema(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// freshSchema creates a schema that is dropped after the test and returns
// dsn with its search_path set to it.
func freshSchema(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "test_" + strings.ReplaceAll(core.NewID(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Nodes of a cluster start together against one database; exactly one of
// them applies each migration.
func TestPostgresConcurrentMigrations(t *testing.T) {
	dsn := os.Getenv("SAY_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("SAY_TEST_POSTGRES not set")
	}
	dsn = freshSchema(t, dsn)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenPostgres(context.Background(), dsn)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// seed creates a principal and a room it owns.
func seed(t *testing.T, s Store) (core.Principal, core.Room) {
	t.Helper()
	ctx := context.Background()
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Ada"}
	if err := s.CreatePrincipal(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := core.Room{ID: core.NewID(), Kind: core.RoomGroup, Name: "general"}
	owner := core.Membership{RoomID: r.ID, PrincipalID: p.ID, Role: core.RoleOwner, Scopes: []string{"admin"}}
	if err := s.CreateRoom(ctx, r, owner); err != nil {
		t.Fatal(err)
	}
	return p, r
}

func text(s string) []core.Part {
	return []core.Part{{Kind: core.PartText, Data: []byte(fmt.Sprintf("%q", s))}}
}

func TestPrincipalsRoomsMembers(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)

		got, err := s.GetPrincipal(ctx, p.ID)
		if err != nil || got.DisplayName != "Ada" || got.Kind != core.PrincipalHuman || got.CreatedAt.IsZero() {
			t.Fatalf("GetPrincipal = %+v, %v", got, err)
		}
		if _, err := s.GetPrincipal(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing principal: %v", err)
		}
		room, err := s.GetRoom(ctx, r.ID)
		if err != nil || room.Name != "general" || room.Kind != core.RoomGroup {
			t.Fatalf("GetRoom = %+v, %v", room, err)
		}

		bot := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "bot", CreatedAt: time.Now().Add(time.Second)}
		if err := s.CreatePrincipal(ctx, bot); err != nil {
			t.Fatal(err)
		}
		ps, err := s.Principals(ctx)
		if err != nil || len(ps) != 2 || ps[0].ID != p.ID || ps[1].ID != bot.ID {
			t.Fatalf("Principals = %+v, %v", ps, err)
		}
		if err := s.AddMember(ctx, core.Membership{RoomID: r.ID, PrincipalID: bot.ID, Role: core.RoleMember}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, core.Membership{RoomID: r.ID, PrincipalID: bot.ID, Role: core.RoleMember}); err == nil {
			t.Fatal("adding a member twice succeeded")
		}
		ms, err := s.Members(ctx, r.ID)
		if err != nil || len(ms) != 2 {
			t.Fatalf("Members = %+v, %v", ms, err)
		}
		m, err := s.GetMember(ctx, r.ID, p.ID)
		if err != nil || m.Role != core.RoleOwner || len(m.Scopes) != 1 || m.Scopes[0] != "admin" {
			t.Fatalf("GetMember = %+v, %v", m, err)
		}
		rooms, err := s.RoomsFor(ctx, bot.ID)
		if err != nil || len(rooms) != 1 || rooms[0].ID != r.ID {
			t.Fatalf("RoomsFor = %+v, %v", rooms, err)
		}
	})
}

func TestTokens(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, _ := seed(t, s)
		secret, err := s.CreateToken(ctx, p.ID, "cli", time.Time{})
		if err != nil || !strings.HasPrefix(secret, "say_") {
			t.Fatalf("CreateToken = %q, %v", secret, err)
		}
		other, err := s.CreateToken(ctx, p.ID, "cli", time.Time{})
		if err != nil || other == secret {
			t.Fatalf("second CreateToken = %q, %v", other, err)
		}
		got, err := s.PrincipalByToken(ctx, secret)
		if err != nil || got.ID != p.ID || got.Kind != core.PrincipalHuman {
			t.Fatalf("PrincipalByToken = %+v, %v", got, err)
		}
		if _, err := s.PrincipalByToken(ctx, secret+"x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong secret: %v", err)
		}
		if _, err := s.CreateToken(ctx, "no-such-principal", "cli", time.Time{}); err == nil {
			t.Fatal("token for unknown principal was created")
		}
	})
}

func TestTokenExpiryAndRevocation(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, _ := seed(t, s)
		expired, err := s.CreateToken(ctx, p.ID, "session", time.Now().Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PrincipalByToken(ctx, expired); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired token: %v", err)
		}
		live, err := s.CreateToken(ctx, p.ID, "session", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PrincipalByToken(ctx, live); err != nil {
			t.Fatalf("live token: %v", err)
		}
		if err := s.RevokeToken(ctx, live); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PrincipalByToken(ctx, live); !errors.Is(err, ErrNotFound) {
			t.Fatalf("revoked token: %v", err)
		}
		if err := s.RevokeToken(ctx, live); err != nil {
			t.Fatalf("revoking twice: %v", err)
		}
	})
}

func TestUsers(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Ann"}
		if err := s.CreateUser(ctx, p, Login{Username: "ann", PasswordHash: "h1"}); err != nil {
			t.Fatal(err)
		}
		l, err := s.LoginByUsername(ctx, "ann")
		if err != nil || l.PrincipalID != p.ID || l.PasswordHash != "h1" {
			t.Fatalf("LoginByUsername = %+v, %v", l, err)
		}
		if _, err := s.LoginByUsername(ctx, "bob"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown username: %v", err)
		}
		q := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Other Ann"}
		if err := s.CreateUser(ctx, q, Login{Username: "ann", PasswordHash: "h2"}); !errors.Is(err, ErrUsernameTaken) {
			t.Fatalf("duplicate username: %v", err)
		}
		if _, err := s.GetPrincipal(ctx, q.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("principal survived a taken username: %v", err)
		}
	})
}

func TestSetupOnce(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if done, err := s.SetupDone(ctx); err != nil || done {
			t.Fatalf("SetupDone before = %v, %v", done, err)
		}
		// Setup races from several nodes; exactly one wins.
		var wg sync.WaitGroup
		errs := make([]error, 5)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "admin"}
				errs[i] = s.Setup(ctx, p, Login{Username: fmt.Sprintf("admin%d", i), PasswordHash: "h"})
			}()
		}
		wg.Wait()
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
				l, err := s.LoginByUsername(ctx, fmt.Sprintf("admin%d", i))
				if err != nil {
					t.Fatal(err)
				}
				p, err := s.GetPrincipal(ctx, l.PrincipalID)
				if err != nil || !p.Admin {
					t.Fatalf("admin = %+v, %v", p, err)
				}
			case errors.Is(err, ErrSetupDone):
				if _, err := s.LoginByUsername(ctx, fmt.Sprintf("admin%d", i)); !errors.Is(err, ErrNotFound) {
					t.Fatalf("losing setup left a login: %v", err)
				}
			default:
				t.Fatalf("Setup: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("%d setups succeeded", won)
		}
		if done, err := s.SetupDone(ctx); err != nil || !done {
			t.Fatalf("SetupDone after = %v, %v", done, err)
		}
	})
}

func TestCreateRoomIsAtomic(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		r := core.Room{ID: core.NewID(), Kind: core.RoomGroup, Name: "x"}
		owner := core.Membership{RoomID: r.ID, PrincipalID: "no-such-principal", Role: core.RoleOwner}
		if err := s.CreateRoom(ctx, r, owner); err == nil {
			t.Fatal("room with unknown owner was created")
		}
		if _, err := s.GetRoom(ctx, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("room survived a failed create: %v", err)
		}
	})
}

func TestAppendAndReplay(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		var first core.Event
		for i := range 5 {
			e, err := s.AppendEvent(ctx, core.Event{
				RoomID:   r.ID,
				Type:     core.EventMessage,
				SenderID: p.ID,
				Parts:    text(fmt.Sprint("hello ", i)),
				Stream:   core.StreamFinal,
			}, AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if e.Seq != int64(i+1) || e.ID == "" {
				t.Fatalf("event %d: seq %d id %q", i, e.Seq, e.ID)
			}
			if i == 0 {
				first = e
			}
		}

		got, err := s.EventsAfter(ctx, r.ID, 2, 10)
		if err != nil || len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
			t.Fatalf("EventsAfter(2) = %+v, %v", got, err)
		}
		got, err = s.EventsAfter(ctx, r.ID, 0, 2)
		if err != nil || len(got) != 2 {
			t.Fatalf("EventsAfter limit = %d, %v", len(got), err)
		}
		before, err := s.EventsBefore(ctx, r.ID, 5, 2)
		if err != nil || len(before) != 2 || before[0].Seq != 3 || before[1].Seq != 4 {
			t.Fatalf("EventsBefore(5, 2) = %+v, %v", before, err)
		}
		e := got[0]
		if e.ID != first.ID || !e.CreatedAt.Equal(first.CreatedAt) || e.Stream != core.StreamFinal ||
			len(e.Parts) != 1 || e.Parts[0].Kind != core.PartText || string(e.Parts[0].Data) != `"hello 0"` {
			t.Fatalf("round trip: got %+v, want %+v", e, first)
		}

		reply, err := s.AppendEvent(ctx, core.Event{
			RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID,
			Causation: core.Causation{CausedBy: first.ID, HopCount: 2},
		}, AppendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		back, err := s.GetEvent(ctx, reply.ID)
		if err != nil || back.Causation != reply.Causation || back.Parts != nil {
			t.Fatalf("GetEvent = %+v, %v", back, err)
		}

		if _, err := s.AppendEvent(ctx, core.Event{RoomID: "nope", Type: core.EventMessage, SenderID: p.ID}, AppendOptions{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("append to missing room: %v", err)
		}
	})
}

func TestStreamingAndEdits(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		other := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "bob"}
		if err := s.CreatePrincipal(ctx, other); err != nil {
			t.Fatal(err)
		}
		msg, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, Stream: core.StreamStreaming}, AppendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateStreaming(ctx, msg.ID, p.ID, text("hel")); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateStreaming(ctx, msg.ID, other.ID, text("hijack")); !errors.Is(err, ErrNotStreaming) {
			t.Fatalf("someone else's streaming update: %v", err)
		}
		streaming, err := s.StreamingFrom(ctx, p.ID)
		if err != nil || len(streaming) != 1 || streaming[0].ID != msg.ID || string(streaming[0].Parts[0].Data) != `"hel"` {
			t.Fatalf("StreamingFrom = %+v, %v", streaming, err)
		}
		if _, _, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: other.ID, Edits: msg.ID}); !errors.Is(err, ErrNotEditable) {
			t.Fatalf("edit by someone else: %v", err)
		}
		if _, _, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: p.ID, Edits: "nope"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("edit of a missing message: %v", err)
		}

		// Ending the stream without parts keeps the streamed ones.
		edit, got, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: p.ID, Edits: msg.ID})
		if err != nil {
			t.Fatal(err)
		}
		if edit.Type != core.EventEdit || edit.Seq != msg.Seq+1 || edit.Stream != core.StreamFinal || edit.Edits != msg.ID ||
			string(edit.Parts[0].Data) != `"hel"` || got.Stream != core.StreamFinal || !got.EditedAt.IsZero() {
			t.Fatalf("ending the stream: edit %+v, message %+v", edit, got)
		}
		if err := s.UpdateStreaming(ctx, msg.ID, p.ID, text("late")); !errors.Is(err, ErrNotStreaming) {
			t.Fatalf("streaming update after the end: %v", err)
		}
		if streaming, _ := s.StreamingFrom(ctx, p.ID); len(streaming) != 0 {
			t.Fatalf("still streaming: %+v", streaming)
		}
		if _, _, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: p.ID, Edits: msg.ID}); !errors.Is(err, ErrNotEditable) {
			t.Fatalf("empty edit of a final message: %v", err)
		}
		if _, _, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: p.ID, Edits: edit.ID, Parts: text("x")}); !errors.Is(err, ErrNotEditable) {
			t.Fatalf("edit of an edit: %v", err)
		}

		edit2, _, err := s.AppendEdit(ctx, core.Event{RoomID: r.ID, SenderID: p.ID, Edits: msg.ID, Parts: text("hello")})
		if err != nil || edit2.Stream != "" {
			t.Fatalf("edit of a final message: %+v, %v", edit2, err)
		}
		back, err := s.GetEvent(ctx, msg.ID)
		if err != nil || string(back.Parts[0].Data) != `"hello"` || back.Stream != core.StreamFinal ||
			!back.EditedAt.Equal(edit2.CreatedAt) || back.Seq != msg.Seq || back.Edits != "" {
			t.Fatalf("message after edits = %+v, %v", back, err)
		}
		log, err := s.EventsAfter(ctx, r.ID, 0, 10)
		if err != nil || len(log) != 3 || log[2].ID != edit2.ID || log[2].Edits != msg.ID {
			t.Fatalf("log = %+v, %v", log, err)
		}
	})
}

func TestIdempotencyKey(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		send := func(key, body string) core.Event {
			t.Helper()
			e, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, Parts: text(body)}, AppendOptions{IdempotencyKey: key})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		a := send("k1", "first")
		b := send("k1", "retry")
		if b.ID != a.ID || b.Seq != a.Seq || string(b.Parts[0].Data) != `"first"` {
			t.Fatalf("retry returned %+v, want %+v", b, a)
		}
		c := send("k2", "second")
		if c.Seq != a.Seq+1 {
			t.Fatalf("seq after a deduplicated retry = %d, want %d", c.Seq, a.Seq+1)
		}
		log, err := s.EventsAfter(ctx, r.ID, 0, 10)
		if err != nil || len(log) != 2 {
			t.Fatalf("log after retry = %+v, %v", log, err)
		}
	})
}

func TestAgentPolicy(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		if got, _ := s.GetRoom(ctx, r.ID); got.Agents != (core.AgentPolicy{}) {
			t.Fatalf("new room policy = %+v", got.Agents)
		}
		want := core.AgentPolicy{Paused: true, MaxHops: 3, TurnLimit: 20, TurnWindow: 5 * time.Minute, RateLimit: 4}
		if err := s.SetAgentPolicy(ctx, r.ID, want); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetRoom(ctx, r.ID); err != nil || got.Agents != want {
			t.Fatalf("GetRoom policy = %+v, %v", got.Agents, err)
		}
		if rooms, err := s.RoomsFor(ctx, p.ID); err != nil || len(rooms) != 1 || rooms[0].Agents != want {
			t.Fatalf("RoomsFor policy = %+v, %v", rooms, err)
		}
		if err := s.SetAgentPolicy(ctx, "nope", want); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown room = %v", err)
		}
	})
}

// Budgets count automated events only, check them under the room lock and
// leave no seq gap when they refuse one.
func TestBudgets(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		human, r := seed(t, s)
		a1 := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "a1"}
		a2 := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "a2"}
		for _, p := range []core.Principal{a1, a2} {
			if err := s.CreatePrincipal(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		opts := AppendOptions{
			Automated: true,
			Room:      Budget{Max: 4, Window: time.Hour},
			Sender:    Budget{Max: 2, Window: time.Minute},
		}
		long := time.Now().Add(-2 * time.Minute) // outside the sender window
		app := func(p core.Principal, at time.Time, o AppendOptions) (core.Event, error) {
			return s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, CreatedAt: at}, o)
		}
		for _, at := range []time.Time{long, {}, {}} {
			if _, err := app(a1, at, opts); err != nil {
				t.Fatal(err)
			}
		}
		// a1 sent two in the last minute.
		var be *BudgetError
		if _, err := app(a1, time.Time{}, opts); !errors.As(err, &be) || !be.Sender || !be.RetryAt.After(time.Now()) {
			t.Fatalf("sender budget = %v", err)
		}
		if _, err := app(a2, time.Time{}, opts); err != nil {
			t.Fatal(err)
		}
		// The room has had four automated events this hour.
		if _, err := app(a2, time.Time{}, opts); !errors.As(err, &be) || be.Sender || be.Budget.Max != 4 {
			t.Fatalf("room budget = %v", err)
		}
		// Humans are neither counted nor limited.
		if _, err := app(human, time.Time{}, AppendOptions{Room: opts.Room}); err != nil {
			t.Fatal(err)
		}
		// The refusals left no gap: seq 1–4 for the agents, 5 for the human.
		got, err := s.EventsAfter(ctx, r.ID, 0, 10)
		if err != nil || len(got) != 5 || got[4].Seq != 5 || got[4].SenderID != human.ID {
			t.Fatalf("log = %+v, %v", got, err)
		}
		opts.Room.Max = 10
		if _, err := app(a2, time.Time{}, opts); err != nil {
			t.Fatal(err)
		}
	})
}

// A retried send whose event is stored passes after the budget is spent.
func TestBudgetRetry(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, r := seed(t, s)
		a := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "a"}
		if err := s.CreatePrincipal(ctx, a); err != nil {
			t.Fatal(err)
		}
		opts := AppendOptions{IdempotencyKey: "k", Automated: true, Sender: Budget{Max: 1, Window: time.Minute}}
		first, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: a.ID}, opts)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: a.ID}, opts)
		if err != nil || again.ID != first.ID {
			t.Fatalf("retry = %+v, %v", again, err)
		}
		opts.IdempotencyKey = "other"
		var be *BudgetError
		if _, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: a.ID}, opts); !errors.As(err, &be) {
			t.Fatalf("new key = %v", err)
		}
	})
}

// Concurrent automated appends, also from several nodes, admit exactly the
// budget: the room lock serializes the count and the insert.
func TestConcurrentBudget(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, r := seed(t, s)
		a := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "a"}
		if err := s.CreatePrincipal(ctx, a); err != nil {
			t.Fatal(err)
		}
		opts := AppendOptions{Automated: true, Room: Budget{Max: 3, Window: time.Hour}}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, refused := 0, 0
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: a.ID}, opts)
				var be *BudgetError
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
				case errors.As(err, &be):
					refused++
				default:
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if ok != 3 || refused != 9 {
			t.Fatalf("ok %d, refused %d", ok, refused)
		}
	})
}

func TestConcurrentAppends(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, r := seed(t, s)
		const writers, each = 8, 10

		var wg sync.WaitGroup
		errs := make(chan error, writers*each)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range each {
					// Every writer sends the same keys, so each key must be
					// stored exactly once.
					key := fmt.Sprint("k", i)
					if w%2 == 0 {
						key = fmt.Sprint("w", w, "-", i)
					}
					_, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID}, AppendOptions{IdempotencyKey: key})
					if err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}

		events, err := s.EventsAfter(ctx, r.ID, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		want := writers/2*each + each // unique keys from even writers, shared keys from odd ones
		if len(events) != want {
			t.Fatalf("stored %d events, want %d", len(events), want)
		}
		for i, e := range events {
			if e.Seq != int64(i+1) {
				t.Fatalf("event %d has seq %d; seqs must be gapless", i, e.Seq)
			}
		}
	})
}

func TestReopenKeepsDataAndSkipsMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "say.db")
	s, err := Open(ctx, "sqlite:"+path)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := seed(t, s)
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetPrincipal(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSQLitePlaceholders(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                          "SELECT 1",
		"WHERE a = $1 AND b = $12":          "WHERE a = ?1 AND b = ?12",
		"WHERE a = '$1' AND b = $2":         "WHERE a = '$1' AND b = ?2",
		`SELECT "$1" FROM t WHERE x = $1`:   `SELECT "$1" FROM t WHERE x = ?1`,
		"-- costs $5\nSELECT $1":            "-- costs $5\nSELECT ?1",
		"SELECT 'it''s $1', $2":             "SELECT 'it''s $1', ?2",
		"SELECT price$ FROM t WHERE a = $1": "SELECT price$ FROM t WHERE a = ?1",
	} {
		if got := sqlitePlaceholders(in); got != want {
			t.Errorf("sqlitePlaceholders(%q) = %q, want %q", in, got, want)
		}
	}
}
