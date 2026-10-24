# sdk

SDKs generated from [`protocol/`](../protocol).

- `ts/`: `@say/sdk`. `pnpm --filter @say/sdk generate` regenerates
  `src/schema.gen.ts`; `src/index.ts` is a thin client over it. Also exports
  the widget postMessage types from `@say/sdk/embed`.
- `py/`: `say-sdk`. Generation is not wired up yet.
