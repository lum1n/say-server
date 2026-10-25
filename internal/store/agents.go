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

// BuiltinAgent is how the server runs an agent principal itself, by
// calling a model provider (docs/adr/0009-built-in-agents.md).
type BuiltinAgent struct {
	PrincipalID string
	Provider    string
	BaseURL     string
	Model       string
	// APIKey is never shown again once set. Empty uses the server's
	// environment.
	APIKey       string
	SystemPrompt string
	MaxTokens    int
	// DailyTokenBudget caps input plus output tokens per UTC day; zero is
	// no cap.
	DailyTokenBudget int64
	// InputPrice and OutputPrice are what a million tokens cost, in
	// micro-dollars. DailyCostBudget caps what the agent spends per UTC day
	// at those prices; zero is no cap.
	InputPrice, OutputPrice int64
	DailyCostBudget         int64
	Enabled                 bool
	// MCPServers are the servers whose tools the agent may call.
	MCPServers []MCPServer
	// LastError is the latest failure to reply, for the admin to see.
	LastError   string
	LastErrorAt time.Time
	UpdatedAt   time.Time
	CreatedAt   time.Time
}

// MCPServer is an MCP server a built-in agent uses over streamable HTTP.
// Name prefixes its tools' names. Authorization, sent as the
// Authorization header, is never shown again once set.
type MCPServer struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	Authorization string `json:"authorization,omitempty"`
}

// Usage is the tokens an agent used on one day, and what they cost in
// micro-dollars.
type Usage struct {
	InputTokens, OutputTokens int64
	Cost                      int64
}

func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Cost is what input and output tokens cost at a's prices, in
// micro-dollars, rounded up so many small calls don't round to nothing.
func (a BuiltinAgent) Cost(input, output int64) int64 {
	n := input*a.InputPrice + output*a.OutputPrice
	return (n + 999_999) / 1_000_000
}

func (s *sqlStore) SetMemberScopes(ctx context.Context, roomID, principalID string, scopes []string) error {
	b, err := json.Marshal(nonNil(scopes))
	if err != nil {
		return err
	}
	n, err := s.q().SetMemberScopes(ctx, db.SetMemberScopesParams{RoomID: roomID, PrincipalID: principalID, Scopes: string(b)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) CreateBuiltinAgent(ctx context.Context, p core.Principal, a BuiltinAgent) error {
	if p.Kind != core.PrincipalAgent {
		return fmt.Errorf("store: a built-in agent must be an agent principal, not %s", p.Kind)
	}
	a.PrincipalID = p.ID
	return s.tx(ctx, func(q *db.Queries) error {
		if err := createPrincipal(ctx, q, p); err != nil {
			return err
		}
		return createBuiltinAgent(ctx, q, a)
	})
}

func createBuiltinAgent(ctx context.Context, q *db.Queries, a BuiltinAgent) error {
	servers, err := json.Marshal(nonNilServers(a.MCPServers))
	if err != nil {
		return err
	}
	return q.CreateBuiltinAgent(ctx, db.CreateBuiltinAgentParams{
		PrincipalID:      a.PrincipalID,
		Provider:         a.Provider,
		BaseUrl:          a.BaseURL,
		Model:            a.Model,
		ApiKey:           a.APIKey,
		SystemPrompt:     a.SystemPrompt,
		MaxTokens:        int32(a.MaxTokens),
		DailyTokenBudget: a.DailyTokenBudget,
		Enabled:          flag(a.Enabled),
		UpdatedAt:        millis(a.UpdatedAt),
		CreatedAt:        millis(a.CreatedAt),
		McpServers:       string(servers),
		InputPrice:       a.InputPrice,
		OutputPrice:      a.OutputPrice,
		DailyCostBudget:  a.DailyCostBudget,
	})
}

func nonNilServers(s []MCPServer) []MCPServer {
	if s == nil {
		return []MCPServer{}
	}
	return s
}

func (s *sqlStore) UpdateBuiltinAgent(ctx context.Context, a BuiltinAgent, displayName string) error {
	servers, err := json.Marshal(nonNilServers(a.MCPServers))
	if err != nil {
		return err
	}
	return s.tx(ctx, func(q *db.Queries) error {
		if displayName != "" {
			n, err := q.RenamePrincipal(ctx, db.RenamePrincipalParams{ID: a.PrincipalID, DisplayName: displayName})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrNotFound
			}
		}
		return updateBuiltinAgent(ctx, q, a, string(servers))
	})
}

func updateBuiltinAgent(ctx context.Context, q *db.Queries, a BuiltinAgent, servers string) error {
	n, err := q.UpdateBuiltinAgent(ctx, db.UpdateBuiltinAgentParams{
		PrincipalID:      a.PrincipalID,
		Provider:         a.Provider,
		BaseUrl:          a.BaseURL,
		Model:            a.Model,
		ApiKey:           a.APIKey,
		SystemPrompt:     a.SystemPrompt,
		MaxTokens:        int32(a.MaxTokens),
		DailyTokenBudget: a.DailyTokenBudget,
		Enabled:          flag(a.Enabled),
		UpdatedAt:        millis(a.UpdatedAt),
		McpServers:       servers,
		InputPrice:       a.InputPrice,
		OutputPrice:      a.OutputPrice,
		DailyCostBudget:  a.DailyCostBudget,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) GetBuiltinAgent(ctx context.Context, principalID string) (BuiltinAgent, error) {
	row, err := s.q().GetBuiltinAgent(ctx, principalID)
	if err != nil {
		return BuiltinAgent{}, notFound(err)
	}
	return builtinAgent(row)
}

func (s *sqlStore) BuiltinAgents(ctx context.Context) ([]BuiltinAgent, error) {
	rows, err := s.q().ListBuiltinAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BuiltinAgent, len(rows))
	for i, row := range rows {
		if out[i], err = builtinAgent(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func builtinAgent(row db.BuiltinAgent) (BuiltinAgent, error) {
	a := BuiltinAgent{
		PrincipalID:      row.PrincipalID,
		Provider:         row.Provider,
		BaseURL:          row.BaseUrl,
		Model:            row.Model,
		APIKey:           row.ApiKey,
		SystemPrompt:     row.SystemPrompt,
		MaxTokens:        int(row.MaxTokens),
		DailyTokenBudget: row.DailyTokenBudget,
		InputPrice:       row.InputPrice,
		OutputPrice:      row.OutputPrice,
		DailyCostBudget:  row.DailyCostBudget,
		Enabled:          row.Enabled != 0,
		LastError:        row.LastError,
		UpdatedAt:        time.UnixMilli(row.UpdatedAt),
		CreatedAt:        time.UnixMilli(row.CreatedAt),
	}
	if row.LastErrorAt != 0 {
		a.LastErrorAt = time.UnixMilli(row.LastErrorAt)
	}
	if err := json.Unmarshal([]byte(row.McpServers), &a.MCPServers); err != nil {
		return a, fmt.Errorf("store: built-in agent %s: mcp_servers: %w", row.PrincipalID, err)
	}
	return a, nil
}

func (s *sqlStore) SetBuiltinAgentError(ctx context.Context, principalID, msg string) error {
	var at int64
	if msg != "" {
		at = time.Now().UnixMilli()
	}
	return s.q().SetBuiltinAgentError(ctx, db.SetBuiltinAgentErrorParams{PrincipalID: principalID, LastError: msg, LastErrorAt: at})
}

// usageDay is the UTC day that usage at t counts towards.
func usageDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

func (s *sqlStore) AddAgentUsage(ctx context.Context, principalID string, at time.Time, u Usage) error {
	return s.q().AddAgentUsage(ctx, db.AddAgentUsageParams{
		PrincipalID:  principalID,
		Day:          usageDay(at),
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Cost:         u.Cost,
	})
}

func (s *sqlStore) AgentUsage(ctx context.Context, principalID string, at time.Time) (Usage, error) {
	row, err := s.q().AgentUsage(ctx, db.AgentUsageParams{PrincipalID: principalID, Day: usageDay(at)})
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, err
	}
	return Usage{InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, Cost: row.Cost}, nil
}

func (s *sqlStore) AgentCursor(ctx context.Context, principalID, roomID string) (int64, error) {
	seq, err := s.q().AgentCursor(ctx, db.AgentCursorParams{PrincipalID: principalID, RoomID: roomID})
	return seq, notFound(err)
}

func (s *sqlStore) SetAgentCursor(ctx context.Context, principalID, roomID string, seq int64) error {
	return s.q().SetAgentCursor(ctx, db.SetAgentCursorParams{PrincipalID: principalID, RoomID: roomID, Seq: seq})
}

func (s *sqlStore) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (int64, bool, error) {
	now := time.Now()
	gen, err := s.q().AcquireLease(ctx, db.AcquireLeaseParams{
		Name:        name,
		Holder:      holder,
		ExpiresAt:   now.Add(ttl).UnixMilli(),
		ExpiresAt_2: now.UnixMilli(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return gen, true, nil
}

func (s *sqlStore) ReleaseLease(ctx context.Context, name, holder string) error {
	return s.q().ReleaseLease(ctx, db.ReleaseLeaseParams{Name: name, Holder: holder})
}
