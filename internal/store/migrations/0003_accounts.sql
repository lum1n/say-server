-- Instance-wide state, one row per key. The 'setup' row is claimed once,
-- in the transaction that creates the first admin, so two nodes running
-- the setup wizard at the same time cannot both succeed.
CREATE TABLE instance (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Password logins for human principals.
CREATE TABLE logins (
    principal_id  TEXT PRIMARY KEY REFERENCES principals (id),
    username      TEXT NOT NULL UNIQUE, -- lowercase
    password_hash TEXT NOT NULL,        -- argon2id, PHC string format
    created_at    BIGINT NOT NULL
);

-- 1 for instance admins, 0 otherwise.
ALTER TABLE principals ADD COLUMN admin INTEGER NOT NULL DEFAULT 0;

-- Unix milliseconds after which the token stops working; 0 never expires.
ALTER TABLE tokens ADD COLUMN expires_at BIGINT NOT NULL DEFAULT 0;
