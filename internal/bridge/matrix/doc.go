// Package matrix bridges a person's own Matrix account to say
// (docs/adr/0014-bridges.md).
//
// It speaks the client-server API directly over HTTP: it logs in as the
// owner (or uses an access token they give), long-polls /sync, and sends
// messages with transaction ids, so a send that is tried twice is still one
// message. Each joined room becomes a say room; the room id is the chat id
// and the event id is the message id.
//
// Encrypted rooms are not readable yet: their messages arrive as a
// placeholder.
package matrix
