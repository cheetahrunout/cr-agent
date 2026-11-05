# M0 findings (spike, 2026-11-05)

Machine: Windows 11 Pro 10.0.26200, AMD Ryzen 7 9800X3D (8 cores, 16 threads), 63 GB RAM, NVIDIA RTX 5090 (32 GB),
driver 610.88. Engine: llama.cpp `b12125` (commit 7d516e25e, CUDA 13 build). Model: `smollm2-360m-q8_0.gguf` (386 MB).

## Engine start

| Run | spawn → `/health` 200 | Notes |
| --- | --- | --- |
| first agent run (normal token) | 506 ms | page cache warm from an earlier manual run |
| normal token | 504 ms | `cuda_context: true` |
| low integrity | 405–450 ms | `integrity: low`, `cuda_context: true` |

Each start uses `CreateProcess(AsUser)` with `CREATE_SUSPENDED | CREATE_NO_WINDOW`, then `AssignProcessToJobObject`, then
`ResumeThread` (ruling B15). After `/health` the agent checks that the PID owning the port is the child's
(`GetExtendedTcpTable`); it was every time. A 386 MB model says little about large models: measure again with a 14B model.

## Low-integrity token (B15, §9.2): works

`--low-il` duplicates the agent's token, sets the mandatory label to S-1-16-4096 and starts llama-server with
`CreateProcessAsUser`. The child's token was read back as **low**. CUDA initialised (nvidia-smi lists the PID as a compute
process), the model loaded from a medium-integrity folder (low IL may read but not write there), and loopback listening and
streaming worked. Speed was the same as at normal integrity (~770 tokens/s). There was no fallback. The fallback path (low IL
starts but holds no CUDA context, or fails to start → retry with the normal token) is built and logged, but it never ran.
**Recommendation: keep low IL on by default from M1.** One watch item: the CUDA JIT/compute cache under `%LOCALAPPDATA%\NVIDIA`
is not writable at low IL. That costs nothing for prebuilt kernels, but a GPU arch without prebuilt kernels would JIT on every start.

## Job Object

- `KILL_ON_JOB_CLOSE`: hard-killing the agent (`Stop-Process -Force`) killed llama-server within 0.7 s.
- `ACTIVE_PROCESS_LIMIT 1`: a child that tries to start a grandchild gets "Not enough quota is available"
  (`TestJobBlocksGrandchildren`).
- Job memory cap = RAM − 4 GiB (58 937 MB here) is set; the tiny model loaded under it. **B15's check with a partially
  offloaded 14B model is still open.**
- The child environment drops every `LLAMA_*` variable: llama-server reads `LLAMA_ARG_*` as flags, so a variable on the PC could
  otherwise add flags the agent did not choose (`TestChildEnvDropsLlamaVars`).
- "Stop cleanly" = `TerminateJobObject` and a wait for exit. llama-server has no shutdown endpoint, and with `--no-slots` it
  keeps nothing worth flushing. A Ctrl-C/Break route would need a shared console, which `CREATE_NO_WINDOW` rules out.

## Streaming and cancel (fake hub `cmd/fakehub`, debug build, real engine)

- `req` → `res.head` 1.2–1.5 ms, first token about 40 ms, then about 770 payloads/s in about 20 ms `res.data` batches
  (779 payloads in 49 frames).
- `cancel` → `res.end{cancelled}` in 0.5–1.1 ms, with **0 payloads after the cancel**. A one-token request sent right after
  was answered in about 18 ms at `-np 1`, so llama-server had freed the slot (generation really stopped).
- Socket loss mid-stream cancels the local request (unit test: the engine sees the disconnect at once).

## Things the plan should know

1. **b12125 prints no CUDA/offload lines at default verbosity.** `ggml_cuda_init` and the `offloaded N/M layers` lines need
   verbose logging, which is forbidden. The agent therefore checks `nvidia-smi --query-compute-apps=pid` after `/health` to see
   whether the child holds a CUDA context. For M1, NVML (`nvmlDeviceGetComputeRunningProcesses`) should do the same check, and
   VRAM measured per process is `[N/A]` under WDDM, so `measured_vram_mb` has to come from the free-memory delta.
2. **Log line format changed** (`0.00.388.457 I srv  server_on_li: listening on …`). The B16 allow-list is per tag. 19 lines were
   suppressed per run. The allow-list in `engine.go` is a first cut for b12125.
3. **B11's CI check cannot be "the binary contains `localhost`"**: Go's own `net` package puts `localhost` and
   `localhost.localdomain` into every binary (3 hits in the release build). The check is now a release-build test
   (`hubs_release_test.go`) that refuses `localhost`/`127.0.0.1`, plus the `debug` tag that holds the only list naming them.
4. `golang.org/x/sys` ≥ v0.36 requires Go 1.26. The module is pinned at `go 1.24.13` with `x/sys v0.35.0` and
   `coder/websocket v1.8.14`, matching §9.1's "Go 1.23+". It builds with the local Go 1.27.3.
5. The B15 port-owner check works through `GetExtendedTcpTable` (IPv4 listeners). llama-server binds only `127.0.0.1`.
6. Not measured in M0 (no router in the path): the B29 silence tolerance of the Grove router, and B17's redirect hops.
