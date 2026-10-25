# ADR 0011: Bot sessions on the WebSocket

Status: accepted (2026-10-24)

## Context

ADR 0003 promises a bot API: the connector protocol with the `agent`
scope, over a WebSocket the bot dials out on. An agent token could already
use `/ws` (`subscribe`, `send`), but a bot built on that had to:

- list its rooms, subscribe to each, and resubscribe on every `rooms` push;
- keep its own cursors, or replay or miss events after a restart;
- work out by itself whether a message is meant for it, repeating the rule
  built-in agents follow (ADR 0005);
- stream a reply through HTTP, because `/ws` had no edit;
- run invisibly: nobody could see whether it was connected, though ADR
  0003 asks for reachability.

## Decision

- **`hello` turns a `/ws` socket into a bot session.** Only agent tokens
  may say it, for the same reason `/mcp` takes only agent tokens: hops are
  counted only for automated senders. Its payload names the bot (`name`,
  `version`, `capabilities`), and its reply carries the agent's principal.
- **The server follows all of the agent's rooms**, and rooms it is added
  to later, on the `rooms` hint and on each heartbeat. `subscribe` and
  `unsubscribe` are refused on a bot session. Events go out as the usual
  `event` and `stream` pushes, after the hello reply.
- **Cursors live on the server.** Each room starts at the agent's stored
  cursor (the `agent_cursors` table that built-in agents use). A room the
  agent never read starts 20 events back, like a built-in agent.
  `ack {room_id, seq}` moves the cursor; cursors only move forward.
  Delivery is at least once: anything not acked comes again after a
  reconnect, so a bot replies with an idempotency key derived from the
  message it answers.
- **The server says which messages ask for an answer.** An event push
  carries `trigger` (`dm`, `responder` or `mention`) on its envelope when
  the message calls on the bot. The rule is `core.Triggers`, the one
  built-in agents use: a complete message from someone else, at most
  5 minutes old, in a room where the agent is not an observer. A streaming
  message triggers when the edit that ends it arrives, and that edit carries
  the trigger; a message read together with its ending edit triggers once.
- **`edit` on `/ws`** is `PATCH /rooms/{id}/messages/{event_id}`, so a bot
  can stream a reply over the one socket.
- **Bots show as online.** A hello writes a `connector_sessions` row, which
  is renewed on every heartbeat and deleted when the socket closes. A row
  that was not renewed in the last two heartbeats plus 10 s does not count,
  and one not renewed for an hour is pruned, so a node that dies leaves no
  bot online for long. `GET /principals` shows a live agent's
  `connector` (name, version, capabilities, connected since). The web app
  shows "online" in the member panel and on the People page, and refetches
  the directory every 30 s, since presence is not pushed.
- Sends and edits go through the same code as REST, so pause, hop limits
  and budgets apply unchanged.
- The TypeScript SDK has a `Bot` class on top of this (handlers per
  message, replies keyed on the message, streaming, acks after the handler
  ends, reconnects), and `examples/echo-bot` uses it.

## Consequences

- Two processes of the same bot both receive every event. Their replies
  post once, because both use the same idempotency key, but both do the
  work. An exclusive session (a lease per agent) can come later.
- The triggers of a bot session cost two indexed reads per complete message
  from someone else (the room and the bot's membership).
- Presence is not pushed. People see a bot go online or offline within
  the directory's refetch interval.
- Built-in agents still call the API in process instead of going through a
  loopback of this protocol, as ADR 0003 intends for first-party
  connectors. They do share the trigger rule and the cursors.
- Typing indicators, outgoing webhooks for HTTP-only bots, the Python SDK
  and the `puppet` scope for bridges are not part of this.
