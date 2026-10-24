-- Postgres placeholder syntax; the SQLite engine rewrites $N to ?N, which
-- SQLite reads as the same numbered parameter. Keep to the portable subset
-- of migrations/0001_init.sql.

-- name: CreatePrincipal :exec
INSERT INTO principals (id, kind, display_name, created_at)
VALUES ($1, $2, $3, $4);

-- name: GetPrincipal :one
SELECT * FROM principals WHERE id = $1;

-- name: CreateRoom :exec
INSERT INTO rooms (id, kind, name, created_at)
VALUES ($1, $2, $3, $4);

-- name: GetRoom :one
SELECT * FROM rooms WHERE id = $1;

-- name: RoomsFor :many
SELECT r.id, r.kind, r.name, r.created_at
FROM rooms r JOIN memberships m ON m.room_id = r.id
WHERE m.principal_id = $1
ORDER BY r.created_at, r.id;

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
                    parts, stream, idempotency_key, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

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
INSERT INTO tokens (hash, principal_id, name, created_at)
VALUES ($1, $2, $3, $4);

-- name: PrincipalByToken :one
SELECT p.* FROM principals p JOIN tokens t ON t.principal_id = p.id
WHERE t.hash = $1;
