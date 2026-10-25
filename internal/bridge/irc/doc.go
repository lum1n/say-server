// Package irc bridges say to IRC in account mode (docs/adr/0014-bridges.md).
//
// The bridge is the person's own IRC client, like a bouncer: it connects
// with their nick, rejoins their channels on every connect, and turns each
// channel into a group room (chat id: the lowercased channel name) and
// each private conversation into a direct room (chat id: the other nick,
// lowercased).
//
// Own messages. The bridge requests echo-message together with
// labeled-response and batch. Every line it sends carries a label; the
// server answers with the echo of that line, labeled, which carries the
// line's msgid, so Send returns the id the server gave and the same
// message read back later through CHATHISTORY is recognized. Labeled
// echoes go to the waiting Send, never to the receive path, so a message
// sent from say is not received again. If the server acks echo-message but
// not labeled-response the bridge turns echo-message off again: an
// unlabeled echo cannot be told apart from a line the person wrote in
// another client. Without echoes Send returns "say:" + the event id, and
// the person's own lines in backfilled history are skipped, as they cannot
// be matched to what say sent. Lines from the person's nick that arrive
// unlabeled (another client on the same account, as ergo's multiclient
// sends them) come in as their own messages. A line is the owner's when
// its account tag names the account the bridge logged in to; without an
// account tag, when it comes from the current nick or the configured one
// (which history written before a nick collision carries).
//
// A message split over several lines comes back as several lines in
// history; Send returns the first line's id, and the ids of all sent lines
// are kept per chat (the last 100, in the saved state) so history lines
// that match them are skipped.
//
// Ids. A message's id is its IRCv3 msgid tag; without one it is a hash of
// its server-time (or the time it arrived), sender, target and text.
//
// Backfill. When the server has CHATHISTORY (ergo does), the bridge reads
// the last 20 lines of each joined channel and of each private
// conversation that had activity in the last 30 days, and after a
// reconnect the lines since the last one it saw. The time it last saw a
// line in each chat is its saved state. The times are read before the
// connection starts, and a chat's time is saved no later than that until
// its catch-up is received, so a live line during the catch-up cannot
// skip what was missed.
//
// IRC has no edits, so the bridge does not implement bridge.Editor.
package irc
