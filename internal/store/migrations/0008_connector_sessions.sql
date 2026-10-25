-- Bot sessions on /ws (docs/adr/0011-bot-api.md). A row lives while its
-- socket is open; seen_at (unix ms) is renewed on every heartbeat, so a
-- node that dies leaves rows that stop counting as live and are pruned.
CREATE TABLE connector_sessions (
    id           TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals (id),
    name         TEXT NOT NULL,
    version      TEXT NOT NULL,
    -- JSON array of capability names.
    capabilities TEXT NOT NULL,
    connected_at BIGINT NOT NULL,
    seen_at      BIGINT NOT NULL
);

CREATE INDEX connector_sessions_seen ON connector_sessions (seen_at);
