# ADR 0002: SQLite by default, Postgres and NATS in cluster mode

Status: accepted (2026-10-24)

## Context

Starting must need nothing but the binary. Horizontal scaling must work from
day one, not as a later rewrite.

## Decision

- **Storage:** SQLite in WAL mode by default; Postgres in cluster mode. One
  portable SQL subset: a single schema, a single `queries.sql` and a single
  store implementation serve both engines. sqlc checks the queries with its
  Postgres engine and generates one `database/sql` package; on SQLite the
  `$N` placeholders are rewritten to `?N`, which SQLite binds the same way.
  Timestamps are unix milliseconds and JSON is TEXT, so no column type
  differs between engines. CI runs the full suite against both.
- **Event log:** each room has a monotonically increasing, gapless `seq`,
  allocated in the database inside the insert transaction by incrementing
  the room's `last_seq`. That update locks the room row, which also
  serializes idempotency-key checks for the room. Clients and connectors
  resume from a cursor.
- **Bus:** an interface with an in-process implementation (single node) and
  an embedded NATS implementation (cluster). In cluster mode each node runs
  a NATS server with no client port, connects to it in process, and routes
  to the other nodes on its cluster port. Nodes find each other through
  `--peers` (host:port list; DNS names that resolve to one node work, and
  NATS gossip completes the mesh from any one live peer); no extra
  infrastructure. A shared `--cluster-secret` keeps other hosts from
  joining. The bus carries notifications only; the database is the source
  of truth, so a missed bus message is repaired by replaying from the
  cursor.
- **Search:** SQLite FTS5 and Postgres tsvector behind one interface.
- **Files:** local disk or S3-compatible storage, served by signed URLs.
  Never base64 over the WebSocket.
- **Stateful sessions** (such as Telegram user accounts) run on exactly one
  node, held by a DB lease with a generation counter.

## Consequences

- Two SQL engines cost maintenance. sqlc and the CI matrix keep it honest.
  The alternative, Postgres only, would kill the zero-dependency start.
- A 3-node cluster test (docker compose with Postgres) is part of M0.
