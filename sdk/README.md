# sdk

SDKs generated from [`protocol/`](../protocol).

- `ts/`: `@say/sdk`. `pnpm --filter @say/sdk generate` regenerates
  `src/schema.gen.ts`; `src/index.ts` is a thin client over it. Also exports
  the widget postMessage types from `@say/sdk/embed`.
- `py/`: `say-sdk`. `make generate-py` regenerates `say/schema_gen.py`
  (TypedDicts, with `datamodel-code-generator` run through `uvx`);
  `say/client.py` is a thin client over it with no dependencies. Its tests
  run against a real server: `make test`, or
  `SAY_BIN=bin/say python3 -m unittest discover -s sdk/py -t sdk/py`.
