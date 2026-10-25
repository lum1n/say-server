-- Built-in agents: agent principals whose replies the server writes by
-- calling a model provider (docs/adr/0009-built-in-agents.md).
CREATE TABLE builtin_agents (
    principal_id       TEXT PRIMARY KEY REFERENCES principals (id),
    provider           TEXT NOT NULL,              -- anthropic | openai
    base_url           TEXT NOT NULL DEFAULT '',   -- '' is the provider's API
    model              TEXT NOT NULL,
    api_key            TEXT NOT NULL DEFAULT '',   -- '' uses the server's env
    system_prompt      TEXT NOT NULL DEFAULT '',
    max_tokens         INTEGER NOT NULL,           -- per reply
    daily_token_budget BIGINT NOT NULL DEFAULT 0,  -- input + output; 0 is unlimited
    enabled            INTEGER NOT NULL DEFAULT 1,
    last_error         TEXT NOT NULL DEFAULT '',
    last_error_at      BIGINT NOT NULL DEFAULT 0,
    updated_at         BIGINT NOT NULL,
    created_at         BIGINT NOT NULL
);

-- Tokens a built-in agent used per UTC day (YYYY-MM-DD).
CREATE TABLE agent_usage (
    principal_id  TEXT NOT NULL REFERENCES principals (id),
    day           TEXT NOT NULL,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (principal_id, day)
);

-- How far a built-in agent has read each room, so a node that takes the
-- agent over continues where the last one stopped.
CREATE TABLE agent_cursors (
    principal_id TEXT NOT NULL REFERENCES principals (id),
    room_id      TEXT NOT NULL REFERENCES rooms (id),
    seq          BIGINT NOT NULL,
    PRIMARY KEY (principal_id, room_id)
);

-- Named leases: at most one node holds each until expires_at (unix ms).
-- generation goes up whenever the holder changes.
CREATE TABLE leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    generation BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);
