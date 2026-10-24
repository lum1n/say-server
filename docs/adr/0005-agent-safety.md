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

## Consequences

- External agents cannot opt out of the limits; they are enforced on send.
- Connectors must propagate `caused_by` when they reply; the core fills it
  in for built-in agents.
