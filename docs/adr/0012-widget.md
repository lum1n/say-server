# ADR 0012: The chat widget

Status: accepted (2026-10-24)

## Context

Sites embed say as a chat with their visitors: `<say-chat>` on the host
page mounts an iframe on `/embed/{key}` (ADR 0001). Visitors are not
instance users: most are anonymous, and some are signed in to the host
site, which knows who they are and say does not. Someone has to answer
them: an agent, people, or an agent that hands over to people. A widget
must only run on the sites it was made for, and nobody should be able to
read another visitor's chat.

## Decision

- **Widgets** are created by instance admins. A widget has a public key
  (`pk_…`, in the host page's HTML), a secret (`sk_…`, kept on the host
  site's server), the origins allowed to embed it, an optional agent, a
  team of people and a greeting.
- **One thread per visitor.** A visitor is a `guest` principal with its own
  `widget-thread` room. Its members are the visitor, the team as room
  admins and the agent as the room's responder. Changing a widget changes
  new threads only.
- **An inbox, not the room list.** The web app keeps widget threads out of
  the room list and shows them in an inbox (`GET /widget-threads`), latest
  activity first and sorted by state: *open* while an agent is the
  responder, *handed over* once nobody is, and *idle* after 30 minutes
  without a message. A handed-over thread whose visitor wrote last never
  goes idle, so a visitor nobody answered stays in front of the team; the
  sidebar counts those. The states come from the thread's latest message
  and its members' scopes, so they need no new table and stay true
  whichever node or client changed them.
- **Only the listed origins may frame it.** The server sends
  `/embed/{key}` with `Content-Security-Policy: frame-ancestors 'self'
  <origins>`, which the browser enforces; every other page gets
  `frame-ancestors 'self'`. The embed page also takes postMessages only
  from its parent's origin, if that origin is listed, and posts only to
  it. The origin check does not stop a script calling the API directly:
  that only makes it an anonymous visitor, which anyone can be.
- **Tokens, not cookies, inside the iframe.** The iframe is a third-party
  frame, where browsers block cookies. `POST /api/v1/embed/{key}/session`
  returns an access token that lasts 90 days. The page keeps it in
  localStorage, which browsers partition per host site, and sends it as a
  bearer token. Browsers cannot set headers on a WebSocket, so `/ws` also
  takes the token as the subprotocol `say.token.<token>`, offered next to
  `say.v1`; the server answers with `say.v1`, never the token.
- **Sessions are lazy.** The embed page creates a visitor on the first
  message, not on page load, so page views create nothing. It resumes at
  once when it holds a token, and when the host gives an identity.
- **Verified identity** is an HS256 JWT that the host site signs with the
  widget secret: `sub` (its user id) is required, `exp` is required and
  at most 24 hours ahead, `name` and `nbf` are optional. Visitors are
  unique per widget and `sub`, so a signed-in user has one thread on every
  device. A token from an anonymous session, or one for another `sub`, is
  never reused for a verified visitor, and the embed page drops a verified
  token when the host stops giving an identity.
- **Signing in keeps the chat.** An anonymous visitor whose token comes
  with an identity, for a `sub` that has no visitor yet, *becomes* that
  verified visitor: the same guest and thread get the `sub`, the guest
  takes the identity's `name` and the thread is renamed to match. The
  anonymous tokens are revoked and a new one is issued, so the old token
  can't read what the visitor writes later on other devices. Nothing is
  copied between rooms. If the `sub` already has a thread (they signed in
  before, maybe elsewhere), the visitor gets that thread and the
  anonymous one stays with the team as it is: merging two logs would
  replay messages to the agent and reorder both histories. JWTs are checked by hand
  (about 50 lines) and only HS256 is accepted, so `alg: none` and
  algorithm confusion can't happen.
- **Handoff uses the responder scope.** The agent answers every message
  while it is the thread's responder (ADR 0005). A person on the team who
  writes in the thread, or the visitor pressing "Talk to a person"
  (`POST /rooms/{id}/handoff`), takes the scope away; after that the agent
  answers only when mentioned. The scope is cleared *before* the person's
  message is appended, so the agent never sees that message as one to
  answer. The "answers everything" box in the member panel hands the
  thread back.
- **Guests** see only their thread. They cannot create rooms (nor can
  ghosts) or read the directory, so member lists now carry each member's
  name and kind.

- **Visitors can be erased** (added 2026-10-25). The visitor, with its
  own token (`DELETE /embed/{key}/session`), or an instance admin
  (`DELETE /visitors/{id}`), deletes the visitor in one transaction: its
  thread with every event, member, agent cursor and bridge link, the
  messages it wrote in any other room it was added to, its tokens, and
  its guest principal. Nothing is kept, so a verified visitor who comes
  back is a new visitor with a new thread. The transaction first locks
  the thread's row, as an append does, so an agent reply that is being
  written finishes first and one that comes later finds no room.
- **The embed shows every part kind** (added 2026-10-25), like the app:
  text, a tool call as its name and status only (its arguments and errors
  are for the team), and other kinds as a placeholder.

## Consequences

- The widget secret is stored in plain text, like model API keys
  (ADR 0009), because the server must compute signatures with it.
  Rotating it ends identity tokens signed with the old one.
- A visitor who clears storage, or opens the site in another browser,
  becomes a new anonymous visitor. Signing in to the host site is the way
  to keep one thread across devices.
- On a shared browser, an anonymous chat goes to whoever signs in there
  next, as with any site that keeps a guest's cart on sign-in. The host
  site can avoid it by clearing the widget (or not embedding it) before
  sign-in.
- Session creation and guest messages are not rate-limited yet. Before a
  widget goes on a busy public site, they need per-IP limits, like
  sign-in.
- The inbox has no per-person unread counts: "waiting" says only that
  the visitor wrote last, which suits a team sharing threads. Messages in
  threads that are not open are not pushed, so the web app rereads the
  inbox every 15 seconds. It reads the latest 200 threads; older idle ones
  drop off the end.
- Firefox has no `location.ancestorOrigins`, so there the embed page reads
  its parent's origin from the referrer. A host page whose referrer policy
  is `no-referrer` shows "not set up for this site" in Firefox.
- Erasing a visitor cannot reach networks a room was bridged to: a
  message already mirrored there stays there.
- Markdown and attachments show as plain text and placeholders in the
  embed, as they do in the app, until the app renders them.
