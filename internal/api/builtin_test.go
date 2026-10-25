package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

func (e *env) admin(name string) (core.Principal, string) {
	e.t.Helper()
	ctx := context.Background()
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: name, Admin: true}
	if err := e.store.CreatePrincipal(ctx, p); err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.store.CreateToken(ctx, p.ID, "test", time.Time{})
	if err != nil {
		e.t.Fatal(err)
	}
	return p, tok
}

func TestBuiltinAgentsAPI(t *testing.T) {
	e := newEnv(t)
	_, admin := e.admin("Root")
	_, bob := e.user("Bob", core.PrincipalHuman)

	var created wireBuiltinAgent
	code := e.do("POST", "/builtin-agents", admin, map[string]any{
		"display_name": "Helper", "provider": "anthropic", "api_key": "sk-secret", "system_prompt": "Be helpful.",
	}, &created)
	if code != 201 {
		t.Fatalf("create = %d", code)
	}
	if created.Principal.Kind != core.PrincipalAgent || created.Principal.DisplayName != "Helper" ||
		created.Model != "claude-opus-5" || created.MaxTokens != defaultMaxTokens || !created.Enabled || !created.HasAPIKey {
		t.Fatalf("created = %+v", created)
	}
	id := created.Principal.ID

	// Changing one field keeps the others, the key included.
	var updated wireBuiltinAgent
	if code := e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"enabled": false, "daily_token_budget": 5000}, &updated); code != 200 {
		t.Fatalf("update = %d", code)
	}
	if updated.Enabled || updated.DailyTokenBudget != 5000 || !updated.HasAPIKey || updated.SystemPrompt != "Be helpful." {
		t.Fatalf("updated = %+v", updated)
	}
	stored, _ := e.store.GetBuiltinAgent(context.Background(), id)
	if stored.APIKey != "sk-secret" || !stored.UpdatedAt.After(stored.CreatedAt) {
		t.Fatalf("stored = %+v", stored)
	}

	// An OpenAI-compatible endpoint without a key.
	var local wireBuiltinAgent
	code = e.do("POST", "/builtin-agents", admin, map[string]any{
		"display_name": "Local", "provider": "openai", "base_url": "http://localhost:11434/v1", "model": "llama3.2",
	}, &local)
	if code != 201 || local.HasAPIKey || local.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("create local = %d %+v", code, local)
	}

	var list struct{ Agents []wireBuiltinAgent }
	// Both were created in the same millisecond, so their order is the
	// ids' order, which UUIDv7 leaves random within a millisecond.
	if code := e.do("GET", "/builtin-agents", admin, nil, &list); code != 200 || len(list.Agents) != 2 ||
		!slices.ContainsFunc(list.Agents, func(a wireBuiltinAgent) bool { return a.Principal.ID == id }) {
		t.Fatalf("list = %d %+v", code, list)
	}

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"provider": "anthropic"}},
		{"bad provider", map[string]any{"display_name": "X", "provider": "gemini"}},
		{"bad url", map[string]any{"display_name": "X", "provider": "openai", "base_url": "file:///etc/passwd"}},
		{"bad max tokens", map[string]any{"display_name": "X", "provider": "openai", "max_tokens": -1}},
		{"bad budget", map[string]any{"display_name": "X", "provider": "openai", "daily_token_budget": -1}},
		{"bad price", map[string]any{"display_name": "X", "provider": "openai", "input_price": -1}},
		{"huge price", map[string]any{"display_name": "X", "provider": "openai", "output_price": 1e9}},
		{"cost budget without prices", map[string]any{"display_name": "X", "provider": "openai", "daily_cost_budget": 5}},
	} {
		var ae apiError
		if code := e.do("POST", "/builtin-agents", admin, c.body, &ae); code != 422 {
			t.Errorf("%s: %d %+v", c.name, code, ae)
		}
	}
	var ae apiError
	if code := e.do("PATCH", "/builtin-agents/nope", admin, map[string]any{}, &ae); code != 404 {
		t.Errorf("update missing = %d", code)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/builtin-agents"},
		{"POST", "/builtin-agents"},
		{"PATCH", "/builtin-agents/" + id},
	} {
		if code := e.do(c.method, c.path, bob, map[string]any{"display_name": "X", "provider": "openai"}, &ae); code != 403 {
			t.Errorf("%s %s as non-admin = %d", c.method, c.path, code)
		}
	}
}

func TestBuiltinAgentCostAndRename(t *testing.T) {
	e := newEnv(t)
	_, admin := e.admin("Root")
	var a wireBuiltinAgent
	code := e.do("POST", "/builtin-agents", admin, map[string]any{
		"display_name": "Helper", "provider": "anthropic",
		"input_price": 3, "output_price": 15, "daily_cost_budget": 2.5,
	}, &a)
	if code != 201 || a.InputPrice != 3 || a.OutputPrice != 15 || a.DailyCostBudget != 2.5 || a.CostToday != 0 {
		t.Fatalf("create = %d %+v", code, a)
	}
	id := a.Principal.ID
	stored, _ := e.store.GetBuiltinAgent(context.Background(), id)
	if stored.InputPrice != 3_000_000 || stored.OutputPrice != 15_000_000 || stored.DailyCostBudget != 2_500_000 {
		t.Fatalf("stored = %+v", stored)
	}
	if err := e.store.AddAgentUsage(context.Background(), id, time.Now(), store.Usage{InputTokens: 1000, OutputTokens: 100, Cost: stored.Cost(1000, 100)}); err != nil {
		t.Fatal(err)
	}

	// A rename keeps the settings and moves updated_at, so the runner
	// restarts the agent under its new name.
	code = e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"display_name": "  Sage "}, &a)
	if code != 200 || a.Principal.DisplayName != "Sage" || a.InputPrice != 3 || a.CostToday != 0.0045 || a.TokensToday != 1100 {
		t.Fatalf("rename = %d %+v", code, a)
	}
	if p, _ := e.store.GetPrincipal(context.Background(), id); p.DisplayName != "Sage" {
		t.Fatalf("principal = %+v", p)
	}
	if after, _ := e.store.GetBuiltinAgent(context.Background(), id); !after.UpdatedAt.After(stored.UpdatedAt) {
		t.Fatalf("updated_at did not move: %v → %v", stored.UpdatedAt, after.UpdatedAt)
	}
	var ae apiError
	if code := e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"display_name": " "}, &ae); code != 422 {
		t.Errorf("empty name = %d", code)
	}
	// Dropping the prices while a cost budget is set would lift the budget.
	if code := e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"input_price": 0, "output_price": 0}, &ae); code != 422 {
		t.Errorf("budget without prices = %d", code)
	}
	if code := e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"input_price": 0, "output_price": 0, "daily_cost_budget": 0}, &a); code != 200 || a.DailyCostBudget != 0 {
		t.Errorf("clear prices and budget = %d %+v", code, a)
	}
}

func TestBuiltinAgentMCPServers(t *testing.T) {
	e := newEnv(t)
	_, admin := e.admin("Root")
	var a wireBuiltinAgent
	code := e.do("POST", "/builtin-agents", admin, map[string]any{
		"display_name": "Helper", "provider": "anthropic",
		"mcp_servers": []map[string]any{
			{"name": "wx", "url": "https://wx.example/mcp", "authorization": "Bearer wx"},
			{"name": "kb", "url": "http://kb.local/mcp"},
		},
	}, &a)
	if code != 201 || len(a.MCPServers) != 2 || !a.MCPServers[0].HasAuthorization || a.MCPServers[1].HasAuthorization {
		t.Fatalf("create = %d %+v", code, a.MCPServers)
	}
	id := a.Principal.ID
	auth := func() []string {
		stored, _ := e.store.GetBuiltinAgent(context.Background(), id)
		var out []string
		for _, m := range stored.MCPServers {
			out = append(out, m.Name+"="+m.Authorization)
		}
		return out
	}

	// Left out, the header stays for the same name and URL; a new URL
	// drops it, and an empty one clears it.
	code = e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"mcp_servers": []map[string]any{
		{"name": "kb", "url": "http://kb.local/mcp", "authorization": "Bearer kb"},
		{"name": "wx", "url": "https://wx.example/mcp"},
	}}, &a)
	if code != 200 || !slices.Equal(auth(), []string{"kb=Bearer kb", "wx=Bearer wx"}) {
		t.Fatalf("reorder = %d %v", code, auth())
	}
	e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"mcp_servers": []map[string]any{
		{"name": "kb", "url": "http://kb.local/mcp", "authorization": ""},
		{"name": "wx", "url": "https://evil.example/mcp"},
	}}, &a)
	if !slices.Equal(auth(), []string{"kb=", "wx="}) {
		t.Fatalf("moved = %v", auth())
	}
	// Other changes keep the list.
	e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"enabled": false}, &a)
	if len(a.MCPServers) != 2 {
		t.Fatalf("after enabled = %+v", a.MCPServers)
	}
	e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"mcp_servers": []any{}}, &a)
	if a.MCPServers == nil || len(a.MCPServers) != 0 {
		t.Fatalf("cleared = %+v", a.MCPServers)
	}

	server := func(name, url string) map[string]any { return map[string]any{"name": name, "url": url} }
	for _, c := range []struct {
		name    string
		servers []map[string]any
	}{
		{"bad name", []map[string]any{server("a b", "http://x")}},
		{"empty name", []map[string]any{server("", "http://x")}},
		{"same name", []map[string]any{server("a", "http://x"), server("a", "http://y")}},
		{"bad url", []map[string]any{server("a", "file:///etc/passwd")}},
		{"too many", slices.Repeat([]map[string]any{server("a", "http://x")}, maxMCPServers+1)},
	} {
		var ae apiError
		if code := e.do("PATCH", "/builtin-agents/"+id, admin, map[string]any{"mcp_servers": c.servers}, &ae); code != 422 {
			t.Errorf("%s: %d %+v", c.name, code, ae)
		}
	}
}

func TestResponderSwitch(t *testing.T) {
	e := newEnv(t)
	_, ada := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	bot, botTok := e.user("Bot", core.PrincipalAgent)
	r := e.room(ada, "lab")
	e.join(ada, r.ID, bob.ID, core.RoleMember)
	e.join(ada, r.ID, bot.ID, core.RoleMember)

	var m wireMember
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/"+bot.ID, ada, map[string]any{"responder": true}, &m); code != 200 {
		t.Fatalf("set responder = %d", code)
	}
	if !slices.Equal(m.Scopes, []string{core.ScopeResponder}) {
		t.Fatalf("scopes = %v", m.Scopes)
	}
	var members struct{ Members []wireMember }
	e.do("GET", "/rooms/"+r.ID+"/members", ada, nil, &members)
	if i := slices.IndexFunc(members.Members, func(m wireMember) bool { return m.PrincipalID == bot.ID }); !slices.Equal(members.Members[i].Scopes, []string{core.ScopeResponder}) {
		t.Fatalf("members = %+v", members)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/"+bot.ID, ada, map[string]any{"responder": false}, &m); code != 200 || len(m.Scopes) != 0 {
		t.Fatalf("unset responder = %d %v", code, m.Scopes)
	}

	var ae apiError
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/"+bob.ID, ada, map[string]any{"responder": true}, &ae); code != 422 {
		t.Errorf("a person as responder = %d", code)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/"+bot.ID, bobTok, map[string]any{"responder": true}, &ae); code != 403 {
		t.Errorf("a plain member = %d", code)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/"+bot.ID, botTok, map[string]any{"responder": true}, &ae); code != 403 {
		t.Errorf("the agent itself = %d", code)
	}
	if code := e.do("PATCH", "/rooms/"+r.ID+"/members/nope", ada, map[string]any{"responder": true}, &ae); code != 404 {
		t.Errorf("a non-member = %d", code)
	}
}
