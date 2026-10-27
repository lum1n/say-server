# say

One self-hostable service for all chat: human↔human, human↔agent,
agent↔agent, and bridged networks.

say has a **native chat core**: principals, rooms and an ordered event log.
Every external network, bot and agent is a *connector* attached to that
core, speaking one protocol. It ships as a single static binary, starts with
zero configuration on SQLite, and scales horizontally by running the same
binary against Postgres with an embedded NATS cluster bus.

> "say" is a placeholder name.

## Status

Early. The core API works: rooms, members, messages, and realtime delivery
over WebSocket, SSE and long polling. The web app has the setup wizard,
sign-in, rooms with live chat, and a page where admins add people and
agents. Agents can join rooms over MCP at `/mcp` or as bots on the
WebSocket, or run inside the server
as built-in agents on Anthropic or any OpenAI-compatible model, within each
room's agent limits. Their replies stream into the room as they are
written. say also runs inside other programs: as a Go package, in iOS and
Android apps, through a C library, and as `say tui` in the terminal.
Sites can also put a chat widget on their pages. People connect their own
accounts on Matrix, Slack, IRC, email, Telegram and Signal, and chat there
from say. See the milestones below.

## Quick start (target)

```sh
curl -fsSL get.<name>.sh | sh && say
# or
docker run -p 7290:7290 -v data:/data say   # after `make image`
```

Either opens a browser setup wizard that creates the admin account.

### Setup wizard

On first start the server logs a link with a one-time setup code:

```
INFO open the setup wizard to create the admin url="http://localhost:7290/setup?code=…"
```

Open it to create the admin. The code means that whoever reaches a fresh
public instance first cannot claim it. Set `--setup-code` (or
`SAY_SETUP_CODE`) to choose the code yourself, for example to give every
cluster node the same one. Once an admin exists the wizard is closed and
people sign in at `/login`. Admins add people and agents on the web app's
People page, or with `POST /api/v1/principals`.

Cluster mode is the same binary:

```sh
say serve --db postgres://… --peers node-a:6222,node-b:6222 --cluster-secret …
```

Every node shares the Postgres database and listens for the others on
`--cluster-addr` (default `:6222`). `--peers` lists other nodes' cluster
addresses; reaching any one live node is enough, since nodes tell each
other about the rest, and a node that finds itself in the list ignores
that entry, so all nodes can share one list. All nodes must use the same
`--cluster-secret`. Each flag also reads `SAY_PEERS`, `SAY_CLUSTER_ADDR`
and `SAY_CLUSTER_SECRET`.

## Building from source

Requires Go and pnpm.

```sh
make build   # builds web/ and widget/, then the Go binary into bin/say
make test    # go vet, Go tests, TypeScript typecheck
bin/say      # listens on :7290, stores data in data/say.db
```

The store tests run against SQLite, and also against Postgres when
`SAY_TEST_POSTGRES` holds a URL; each test gets its own schema:

```sh
SAY_TEST_POSTGRES='postgres://say:say@localhost:5432/say?sslmode=disable' make test
```

With Docker:

```sh
make image          # builds the say image (distroless, data in /data)
make cluster-test   # three nodes and Postgres in docker compose: a client on
                    # one node sees messages posted to another, and replays
                    # what it missed after reconnecting elsewhere
```

The cluster test (`test/cluster`) only talks HTTP, so it also runs against
any cluster you start yourself, as long as every node has the same
`SAY_SETUP_CODE`:

```sh
SAY_CLUSTER_NODES=http://a:7290,http://b:7290,http://c:7290 SAY_SETUP_CODE=… \
  go test -count=1 ./test/cluster
```

To try the API from a script, create an agent and use its token (it never
expires; people sign in through the web app instead):

```sh
bin/say principal add --name Bot --kind agent   # prints the principal id and a token
TOKEN=say_…
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"group","name":"general"}' localhost:7290/api/v1/rooms
curl -N -H "Authorization: Bearer $TOKEN" localhost:7290/api/v1/rooms/<room id>/events/stream
```

The API is described in `protocol/openapi.yaml`.

### Agents over MCP

An agent such as Claude Code joins with an agent token (create one on the
People page or with `say principal add`) and the MCP server at `/mcp`. Add
the agent to a room first; it only sees rooms it is a member of.

```sh
claude mcp add --transport http say http://localhost:7290/mcp \
  --header "Authorization: Bearer say_…"
```

The tools are `list_rooms`, `read`, `send`, `wait_for_messages` and
`create_room`. Replies count toward the room's hop limit
([ADR 0008](./docs/adr/0008-mcp-server.md)).

### Bots

A bot is a program that connects with an agent token and says `hello` on
the WebSocket. The server then sends it the events of every room the agent
is in, says which messages ask it for an answer (DMs, `@name` mentions, and
everything in rooms where it **answers everything**), and keeps its place
in each room, so a restarted bot carries on where it left off. People see
it as online while it is connected
([ADR 0011](./docs/adr/0011-bot-api.md)).

The TypeScript SDK has a `Bot` class for Node 22 or later:

```ts
import { Bot } from "@say/sdk";

const bot = new Bot({ url: "http://localhost:7290", token: "say_…", name: "echo-bot" });
bot.on("message", (msg, ctx) => ctx.reply(`You said: ${msg.text}`));
await bot.start();
```

`ctx.stream()` writes a reply as it goes. `examples/echo-bot` is a whole bot;
run it with `SAY_TOKEN=say_… pnpm --filter @say/echo-bot start`.

### Built-in agents

Admins add built-in agents on the People page: a name, a provider
(Anthropic, or OpenAI and anything that speaks its API, such as Ollama at
`http://localhost:11434/v1`), a model, instructions, the longest reply and
daily budgets. A token budget counts input and output tokens. For a budget
in dollars, enter the model's prices per million tokens under **Spending**
and a limit per day; the People page then shows what each agent spent
today. Leave the API key empty to use the server's `ANTHROPIC_API_KEY` or
`OPENAI_API_KEY`. **Edit** changes any of it, the name included.

Add the agent to a room like anyone else. It answers direct messages and
messages that mention it as `@name`. Tick **answers everything** next to it
in the member panel to make it answer every message in that room. Two
agents that answer everything will talk to each other until the room's
**Replies in a row** limit stops them. In a cluster each agent runs on one
node at a time ([ADR 0009](./docs/adr/0009-built-in-agents.md)).

Under **Tools**, give an agent MCP servers (streamable HTTP) with an
optional `Authorization` header. The model sees their tools as
`name__tool` and may call them before it answers, up to 8 rounds per
reply. say's own `/mcp` works too: with another agent's token, a built-in
agent can read the rooms that agent is in. The reply shows each tool it
called and its arguments, but not what the tool returned.

Replies stream into the room as the model writes them. Any client can
stream a message the same way: send it with `"stream": "streaming"`, then
`PATCH` it with new parts and finally with `"stream": "final"`
([ADR 0010](./docs/adr/0010-streaming-and-edits.md)).

### Agent limits

Every agent send, over REST, WebSocket or MCP, goes through the room's
limits ([ADR 0005](./docs/adr/0005-agent-safety.md)):

- **Pause agents** in the room header stops every agent from sending.
- **Replies in a row** (`max_hop_count`, default 8) caps a chain of agent
  replies; a person's message starts a new chain.
- **Messages from all agents** (default 60 per 10 minutes) and **messages
  per agent per minute** (default 10) catch agents that talk without
  replying to anything. They answer 429 with `Retry-After`.

People who own or run a room set these in its member panel, or with
`PATCH /api/v1/rooms/{id}`. Agents cannot change them, even in rooms they
own. People are never limited.

### Embedding say in apps

A program can run a whole say node inside itself, with no server: rooms,
the log, built-in agents and bots all live in its process and its SQLite
file. It speaks the same REST and WebSocket API as any client, over an
in-memory connection, so nothing listens on a port unless you ask
([ADR 0013](./docs/adr/0013-embedding.md)).

In Go:

```go
node, err := say.Open(ctx, say.Config{DB: "notes.db"})
defer node.Close()
me, err := node.LocalUser(ctx, "ada", "Ada", "my-app") // no setup wizard
c := node.Client(me.Token)
room, err := c.CreateRoom(ctx, "group", "Notes")
c.Send(ctx, room.ID, []say.Part{say.TextPart("hello")}, "")
rt, err := c.Realtime(ctx) // pushes from rooms you Subscribe to
```

`node.Listen("127.0.0.1:0")` also serves the API and the web app on TCP,
for a WebView or for bots. `say.NewClient(url, token)` is the same client
for a remote server.

Other languages:

- **Android:** `make android` builds `build/say.aar` (needs the SDK and
  NDK); **iOS:** `make ios` builds `build/Say.xcframework` on a Mac. Both
  have `Open`, `localUser`, `request` (REST, JSON strings) and `connect`
  (a WebSocket session with a listener).
- **C, Rust, Python, …:** `make libsay` builds `build/libsay.so` (or
  `.dylib`) and `libsay.h`. See `examples/c/notes.c` and
  `examples/python/notes.py` (ctypes).

These builds leave out Telegram accounts, which add about 10 MB: an
account belongs on your home say ([ADR 0015](./docs/adr/0015-sync.md)).
`make android EMBED_TAGS=` keeps them; a Go program leaves them out with
`go build -tags notelegram`.

**Terminal:** `say tui` chats in the terminal. It keeps its own node in
`~/.local/share/say` and signs you in as your login user; `--listen
127.0.0.1:7290` lets the web app and bots in too, and `--server URL --token
say_…` uses a remote server instead. `/new NAME` makes a room, `/dm NAME`
opens direct messages with someone, `/invite NAME` adds someone, Tab moves
to the room list and Ctrl+N/Ctrl+P switch rooms. PgUp at the top of a room
reads earlier messages, and ↑ in an empty input edits your last message.
`/members` shows the room's members and agent settings; owners change them
with `/agents pause`, `/agents resume`, `/agents hops|turns|window|rate N`
and `/responder NAME on|off`.

### Widgets

A widget puts a chat on your own site. Admins add one on the **Widgets**
page: a name, the sites that may embed it (like `https://shop.example`),
who answers (an agent, a team of people, or both) and a greeting. Paste
the snippet it shows into your site:

```html
<script src="https://chat.example.com/widget.js" async></script>
<say-chat widget="pk_…"></say-chat>
```

This shows a chat button in the corner that opens a chat panel. Add
`inline` to fill the element instead, and `open` to start open. Set
`--say-accent`, `--say-bg`, `--say-text`, `--say-font` or `--say-radius`
on the element to theme it. The element fires `open`, `close` and
`unread` (with `detail.count`) events and has `open()`, `close()` and
`toggle()` methods.

Each visitor gets their own thread with the widget's agent and team. The
team finds the threads in the **Inbox** rather than the room list, sorted
into open (the agent answers), handed over (the team's) and idle (no
message for 30 minutes), with the visitors still waiting for an answer
marked and counted. The agent answers until a person on the
team writes in the thread, or until the visitor presses **Talk to a
person**. Visitors are anonymous unless your server vouches for them: sign
a JWT with HS256 and the widget's secret, with `sub` (your user id), `exp`
(at most 24 hours ahead) and optionally `name`, and set it as the
`identity` attribute. A signed-in visitor then has the same thread on
every device, and a visitor who chatted before signing in keeps that chat
([ADR 0012](./docs/adr/0012-widget.md)).

A visitor can press **Delete chat** to erase their thread, their messages
and themselves; the next message starts a new chat. When a visitor asks
you to delete their data, an instance admin can do the same from the
thread's page (**Delete visitor**) or with `DELETE /api/v1/visitors/{id}`.

**Switch off** takes a widget off your sites for a while: they say the
chat is not available and its visitors cannot write, until you switch it
on again. **Delete** removes it for good and signs its visitors out; the
team keeps their threads.

### Accounts on other networks

On the **Accounts** page, anyone connects their own account on another
network. Its chats show up as rooms, people there show up by name, and
what you write in those rooms goes out as you. Nobody else in the room,
person or agent, can write as you; their messages stay in say. Messages
you send from your phone or other apps show up as yours.

| Network | What you need |
|---|---|
| Matrix | Homeserver URL, user ID, and a password or access token |
| Slack | A Slack app with user scopes and Socket Mode: its user token (`xoxp-…`) and app token (`xapp-…`) |
| IRC | Server, nick, channels; SASL or server password if needed |
| Email | IMAP and SMTP servers, address and password |
| Telegram | `api_id` and `api_hash` from my.telegram.org, and your phone number; the code Telegram sends is asked on the page |
| Signal | A current `signal-cli` (the Docker image has one, for amd64 and arm64; otherwise install it on the server, as Signal turns old versions away); scan the QR code on the page with Signal on your phone (Settings → Linked devices). In a cluster its keys move with the bridge, so every node needs `signal-cli` |

Chats active in the last 30 days are brought in with their latest
messages. A room shows its network in the header, and whether it is
connected. If a network is down, what you write is sent when it is back,
if within ten minutes; otherwise the message says it was not sent. A send
that was cut off says it "may not have reached" the network, and is never
repeated by itself ([ADR 0014](./docs/adr/0014-bridges.md)).

**Delete** on the Accounts page logs say out of the account, then removes
it: Matrix logs out the session say made with your password (an access
token you gave is yours and stays logged in), Telegram ends say's
session, and Signal stops say's device and deletes its keys. Signal lets
only your phone remove a linked device, so remove say under Linked
devices there too. The rooms stay, with their messages, no longer
bridged. If the network can't be reached, say keeps trying for ten
minutes, then removes the account anyway.

Only official routes are used: no personal WhatsApp, no Discord user
accounts, no Slack web-session tokens ([ADR
0004](./docs/adr/0004-bridge-risk-policy.md)). The server holds your
credentials and reads these messages; bridged messages are never
end-to-end encrypted, and Matrix's encrypted rooms aren't readable yet.

### Syncing with your home say

A say on your laptop or phone can sync with the say you run on a server,
your home. On the device's **Accounts** page, add **say (home)** with
home's address and your username there, and type your password when it
asks; the device keeps a token of its own instead (`POST
/api/v1/devices`), named after the device. Home's rooms show up as rooms
on the device with the last 30 days in them, at most 500 messages each;
people and agents there show up by name; what you write on the device
goes to home as you, and what you write on the web or another device
shows up as yours. Edits go both ways.

Offline, the device keeps working: what you write is sent, in order,
whenever home is back, however long that takes, and never twice. Rooms
that are not from home stay on the device, and so do the messages of the
device's own agents. **Delete** revokes the device's token on home.
Attachments, reactions and read state don't sync yet ([ADR
0015](./docs/adr/0015-sync.md)).

### Relay bots

Instance admins add relay bots on the same page. A room's owner links the
room to a channel on the other side, and the bot mirrors it both ways:
everyone's messages go out under their names, and people there show up
as members of the room.

| Network | What you need |
|---|---|
| Discord | A bot from discord.com/developers with the Message Content intent, invited with View Channels, Send Messages, Read Message History and Manage Webhooks; link rooms by channel id |
| Telegram bot | A bot token from @BotFather with privacy mode off, added to the group; send `/chatid` there for the id to link |
| Webhooks | A secret of your choosing. Programs post `{"chat": "alerts", "text": "…", "username": "Grafana"}` to the hook URL shown on the page, with `Authorization: Bearer <secret>`. Optionally a URL that say posts the room's messages to, as JSON or in the Slack or Discord webhook format |

After editing `internal/store/queries.sql` or a migration, run
`make generate`.

`go build ./cmd/say` works without the web build; the binary then serves a
placeholder page instead of the app.

## Layout

| Path | What |
|---|---|
| `cmd/say` | The binary: `serve`, `tui`, `principal add` |
| `say/` | The embeddable node (`say.Open`), its in-memory endpoint and a Go client |
| `mobile/` | gomobile bindings of the node for iOS and Android |
| `cmd/libsay` | The node as a C shared library |
| `internal/tui` | The terminal client |
| `internal/core` | Domain model: principals, rooms, events, causation |
| `internal/store` | Storage interface; SQLite and Postgres implementations |
| `internal/bus` | Fan-out bus; in-process and embedded NATS implementations |
| `internal/api` | HTTP, WebSocket and SSE API, bot sessions, and the MCP server at `/mcp` |
| `internal/agents` | Runs built-in agents, one node per agent through a lease |
| `internal/llm` | Model providers for built-in agents: Anthropic and OpenAI-compatible |
| `internal/bridge` | Bridges to other networks: accounts, relay bots, delivery from the log; one package per network |
| `internal/connector` | Connector protocol, capabilities, loopback for first-party connectors |
| `web/` | TanStack Start (React) app in SPA mode, embedded into the binary; includes `/embed` routes |
| `widget/` | Tiny vanilla-TS loader that defines `<say-chat>` |
| `protocol/` | The OpenAPI and JSON Schema contract; source of truth for SDKs |
| `sdk/ts`, `sdk/py` | SDKs generated from `protocol/`; `sdk/ts` also has the `Bot` class |
| `examples/` | An example bot, and libsay from C and Python |
| `docs/adr` | Architecture decision records |

## Design

- [GLOSSARY.md](./GLOSSARY.md) defines the domain terms.
- [ADR 0001: Stack](./docs/adr/0001-stack.md)
- [ADR 0002: Storage and scaling](./docs/adr/0002-storage-and-scaling.md)
- [ADR 0003: Connector protocol](./docs/adr/0003-connector-protocol.md)
- [ADR 0004: Bridge risk policy](./docs/adr/0004-bridge-risk-policy.md)
- [ADR 0005: Agent safety](./docs/adr/0005-agent-safety.md)
- [ADR 0006: Realtime delivery and access tokens](./docs/adr/0006-realtime-and-access-tokens.md)
- [ADR 0007: Setup wizard and sign-in](./docs/adr/0007-setup-and-sign-in.md)
- [ADR 0008: MCP server for agents](./docs/adr/0008-mcp-server.md)
- [ADR 0009: Built-in agents](./docs/adr/0009-built-in-agents.md)
- [ADR 0010: Streaming messages and edits](./docs/adr/0010-streaming-and-edits.md)
- [ADR 0011: Bot sessions on the WebSocket](./docs/adr/0011-bot-api.md)
- [ADR 0012: The chat widget](./docs/adr/0012-widget.md)
- [ADR 0013: Embedding say in apps](./docs/adr/0013-embedding.md)
- [ADR 0014: Bridges — your own accounts first](./docs/adr/0014-bridges.md)
- [ADR 0015: Sync with a home say](./docs/adr/0015-sync.md)

## Privacy

The server reads every message. There is no end-to-end encryption in v1, and
bridged messages are never end-to-end encrypted. Transport is TLS.

## Milestones

- **M0 Core:** principals, rooms, event log, WS/HTTP API and contract; web
  app; SQLite and Postgres; in-process and NATS bus; setup wizard; 3-node
  cluster test in CI.
- **M1 Agents:** bot API, MCP server, built-in LLM agents, loop safety.
- **M2 Embedding (done):** the node as a Go package, iOS and Android
  bindings, a C library and a terminal client. Also a chat widget for
  other sites: `<say-chat>`, guest and verified visitors, handoff.
- **M3 Bridges (done):** your own accounts on Matrix, Slack, IRC, email,
  Telegram and Signal, whose chats become rooms; relay bots for Discord,
  the Telegram Bot API and webhooks.
- **M4:** WhatsApp Cloud.
- **M5:** A2A, WASM hooks, connector registry.

## License

[Apache-2.0](./LICENSE).
