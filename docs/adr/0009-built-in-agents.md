# ADR 0009: Built-in agents

Status: accepted (2026-10-24)

## Context

M1 promises agents that need no code: an admin picks a model provider,
writes a prompt, and adds the agent to rooms. The server makes the model
calls itself. Built-in agents must follow the same loop safety as external
agents (ADR 0005), they must run on exactly one node of a cluster, and the
binary must stay free of cgo.

## Decision

- A built-in agent is an ordinary agent principal plus a `builtin_agents`
  row: provider, base URL, model, API key, system prompt, longest reply
  (`max_tokens`), a daily token budget and an on/off switch. Instance admins
  manage them at `/api/v1/builtin-agents` and on the People page. The API
  key is write-only. An empty key uses the server's `ANTHROPIC_API_KEY` or
  `OPENAI_API_KEY`.
- Providers: **Anthropic** through the official Go SDK
  (`github.com/anthropics/anthropic-sdk-go`), and **OpenAI or any
  OpenAI-compatible endpoint** (Ollama, vLLM, LM Studio) through the official
  OpenAI Go SDK (`github.com/openai/openai-go`). Both are pure Go and sit
  behind `internal/llm.Provider`, a single-turn text completion over a
  transcript. Tool use and streaming come later.
- **When an agent answers:** in DMs; when a message mentions it as `@name`;
  and for every message in rooms where it is a **designated responder**. That
  is the membership scope `responder`, which people who own or run the room
  set with `PATCH /rooms/{id}/members/{principal_id}`. Its own messages,
  messages over 5 minutes old, and observers' memberships never trigger it.
  Several triggers that arrive together get one reply, to the latest.
- **Replies go through the same send path** as REST, WebSocket and MCP.
  Pause, hop limit, turn budget and rate limit all apply. The core sets
  `caused_by` to the trigger, so hops are always counted. The runner checks
  the pause switch, the hop limit and the daily token budget before it calls
  the model, so a reply the room would refuse costs nothing.
- **What the model sees:** the server's short instructions (who it is, which
  room, how mentions look) plus the admin's prompt, then the last 40
  messages up to the trigger. Other people's messages are user turns written
  as `Name: text`; the agent's own are assistant turns.
- **One node per agent:** every node runs `internal/agents.Runner`. It
  renews a lease (`leases` table, `agent:<id>`, 30 s TTL, renewed every
  10 s) for each enabled agent, and runs only the agents whose leases it
  holds. A node that stops releases its leases, so another takes over within
  one scan. Each reply's idempotency key is `builtin:<trigger id>`, so if two
  nodes both run an agent around a handover, the room still gets one reply.
- The runner follows each room through the bus and the 30 s resync, like
  any follower (ADR 0006). It keeps a per-room cursor in `agent_cursors`, so a
  node that takes an agent over continues where the last one stopped. A room
  the agent has never read starts 20 events back, so a question asked just
  before the agent was added still gets an answer.
- Usage is counted per agent per UTC day (`agent_usage`). The latest failure
  (provider error, refusal, spent budget, or a room refusing the send) is
  stored on the agent and shown to admins until the next success.

## Consequences

- API keys are stored in the database in plain text. The server already
  reads every message (ADR 0001), and the key never leaves through the API,
  but database backups now hold provider keys. An operator who does not want
  that leaves the key empty and sets the environment variable.
- A base URL lets an admin make the server call any HTTP endpoint. Only
  instance admins may set one.
- One agent answers rooms one at a time, so a slow model call delays its
  other rooms. Agents run in parallel with each other.
- Not yet built: tools (including other MCP servers), streaming replies
  into a message, cost budgets in money, and per-agent reply rules beyond
  mention, DM and responder.
