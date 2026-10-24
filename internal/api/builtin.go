package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/bus"
	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/llm"
	"github.com/vegard/say-v2/internal/store"
)

// Built-in agents are agent principals the server runs itself
// (docs/adr/0009-built-in-agents.md). Instance admins configure them here;
// internal/agents runs them.

const (
	defaultMaxTokens  = 1024
	maxMaxTokens      = 64000
	maxSystemPrompt   = 20000
	maxAgentFieldSize = 2000
	maxMCPServers     = 8
)

// SendParts is how built-in agents write: the same send path as REST, the
// WebSocket and MCP, so every room limit applies. A streaming message is
// then changed with StreamParts and ended with EndParts. created is false
// when idempotencyKey returned an earlier message.
func (s *Server) SendParts(ctx context.Context, me core.Principal, roomID string, parts []core.Part, causedBy, idempotencyKey string, streaming bool) (e core.Event, created bool, err error) {
	msg := sendMessage{Parts: toWireParts(parts), CausedBy: causedBy, Stream: core.StreamFinal}
	if streaming {
		msg.Stream = core.StreamStreaming
	}
	return s.send(ctx, me, roomID, msg, idempotencyKey)
}

// StreamParts replaces the parts of me's streaming message eventID.
func (s *Server) StreamParts(ctx context.Context, me core.Principal, roomID, eventID string, parts []core.Part) error {
	wire := toWireParts(parts)
	_, err := s.edit(ctx, me, roomID, eventID, editMessage{Parts: &wire, Stream: core.StreamStreaming})
	return err
}

// EndParts ends the stream of me's message eventID with parts, or with
// what it streamed when parts is nil.
func (s *Server) EndParts(ctx context.Context, me core.Principal, roomID, eventID string, parts []core.Part) error {
	body := editMessage{Stream: core.StreamFinal}
	if parts != nil {
		wire := toWireParts(parts)
		body.Parts = &wire
	}
	_, err := s.edit(ctx, me, roomID, eventID, body)
	return err
}

// SendText, StreamText and EndText are SendParts, StreamParts and EndParts
// for a message of one text part, or none for empty text.
func (s *Server) SendText(ctx context.Context, me core.Principal, roomID, text, causedBy, idempotencyKey string, streaming bool) (core.Event, bool, error) {
	return s.SendParts(ctx, me, roomID, textParts(text), causedBy, idempotencyKey, streaming)
}

func (s *Server) StreamText(ctx context.Context, me core.Principal, roomID, eventID, text string) error {
	return s.StreamParts(ctx, me, roomID, eventID, textParts(text))
}

func (s *Server) EndText(ctx context.Context, me core.Principal, roomID, eventID, text string) error {
	var parts []core.Part
	if text != "" {
		parts = textParts(text)
	}
	return s.EndParts(ctx, me, roomID, eventID, parts)
}

func textParts(text string) []core.Part {
	if text == "" {
		return []core.Part{}
	}
	return []core.Part{core.TextPart(text)}
}

func toWireParts(parts []core.Part) []wirePart {
	out := make([]wirePart, len(parts))
	for i, p := range parts {
		out[i] = wirePart{Kind: p.Kind, Data: p.Data}
	}
	return out
}

type wireBuiltinAgent struct {
	Principal        wirePrincipal   `json:"principal"`
	Provider         string          `json:"provider"`
	BaseURL          string          `json:"base_url"`
	Model            string          `json:"model"`
	HasAPIKey        bool            `json:"has_api_key"`
	SystemPrompt     string          `json:"system_prompt"`
	MaxTokens        int             `json:"max_tokens"`
	DailyTokenBudget int64           `json:"daily_token_budget"`
	TokensToday      int64           `json:"tokens_today"`
	Enabled          bool            `json:"enabled"`
	MCPServers       []wireMCPServer `json:"mcp_servers"`
	LastError        string          `json:"last_error,omitempty"`
	LastErrorAt      *time.Time      `json:"last_error_at,omitempty"`
}

// wireMCPServer is an agent's MCP server as admins see it: the
// Authorization header is write-only.
type wireMCPServer struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	HasAuthorization bool   `json:"has_authorization"`
}

func (s *Server) toBuiltinAgent(ctx context.Context, a store.BuiltinAgent) (wireBuiltinAgent, error) {
	p, err := s.store.GetPrincipal(ctx, a.PrincipalID)
	if err != nil {
		return wireBuiltinAgent{}, err
	}
	used, err := s.store.AgentUsage(ctx, a.PrincipalID, time.Now())
	if err != nil {
		return wireBuiltinAgent{}, err
	}
	out := wireBuiltinAgent{
		Principal:        toPrincipal(p),
		Provider:         a.Provider,
		BaseURL:          a.BaseURL,
		Model:            a.Model,
		HasAPIKey:        a.APIKey != "",
		SystemPrompt:     a.SystemPrompt,
		MaxTokens:        a.MaxTokens,
		DailyTokenBudget: a.DailyTokenBudget,
		TokensToday:      used.Total(),
		Enabled:          a.Enabled,
		MCPServers:       []wireMCPServer{},
		LastError:        a.LastError,
	}
	for _, m := range a.MCPServers {
		out.MCPServers = append(out.MCPServers, wireMCPServer{Name: m.Name, URL: m.URL, HasAuthorization: m.Authorization != ""})
	}
	if !a.LastErrorAt.IsZero() {
		at := a.LastErrorAt.UTC()
		out.LastErrorAt = &at
	}
	return out, nil
}

func requireAdmin(me core.Principal) error {
	if !me.Admin {
		return errorf(http.StatusForbidden, "forbidden", "only instance admins manage built-in agents")
	}
	return nil
}

func (s *Server) listBuiltinAgents(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if err := requireAdmin(me); err != nil {
		writeError(w, err)
		return
	}
	all, err := s.store.BuiltinAgents(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]wireBuiltinAgent, len(all))
	for i, a := range all {
		if out[i], err = s.toBuiltinAgent(r.Context(), a); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// builtinAgentPatch is the body of POST and PATCH /builtin-agents. On PATCH
// absent fields stay as they are.
type builtinAgentPatch struct {
	DisplayName      *string `json:"display_name"` // POST only
	Provider         *string `json:"provider"`
	BaseURL          *string `json:"base_url"`
	Model            *string `json:"model"`
	APIKey           *string `json:"api_key"`
	SystemPrompt     *string `json:"system_prompt"`
	MaxTokens        *int    `json:"max_tokens"`
	DailyTokenBudget *int64  `json:"daily_token_budget"`
	Enabled          *bool   `json:"enabled"`
	// MCPServers replaces the agent's list. A server that leaves out
	// authorization keeps the one it had, if its name and URL are the same.
	MCPServers *[]mcpServerPatch `json:"mcp_servers"`
}

type mcpServerPatch struct {
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	Authorization *string `json:"authorization"`
}

var mcpServerName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// applyMCPServers checks a new list of servers and carries over the
// Authorization headers it leaves out.
func applyMCPServers(old []store.MCPServer, patch []mcpServerPatch) ([]store.MCPServer, error) {
	if len(patch) > maxMCPServers {
		return nil, errorf(http.StatusUnprocessableEntity, "invalid", "an agent has at most %d MCP servers", maxMCPServers)
	}
	out := []store.MCPServer{}
	for _, p := range patch {
		m := store.MCPServer{Name: strings.TrimSpace(p.Name), URL: strings.TrimSpace(p.URL)}
		if !mcpServerName.MatchString(m.Name) {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "an MCP server name is 1 to 32 letters, digits, _ or -")
		}
		if slices.ContainsFunc(out, func(o store.MCPServer) bool { return o.Name == m.Name }) {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "two MCP servers are called %q", m.Name)
		}
		if len(m.URL) > maxAgentFieldSize || !httpURL(m.URL) {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "MCP server %q needs an http or https URL", m.Name)
		}
		if p.Authorization != nil {
			if len(*p.Authorization) > maxAgentFieldSize {
				return nil, errorf(http.StatusUnprocessableEntity, "invalid", "authorization is longer than %d bytes", maxAgentFieldSize)
			}
			m.Authorization = strings.TrimSpace(*p.Authorization)
		} else if i := slices.IndexFunc(old, func(o store.MCPServer) bool { return o.Name == m.Name && o.URL == m.URL }); i >= 0 {
			// Only to the same URL, so a secret is never sent somewhere new.
			m.Authorization = old[i].Authorization
		}
		out = append(out, m)
	}
	return out, nil
}

func (p builtinAgentPatch) apply(a store.BuiltinAgent) (store.BuiltinAgent, error) {
	if p.Provider != nil {
		a.Provider = *p.Provider
	}
	if _, ok := llm.DefaultModel[a.Provider]; !ok {
		return a, errorf(http.StatusUnprocessableEntity, "invalid", "provider must be anthropic or openai")
	}
	set := func(dst *string, v *string, name string, limit int) error {
		if v == nil {
			return nil
		}
		if len(*v) > limit {
			return errorf(http.StatusUnprocessableEntity, "invalid", "%s is longer than %d bytes", name, limit)
		}
		*dst = strings.TrimSpace(*v)
		return nil
	}
	for _, f := range []struct {
		dst   *string
		v     *string
		name  string
		limit int
	}{
		{&a.BaseURL, p.BaseURL, "base_url", maxAgentFieldSize},
		{&a.Model, p.Model, "model", maxAgentFieldSize},
		{&a.APIKey, p.APIKey, "api_key", maxAgentFieldSize},
		{&a.SystemPrompt, p.SystemPrompt, "system_prompt", maxSystemPrompt},
	} {
		if err := set(f.dst, f.v, f.name, f.limit); err != nil {
			return a, err
		}
	}
	if a.Model == "" {
		a.Model = llm.DefaultModel[a.Provider]
	}
	if a.BaseURL != "" && !httpURL(a.BaseURL) {
		return a, errorf(http.StatusUnprocessableEntity, "invalid", "base_url must be an http or https URL")
	}
	if p.MaxTokens != nil {
		a.MaxTokens = *p.MaxTokens
	}
	if a.MaxTokens == 0 {
		a.MaxTokens = defaultMaxTokens
	}
	if a.MaxTokens < 1 || a.MaxTokens > maxMaxTokens {
		return a, errorf(http.StatusUnprocessableEntity, "invalid", "max_tokens must be from 1 to %d", maxMaxTokens)
	}
	if p.DailyTokenBudget != nil {
		if *p.DailyTokenBudget < 0 {
			return a, errorf(http.StatusUnprocessableEntity, "invalid", "daily_token_budget must be 0 (no limit) or more")
		}
		a.DailyTokenBudget = *p.DailyTokenBudget
	}
	if p.Enabled != nil {
		a.Enabled = *p.Enabled
	}
	if p.MCPServers != nil {
		servers, err := applyMCPServers(a.MCPServers, *p.MCPServers)
		if err != nil {
			return a, err
		}
		a.MCPServers = servers
	}
	return a, nil
}

func (s *Server) createBuiltinAgent(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if err := requireAdmin(me); err != nil {
		writeError(w, err)
		return
	}
	var body builtinAgentPatch
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	var name string
	if body.DisplayName != nil {
		name = *body.DisplayName
	}
	p, err := newPrincipal(core.PrincipalAgent, name, "")
	if err != nil {
		writeError(w, err)
		return
	}
	a, err := body.apply(store.BuiltinAgent{Enabled: true, CreatedAt: p.CreatedAt, UpdatedAt: p.CreatedAt})
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CreateBuiltinAgent(r.Context(), p, a); err != nil {
		writeError(w, err)
		return
	}
	a.PrincipalID = p.ID
	s.publish(r.Context(), bus.BuiltinAgentsSubject)
	out, err := s.toBuiltinAgent(r.Context(), a)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateBuiltinAgent(w http.ResponseWriter, r *http.Request, me core.Principal) {
	if err := requireAdmin(me); err != nil {
		writeError(w, err)
		return
	}
	var body builtinAgentPatch
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.DisplayName != nil {
		writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "display_name cannot be changed yet"))
		return
	}
	a, err := s.store.GetBuiltinAgent(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		err = errorf(http.StatusNotFound, "not_found", "no such built-in agent")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if a, err = body.apply(a); err != nil {
		writeError(w, err)
		return
	}
	// Runners restart an agent whose updated_at changed, so it must move
	// even when two updates land in the same millisecond.
	now := time.Now().Truncate(time.Millisecond)
	if !now.After(a.UpdatedAt) {
		now = a.UpdatedAt.Add(time.Millisecond)
	}
	a.UpdatedAt = now
	if err := s.store.UpdateBuiltinAgent(r.Context(), a); err != nil {
		writeError(w, err)
		return
	}
	s.publish(r.Context(), bus.BuiltinAgentsSubject)
	out, err := s.toBuiltinAgent(r.Context(), a)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateMember makes an agent a designated responder in a room, or stops
// it being one. Like the agent policy, only people who own or run the room
// may.
func (s *Server) updateMember(w http.ResponseWriter, r *http.Request, me core.Principal) {
	roomID, principalID := r.PathValue("id"), r.PathValue("principal_id")
	mine, err := s.member(r.Context(), me, roomID)
	if err != nil {
		writeError(w, err)
		return
	}
	if me.Kind != core.PrincipalHuman || mine.Role != core.RoleOwner && mine.Role != core.RoleAdmin && !me.Admin {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "only people who own or run the room change members"))
		return
	}
	var body struct {
		Responder *bool `json:"responder"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, err)
		return
	}
	m, err := s.store.GetMember(r.Context(), roomID, principalID)
	if errors.Is(err, store.ErrNotFound) {
		err = errorf(http.StatusNotFound, "not_found", "not a member of this room")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if body.Responder != nil {
		p, err := s.store.GetPrincipal(r.Context(), principalID)
		if err != nil {
			writeError(w, err)
			return
		}
		if *body.Responder && p.Kind != core.PrincipalAgent {
			writeError(w, errorf(http.StatusUnprocessableEntity, "invalid", "only agents can be responders"))
			return
		}
		m.Scopes = slices.DeleteFunc(slices.Clone(m.Scopes), func(s string) bool { return s == core.ScopeResponder })
		if *body.Responder {
			m.Scopes = append(m.Scopes, core.ScopeResponder)
		}
		if err := s.store.SetMemberScopes(r.Context(), roomID, principalID, m.Scopes); err != nil {
			writeError(w, err)
			return
		}
		s.notifyRooms(r.Context(), principalID)
	}
	writeJSON(w, http.StatusOK, toMember(m))
}
