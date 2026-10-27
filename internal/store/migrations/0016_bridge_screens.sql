-- A screen code shows a bridge's login step, such as Signal's QR code, on
-- another screen without signing in there: on a laptop or tablet when the
-- phone that must scan the code is the one the person uses say on. It is
-- kept as its SHA-256, read-only, and good until it expires.
CREATE TABLE bridge_screens (
    hash       TEXT PRIMARY KEY,
    bridge_id  TEXT NOT NULL REFERENCES bridges (id),
    expires_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);

CREATE INDEX bridge_screens_bridge ON bridge_screens (bridge_id);
