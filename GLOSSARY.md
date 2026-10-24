# Glossary

**Principal**
: Anything that can send an event. Kinds: `human`, `agent`, `guest`
  (anonymous widget visitor), `ghost` (a remote user seen through a bridge),
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

**Part**
: A typed piece of a message event: text/markdown, attachment,
  tool-call/result, card/form. A message has a streaming state, `streaming`
  then `final`.

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
  and every message in rooms where it is a responder.

**Responder**
: An agent whose membership in a room has the `responder` scope. A
  built-in agent that is a responder answers every message in the room.

**Bridge**
: A connector that mirrors rooms to an external network.

**BridgeLink**
: Ties one room to one remote channel on one bridge.

**IdentityLink**
: Maps a remote user on a network to a ghost principal. Merging ghosts into
  one person is manual or opt-in.

**Reachability**
: A connector's state: `live`, `offline` or `unlinked`. Sending to an
  offline connector fails with `409`.

**Lease**
: A database row that grants one node the right to run something stateful,
  such as a built-in agent or a Telegram user session, until it expires.
  Its generation counter goes up each time it changes hands, so a stale
  holder can be fenced.

**Outbox**
: Commands written in the same transaction as the message that caused them,
  delivered afterwards.

**Unknown (send state)**
: A send whose outcome could not be confirmed. It is never retried
  automatically.

**Widget**
: An embeddable chat on a third-party site: the `<say-chat>` element, which
  mounts an iframe on `/embed/$widgetId`.

**Publishable key**
: The public identifier of a widget, checked against its allowed origins.

**Verified identity**
: A short HMAC-signed JWT from the host site carrying its own user id, which
  maps to a principal.
