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

		bot := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "bot"}
		if err := s.CreatePrincipal(ctx, bot); err != nil {
			t.Fatal(err)
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
			}, "")
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
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		back, err := s.GetEvent(ctx, reply.ID)
		if err != nil || back.Causation != reply.Causation || back.Parts != nil {
			t.Fatalf("GetEvent = %+v, %v", back, err)
		}

		if _, err := s.AppendEvent(ctx, core.Event{RoomID: "nope", Type: core.EventMessage, SenderID: p.ID}, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("append to missing room: %v", err)
		}
	})
}

func TestIdempotencyKey(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		send := func(key, body string) core.Event {
			t.Helper()
			e, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, Parts: text(body)}, key)
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
		pending, err := s.PendingOutbox(ctx, 10)
		if err != nil || len(pending) != 2 {
			t.Fatalf("outbox after retry = %+v, %v", pending, err)
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
					_, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID}, key)
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

func TestOutbox(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		var ids []string
		for range 3 {
			e, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID}, "")
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, e.ID)
		}
		pending, err := s.PendingOutbox(ctx, 10)
		if err != nil || len(pending) != 3 || pending[0].EventID != ids[0] || pending[0].Seq != 1 || pending[0].RoomID != r.ID {
			t.Fatalf("PendingOutbox = %+v, %v", pending, err)
		}
		if err := s.AckOutbox(ctx, ids[0]); err != nil {
			t.Fatal(err)
		}
		pending, err = s.PendingOutbox(ctx, 10)
		if err != nil || len(pending) != 2 || pending[0].EventID != ids[1] {
			t.Fatalf("after ack = %+v, %v", pending, err)
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
