// Package telegram bridges say to a person's own Telegram account
// (docs/adr/0014-bridges.md). It speaks MTProto through gotd/td with the
// instance's own api_id and api_hash, which Telegram sanctions for third
// party clients (docs/adr/0004-bridge-risk-policy.md): it waits out flood
// waits and only sends what the owner writes.
//
// Chat ids are "user:<id>" for private chats, "chat:<id>" for basic groups
// and "channel:<id>" for supergroups and channels; message ids are
// Telegram's, which are unique within a chat.
package telegram
