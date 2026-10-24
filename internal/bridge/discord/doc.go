// Package discord bridges say rooms to Discord channels through a relay
// bot (docs/adr/0014-bridges.md).
//
// It runs in bot mode only: Discord has no sanctioned way to act as a
// person, and automating a personal account gets it banned. An admin
// makes a bot application, invites it to a server and links rooms to
// channels by their ids.
//
// The bot reads channels over the Gateway (a websocket) and writes through
// a webhook it owns in each channel, so messages from say show under the
// name of who wrote them. Without the Manage Webhooks permission it posts
// as itself, with the name in bold. Both are called over plain HTTP and a
// websocket, without a Discord SDK.
package discord
