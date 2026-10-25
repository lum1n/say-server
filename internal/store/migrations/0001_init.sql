-- Portable subset: this file runs unchanged on SQLite and Postgres.
-- Timestamps are unix milliseconds; JSON is stored as TEXT.

CREATE TABLE principals (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at   BIGINT NOT NULL
);

CREATE TABLE rooms (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    last_seq   BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL
);

CREATE TABLE memberships (
    room_id      TEXT NOT NULL REFERENCES rooms (id),
    principal_id TEXT NOT NULL REFERENCES principals (id),
    role         TEXT NOT NULL,
    scopes       TEXT NOT NULL, -- JSON array of strings
    PRIMARY KEY (room_id, principal_id)
);

CREATE INDEX memberships_principal ON memberships (principal_id);

CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    room_id         TEXT NOT NULL REFERENCES rooms (id),
    seq             BIGINT NOT NULL,
    type            TEXT NOT NULL,
    sender_id       TEXT NOT NULL REFERENCES principals (id),
    caused_by       TEXT NOT NULL DEFAULT '',
    hop_count       INTEGER NOT NULL DEFAULT 0,
    parts           TEXT NOT NULL, -- JSON array of {kind, data}
    stream          TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at      BIGINT NOT NULL,
    UNIQUE (room_id, seq)
);

CREATE UNIQUE INDEX events_idempotency
    ON events (room_id, sender_id, idempotency_key)
    WHERE idempotency_key <> '';

-- Written in the same transaction as the event, so delivery to connectors
-- survives a crash between commit and publish.
CREATE TABLE outbox (
    event_id   TEXT PRIMARY KEY REFERENCES events (id),
    room_id    TEXT NOT NULL,
    seq        BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

CREATE INDEX outbox_order ON outbox (created_at, room_id, seq);
