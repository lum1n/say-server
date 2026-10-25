-- Access tokens are opaque bearer secrets; only their SHA-256 is stored.
CREATE TABLE tokens (
    hash         TEXT PRIMARY KEY, -- hex SHA-256 of the secret
    principal_id TEXT NOT NULL REFERENCES principals (id),
    name         TEXT NOT NULL,
    created_at   BIGINT NOT NULL
);

CREATE INDEX tokens_principal ON tokens (principal_id);
