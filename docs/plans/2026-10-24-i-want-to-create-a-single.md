# say-v2: one self-hostable service for all chat (human↔human, human↔agent, agent↔agent, bridged)

## Context
`../say` and `../chatty` were both **aggregators**: they had no native rooms, agents were not participants, and there was no widget. They proved useful patterns for the bridge layer, but neither had a chat core. say-v2 inverts that. A **native chat core** comes first, and every external network, bot and agent is a *connector* attached to it. The project is public, open source (Apache-2.0) and community-extensible. It must be dead simple to start with and able to scale horizontally from day one.

Decisions so far:
- Self-host first, one binary.
- Bidirectional room bridges.
- Agents connect via MCP, HTTP/WS, built-in LLM agents and A2A.
- TLS with a trusted server (no E2EE in v1).
- Apache-2.0.
- Horizontal scaling from day one.
- Bridges: Discord, Slack, Telegram (bot **and user**), Matrix, IRC, Signal, WhatsApp, email and webhooks. **No ban-risk paths.**

## Tech stack (chosen on fit, not on history)

**Go core, single static binary.** Why Go:
- Several of the hard parts are Go-native and embeddable:
  - Embedded **NATS** (cluster bus) inside the binary.
  - **gotd/td**, a pure-Go MTProto client, for Telegram user mode.
  - mautrix-go (Matrix), discordgo, slack-go and ergochat/irc-go for bridges.
  - wazero (pure-Go WASM) for sandboxed plugins.
  - certmagic for automatic TLS.
  - The official MCP Go SDK, the A2A Go SDK, and the Anthropic and OpenAI Go SDKs.
- It cross-compiles to every OS with no cgo, using `modernc.org/sqlite`.
- Rust has a higher barrier for contributors. TypeScript/Bun has a richer bot ecosystem, but it can't embed a cluster bus and its binaries are heavier. Elixir's single-binary story is weak.

| Concern | Choice |
|---|---|
| Storage | **SQLite (WAL) by default**, **Postgres in cluster mode**. One portable SQL subset via sqlc for both engines, and CI runs the full suite against both. This is the one deliberate cost of "simple *and* horizontal". |
| Bus / fan-out | Interface with an in-process implementation (single node) and **embedded NATS** (cluster: nodes find each other via a `--peers` flag or a DNS name; no extra infrastructure) |
| Event log | A per-room, monotonically increasing `seq`, allocated in the DB. Clients and connectors resume from a cursor. |
| Search | SQLite FTS5 / Postgres tsvector behind one interface |
| Files | Local disk or S3-compatible storage, with signed URLs (never base64 over the WebSocket) |
| Realtime | WebSocket (JSON envelopes `{id,type,status,payload}`) plus SSE for simple bots and token streaming. Long-poll fallback. |
| Contract | **One** OpenAPI and JSON Schema protocol definition. TypeScript and Python SDKs are generated from it. |
| Web app | **TanStack Start (React) in SPA mode**: Router, Query, and file routes. The static build is embedded in the binary via `go:embed`, and the Go core serves the API. Server functions/SSR are not used, because they would need a Node/Bun runtime next to the binary and break single-binary install. Data goes through the generated TS SDK and TanStack Query, with WS events pushed into the Query cache. |
| Widget | A **tiny vanilla-TS loader** (`/widget.js`, ~3 KB) that defines `<say-chat>`. It mounts an **iframe** pointing at an `/embed/$widgetId` route of the same TanStack Start app, and the two communicate via a postMessage API (open/close, identity token, theme, unread events). The iframe isolates CSS and auth from the host page and lets the widget reuse the app's components. |
| TLS | certmagic auto-HTTPS when a domain is set |

**Dead-simple start:** `curl -fsSL get.<name>.sh | sh && say`, or `docker run -p 8080:8080 -v data:/data say`. Either opens a browser setup wizard that creates the admin. Cluster mode is the same binary run as `say serve --db postgres://… --peers …`.

## Core domain model
- **Principal**: kind `human | agent | guest | ghost (bridged remote user) | system`. Everyone sends through the same path, so human↔human, human↔agent and agent↔agent need no special cases.
- **Room**: kind `dm | group | channel | widget-thread`. **Membership** carries a role and scopes.
- **Event**: `(room, seq)`. Message events are made of typed **parts**: text/markdown, attachment, tool-call/result, card/form, and a streaming state (`streaming → final`). Edits, reactions, threads, typing and receipts are separate event types.
- **Connector**: an out-of-process or in-process participant with **capabilities** (from chatty) and scoped credentials. **BridgeLink** ties a room to a remote channel. **IdentityLink** maps a remote user to a ghost principal.
- **Causation chain** on every message: `caused_by` and `hop_count`. This underpins agent loop protection.

## Extensibility: one protocol for bridges, bots and agents
1. **Connector protocol.** Any language. Connectors **dial out** to the hub over WebSocket (from say), so no public endpoint is needed. Credentials are scoped. Bridges get an extra `puppet` scope so they can post as ghosts. First-party connectors use the *same* protocol over an in-process loopback, which dogfoods it.
2. **Agent surfaces:**
   - **HTTP/WS bot API**: the connector protocol with the agent scope.
   - **MCP server** at `/mcp`, with tools: `list_rooms`, `read`, `send`, `wait_for_messages`, `create_room`.
   - **Built-in LLM agents**: configured in the UI with a provider key (Anthropic, OpenAI, or any OpenAI-compatible endpoint such as Ollama), a prompt, and tools that include *other connected MCP servers*.
   - **A2A**: every agent principal gets an A2A Agent Card and endpoint, and external A2A agents can be invited into rooms. MCP and A2A sit behind adapters because both specs are still moving.
3. **Outgoing webhooks** for any event.
4. **Phase 2: WASM hooks** (wazero), sandboxed in-process filters for moderation, slash commands and transforms.
5. Plugin registry: a simple index repo of connectors and hooks, plus `say connector add <name>`.

## Reliability rules carried from say (non-negotiable)
- Client idempotency keys.
- A `commands` and outbox table written in the same transaction as the message.
- Inbound messages are deduplicated on `(link, remote_msg_id)`.
- A send whose outcome is uncertain becomes `unknown` and is **never auto-retried**.
- **One active session per stateful account**, held through a DB lease with a generation counter. This matters in a cluster: a Telegram user session must not run on two nodes.
- Connector reachability (`live/offline/unlinked`) is shown, and sending to an offline connector returns `409` instead of queueing silently.
- Push notifications carry no message content.
- Retention is configurable per room or instance.

## Agent↔agent safety (designed in, not bolted on)
- Default: agents reply only when mentioned, or in rooms where they are designated responders.
- Per-room limits: `max_hop_count`, an agent turn budget per window, and rate limits.
- Per-agent token and cost budgets for built-in agents.
- A human "pause agents" switch for each room. Loop detection uses the causation chain.

## Embedding
- A publishable key plus an allowed-origins list per widget.
- Anonymous guest principals by default.
- **Identity verification**: the host site signs a short JWT (HMAC with a widget secret) carrying its own user id, and that maps to a principal.
- A widget room can route to a human team, an agent, or an agent with human handoff.
- Theming via CSS custom properties, passed to the iframe as a postMessage theme object.

## Bridges: constraints and the no-ban rule
| Network | Path | Risk / constraint |
|---|---|---|
| Webhooks, email | Inbound and outbound HTTP; SMTP out, inbound via IMAP or webhook | None |
| IRC | Relay bot (ergochat/irc-go) | None |
| Matrix | Bot account by default; appservice (proper ghosts) if the user runs a homeserver | E2EE rooms need the bot to handle encryption (mautrix-go supports it) |
| Discord | A bot the user creates | The message-content intent is privileged and needs verification above 100 servers; ghosts are rendered via webhooks |
| Slack | A Slack app installed per workspace | Since the 2025 terms, non-Marketplace apps get heavily rate-limited `conversations.history`, so realtime works but backfill is limited |
| Telegram bot | Bot API | None |
| **Telegram user** | **MTProto via gotd/td**, the official third-party-client route; each instance supplies its own `api_id`/`api_hash` from my.telegram.org | Legitimate under Telegram's terms. Flood-wait limits must be respected, and the connector must never be used for bulk messaging. This is the stateful session that needs the lease. |
| **Signal** | **Linked secondary device** via signal-cli, run as a managed subprocess, with JSON-RPC to the connector | There is no official API, and this is how every Signal bridge works. The risk is low but **not zero**: personal use only, no automation that looks like spam. **License:** libsignal is AGPL and signal-cli is GPL, so it must stay **out of process** to keep the core Apache-2.0. It ships in the Docker image; the bare binary needs Java/signal-cli. |
| **WhatsApp** | **Business Cloud API only** (official) | No ban risk, but it needs a business account and charges per conversation. Personal-account bridging (whatsmeow) is reverse-engineered and against the terms, so it is **excluded from first-party** and only possible as a community plugin with a warning. |

## Constraints to keep discussing
1. **Simple vs horizontal.** Keeping two DB engines costs maintenance; mitigate with sqlc plus a CI matrix. The alternative, Postgres only, kills zero-dependency start.
2. **Lowest common denominator.** Features degrade by connector capability (reactions, edits, threads). Native rooms show "bridged" fallbacks.
3. **Identity across networks.** Ghosts are per network. Merging them into one person is manual or opt-in, never automatic.
4. **Abuse on public embeds and instances.** Rate limits, guest captcha hooks, a moderation queue and report flows are needed before the first public widget.
5. **Privacy honesty.** The server reads messages, and bridged messages are never E2EE. Say so plainly in the UI.
6. **Moving specs.** MCP and A2A versions are pinned behind adapters.
7. **Naming and branding.** "say" is a placeholder.

## Milestones
- **M0 Core:**
  - principals, rooms, the event log, the WS/HTTP API and the OpenAPI/JSON Schema contract;
  - the web app;
  - SQLite *and* Postgres;
  - the in-process bus *and* the NATS bus;
  - the setup wizard;
  - a 3-node cluster test in CI.
- **M1 Agents:** the bot API, the MCP server, built-in LLM agents, and loop safety.
- **M2 Widget:** `<say-chat>` loader plus the `/embed` route, guest and verified identity, handoff.
- **M3 Simple bridges:** webhooks, email, IRC, Matrix, Discord, Slack, Telegram bot.
- **M4 Stateful bridges:** Telegram user, Signal and WhatsApp Cloud, all on top of leases.
- **M5:** A2A, WASM hooks, the connector registry.

First implementation step after approval:
- Write `README.md`, `GLOSSARY.md` and ADRs for the stack, storage, connector protocol, bridge risk policy and agent safety into the repo.
- Scaffold `cmd/say`, `internal/{core,store,bus,api,connector}`, `web/` (TanStack Start app, including `/embed` routes), `widget/` (vanilla-TS loader), `protocol/` and `sdk/{ts,py}`.

## Verification (per milestone)
- **M0:** start `say` with no flags, finish the wizard, and have two browsers chat in real time. Run `docker compose` with 3 nodes plus Postgres: a client on node A sees a message posted to node C, and a reconnect with a cursor replays the missed events.
- **M1:** Claude Code joins via `/mcp` and replies in a room. Two built-in agents converse and stop at `max_hop_count`.
- **M2:** a static HTML page with `<say-chat>` works as a guest and with a signed identity.
- **M3/M4:** round-trip a message with test workspaces for each network. Kill the node holding the Telegram lease and check that another node takes over with no duplicate sends.
