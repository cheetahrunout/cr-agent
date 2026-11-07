# CR agent link protocol

One WebSocket per machine from the agent (on the host's PC) to the hub at `wss://cr-ai.apps.grovestreet.zip/agent`, JSON text
frames, one envelope. This folder is the single source for both sides: the hub pins it as a git submodule and generates its
TypeScript types from `schema.json`; the agent generates its Go structs from the same file. Both test suites must parse and emit every
file under `golden/` byte-for-byte in meaning (`golden/README.md`).

The design and the reasons are in the hub repository's `PLAN.md` (§10 and rulings B1–B12). This file is the operational summary.

## Versions

- `proto` is `major.minor`, independent of the agent's semver. **Minor is additive only**: new optional fields, new frame types.
  Major breaks. v1 starts at `1.0`.
- The agent sends its `proto` in `hello`; the hub answers `welcome` with the `proto` it will speak (the agent's minor or lower) and
  sends only the fields of that minor. The hub accepts any agent with the same major.
- Below the hub's minimum agent version the agent is accepted in `mode: update_required`: heartbeats, `hw`, `status` and the
  `update` command still work; nothing else is routed. A major mismatch closes with 4001.

## Envelope and ids

`{"v":1,"t":"<type>","id":"r-01J…"|"c-01J…", "seq":n, …payload}`

- `id` is present on requests (`r-`) and commands (`c-`) and is minted **only by the hub** (ULID). Request ids die with the socket;
  late frames for unknown ids are dropped.
- Long jobs (downloads, inspects, pending approvals, loads) are **agent-owned and keyed by `model_id`**: they survive a dropped
  socket, `hello` lists them in `jobs[]`, their progress travels in `status` frames, and a repeated `download` for the same
  `model_id` attaches to the running job.
- Frames ≤ 256 KiB; hard `maxPayload` 1 MiB on both sides. Larger request bodies are split into `req.body` parts with `seq`,
  reassembled in order, interleaved with other traffic.

## Decoder rule (ruling B2)

- `cmd.args` and `req` are **strict**: unknown fields are rejected (`DisallowUnknownFields`); the hub sends only the fields of the
  negotiated minor.
- `welcome`, `ping`, `pong` and every other frame are **tolerant**: unknown fields are ignored.
- An unknown `t` is logged and dropped, never fatal.

## Authentication and pairing (ruling B1)

Pairing is HTTP, not a frame: `POST /agent/pair {code, key_id, key_secret, prior_machine_id?}` → `200 {machine_id, slug}` or
`202 {pending: true}` (poll every 3 s, up to 10 min) while the host chooses *Replace key* / *Pair as new* in the hub. The agent mints
the machine key from its own random source: `key_id` is exactly 8 and the secret exactly 52 RFC 4648 base32 characters (A-Z, 2-7;
260 bits). The hub mints `machine_id`. The WebSocket upgrade carries
`Authorization: Bearer crm_<key_id>_<secret>`; the hub authenticates **before** the upgrade.

Close codes: 4000 key not accepted (retry hourly and at start), 4001 upgrade required (major mismatch), 4002 replaced by a newer
socket, 4003 machine revoked (stop reconnecting).

## Keepalive and reconnect

- Hub `ping` every 25 s, agent `pong`; either side treats 60 s without any frame as dead (Grove's limit is ~75 s).
- Reconnect with full jitter: `random(0, min(30 s, 1 s × 2^n))`, reset after 60 s healthy; after a clean `going_away`/1012 close,
  0–3 s. Every connection starts with `hello` carrying the agent's full state; the hub rebuilds its view from it.
- If the socket closes for any reason the agent aborts every local request in flight.

## Progress and state changes

- While a download moves bytes the agent sends one `status` frame a second with the job's `pct`, `bytes_done` and `bytes_total`,
  and one more at once on every state change (a job starts, waits for a click, pauses, ends; the engine changes state).
- `models[]` is in a `status` frame only when a model's state changed; a frame without it leaves the hub's list as it is.
- A `repo` of owner `local-file` is a file the host added on the PC (ruling B64); there is nothing to fetch. `commit` is the first 40 hex of its SHA-256 (of the list of its parts' hashes for a multi-part set). The hub cannot start that import; `download` for the owner is refused with `denied`, `inspect` answers only for an added file, and `delete` forgets it and leaves the file.
- A download waiting its turn (one runs at a time) is reported as `running` with 0 bytes done.
- `hello` has no `engine_error`: when an engine error is pending at connect, the agent sends a `status` frame right after `welcome`.
- `hf_token` travels only in `hello`: a token set or removed while connected reaches the hub at the next connection.
- Capability `parallel` (agent 0.2.8, ruling B74): a `load` may set `settings.parallel` to 1..4, the people sharing the engine at once. The engine gets `-np parallel -no-kvu`: each slot has `ctx / parallel` of the cache, and a request larger than its slot is refused by the engine without touching the others. An agent without the capability refuses `parallel` other than 1, so the hub offers sharing only to an agent that lists it. A hub older than this capability drops a `hello` that lists it, so the hub goes first.

## Frames

| Dir | `t` | Payload |
| --- | --- | --- |
| A→G | `hello` | `agent_version, proto, platform, backend, capabilities[], supported_archs[], llama_tag, hardware, engine, models[], jobs[], inflight[], last_disconnect_reason, local_pause, latest_verified_release?` |
| G→A | `welcome` | `proto, mode: normal\|update_required, ping_interval_s, limits{max_frame, max_body}, hub_version` |
| G→A | `reject` | `code, message` (then close) |
| A→G | `hw` | `pools[], cpu, ram, disk, os, driver` (also inside `hello.hardware`) |
| A→G | `status` | `engine, model_id?, jobs[], local_pause, sleeping?, settings_in_use?` |
| G→A | `cmd` | `id, op, args` — `op` ∈ `download, inspect, load, unload, pause, resume, cancel, delete, update, config` |
| A→G | `cmd.ack` / `cmd.progress` / `cmd.done` / `cmd.err` | `id`, plus `pct, bytes` / `result` / `code, message, field?` |
| G→A | `req` | `id, path ∈ {/v1/chat/completions, /v1/models, /health}, stream, body` or `body_parts` |
| G→A | `req.body` | `id, seq, data` |
| A→G | `res.head` | `id, status, ctype` |
| A→G | `res.data` | `id, d:[…raw SSE data payloads or one body…]` |
| A→G | `res.end` | `id, reason: done\|cancelled\|error, error?` |
| G→A | `cancel` | `id` (an `r-` or a `c-` id) |
| both | `ping` / `pong` | `ts` |

The exact shapes, enums and bounds are in `schema.json`.
