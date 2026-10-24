# ADR 0006: Realtime delivery reads the store; opaque access tokens

Status: accepted (2026-10-24)

## Context

Clients and connectors need live events over WebSocket, SSE and a
long-poll fallback, in every node of a cluster, and must resume after a
reconnect without gaps or duplicates. Every API call also needs to know
who is calling, before the setup wizard and login exist.

## Decision

- **Delivery reads the store.** A follower (one per WebSocket
  subscription, SSE stream or long poll) subscribes to the bus subject
  `room.<id>`, then reads `events WHERE seq > cursor` and advances its
  cursor. A bus message only wakes it. Replay, live delivery and catching
  up after a missed notification are one code path, and order is always
  seq order. Followers also reread every 30 seconds, so a lost
  notification costs latency, not events.
- **Cursors are per room.** WebSocket `subscribe` takes `after`; SSE uses
  the seq as its event id, so `Last-Event-ID` resumes by itself; the long
  poll is `GET /rooms/{id}/events?after=N&wait=S`.
- **Slow clients are dropped, not buffered.** A WebSocket more than 256
  frames behind is closed with 1008 and resumes from its cursors.
- **Access tokens** are opaque secrets (`say_…`). Only their SHA-256 is
  stored. They are sent as `Authorization: Bearer`, or by browsers in the
  `say_token` cookie (a WebSocket or EventSource cannot set headers).
  Cookie use is guarded by requiring `application/json` request bodies and
  by the WebSocket's Origin check. Until the setup wizard exists,
  `say principal add` creates a principal and prints a token.
- Rooms the caller is not in answer 404, the same as missing rooms.

## Consequences

- Every follower costs one indexed query per wake-up. Fan-out to many
  followers of one room repeats the same read; caching the tail of hot
  rooms is a later optimisation that does not change the protocol.
- Tokens do not expire yet. Expiry, revocation and login come with the
  setup wizard (superseded by ADR 0007: session tokens expire after 30
  days and sign-out revokes them).
