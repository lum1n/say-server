# ADR 0013: Embedding say in apps

Status: accepted (2026-10-24)

## Context

say should be embeddable: it should run *inside* a mobile app, a terminal
UI or any other program, not only as a server that clients reach over the
network. (ADR 0012's web widget is a different thing: a chat on someone
else's web page.) An app that embeds say wants the whole core in its own
process (rooms, the log, agents, bots), without a server or a setup
wizard, and from whatever language it is written in.

## Decision

- **The whole core runs in the app.** Package `say` (`say/`) is the node:
  `say.Open` starts the store, bus, API and built-in agent runner, the same
  wiring `say serve` had. `say serve` is now built on it, so the server
  and every embedder run the same code, cluster options included.
- **One contract, in memory.** Embedders speak the REST and WebSocket
  protocol of `protocol/`, as remote clients do. The node serves its HTTP
  handler on an in-memory listener built from `net.Pipe`, and
  `Node.HTTPClient` dials it. So REST, SSE, long polling and the WebSocket
  all work unchanged, with no OS socket: nothing for other local apps to
  reach, and nothing for iOS's sandbox to object to. No second API exists
  to keep in step with the first.
- **A port only on request.** `Node.Listen(addr)` also serves the node on
  TCP: for a WebView that shows the web app, or for bots and MCP clients
  on the same machine.
- **No wizard for single-user apps.** `Node.LocalUser(username, name,
  client)` creates the person at the device (the admin, if first) with a
  random password and returns a token that never expires. It replaces the
  token it last issued to the same client, so an app may call it on every
  start without piling up tokens.
- **Bindings are strings.** Every other language gets JSON strings in and
  out, over the same API:
  - `Node.Request(method, path, token, body)` and `Node.Connect(token)`
    (envelopes over a session) in Go;
  - `mobile/` (package `say`) for `gomobile bind`: an `.aar` for Android
    (`make android`) and an `.xcframework` for iOS (`make ios`, on macOS),
    with a `Listener` callback interface for sessions;
  - `cmd/libsay`, a C shared library (`make libsay`) with integer handles,
    caller-freed strings and `say_recv`, which the caller polls. A Go
    thread never calls into a foreign runtime, which keeps Python, Rust and
    others simple. It is the only cgo build; the server stays
    `CGO_ENABLED=0`.
- **A typed Go client.** `say.Client` (with `Realtime`, which keeps cursors
  and reconnects) works on an embedded node or a remote server alike.
  `say tui` is built on it: by default it embeds a node in the user's data
  directory and signs in as the login user, and with `--server` and
  `--token` it is a remote client. `--listen` lets the web app and bots in
  while the TUI runs.

## Consequences

- Sizes: the C library is about 50 MB and the Android `.aar` about 30 MB
  for two ABIs, mostly the SQLite translation and the model SDKs. Raw HTTP
  for providers (an open todo) would cut both.
- `net.Pipe` has no buffer. A client that holds a lock while it writes,
  which the read loop needs to take the reply, deadlocks where TCP would
  hide the problem. `say.Realtime` writes outside its lock for this
  reason.
- An embedded node is its own instance. Syncing it with a remote say
  (offline first, or one person's devices) is not designed yet; a
  connector that bridges two instances is the likely shape.
- Built-in agents run in the app too. On a phone, background limits stop
  them when the app sleeps.
- The iOS build has not been checked: it needs a Mac. The Android `.aar`
  ran on an emulator (API 35, x86_64).
