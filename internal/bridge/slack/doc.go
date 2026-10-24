// Package slack bridges say to Slack (docs/adr/0014-bridges.md).
//
// It runs in account mode only, through the official route (ADR 0004): a
// Slack app the person creates and installs in their workspace with user
// scopes. The user token (xoxp-) reads and posts as them, so what they
// write in say shows up in Slack as their own message; the app-level token
// (xapp-) opens a Socket Mode connection, so say needs no public URL for
// Slack's events. Web-client tokens (xoxc- with the d cookie) are refused:
// using them breaks Slack's terms.
//
// The Web API is called over plain HTTP and Socket Mode over a websocket,
// without a Slack SDK.
package slack
