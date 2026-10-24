-- Chat widgets embedded on other sites, and the visitors who use them
-- (docs/adr/0012-widget.md).
CREATE TABLE widgets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    -- pk_…: in the host page's HTML, so it is not a secret.
    public_key TEXT NOT NULL UNIQUE,
    -- sk_…: signs identity tokens on the host site's server. Kept in plain
    -- text, since the server needs it to check signatures.
    secret     TEXT NOT NULL,
    origins    TEXT NOT NULL, -- JSON array of allowed host origins
    agent_id   TEXT NOT NULL DEFAULT '', -- '' is no agent
    team       TEXT NOT NULL, -- JSON array of human principal ids
    greeting   TEXT NOT NULL DEFAULT '',
    updated_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

-- One guest principal and widget thread per visitor. subject is the host
-- site's user id for verified visitors and '' for anonymous ones.
CREATE TABLE widget_visitors (
    principal_id TEXT PRIMARY KEY REFERENCES principals (id),
    widget_id    TEXT NOT NULL REFERENCES widgets (id),
    subject      TEXT NOT NULL DEFAULT '',
    room_id      TEXT NOT NULL REFERENCES rooms (id),
    created_at   BIGINT NOT NULL
);

CREATE UNIQUE INDEX widget_visitors_subject
    ON widget_visitors (widget_id, subject)
    WHERE subject <> '';
