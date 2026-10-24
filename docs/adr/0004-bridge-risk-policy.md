# ADR 0004: Bridge risk policy — no ban-risk paths in first party

Status: accepted (2026-10-24)

## Context

Users must not lose their accounts because they connected them to say.
Some networks have no official API; some official APIs are restricted.

## Decision

First-party bridges use only official or sanctioned routes:

| Network | Path | Constraint |
|---|---|---|
| Webhooks, email | HTTP in/out; SMTP out, IMAP or webhook in | None |
| IRC | Relay bot | None |
| Matrix | Bot account; appservice when the user runs a homeserver | E2EE rooms need the bot to handle encryption |
| Discord | A bot the user creates; ghosts rendered via webhooks | Message-content intent needs verification above 100 servers |
| Slack | A Slack app per workspace | Non-Marketplace apps get rate-limited history: realtime works, backfill is limited |
| Telegram bot | Bot API | None |
| Telegram user | MTProto via gotd/td with the instance's own `api_id`/`api_hash` | Respect flood-wait; never bulk messaging; runs under a lease |
| Signal | Linked secondary device via signal-cli as a managed subprocess (JSON-RPC) | No official API; low but non-zero risk; personal use only. GPL/AGPL, so out of process. The Docker image ships signal-cli's native build (amd64), pinned and kept current, since Signal's servers turn old versions away |
| WhatsApp | Business Cloud API only | Needs a business account; per-conversation pricing |

**Excluded from first party:** personal WhatsApp bridging (whatsmeow) and any
other reverse-engineered route that breaks a network's terms. Such
connectors may exist only as community plugins, flagged with a warning in
the registry and the UI.

## Consequences

- Some networks need setup by the user (bot tokens, apps, API ids).
- Features degrade by capability; the UI says so.
- Ghosts are per network; merging identities is never automatic.
