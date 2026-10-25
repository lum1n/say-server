package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lum1n/say-v2/internal/store/db"
)

// ConnectorSession is a bot's open /ws session (docs/adr/0011-bot-api.md).
type ConnectorSession struct {
	ID           string
	PrincipalID  string
	Name         string
	Version      string
	Capabilities []string
	ConnectedAt  time.Time
	SeenAt       time.Time
}

// sessionRetention is how long a session that stopped being renewed, on a
// node that died, is kept before it is pruned.
const sessionRetention = time.Hour

func (s *sqlStore) SaveConnectorSession(ctx context.Context, cs ConnectorSession) error {
	caps, err := json.Marshal(nonNil(cs.Capabilities))
	if err != nil {
		return err
	}
	if err := s.q().PruneConnectorSessions(ctx, cs.SeenAt.Add(-sessionRetention).UnixMilli()); err != nil {
		return err
	}
	return s.q().SaveConnectorSession(ctx, db.SaveConnectorSessionParams{
		ID:           cs.ID,
		PrincipalID:  cs.PrincipalID,
		Name:         cs.Name,
		Version:      cs.Version,
		Capabilities: string(caps),
		ConnectedAt:  cs.ConnectedAt.UnixMilli(),
		SeenAt:       cs.SeenAt.UnixMilli(),
	})
}

func (s *sqlStore) DeleteConnectorSession(ctx context.Context, id string) error {
	return s.q().DeleteConnectorSession(ctx, id)
}

func (s *sqlStore) ConnectorSessions(ctx context.Context, seenSince time.Time) ([]ConnectorSession, error) {
	rows, err := s.q().ConnectorSessions(ctx, seenSince.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]ConnectorSession, len(rows))
	for i, r := range rows {
		var caps []string
		if err := json.Unmarshal([]byte(r.Capabilities), &caps); err != nil {
			return nil, err
		}
		out[i] = ConnectorSession{
			ID:           r.ID,
			PrincipalID:  r.PrincipalID,
			Name:         r.Name,
			Version:      r.Version,
			Capabilities: caps,
			ConnectedAt:  time.UnixMilli(r.ConnectedAt),
			SeenAt:       time.UnixMilli(r.SeenAt),
		}
	}
	return out, nil
}
