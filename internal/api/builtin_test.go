package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
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
	if code := e.do("GET", "/builtin-agents", admin, nil, &list); code != 200 || len(list.Agents) != 2 || list.Agents[0].Principal.ID != id {
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
