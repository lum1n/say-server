# protocol

The contract between the say core and everything that talks to it.

- `openapi.yaml`: the HTTP API.
- `envelope.schema.json`: the WebSocket/SSE frame `{id, type, status, payload}`.

SDKs in `sdk/` are generated from these files (`make generate`). Change the
contract here first.
