// Package telegrambot is a relay bot on Telegram through the Bot API
// (docs/adr/0014-bridges.md). It runs in bot mode only: an admin makes a
// bot with @BotFather and room owners link rooms to the groups and
// channels it is in, by chat id. Messages from say go out as the bot,
// with the sender's name in bold.
//
// It long-polls getUpdates, so say needs no public URL, and keeps the
// update offset in the bridge's state so another node goes on where the
// last one stopped. The API is called over plain HTTP without an SDK.
//
// Chat ids are Telegram's (-1001234567890 for a supergroup), or a chat's
// and a forum topic's joined by a slash (-1001234567890/42) for a room
// linked to one topic; message ids are Telegram's, which are unique
// within a chat. The account driver for
// a person's own Telegram account is package telegram.
package telegrambot
