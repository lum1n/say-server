# say

One self-hostable service for all chat: human↔human, human↔agent,
agent↔agent, and bridged networks.

say has a **native chat core**: principals, rooms and an ordered event log.
Every external network, bot and agent is a *connector* attached to that
core, speaking one protocol. It ships as a single static binary, starts with
zero configuration on SQLite, and scales horizontally by running the same
binary against Postgres with an embedded NATS cluster bus.

> "say" is a placeholder name.

## Status

Early scaffolding. Nothing here is usable yet. See the milestones below.

## Quick start (target)

```sh
curl -fsSL get.<name>.sh | sh && say
# or
docker run -p 8080:8080 -v data:/data say
```

Either opens a browser setup wizard that creates the admin account.

Cluster mode is the same binary:

```sh
say serve --db postgres://… --peers node-b:6222,node-c:6222
```

## Building from source

Requires Go and pnpm.

```sh
make build   # builds web/ and widget/, then the Go binary into bin/say
make test    # go vet, Go tests, TypeScript typecheck
bin/say      # listens on :8080, stores data in data/say.db
```

The store tests run against SQLite, and also against Postgres when
`SAY_TEST_POSTGRES` holds a URL; each test gets its own schema:

```sh
SAY_TEST_POSTGRES='postgres://say:say@localhost:5432/say?sslmode=disable' make test
```

After editing `internal/store/queries.sql` or a migration, run
`make generate`.

`go build ./cmd/say` works without the web build; the binary then serves a
placeholder page instead of the app.

## Layout

| Path | What |
|---|---|
| `cmd/say` | The binary |
| `internal/core` | Domain model: principals, rooms, events, causation |
| `internal/store` | Storage interface; SQLite and Postgres implementations |
| `internal/bus` | Fan-out bus; in-process and embedded NATS implementations |
| `internal/api` | HTTP, WebSocket and SSE API |
| `internal/connector` | Connector protocol, capabilities, loopback for first-party connectors |
| `web/` | TanStack Start (React) app in SPA mode, embedded into the binary; includes `/embed` routes |
| `widget/` | Tiny vanilla-TS loader that defines `<say-chat>` |
| `protocol/` | The OpenAPI and JSON Schema contract; source of truth for SDKs |
| `sdk/ts`, `sdk/py` | SDKs generated from `protocol/` |
| `docs/adr` | Architecture decision records |

## Design

- [GLOSSARY.md](./GLOSSARY.md) defines the domain terms.
- [ADR 0001: Stack](./docs/adr/0001-stack.md)
- [ADR 0002: Storage and scaling](./docs/adr/0002-storage-and-scaling.md)
- [ADR 0003: Connector protocol](./docs/adr/0003-connector-protocol.md)
- [ADR 0004: Bridge risk policy](./docs/adr/0004-bridge-risk-policy.md)
- [ADR 0005: Agent safety](./docs/adr/0005-agent-safety.md)

## Privacy

The server reads every message. There is no end-to-end encryption in v1, and
bridged messages are never end-to-end encrypted. Transport is TLS.

## Milestones

- **M0 Core:** principals, rooms, event log, WS/HTTP API and contract; web
  app; SQLite and Postgres; in-process and NATS bus; setup wizard; 3-node
  cluster test in CI.
- **M1 Agents:** bot API, MCP server, built-in LLM agents, loop safety.
- **M2 Widget:** `<say-chat>` loader and `/embed` route, guest and verified
  identity, handoff.
- **M3 Simple bridges:** webhooks, email, IRC, Matrix, Discord, Slack,
  Telegram bot.
- **M4 Stateful bridges:** Telegram user, Signal, WhatsApp Cloud, on leases.
- **M5:** A2A, WASM hooks, connector registry.

## License

[Apache-2.0](./LICENSE).
