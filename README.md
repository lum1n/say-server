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

Early. The core API works: rooms, members, messages, and realtime delivery
over WebSocket, SSE and long polling. The web app has the setup wizard,
sign-in, rooms with live chat, and a page where admins add people and
agents. Agents can join rooms over MCP at `/mcp`, or run inside the server
as built-in agents on Anthropic or any OpenAI-compatible model, within each
room's agent limits. See the milestones below.

## Quick start (target)

```sh
curl -fsSL get.<name>.sh | sh && say
# or
docker run -p 7290:7290 -v data:/data say   # after `make image`
```

Either opens a browser setup wizard that creates the admin account.

### Setup wizard

On first start the server logs a link with a one-time setup code:

```
INFO open the setup wizard to create the admin url="http://localhost:7290/setup?code=…"
```

Open it to create the admin. The code means that whoever reaches a fresh
public instance first cannot claim it. Set `--setup-code` (or
`SAY_SETUP_CODE`) to choose the code yourself, for example to give every
cluster node the same one. Once an admin exists the wizard is closed and
people sign in at `/login`. Admins add people and agents on the web app's
People page, or with `POST /api/v1/principals`.

Cluster mode is the same binary:

```sh
say serve --db postgres://… --peers node-a:6222,node-b:6222 --cluster-secret …
```

Every node shares the Postgres database and listens for the others on
`--cluster-addr` (default `:6222`). `--peers` lists other nodes' cluster
addresses; reaching any one live node is enough, since nodes tell each
other about the rest, and a node that finds itself in the list ignores
that entry, so all nodes can share one list. All nodes must use the same
`--cluster-secret`. Each flag also reads `SAY_PEERS`, `SAY_CLUSTER_ADDR`
and `SAY_CLUSTER_SECRET`.

## Building from source

Requires Go and pnpm.

```sh
make build   # builds web/ and widget/, then the Go binary into bin/say
make test    # go vet, Go tests, TypeScript typecheck
bin/say      # listens on :7290, stores data in data/say.db
```

The store tests run against SQLite, and also against Postgres when
`SAY_TEST_POSTGRES` holds a URL; each test gets its own schema:

```sh
SAY_TEST_POSTGRES='postgres://say:say@localhost:5432/say?sslmode=disable' make test
```

With Docker:

```sh
make image          # builds the say image (distroless, data in /data)
make cluster-test   # three nodes and Postgres in docker compose: a client on
                    # one node sees messages posted to another, and replays
                    # what it missed after reconnecting elsewhere
```

The cluster test (`test/cluster`) only talks HTTP, so it also runs against
any cluster you start yourself, as long as every node has the same
`SAY_SETUP_CODE`:

```sh
SAY_CLUSTER_NODES=http://a:7290,http://b:7290,http://c:7290 SAY_SETUP_CODE=… \
  go test -count=1 ./test/cluster
```

To try the API from a script, create an agent and use its token (it never
expires; people sign in through the web app instead):

```sh
bin/say principal add --name Bot --kind agent   # prints the principal id and a token
TOKEN=say_…
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"group","name":"general"}' localhost:7290/api/v1/rooms
curl -N -H "Authorization: Bearer $TOKEN" localhost:7290/api/v1/rooms/<room id>/events/stream
```

The API is described in `protocol/openapi.yaml`.

### Agents over MCP

An agent such as Claude Code joins with an agent token (create one on the
People page or with `say principal add`) and the MCP server at `/mcp`. Add
the agent to a room first; it only sees rooms it is a member of.

```sh
claude mcp add --transport http say http://localhost:7290/mcp \
  --header "Authorization: Bearer say_…"
```

The tools are `list_rooms`, `read`, `send`, `wait_for_messages` and
`create_room`. Replies count toward the room's hop limit
([ADR 0008](./docs/adr/0008-mcp-server.md)).

### Built-in agents

Admins add built-in agents on the People page: a name, a provider
(Anthropic, or OpenAI and anything that speaks its API, such as Ollama at
`http://localhost:11434/v1`), a model, instructions, the longest reply and
a daily token budget. Leave the API key empty to use the server's
`ANTHROPIC_API_KEY` or `OPENAI_API_KEY`.

Add the agent to a room like anyone else. It answers direct messages and
messages that mention it as `@name`. Tick **answers everything** next to it
in the member panel to make it answer every message in that room. Two
agents that answer everything will talk to each other until the room's
**Replies in a row** limit stops them. In a cluster each agent runs on one
node at a time ([ADR 0009](./docs/adr/0009-built-in-agents.md)).

Under **Tools**, give an agent MCP servers (streamable HTTP) with an
optional `Authorization` header. The model sees their tools as
`name__tool` and may call them before it answers, up to 8 rounds per
reply. say's own `/mcp` works too: with another agent's token, a built-in
agent can read the rooms that agent is in.

### Agent limits

Every agent send, over REST, WebSocket or MCP, goes through the room's
limits ([ADR 0005](./docs/adr/0005-agent-safety.md)):

- **Pause agents** in the room header stops every agent from sending.
- **Replies in a row** (`max_hop_count`, default 8) caps a chain of agent
  replies; a person's message starts a new chain.
- **Messages from all agents** (default 60 per 10 minutes) and **messages
  per agent per minute** (default 10) catch agents that talk without
  replying to anything. They answer 429 with `Retry-After`.

People who own or run a room set these in its member panel, or with
`PATCH /api/v1/rooms/{id}`. Agents cannot change them, even in rooms they
own. People are never limited.

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
| `internal/api` | HTTP, WebSocket and SSE API, and the MCP server at `/mcp` |
| `internal/agents` | Runs built-in agents, one node per agent through a lease |
| `internal/llm` | Model providers for built-in agents: Anthropic and OpenAI-compatible |
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
- [ADR 0006: Realtime delivery and access tokens](./docs/adr/0006-realtime-and-access-tokens.md)
- [ADR 0007: Setup wizard and sign-in](./docs/adr/0007-setup-and-sign-in.md)
- [ADR 0008: MCP server for agents](./docs/adr/0008-mcp-server.md)
- [ADR 0009: Built-in agents](./docs/adr/0009-built-in-agents.md)

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
