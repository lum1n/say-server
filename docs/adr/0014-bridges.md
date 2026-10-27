# ADR 0014: Bridges — your own accounts first

Status: accepted (2026-10-24)

## Context

say should hold all of a person's chat. The networks they already use
(Matrix, Slack, IRC, email, Telegram, Signal) are where their
conversations are. A relay bot that mirrors a shared room into a channel
is useful, but it is not *their* chat: the people they talk to are on the
other side, and replies must come from them, not from a bot.

`say` v1 and chatty were aggregators of exactly that kind (Signal,
Telegram, Slack, Teams). Beeper does the same with the mautrix bridges,
but accepts ban-risk routes (personal WhatsApp, Discord user accounts,
Slack web-session tokens) that ADR 0004 rules out.

Bridges also need reliable delivery. The `outbox` table written with every
event (ADR 0003) was never read by anything and only grew.

## Decision

- **Accounts first.** A bridge in `account` mode is a person's own account
  on a network. Each chat it sees becomes a room of that person (a
  *portal*), made with its link in one transaction, as messages arrive or
  in a backfill of chats active in the last 30 days. People on the other
  side are ghost principals (one per remote user and bridge, the
  IdentityLink); they join a portal when they first speak.
- **Only the owner writes as the owner.** The owner's messages in a portal
  go out as them, with no name prefix. Other members' messages, people or
  agents, stay in say: nobody, and no agent, can post as someone else. The
  owner's messages from other clients (their phone) come in as the owner,
  not as a ghost.
- **Relay bots second.** In `bot` mode (instance admins only) a bot
  mirrors rooms that room owners link to its channels. It relays
  everyone's messages, showing their names, except its own ghosts'.
- **The log is the outbox.** Each link keeps a cursor over its room's log.
  The node running the bridge reads past it and sends complete messages
  (a stream once it ends) and edits of sent messages. Each outgoing
  message gets a row in `bridge_messages`: `sending`, then `sent` with the
  remote id, `failed` when the network refused it, or `unknown`. A row
  left `sending` by a node that stopped becomes `unknown` and is never
  retried. The `outbox` table is dropped.
- **Offline is shown, not refused.** A message sent while a bridge is
  down is accepted, the room shows the bridge as offline, and it is
  delivered when the bridge is back if it is under ten minutes old;
  older ones become `expired`. A network whose sends are idempotent, such as
  sync with a home say, retries and never expires
  ([ADR 0015](./0015-sync.md)). This replaces ADR 0003's rule of refusing
  sends with 409, which would have blocked native chat in a room while one
  of its networks was down. Delivery states show on the message.
- **Exactly once in.** An incoming message's row is written first, with
  its event id chosen in advance, and the event is sent with the
  idempotency key `bridge:<link>:<remote id>`. A repeat of the remote id
  is ignored, and a crash between the row and the event is finished on
  the next delivery. Delivery and receiving on a link take one lock, so
  the network's echo of a message being sent is only read once its remote
  id is recorded and is recognized as ours.
- **One node per bridge.** Each enabled bridge is held through the lease
  `bridge:<id>` (as built-in agents are). The holder runs the network
  connection, delivery and a heartbeat (`seen_at`); a bridge is live while
  it was seen in the last 30 seconds.
- **Login steps go to the owner.** A network that needs a code, a password
  or a QR code to be scanned asks for it with `Host.Ask`. The prompt is
  stored on the bridge and shown on the Accounts page; the answer
  (`POST /bridges/{id}/answer`) reaches the holder over the bus.
- **Settings are per network, secrets write-only.** Each network declares
  its fields; secrets are stored in plain text (the server needs them, as
  with built-in agents' keys) and never returned.
- **In process, not over the connector protocol.** Networks are Go
  packages under `internal/bridge/` that the node runs directly. ADR
  0003's dogfooding of the connector protocol for first-party bridges is
  deferred: the bot WebSocket has no puppet scope yet, and every bridge
  needs the lease, the cursors and the delivery rows that live in the core.
  Out-of-process bridges can come with the connector registry (M5).

- **Deleting logs out, and keeps the rooms.** `DELETE /bridges/{id}`
  marks the bridge (`deleted_at`) and it shows as `deleting`. The node
  that takes its lease stops it, calls the network's `LogOut` with the
  stored config and state, and deletes the bridge, its links, their
  delivery rows and its ghosts' mappings in one transaction. A logout
  that fails shows its error and is tried again each minute; after ten
  minutes the bridge is deleted anyway, leaving the session for the
  person to end on the network. Portal rooms and linked rooms stay as
  ordinary rooms with their logs, and ghosts stay as their senders: the
  history is the person's, and they can leave or delete those rooms
  themselves. Adding the account again makes new portals and ghosts.
  - Matrix logs out the session say made with the password, which
    removes its device. An access token given in the settings is the
    person's own session and is left alone.
  - Telegram ends say's MTProto session (`auth.logOut`).
  - Signal can't remove a linked device from the device itself (only the
    primary can), so say runs signal-cli's `unregister`, which stops
    messages being queued for it, then deletes its keys and data. The
    device stays under Linked devices on the phone until the person
    removes it; the Accounts page says so.
  - Slack, IRC, email and the relay bots hold no session say made: their
    tokens and passwords are the person's or the admin's, and deleting
    only forgets them.

### Networks (account mode)

| Network | Route | Notes |
|---|---|---|
| Matrix | Client-server API with the user's login | No E2EE yet: encrypted rooms show a placeholder |
| Slack | A Slack app with user scopes: user token + Socket Mode app token | Posts as the user; no web-session tokens |
| IRC | The user's own nick, SASL | Channels from settings; private messages become DMs |
| Email | The user's mailbox over IMAP and SMTP | One room per correspondent set; threading headers |
| Telegram | MTProto (gotd/td) with the instance's own `api_id`/`api_hash` | Login code and 2FA through prompts; flood-wait respected |
| Signal | signal-cli as a linked device, out of process (GPL) | QR prompt; no history to backfill; its data moves with the lease |

Discord has no sanctioned way to act as a user, so it is a relay bot
only, as are the Telegram Bot API and webhooks.

### Networks (bot mode)

| Network | Route | Notes |
|---|---|---|
| Discord | A bot the admin creates, over the Gateway and REST | People in say show under their own names through a channel webhook named "say" (Manage Webhooks), else as "**Name**: text"; missed messages are fetched on reconnect |
| Telegram bot | Bot API with long polling | Privacy mode off to see all group messages; `/chatid` tells the chat id to link |
| Webhooks | `POST /api/v1/hooks/{bridge id}` in; a POST to the admin's URL out | Out in say's JSON, or the Slack or Discord webhook format; optional HMAC signature |

A network may also take messages over HTTP (`Network.Hook`). Any node
answers the hook: receiving needs only the store, so the request does not
have to reach the node holding the bridge's lease. In a relay bot every
message must be for a linked chat, or none is taken.

A network that hands back a remote id the link already has (a webhook
that always answers the same id) gets its send kept as sent, without the
id.

## Consequences

- Portals are ordinary rooms: agents can be added, search and the TUI work
  on them, and only their owner's messages leave say.
- A send cut off by a crash shows "may not have reached" and is never
  repeated; the person decides.
- signal-cli keeps the Signal account's keys on the node's disk. So that
  another node taking the lease does not link a new device, the bridge
  copies signal-cli's `data/` into its state (gzipped, at most 16 MB)
  each time signal-cli stops, including right after linking, and a node
  whose directory does not have the latest copy restores it first. A node
  holds the lease until its connection has stopped, so a handover gets
  the copy taken on the way out. A node that dies leaves the copy from
  signal-cli's last stop; Signal repairs sessions that moved on since, as
  after a restored backup. A data directory set in the bridge's settings
  may hold other accounts and is not copied: it stays on its node.
- gotd/td adds about 25 MB to the binary.
- Bridged messages are never end-to-end encrypted, and the server holds
  every account's credentials and sessions. The Accounts page says so.
