# ADR 0003: One connector protocol for bridges, bots and agents

Status: accepted (2026-10-24)

## Context

Bridges, bots and agents all read and write rooms. Separate APIs for each
would multiply the surface and the bugs. Connectors must be writable in any
language and runnable behind NAT.

## Decision

- Connectors **dial out** to the hub over WebSocket, so they need no public
  endpoint. Frames are JSON envelopes `{id, type, status, payload}`, defined
  in `protocol/`.
- Every connector declares **capabilities** (edits, reactions, threads,
  typing, attachments, streaming, …). The core degrades features per
  capability and shows "bridged" fallbacks in native rooms.
- Credentials are **scoped**. Bridges get the extra `puppet` scope to post as
  ghost principals. Agents get the `agent` scope.
- First-party connectors use the **same protocol** over an in-process
  loopback, so the protocol is always dogfooded.
- Agent surfaces on top of the protocol:
  - HTTP/WS bot API (the connector protocol with the `agent` scope), plus SSE
    for simple bots and token streaming. Bot sessions on `/ws` are
    [ADR 0011](./0011-bot-api.md).
  - MCP server at `/mcp`: `list_rooms`, `read`, `send`,
    `wait_for_messages`, `create_room`.
  - Built-in LLM agents (Anthropic, OpenAI or any OpenAI-compatible
    endpoint), whose tools may include other connected MCP servers.
  - A2A: every agent principal gets an Agent Card and endpoint; external A2A
    agents can be invited into rooms.
  MCP and A2A sit behind adapters with pinned versions, because both specs
  are still moving.
- Outgoing webhooks for any event. WASM hooks (wazero) come in phase 2.

## Reliability rules

- Client idempotency keys on every send.
- Commands are written to an outbox in the same transaction as the message.
  *Superseded by [ADR 0014](./0014-bridges.md): the room log is the outbox,
  read by each bridge link from a cursor.*
- Inbound bridged messages are deduplicated on `(link, remote_msg_id)`.
- A send with an uncertain outcome becomes `unknown` and is never
  auto-retried.
- Connector reachability (`live`, `offline`, `unlinked`) is visible.
  Sending to an offline connector returns `409` instead of queueing silently.
  *For bridges, [ADR 0014](./0014-bridges.md) replaces this: the room shows
  the bridge as offline, and messages under ten minutes old go out when it
  is back.*
- Push notifications carry no message content.
- Retention is configurable per room or instance.
