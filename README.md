# CR Agent

The program a host installs on their Windows + NVIDIA PC for the CR Local AI Hub. It reports the hardware, downloads GGUF models
straight from Hugging Face, runs llama.cpp's `llama-server` on `127.0.0.1`, and keeps one outbound WebSocket to the hub. Nothing
ever connects in to the PC.

- `protocol/`: the link protocol (JSON schema and golden frames), pinned by the hub as a submodule.
- `cmd/cragent/`: the agent. `cmd/launcher/`: the stable launcher that starts the current version and rolls back an unhealthy one.
- `cmd/signtool/`: the offline release signing tool (passphrase-derived Ed25519 key, minisign format).
- `internal/`: link, supervisor, downloader, inspect, updater, tray, store.

Releases are published here with a signed `manifest.json`; the agent fetches and verifies them itself. The design is in the hub
repository's `PLAN.md` (§9, §10, §12, §13 and the rulings in §6.4).

Security invariant: a hacked hub can never run code on a host's PC. The hub only names things; the agent derives every URL, hash,
path and command line itself and refuses everything else.
