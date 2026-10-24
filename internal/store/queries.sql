-- Postgres placeholder syntax; the SQLite engine rewrites $N to ?N, which
-- SQLite reads as the same numbered parameter. Keep to the portable subset
-- of migrations/0001_init.sql.

-- name: CreatePrincipal :exec
INSERT INTO principals (id, kind, display_name, admin, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetPrincipal :one
SELECT * FROM principals WHERE id = $1;

-- name: ListPrincipals :many
SELECT * FROM principals ORDER BY created_at, id;

-- name: RenamePrincipal :execrows
UPDATE principals SET display_name = $2 WHERE id = $1;

-- name: CreateRoom :exec
INSERT INTO rooms (id, kind, name, created_at)
VALUES ($1, $2, $3, $4);

-- name: GetRoom :one
SELECT * FROM rooms WHERE id = $1;

-- name: RoomsFor :many
SELECT r.*
FROM rooms r JOIN memberships m ON m.room_id = r.id
WHERE m.principal_id = $1
ORDER BY r.created_at, r.id;

-- name: SetAgentPolicy :execrows
UPDATE rooms
SET agents_paused = $2, max_hop_count = $3, agent_turn_limit = $4,
    agent_turn_window = $5, agent_rate_limit = $6
WHERE id = $1;

-- name: AddMember :exec
INSERT INTO memberships (room_id, principal_id, role, scopes)
VALUES ($1, $2, $3, $4);

-- name: GetMember :one
SELECT * FROM memberships WHERE room_id = $1 AND principal_id = $2;

-- name: Members :many
SELECT * FROM memberships WHERE room_id = $1 ORDER BY principal_id;

-- name: BumpSeq :one
UPDATE rooms SET last_seq = last_seq + 1 WHERE id = $1 RETURNING last_seq;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: EventByIdempotencyKey :one
SELECT * FROM events
WHERE room_id = $1 AND sender_id = $2 AND idempotency_key = $3;

-- name: InsertEvent :exec
INSERT INTO events (id, room_id, seq, type, sender_id, caused_by, hop_count,
                    parts, stream, idempotency_key, automated, edits, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- Only a message that is still streaming changes in place; the stream
-- condition makes a late update after the final edit a no-op.
-- name: UpdateStreaming :execrows
UPDATE events SET parts = $3
WHERE id = $1 AND sender_id = $2 AND type = 'message' AND stream = 'streaming';

-- name: ReplaceMessage :exec
UPDATE events SET parts = $2, stream = $3, edited_at = $4 WHERE id = $1;

-- name: StreamingFrom :many
SELECT * FROM events WHERE sender_id = $1 AND stream = 'streaming'
ORDER BY created_at, id;

-- The created_at of the room's automated event OFFSET places back from the
-- latest; no row means the room has no more than OFFSET of them.
-- name: AutomatedAt :one
SELECT created_at FROM events
WHERE room_id = $1 AND automated = 1
ORDER BY seq DESC
LIMIT 1 OFFSET $2;

-- name: AutomatedFromSenderAt :one
SELECT created_at FROM events
WHERE room_id = $1 AND sender_id = $2 AND automated = 1
ORDER BY seq DESC
LIMIT 1 OFFSET $3;

-- name: EventsAfter :many
SELECT * FROM events
WHERE room_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3;

-- name: EventsBefore :many
SELECT * FROM events
WHERE room_id = $1 AND seq < $2
ORDER BY seq DESC
LIMIT $3;

-- name: InsertOutbox :exec
INSERT INTO outbox (event_id, room_id, seq, created_at)
VALUES ($1, $2, $3, $4);

-- name: PendingOutbox :many
SELECT * FROM outbox ORDER BY created_at, room_id, seq LIMIT $1;

-- name: DeleteOutbox :exec
DELETE FROM outbox WHERE event_id = $1;

-- name: CreateToken :exec
INSERT INTO tokens (hash, principal_id, name, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: PrincipalByToken :one
SELECT p.* FROM principals p JOIN tokens t ON t.principal_id = p.id
WHERE t.hash = $1 AND (t.expires_at = 0 OR t.expires_at > $2);

-- name: DeleteToken :exec
DELETE FROM tokens WHERE hash = $1;

-- name: ClaimSetup :execrows
INSERT INTO instance (key, value) VALUES ('setup', $1)
ON CONFLICT (key) DO NOTHING;

-- name: SetupDone :one
SELECT count(*) FROM instance WHERE key = 'setup';

-- name: CreateLogin :execrows
INSERT INTO logins (principal_id, username, password_hash, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (username) DO NOTHING;

-- name: LoginByUsername :one
SELECT * FROM logins WHERE username = $1;

-- name: SetMemberScopes :execrows
UPDATE memberships SET scopes = $3 WHERE room_id = $1 AND principal_id = $2;

-- name: CreateBuiltinAgent :exec
INSERT INTO builtin_agents (principal_id, provider, base_url, model, api_key,
                            system_prompt, max_tokens, daily_token_budget,
                            enabled, updated_at, created_at, mcp_servers,
                            input_price, output_price, daily_cost_budget)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: UpdateBuiltinAgent :execrows
UPDATE builtin_agents
SET provider = $2, base_url = $3, model = $4, api_key = $5, system_prompt = $6,
    max_tokens = $7, daily_token_budget = $8, enabled = $9, updated_at = $10,
    mcp_servers = $11, input_price = $12, output_price = $13,
    daily_cost_budget = $14
WHERE principal_id = $1;

-- name: GetBuiltinAgent :one
SELECT * FROM builtin_agents WHERE principal_id = $1;

-- name: ListBuiltinAgents :many
SELECT * FROM builtin_agents ORDER BY created_at, principal_id;

-- name: SetBuiltinAgentError :exec
UPDATE builtin_agents SET last_error = $2, last_error_at = $3
WHERE principal_id = $1;

-- name: AddAgentUsage :exec
INSERT INTO agent_usage (principal_id, day, input_tokens, output_tokens, cost)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (principal_id, day) DO UPDATE
SET input_tokens = agent_usage.input_tokens + excluded.input_tokens,
    output_tokens = agent_usage.output_tokens + excluded.output_tokens,
    cost = agent_usage.cost + excluded.cost;

-- name: AgentUsage :one
SELECT input_tokens, output_tokens, cost FROM agent_usage
WHERE principal_id = $1 AND day = $2;

-- name: AgentCursor :one
SELECT seq FROM agent_cursors WHERE principal_id = $1 AND room_id = $2;

-- Cursors only move forward.
-- name: SetAgentCursor :exec
INSERT INTO agent_cursors (principal_id, room_id, seq) VALUES ($1, $2, $3)
ON CONFLICT (principal_id, room_id) DO UPDATE SET seq = excluded.seq
WHERE excluded.seq > agent_cursors.seq;

-- Takes the lease for holder when it is free, expired or already holder's,
-- and returns its generation; no row means someone else holds it.
-- name: AcquireLease :one
INSERT INTO leases (name, holder, generation, expires_at)
VALUES ($1, $2, 1, $3)
ON CONFLICT (name) DO UPDATE
SET holder = excluded.holder,
    generation = CASE WHEN leases.holder = excluded.holder
                      THEN leases.generation ELSE leases.generation + 1 END,
    expires_at = excluded.expires_at
WHERE leases.holder = excluded.holder OR leases.expires_at < $4
RETURNING generation;

-- name: ReleaseLease :exec
DELETE FROM leases WHERE name = $1 AND holder = $2;

-- name: SaveConnectorSession :exec
INSERT INTO connector_sessions (id, principal_id, name, version, capabilities, connected_at, seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET seen_at = excluded.seen_at;

-- name: PruneConnectorSessions :exec
DELETE FROM connector_sessions WHERE seen_at < $1;

-- name: DeleteConnectorSession :exec
DELETE FROM connector_sessions WHERE id = $1;

-- name: ConnectorSessions :many
SELECT id, principal_id, name, version, capabilities, connected_at, seen_at
FROM connector_sessions WHERE seen_at >= $1
ORDER BY connected_at, id;

-- name: CreateWidget :exec
INSERT INTO widgets (id, name, public_key, secret, origins, agent_id, team,
                     greeting, updated_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: UpdateWidget :execrows
UPDATE widgets
SET name = $2, secret = $3, origins = $4, agent_id = $5, team = $6,
    greeting = $7, updated_at = $8
WHERE id = $1;

-- name: GetWidget :one
SELECT * FROM widgets WHERE id = $1;

-- name: WidgetByKey :one
SELECT * FROM widgets WHERE public_key = $1;

-- name: ListWidgets :many
SELECT * FROM widgets ORDER BY created_at, id;

-- name: CreateVisitor :execrows
INSERT INTO widget_visitors (principal_id, widget_id, subject, room_id, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING;

-- name: VisitorByPrincipal :one
SELECT * FROM widget_visitors WHERE principal_id = $1;

-- name: VisitorBySubject :one
SELECT * FROM widget_visitors WHERE widget_id = $1 AND subject = $2;
