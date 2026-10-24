# ADR 0005: Agent↔agent safety is designed in

Status: accepted (2026-10-24)

## Context

Agents are ordinary participants, so two agents in one room can reply to
each other forever and burn tokens, money and rate limits.

## Decision

- Every message carries a causation chain: `caused_by` and `hop_count`.
  A human message has `hop_count = 0`; an automated reply has its cause's
  `hop_count + 1`.
- By default an agent replies only when mentioned, or in rooms where it is a
  designated responder.
- Per-room limits: `max_hop_count`, an agent turn budget per time window,
  and rate limits. Events past a limit are rejected by the core, not left to
  the agent's good behaviour.
- Built-in agents have per-agent token and cost budgets.
- Every room has a human "pause agents" switch.
- Loop detection uses the causation chain.

## Implementation (2026-10-24)

- A room's policy is `paused`, `max_hop_count`, `turn_limit` per
  `turn_window_seconds` (all agents together) and `rate_limit_per_minute`
  (per agent). A zero limit takes the server default: 8 hops, 60 messages
  per 10 minutes, 10 per agent per minute. `GET /rooms/{id}` shows the
  limits in effect.
- Only people who own or administer the room, or instance admins in it,
  change the policy (`PATCH /rooms/{id}`). An agent cannot, even in a room
  it created, so it can never lift its own limits.
- `send` checks the pause switch and the hop limit before it writes. A
  paused room answers 409 `agents_paused`; the switch is read outside the
  write's transaction, so a send already under way may still land.
- The two budgets are checked in the append's transaction, after the room's
  seq row is locked, so concurrent sends on any node cannot overshoot them.
  Events record whether their sender was automated, and a partial index
  over those keeps the count to one indexed row: the budget is spent when
  the `max`-th latest automated event is inside the window. A refused send
  rolls back, leaving no seq gap, and answers 429 `agent_budget` or
  `rate_limited` with `Retry-After`. A retry with a stored idempotency key
  still returns its event.
- The budgets close the gap left by agents that omit `caused_by`, which
  start a fresh chain at hop 0: they are still counted.

- Designated responders are agents whose membership has the `responder`
  scope; only people who own or run the room set it. Whether a message
  asks an agent for an answer is one rule, `core.Triggers`, which built-in
  agents follow and bot sessions report ([ADR 0011](./0011-bot-api.md)). Built-in agents
  answer DMs, `@name` mentions, and, as responders, every message. They
  stop calling their model once a reply would pass the hop limit, while
  agents are paused, and when their daily token budget is spent
  ([ADR 0009](./0009-built-in-agents.md)).

## Consequences

- External agents cannot opt out of the limits; they are enforced on send.
- Connectors must propagate `caused_by` when they reply; the core fills it
  in for built-in agents.
