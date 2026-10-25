-- Bridges to other networks (docs/adr/0014-bridges.md). Delivery follows
-- the room log with a cursor per link, so the outbox, which nothing read,
-- is gone.
DROP TABLE outbox;

-- A bridge in account mode is one person's own account on a network; its
-- chats become rooms the owner is in. In bot mode an admin's relay bot
-- mirrors existing rooms.
CREATE TABLE bridges (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL REFERENCES principals (id),
    mode       TEXT NOT NULL, -- account, bot
    network    TEXT NOT NULL,
    name       TEXT NOT NULL,
    -- JSON object, the network's settings with its secrets in plain text.
    config     TEXT NOT NULL,
    -- JSON object the running node keeps across restarts: a session, a
    -- sync position, the account's own remote id.
    state      TEXT NOT NULL DEFAULT '{}',
    -- JSON object, a login step waiting for the owner; '' when none.
    prompt     TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_error TEXT NOT NULL DEFAULT '',
    -- Renewed by the node that runs the bridge; 0 when none does.
    seen_at    BIGINT NOT NULL DEFAULT 0,
    updated_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

CREATE INDEX bridges_owner ON bridges (owner_id);

-- A BridgeLink: one room mirrored to one remote chat (a portal, in
-- account mode). cursor is the
-- last seq of the room handled for delivery.
CREATE TABLE bridge_links (
    id          TEXT PRIMARY KEY,
    bridge_id   TEXT NOT NULL REFERENCES bridges (id),
    room_id     TEXT NOT NULL REFERENCES rooms (id),
    remote_id   TEXT NOT NULL,
    remote_name TEXT NOT NULL DEFAULT '',
    cursor      BIGINT NOT NULL,
    created_at  BIGINT NOT NULL,
    UNIQUE (bridge_id, remote_id)
);

CREATE INDEX bridge_links_room ON bridge_links (room_id);

-- An IdentityLink: a remote user of a bridge seen as a ghost principal.
CREATE TABLE bridge_ghosts (
    bridge_id      TEXT NOT NULL REFERENCES bridges (id),
    remote_user_id TEXT NOT NULL,
    principal_id   TEXT NOT NULL UNIQUE REFERENCES principals (id),
    created_at     BIGINT NOT NULL,
    PRIMARY KEY (bridge_id, remote_user_id)
);

-- One row per message crossing a link, either way. Outgoing rows go
-- sending → sent, or failed or expired; one left sending by a node that
-- stopped becomes unknown and is never retried. An incoming row is
-- written before its event, with the event's id chosen in advance, so a
-- crash between the two can't make the message twice; hence no foreign
-- key on event_id.
CREATE TABLE bridge_messages (
    link_id    TEXT NOT NULL REFERENCES bridge_links (id),
    event_id   TEXT NOT NULL,
    remote_id  TEXT NOT NULL DEFAULT '',
    direction  TEXT NOT NULL, -- in, out
    state      TEXT NOT NULL,
    error      TEXT NOT NULL DEFAULT '',
    updated_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (link_id, event_id)
);

CREATE UNIQUE INDEX bridge_messages_remote
    ON bridge_messages (link_id, remote_id)
    WHERE remote_id <> '';
CREATE INDEX bridge_messages_event ON bridge_messages (event_id);
CREATE INDEX bridge_messages_sending ON bridge_messages (state) WHERE state = 'sending';
