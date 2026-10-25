package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lum1n/say-v2/internal/core"
)

func TestBuiltinAgents(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ada, r := seed(t, s)
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
		if err != nil || got.APIKey != "secret" || got.MaxTokens != 500 || !got.Enabled || got.CreatedAt.IsZero() || !got.LastErrorAt.IsZero() || len(got.MCPServers) != 0 {
			t.Fatalf("GetBuiltinAgent = %+v, %v", got, err)
		}
		got.Enabled = false
		got.Model = "m2"
		servers := []MCPServer{{Name: "wx", URL: "https://wx.example/mcp", Authorization: "Bearer t"}, {Name: "kb", URL: "http://kb/mcp"}}
		got.MCPServers = servers
		got.InputPrice, got.OutputPrice, got.DailyCostBudget = 3_000_000, 15_000_000, 5_000_000
		if err := s.UpdateBuiltinAgent(ctx, got, "Bot 2"); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetPrincipal(ctx, p.ID); err != nil || got.DisplayName != "Bot 2" {
			t.Fatalf("renamed principal = %+v, %v", got, err)
		}
		if err := s.SetBuiltinAgentError(ctx, p.ID, "boom"); err != nil {
			t.Fatal(err)
		}
		all, err := s.BuiltinAgents(ctx)
		if err != nil || len(all) != 1 || all[0].Model != "m2" || all[0].Enabled || all[0].LastError != "boom" || all[0].LastErrorAt.IsZero() ||
			!reflect.DeepEqual(all[0].MCPServers, servers) || all[0].InputPrice != 3_000_000 || all[0].OutputPrice != 15_000_000 || all[0].DailyCostBudget != 5_000_000 {
			t.Fatalf("BuiltinAgents = %+v, %v", all, err)
		}
		if _, err := s.GetBuiltinAgent(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing agent: %v", err)
		}
		for _, name := range []string{"", "Ghost"} {
			if err := s.UpdateBuiltinAgent(ctx, BuiltinAgent{PrincipalID: "nope"}, name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("update missing agent (rename %q): %v", name, err)
			}
		}
		// Someone who isn't a built-in agent isn't renamed by one's update.
		if err := s.UpdateBuiltinAgent(ctx, BuiltinAgent{PrincipalID: ada.ID}, "Ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update a human: %v", err)
		}
		if got, err := s.GetPrincipal(ctx, ada.ID); err != nil || got.DisplayName != "Ada" {
			t.Fatalf("human after a failed update = %+v, %v", got, err)
		}

		// Usage adds up per UTC day.
		day := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
		for range 2 {
			if err := s.AddAgentUsage(ctx, p.ID, day, Usage{InputTokens: 10, OutputTokens: 3, Cost: 7}); err != nil {
				t.Fatal(err)
			}
		}
		if u, err := s.AgentUsage(ctx, p.ID, day); err != nil || u != (Usage{20, 6, 14}) || u.Total() != 26 {
			t.Fatalf("AgentUsage = %+v, %v", u, err)
		}
		if u, err := s.AgentUsage(ctx, p.ID, day.Add(2*time.Hour)); err != nil || u.Total() != 0 {
			t.Fatalf("next day's usage = %+v, %v", u, err)
		}

		// Cursors.
		if _, err := s.AgentCursor(ctx, p.ID, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unread room: %v", err)
		}
		for _, seq := range []int64{4, 9, 6} {
			if err := s.SetAgentCursor(ctx, p.ID, r.ID, seq); err != nil {
				t.Fatal(err)
			}
		}
		if seq, err := s.AgentCursor(ctx, p.ID, r.ID); err != nil || seq != 9 {
			t.Fatalf("AgentCursor = %d, %v (cursors only move forward)", seq, err)
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

func TestConnectorSessions(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, _ := seed(t, s)
		now := time.Now().Truncate(time.Millisecond)
		a := ConnectorSession{ID: core.NewID(), PrincipalID: p.ID, Name: "echo", Version: "1.0",
			Capabilities: []string{"streaming"}, ConnectedAt: now.Add(-time.Minute), SeenAt: now.Add(-time.Minute)}
		b := ConnectorSession{ID: core.NewID(), PrincipalID: p.ID, Name: "other", ConnectedAt: now, SeenAt: now}
		stale := ConnectorSession{ID: core.NewID(), PrincipalID: p.ID, Name: "dead",
			ConnectedAt: now.Add(-3 * time.Hour), SeenAt: now.Add(-2 * time.Hour)}
		for _, cs := range []ConnectorSession{stale, a, b} {
			if err := s.SaveConnectorSession(ctx, cs); err != nil {
				t.Fatal(err)
			}
		}
		// Renewing a keeps its other fields.
		renewed := a
		renewed.Name, renewed.SeenAt = "ignored", now
		if err := s.SaveConnectorSession(ctx, renewed); err != nil {
			t.Fatal(err)
		}
		got, err := s.ConnectorSessions(ctx, now.Add(-30*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		a.SeenAt = now
		b.Capabilities = []string{}
		if want := []ConnectorSession{a, b}; !reflect.DeepEqual(got, want) {
			t.Fatalf("live sessions:\n got %+v\nwant %+v", got, want)
		}
		if all, err := s.ConnectorSessions(ctx, time.Time{}); err != nil || len(all) != 2 {
			t.Fatalf("stale session not pruned: %+v, %v", all, err)
		}
		if err := s.DeleteConnectorSession(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ConnectorSessions(ctx, time.Time{}); err != nil || len(got) != 1 || got[0].ID != a.ID {
			t.Fatalf("after delete: %+v, %v", got, err)
		}
	})
}
