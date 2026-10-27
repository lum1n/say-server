# ADR 0018: Devices, sessions and pairing

Status: accepted (2026-10-27)

## Context

A person reaches their node (ADR 0017) from several devices: the phone
that deployed it, a tablet, a laptop's browser, a node syncing with it
(ADR 0015). Each was signed in with a token, but tokens had no id, so
nobody could see what was signed in as them or sign a lost phone out.
The claim gave the phone a 30-day browser session, so the app would have
been signed out a month later. Adding a second device needed the
password, which the app made up and keeps in the Keychain. A revoked
token also kept any WebSocket it had already opened.

## Decision

- **Every token has an id**, and records the client that asked for it
  (its User-Agent) and when it was last used. The last use is written at
  most hourly, so authenticating stays a read. Older tokens take an id
  from their hash, which is no secret.
- **People manage their own tokens:** `GET /principals/{id}/tokens`
  lists them, marking the one the request came with, and
  `DELETE /principals/{id}/tokens/{token_id}` revokes one. Instance
  admins can do both for anyone, such as revoking an agent's token. The
  web app shows this as **Devices**.
- **Revoking cuts off what is connected.** WebSockets and event streams
  check their token at each heartbeat (25 s). A WebSocket whose token is
  gone closes with **4001**; clients stop reconnecting and sign in
  again. The TS and Go clients do.
- **Devices get tokens that don't expire.** `POST /devices` names a
  token `device:<name>` and replaces an earlier one of that name.
  Claiming a node now pairs the device (`deploy.Claim`, `ClaimNode`)
  and signs out the session setup made, so the phone holds one token
  the person can see and revoke.
- **A pairing code adds a device without the password.** On a
  signed-in device, `POST /pairings` makes a code like `K3X9Q-2M7TA`:
  ten Crockford base32 characters (50 bits), good once, for 10 minutes,
  stored only as its hash. The new device sends it to `POST /devices`
  instead of a username and password. Case, dashes, spaces and O/I/L
  for 0/1 don't matter when typing it. Apps show it as a QR code of
  `<node URL>/pair?code=<code>`: the app scanning it gets the node and
  the code, and a phone camera without the app opens `/pair`, which
  says to use the app. People only; agents use `say principal add`.

## Consequences

- A lost phone is signed out from any other device within half a
  minute, and its pairing codes die with their 10 minutes.
- Guessing a pairing code online means about 2^50 tries in 10 minutes;
  rate limits on unauthenticated routes are still to come (ADR 0007).
- A token's last use can be up to an hour stale.
- Each long-lived connection costs one indexed read per heartbeat.
- `deploy.Claim` and `ClaimNode` take a device name, so their callers
  changed.
