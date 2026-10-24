package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

func TestBuiltinAgents(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, r := seed(t, s)
		p := core.Principal{ID: core.NewID(), Kind: core.PrincipalAgent, DisplayName: "Bot"}
		a := BuiltinAgent{
			Provider: "anthropic", Model: "m", APIKey: "secret",
			SystemPrompt: "be kind", MaxTokens: 500, DailyTokenBudget: 1000, Enabled: true,
		}
		if err := s.CreateBuiltinAgent(ctx, p, a); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetPrincipal(ctx, p.ID); err != nil || got.Kind != core.PrincipalAgent {
			t.Fatalf("GetPrincipal = %+v, %v", got, err)
		}
		human := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "No"}
		if err := s.CreateBuiltinAgent(ctx, human, a); err == nil {
			t.Fatal("a human became a built-in agent")
		}
		got, err := s.GetBuiltinAgent(ctx, p.ID)
		if err != nil || got.APIKey != "secret" || got.MaxTokens != 500 || !got.Enabled || got.CreatedAt.IsZero() || !got.LastErrorAt.IsZero() {
			t.Fatalf("GetBuiltinAgent = %+v, %v", got, err)
		}
		got.Enabled = false
		got.Model = "m2"
		if err := s.UpdateBuiltinAgent(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBuiltinAgentError(ctx, p.ID, "boom"); err != nil {
			t.Fatal(err)
		}
		all, err := s.BuiltinAgents(ctx)
		if err != nil || len(all) != 1 || all[0].Model != "m2" || all[0].Enabled || all[0].LastError != "boom" || all[0].LastErrorAt.IsZero() {
			t.Fatalf("BuiltinAgents = %+v, %v", all, err)
		}
		if _, err := s.GetBuiltinAgent(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing agent: %v", err)
		}
		if err := s.UpdateBuiltinAgent(ctx, BuiltinAgent{PrincipalID: "nope"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update missing agent: %v", err)
		}

		// Usage adds up per UTC day.
		day := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
		for range 2 {
			if err := s.AddAgentUsage(ctx, p.ID, day, Usage{InputTokens: 10, OutputTokens: 3}); err != nil {
				t.Fatal(err)
			}
		}
		if u, err := s.AgentUsage(ctx, p.ID, day); err != nil || u != (Usage{20, 6}) || u.Total() != 26 {
			t.Fatalf("AgentUsage = %+v, %v", u, err)
		}
		if u, err := s.AgentUsage(ctx, p.ID, day.Add(2*time.Hour)); err != nil || u.Total() != 0 {
			t.Fatalf("next day's usage = %+v, %v", u, err)
		}

		// Cursors.
		if _, err := s.AgentCursor(ctx, p.ID, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unread room: %v", err)
		}
		for _, seq := range []int64{4, 9} {
			if err := s.SetAgentCursor(ctx, p.ID, r.ID, seq); err != nil {
				t.Fatal(err)
			}
		}
		if seq, err := s.AgentCursor(ctx, p.ID, r.ID); err != nil || seq != 9 {
			t.Fatalf("AgentCursor = %d, %v", seq, err)
		}

		// Responder scope.
		if err := s.AddMember(ctx, core.Membership{RoomID: r.ID, PrincipalID: p.ID, Role: core.RoleMember}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMemberScopes(ctx, r.ID, p.ID, []string{"responder"}); err != nil {
			t.Fatal(err)
		}
		if m, err := s.GetMember(ctx, r.ID, p.ID); err != nil || !slices.Equal(m.Scopes, []string{"responder"}) {
			t.Fatalf("GetMember = %+v, %v", m, err)
		}
		if err := s.SetMemberScopes(ctx, r.ID, "nope", nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("scopes of a non-member: %v", err)
		}
	})
}

func TestLeases(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		gen, ok, err := s.AcquireLease(ctx, "agent:x", "node-a", time.Minute)
		if err != nil || !ok || gen != 1 {
			t.Fatalf("first acquire = %d, %v, %v", gen, ok, err)
		}
		if _, ok, err := s.AcquireLease(ctx, "agent:x", "node-b", time.Minute); err != nil || ok {
			t.Fatalf("b took a's live lease: %v, %v", ok, err)
		}
		// Renewing keeps the generation.
		if gen, ok, err := s.AcquireLease(ctx, "agent:x", "node-a", 50*time.Millisecond); err != nil || !ok || gen != 1 {
			t.Fatalf("renew = %d, %v, %v", gen, ok, err)
		}
		time.Sleep(100 * time.Millisecond)
		// An expired lease changes hands and the generation goes up.
		if gen, ok, err := s.AcquireLease(ctx, "agent:x", "node-b", time.Minute); err != nil || !ok || gen != 2 {
			t.Fatalf("takeover = %d, %v, %v", gen, ok, err)
		}
		// Releasing someone else's lease does nothing; releasing your own
		// frees it at once.
		if err := s.ReleaseLease(ctx, "agent:x", "node-a"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.AcquireLease(ctx, "agent:x", "node-a", time.Minute); ok {
			t.Fatal("a's release freed b's lease")
		}
		if err := s.ReleaseLease(ctx, "agent:x", "node-b"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.AcquireLease(ctx, "agent:x", "node-a", time.Minute); !ok {
			t.Fatal("a released lease is still held")
		}

		// Nodes racing for a free lease: exactly one wins.
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, ok, err := s.AcquireLease(ctx, "agent:race", string(rune('a'+i)), time.Minute)
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if winners != 1 {
			t.Fatalf("%d nodes hold the lease", winners)
		}
	})
}
