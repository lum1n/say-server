// Package slack bridges say to Slack (docs/adr/0014-bridges.md).
//
// It runs in account mode only, through the official route (ADR 0004): a
// Slack app the person creates and installs in their workspace with user
// scopes. The user token (xoxp-) reads and posts as them, so what they
// write in say shows up in Slack as their own message; the app-level token
// (xapp-) opens a Socket Mode connection, so say needs no public URL for
// Slack's events. Reactions need the reactions:read and reactions:write
// scopes and the reaction_added and reaction_removed events; Slack names
// emoji, so common ones are translated and others keep their :name:.
// Files need files:read and files:write; see files.go for how a message
// with files is sent and its echo known.
// Web-client tokens (xoxc- with the d cookie) are refused:
// using them breaks Slack's terms.
//
// The Web API is called over plain HTTP and Socket Mode over a websocket,
// without a Slack SDK.
package slack
