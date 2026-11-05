# Golden frames

Every `*.json` here is one frame. Both the hub (TypeScript) and the agent (Go) must:

1. parse each file without error (strict for `cmd` and `req`, tolerant for the rest);
2. re-emit it and get a JSON document equal in meaning (same keys and values; key order and whitespace do not matter);
3. reject each file under `invalid/` with the named reason.

Add a frame here whenever the schema gains a field. CI on both repos runs this folder.
