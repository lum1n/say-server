# ADR 0002: SQLite by default, Postgres and NATS in cluster mode

Status: accepted (2026-10-24)

## Context

Starting must need nothing but the binary. Horizontal scaling must work from
day one, not as a later rewrite.

## Decision

- **Storage:** SQLite in WAL mode by default; Postgres in cluster mode. One
  portable SQL subset, generated with sqlc for both engines. CI runs the
  full suite against both.
- **Event log:** each room has a monotonically increasing `seq`, allocated
  in the database inside the insert transaction. Clients and connectors
  resume from a cursor.
- **Bus:** an interface with an in-process implementation (single node) and
  an embedded NATS implementation (cluster). Nodes find each other through
  `--peers` or a DNS name; no extra infrastructure. The bus carries
  notifications only; the database is the source of truth, so a missed bus
  message is repaired by replaying from the cursor.
- **Search:** SQLite FTS5 and Postgres tsvector behind one interface.
- **Files:** local disk or S3-compatible storage, served by signed URLs.
  Never base64 over the WebSocket.
- **Stateful sessions** (such as Telegram user accounts) run on exactly one
  node, held by a DB lease with a generation counter.

## Consequences

- Two SQL engines cost maintenance. sqlc and the CI matrix keep it honest.
  The alternative, Postgres only, would kill the zero-dependency start.
- A 3-node cluster test (docker compose with Postgres) is part of M0.
