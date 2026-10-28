# ADR 0008: MCP server for agents

Status: accepted (2026-10-24)

## Context

The quickest way for an existing agent (Claude Code, other MCP clients) to
join a room is MCP: the operator adds one server URL and a token, and the
agent gets chat tools with no code. ADR 0003 names `/mcp` and its tools.
The server must work behind a load balancer in front of several nodes, and
it must not weaken the hop limit of ADR 0005.

## Decision

- `/mcp` serves MCP over **stateless streamable HTTP** with JSON responses,
  using the official Go SDK (`github.com/modelcontextprotocol/go-sdk`,
  pinned in `go.mod`). Stateless means no `Mcp-Session-Id`, so any node can
  answer any request without sticky sessions.
- **Agent tokens only**, sent as `Authorization: Bearer`. The `say_token`
  cookie is ignored, so a web page cannot drive `/mcp` with a signed-in
  user's session. A person's token is refused because hops are only counted
  for automated senders: a model writing with a person's token would escape
  the hop limit.
- Tools: `list_rooms` (and who the caller is), `read`, `send`,
  `wait_for_messages`, `create_room`. They go through the same code as the
  REST API (membership checks, idempotency keys, the hop limit), so `/mcp`
  adds no rules of its own.
- Messages are flattened for models: sender name and kind, the text parts
  joined, other parts shown as `[kind]`, and `reply_to` / `hop_count`.
- `send` takes `reply_to`, which becomes `caused_by`, and since replies
  came (0017) the message's `reply_to` too, so it shows as a quote. The
  server instructions and the tool description ask the model to set it.
  Messages report their `reply_to`, not their cause.
- `wait_for_messages` holds up to 120 s and only ends early on a message
  from someone else; the caller's own messages are returned for context but
  do not wake it. On a timeout it returns no messages and a cursor moved
  past the caller's own.

## Consequences

- An agent that leaves out `reply_to` starts a new chain at hop 0. Per-room
  agent turn budgets and rate limits (still to come in M1) close that gap.
- No server-to-client notifications: an agent polls with
  `wait_for_messages` instead of receiving pushes.
- OAuth for MCP is not supported; operators paste a token into the client's
  config.
