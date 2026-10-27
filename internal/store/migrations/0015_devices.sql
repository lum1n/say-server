-- Tokens get an id, so a person can list their sessions and devices and
-- revoke one, and when each was last used and from what client
-- (docs/adr/0018-devices.md). Older tokens take an id from their hash,
-- which is no secret.
ALTER TABLE tokens ADD COLUMN id TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN last_used_at BIGINT NOT NULL DEFAULT 0;
UPDATE tokens SET id = 'tok_' || substr(hash, 1, 24) WHERE id = '';
CREATE UNIQUE INDEX tokens_id ON tokens (id);

-- A pairing code lets a signed-in device add another one: it is shown
-- once, kept as its SHA-256 and redeemed once, before it expires.
CREATE TABLE pairings (
    hash         TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals (id),
    expires_at   BIGINT NOT NULL,
    created_at   BIGINT NOT NULL
);

CREATE INDEX pairings_principal ON pairings (principal_id);
