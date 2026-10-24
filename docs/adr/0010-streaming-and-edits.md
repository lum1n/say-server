# ADR 0010: Streaming messages and edits

Status: accepted (2026-10-24)

## Context

A model takes seconds to write a reply. People should see it as it is
written, not after it is done. The domain model already has a stream state
on messages (`streaming → final`) and an `edit` event type (GLOSSARY), but
every send was final and nothing could change a message.

Logging every chunk as an event would fill the room's log with dozens of
edits per reply. Each would take a seq, reach every connector through the
outbox, and be replayed to every client that reconnects. Chunks are only
worth something while the message is being written.

## Decision

- **A message may start streaming.** `POST /rooms/{id}/messages` (and the
  WebSocket `send`) take `stream: "streaming"`. Such a message may start
  without parts. Starting it is a send like any other: it takes a seq,
  counts toward the agent budgets and the hop limit, and is refused while
  agents are paused.
- **Streaming updates are not logged.** `PATCH
  /rooms/{id}/messages/{event_id}` with parts replaces a streaming
  message's parts in its row. Nothing is appended to the log, and no seq is
  used. The message, as it now is, goes out on the bus subject
  `room.<id>.stream`. WebSocket subscribers get it as a push of type
  `stream`, and SSE readers as `event: stream` without an id. These frames
  are not replayed, and a slow client may miss some: each one carries the
  whole message, so the next frame makes up for a lost one.
- **The end of a stream is an edit event.** `PATCH` with
  `stream: "final"`, with or without new parts, appends an `edit` event
  that names the message in `edits` and carries its whole final parts, with
  `stream: "final"`. In the same transaction the message's row becomes
  final. A client that missed every frame, or replays from a cursor, gets
  the finished message from that edit.
- **Final messages can be edited.** A `PATCH` with parts on a final message
  appends an `edit` (without `stream`) and sets the message's `edited_at`.
  A final message cannot stream again (409 `not_streaming`). Only the
  sender may edit, and only messages.
- **Rows hold the latest content.** Because the message row is updated,
  reading the log (pages, replay, MCP `read`, an agent's history) always
  shows each message as it now is. Clients apply edit events only when they
  arrive live and in order. In a page read over HTTP the messages are
  already current.
- **Edits are not turns.** Edits do not count against agent budgets or
  rate limits, and are allowed while agents are paused, so a reply under
  way can finish. Only the message that started the stream was checked.
- **Answering waits for the end.** A built-in agent ignores messages that
  are still streaming and treats the edit that ends a stream as the
  message arriving. MCP `wait_for_messages` does the same, and `read`
  marks unfinished messages `streaming: true`. The edit commits together
  with the message becoming final, so a reader that sees a final message
  also sees its ending edit in the same pass. An agent therefore answers
  once.
- **Built-in agents stream.** Both providers stream (`llm.Request.OnText`).
  The reply message starts with the first text, so an empty answer still
  sends nothing, and is written at most every 250 ms. After a round of
  tool calls the next round's text is added below the earlier text. On a
  failure the message ends with what it has. When an agent starts on a
  node, it ends any of its messages still streaming, which a crashed node
  or a lost lease left behind.

## Consequences

- Each streaming update is a database write and a bus message. Agents
  write at most four times a second. External senders are not throttled
  yet.
- Frames carry the whole message, so a long reply sends roughly n²/2 bytes
  in total over its frames. That is simple and robust to loss, and cheap at
  chat lengths.
- A message from an external sender that never ends its stream stays
  `streaming`; only built-in agents are cleaned up. Agents do not answer
  it.
- Connectors see the start of a stream and its end through the outbox,
  never the chunks. That suits networks that cannot edit often.
- There is no edit history: the log keeps each edit's parts, but the UI
  shows only the latest.
