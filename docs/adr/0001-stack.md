# ADR 0001: Go core, single binary, TanStack Start SPA

Status: accepted (2026-10-24)

## Context

say must be dead simple to install (one download, no dependencies), run on
every OS, scale horizontally, and be easy for the community to extend. The
hard parts are a cluster bus, an MTProto client for Telegram user mode,
bridge libraries, sandboxed plugins, automatic TLS, MCP and A2A.

## Decision

- **Go** for the core, built as one static binary with no cgo.
- Embedded **NATS** for the cluster bus; **gotd/td** for MTProto;
  mautrix-go, discordgo, slack-go and ergochat/irc-go for bridges;
  **wazero** for WASM plugins; **certmagic** for TLS; `modernc.org/sqlite`
  for SQLite; the official MCP, A2A, Anthropic and OpenAI Go SDKs.
- **Web app:** TanStack Start (React) in SPA mode. The static build is
  embedded with `go:embed`; the Go core serves the API. No server functions
  or SSR, because they would need a Node or Bun runtime next to the binary.
  Data goes through the generated TS SDK and TanStack Query, and WebSocket
  events are pushed into the Query cache.
- **Widget:** a tiny vanilla-TS loader (`/widget.js`) defines `<say-chat>`,
  which mounts an iframe on `/embed/$widgetId` of the same app and talks to
  it over postMessage.
- **Contract:** one OpenAPI and JSON Schema definition in `protocol/`. The
  TS and Python SDKs are generated from it.
- **License:** Apache-2.0.

## Alternatives

- **Rust:** comparable binaries, higher barrier for contributors.
- **TypeScript/Bun:** richer bot ecosystem, but cannot embed a cluster bus
  and produces heavier binaries.
- **Elixir:** great for realtime, weak single-binary story.

## Consequences

- Contributors need Go for the core and pnpm for the web app and widget.
- `go build` without a web build still works and serves a placeholder.
- GPL/AGPL components (signal-cli, libsignal) must stay out of process.
