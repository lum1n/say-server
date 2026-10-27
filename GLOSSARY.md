# Glossary

**Principal**
: Anything that can send an event. Kinds: `human`, `agent`, `guest`
  (widget visitor), `ghost` (a remote user seen through a bridge),
  `system`. All kinds send through the same path; there is no special case
  for agents.

**Admin**
: A principal that manages the instance: it adds people and agents. The
  first admin is created by the setup wizard. Not the same as a room's
  `admin` role.

**Setup code**
: The one-time code the server logs on first start. The setup wizard needs
  it to create the first admin.

**Session**
: An access token issued by sign-in, carried in the `say_token` cookie and
  valid for 30 days. Signing out revokes it.

**Room**
: A conversation. Kinds: `dm`, `group`, `channel`, `widget-thread`.

**Membership**
: A principal's place in a room, with a role (`owner`, `admin`, `member`,
  `observer`) and scopes.

**Event**
: One entry in a room's log, identified by `(room, seq)`. `seq` is
  allocated by the database and increases monotonically per room. Message,
  edit, reaction, typing and receipt are event types.

**System event**
: An event that records a change to the room rather than a message:
  `agent-settings` (such as pausing agents) and `handoff` (such as a human
  taking over a widget thread from an agent). Its sender is who made the
  change, so the log shows who did it and when.

**Part**
: A typed piece of a message event: text/markdown, attachment,
  tool-call/result, card/form. A message has a streaming state, `streaming`
  then `final`.

**Streaming message**
: A message whose parts change in place, unlogged, while its sender writes
  it. An `edit` event ends the stream and makes it final.

**Edit**
: An event that changes a message, named in its `edits`, to new parts. The
  message itself always shows its latest parts.

**Cursor**
: The last `seq` a client or connector has seen in a room. Reconnecting with
  a cursor replays everything after it.

**Causation chain**
: `caused_by` (the event that triggered this one) and `hop_count` (how many
  automated hops separate it from a human). Used to stop agent loops.

**Connector**
: A participant that is not a person at a keyboard: a bridge, a bot or an
  agent. It has **capabilities** (what it can do: edits, reactions,
  threads, …) and scoped credentials. First-party connectors run in process
  over a loopback of the same protocol.

**Scope**
: A permission granted to a connector credential, such as `agent` or
  `puppet` (post as ghosts).

**Built-in agent**
: An agent principal the server runs itself by calling a model provider
  (Anthropic, or OpenAI-compatible). It answers DMs and `@name` mentions,
  and every message in rooms where it is a responder. It may call tools
  from the MCP servers its admin gave it; each call shows in its reply as a
  `tool-call` part, without the tool's result.

**Responder**
: An agent whose membership in a room has the `responder` scope. A
  built-in agent that is a responder answers every message in the room,
  and a bot is triggered by every message there.

**Bot**
: A program that takes part as an agent over a bot session.

**Bot session**
: A `/ws` socket whose agent said `hello`. The server follows all the
  agent's rooms from its stored cursors, marks the events that trigger it,
  and shows the agent as online while the socket is open.

**Trigger**
: Why a message asks an agent for an answer: `dm`, `responder` or
  `mention`. Only recent messages from others trigger, never in rooms where
  the agent observes. Built-in agents answer triggers; bots are told them.

**Ack**
: A bot saying it has handled a room's events up to a seq. It moves the
  bot's stored cursor, so events after it come again after a reconnect.

**Bridge**
: A connector that mirrors rooms to an external network. In `account` mode
  it is a person's own account there; in `bot` mode, a relay bot an admin
  runs.

**Account**
: A bridge in account mode: someone's own account on another network. Its
  chats become rooms of that person, and only their messages go out, as
  them.

**Portal**
: A room that mirrors one chat of an account, made when the chat is first
  seen.

**BridgeLink**
: Ties one room to one remote chat or channel on one bridge, with the
  cursor its delivery has reached.

**Relay bot**
: A bridge in bot mode: a bot on Discord or Telegram, or webhooks, that an
  admin adds. Rooms are linked to its channels, and it relays everyone's
  messages under their names.

**Hook**
: The URL `/api/v1/hooks/{bridge id}` where programs post messages to a
  bridge that takes them over HTTP, such as webhooks. Any node takes them,
  up to a limit a minute per bridge.

**IdentityLink**
: Maps a remote user on a network to a ghost principal. Merging ghosts into
  one person is manual or opt-in.

**Ghost**
: A principal for someone on another network, seen through a bridge. It
  never signs in.

**Prompt**
: A login step a bridge needs from its owner: a code, a password, or a QR
  code to scan. It shows on the Accounts page until answered.

**Reachability**
: A connector's state. For bots it is shown as "online" while a bot
  session is open. A bridge is `live`, `login` (waiting for a prompt),
  `error`, `offline` or `disabled`; messages sent while it is down go out
  when it is back, within ten minutes.

**Lease**
: A database row that grants one node the right to run something stateful,
  such as a built-in agent or a Telegram user session, until it expires.
  Its generation counter goes up each time it changes hands, so a stale
  holder can be fenced.

**Unknown (send state)**
: A send whose outcome could not be confirmed. It is never retried
  automatically.

**Delivery**
: How a message went out to a bridge: `sending`, `sent`, `failed` (the
  network refused it), `unknown` (cut off; never retried) or `expired`
  (the bridge was offline for more than ten minutes). The room log is the
  outbox: each link reads it from a cursor.

**Node**
: One running say: store, bus, API and built-in agents. `say serve` runs
  one as a server; an app runs one inside itself with `say.Open`.

**Embedded node**
: A node inside another program, reached over an in-memory connection
  rather than a port.

**Home**
: The say that holds a person's chat for all their devices, usually a
  server. A device syncs with it through a `say` bridge as the person, and
  home's rooms become portals there.

**Device token**
: A token home issues to one paired device, named `device:<name>`. It
  does not expire; revoking it unpairs the device.

**Local user**
: The person at the device of an app that embeds a node. `LocalUser`
  creates them without the setup wizard and gives each client of theirs
  one token.

**Widget**
: An embeddable chat on a third-party site: the `<say-chat>` element, which
  mounts an iframe on `/embed/{key}`. Admins create widgets; each has
  allowed origins, an optional agent, a team and a greeting.

**Public key**
: A widget's `pk_…` identifier, in the host page's HTML. Not a secret.

**Widget secret**
: A widget's `sk_…` key, kept on the host site's server, which signs
  identity tokens.

**Visitor**
: A guest principal that chats through a widget, with its own widget
  thread. Anonymous, or verified by an identity token; an anonymous
  visitor who signs in becomes verified and keeps its thread.

**Widget thread**
: A `widget-thread` room with one visitor, the widget's team and its agent.

**Identity token**
: A short-lived HS256 JWT, signed with the widget secret, in which the host
  site says who its signed-in user is (`sub`, optionally `name`). The same
  `sub` is the same visitor on every device.

**Handoff**
: The agent in a widget thread stops answering every message, because a
  person on the team wrote there or the visitor asked for a person.

**Inbox**
: The widget threads a person is in, kept out of their room list. A
  thread is *open* while an agent answers it, *handed over* after a
  handoff (or from the start, for a widget without an agent) and *idle*
  after 30 minutes without a message. A handed-over thread whose visitor
  wrote last stays handed over: the visitor is *waiting*.
