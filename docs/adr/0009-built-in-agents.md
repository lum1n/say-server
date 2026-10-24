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
  behind `internal/llm.Provider`, which completes a transcript in which the
  model may call tools. Replies stream into their message
  ([ADR 0010](./0010-streaming-and-edits.md)).
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
- **Tools come from MCP servers** the admin lists on the agent (name,
  streamable HTTP URL, optional `Authorization` header; at most 8,
  `builtin_agents.mcp_servers`). The tool loop is our own, over both
  provider SDKs, through the official MCP Go SDK client: no agent framework.
  For each reply the runner opens every server at once and lists its tools,
  which the model sees as `name__tool`. It then calls the model, runs the
  tools it asks for one by one, and sends the results back, up to 8 rounds.
  The reply is the model's first turn without tool calls. Every round
  checks the daily token budget and records usage, and after tools ran it
  rereads the room, so a pause stops the loop. A tool that fails, a tool
  that does not exist, or arguments that are not JSON go back to the model
  as error results. A server that cannot be reached is left out, the agent
  answers without it, and the error is stored for the admin. The header is
  write-only like the API key, and it is kept on an update only while the
  server's name and URL stay the same, so it is never sent to a new URL.
- **Tool use shows in the reply.** Each round's text is a text part,
  followed by a `tool-call` part for each tool it called (`id`, `name`,
  `input`, `status` running/done/failed, `error`). A call shows as running
  while it runs, and the message starts streaming with the first text or
  call. Only the outcome goes into the room, never the result: a tool may
  read what the room's members cannot see, such as another room through
  say's own `/mcp`. Inputs over 4 KiB or not a JSON object are left out,
  errors are cut at 500 bytes, and one reply shows at most 20 calls. A model
  stopped at the round limit leaves a reply with only its calls. A node
  that ends another's leftover reply marks its running calls failed. In
  the model's history the agent's own messages are their text only, so it
  does not learn to write calls out.
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
- An MCP URL lets an admin make the server call any HTTP endpoint, like a
  base URL. Tool headers are stored in plain text, like API keys.
- Tools are called again if a reply is retried: around a lease handover two
  nodes may both run a reply's tools, even though the room gets one
  message. Tools with side effects should be idempotent.
- Opening the servers for every reply costs a handshake per server per
  reply, but no MCP session outlives the reply or the node.
- People see which tools an agent called and with what arguments, but not
  what the tools returned. Arguments may still carry something private the
  model chose to pass on.
- Not yet built: tools from say itself (without a token over `/mcp`), cost
  budgets in money, and per-agent reply rules beyond mention, DM and
  responder.
