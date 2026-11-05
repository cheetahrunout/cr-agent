# Release dry run (M1 slice 2, 2026-11-05)

A local run of the whole release path of PLAN ruling B27, with debug builds and the throwaway test key.
Nothing touched GitHub, the real install folder or autostart. Reproduce with:

    pwsh scripts\dryrun.ps1 -Work <a folder that does not exist yet>

Machine: Windows 11 Pro 10.0.26200, Ryzen 7 9800X3D, 63 GB RAM, RTX 5090. Go 1.27.3 building a `go 1.24` module.

## What ran

| # | Step | Result | Time |
| --- | --- | --- | --- |
| 0 | Four debug agents (0.1.0, 0.1.1, 0.1.2 built to never be healthy, 0.1.3), launcher, tools | built | 4.6 s |
| 0 | Four releases packed, hashed, signed with the test key (`release_seq` 1 to 4) and verified | `signtool verify` ok for each | under 1 s each |
| 1 | 0.1.0 installed into a scratch folder as the installer leaves it (no manifest); started by the launcher; paired | fetched and checked its own manifest, committed `release_seq` 1 | connected 0.4 s after start |
| 2 | `update{0.1.1}` | manifest verified, agent archive fetched and unpacked, engine archive skipped (same hash), pointer flipped | 276 ms |
| 2 | launcher restarts into 0.1.1 on exit code 75 | 0.1.1 verified its signature and files, reconnected | 146 ms after `cmd.done` |
| 2 | health gate for 0.1.1 | `healthy.0.1.1` written, `release_seq` 2 committed, pointer cleared of `pending_since` | 532 ms after start |
| 3 | `update{0.1.2}` (the unhealthy build) | installed and started on trial; it connects but never writes `healthy.0.1.2` | 274 ms |
| 4 | launcher's health gate runs out | 0.1.2 killed, pointer back to 0.1.1, 0.1.1 started | reverted at 2 m 0.0 s; 0.1.1 connected 2 m 0.14 s after the install |
| 4 | 0.1.1 after the revert | sends `status{update:{state: failed, version: 0.1.2}}` once | at once after `welcome` |
| 5 | `update{0.1.0}`, signed again (a fresh valid signature, `release_seq` 1) | refused: "release_seq 1 is older than the installed 2" | fetched only `manifest.json` and `.minisig` |
| 6 | `update{0.1.3}` with one byte of `manifest.json` flipped | refused: "signature does not verify" | fetched only `manifest.json` and `.minisig` |

The launcher's own log of the run:

    18:47:49.101  agent started  version=0.1.0 on_trial=False
    18:47:51.473  agent asked for a restart into the pointer  version=0.1.0
    18:47:51.515  agent started  version=0.1.1 on_trial=True
    18:47:52.019  new version is healthy  version=0.1.1 after_ms=532
    18:47:54.900  agent asked for a restart into the pointer  version=0.1.1
    18:47:54.942  agent started  version=0.1.2 on_trial=True
    18:49:54.918  reverting to the previous version  failed=0.1.2 back_to=0.1.1 why=not healthy within 2m0s
    18:49:54.932  agent started  version=0.1.1 on_trial=False

A second run, after the last code change of the slice, gave the same results: update 272 ms, reconnect 154 ms, healthy after
534 ms, revert 2 m 0.15 s after the install.

Left on disk: `current.json` = `{"version": "0.1.1"}`; versions 0.1.0, 0.1.1 and 0.1.2; `healthy.0.1.0` and `healthy.0.1.1`
(no `healthy.0.1.2`). No process of the dry run was left running.

## What the dry run does not cover

- The archives were small: the agent zip is about 5 MB and the engine archive was a placeholder with the same bytes in every
  release, so the "engine archive is skipped when its hash is installed" path ran, and the "fetch 600 MB" path did not.
- The release server was a local stand-in (`--release-base`, debug builds only), so GitHub's redirect to its asset hosts was
  exercised only in the unit tests.
- The signature came from the test key through `signtool sign -test-key`; typing a passphrase at the console was not part of it.

## Installer check (same day)

`scripts\build-installer.ps1 -Version 0.1.0 -EngineDir <b12125> -DebugBuild -Scratch` built the layout-check installer (22 s), which
was installed silently into a scratch folder (11 s):

    launcher.exe
    current.json                      {"version":"0.1.0"}
    versions\0.1.0\cragent.exe
    versions\0.1.0\launcher.exe
    engine\b12125\                    56 files, 737 MB

It created no autostart entry, no Start menu shortcut and no uninstall entry, and the real `%LOCALAPPDATA%\cr-agent` does not exist
on this PC. Started from that folder, launcher → agent → `llama-server` ran in nested Job Objects: the engine was found in
`engine\b12125`, loaded SmolLM2 under low integrity with a CUDA context in 1.2 s (cold), answered at about 960 tokens/s, and
killing the launcher ended the agent.

The release installer (`scripts\build-installer.ps1 -Version 0.1.0 -EngineDir <b12125>`) also builds, in 37 s: installer 562 MB,
agent archive 5.8 MB, engine archive 609 MB, and an unsigned `manifest.json` with 58 files and 3 archives. It was not run: a release
build refuses to start until a production key is in `protocol/release-keys.json`.

## Release-build rehearsal (pre-flight, 2026-11-06)

Real release binaries and installers, built by `scripts\build-installer.ps1` in a scratch copy of the repository whose
`protocol/release-keys.json` held a rehearsal key from a throwaway passphrase. The copy, the key and the passphrase were deleted
afterwards. The passphrase was typed through a Windows pseudo-console (ConPTY), the real `CONIN$` path, by a small harness.

| Check | Result |
| --- | --- |
| `signtool pubkey` at the console | key derived; input not echoed |
| one wrong letter; three words | refused; nothing written |
| `sign` of 0.1.0 `-first`, 0.1.1 and 0.1.2 `-prev` | `release_seq` 1, 2, 3, about 25 s each; `verify` ok; only 0.1.2 prints the `ROLLBACK TEST` note |
| layout-check installer with the release binaries, silent, into a scratch folder | installed; no autostart, no Start menu entry, no uninstall entry |
| start through the launcher with a scratch `--home`, unpaired | no exit 2; tray window up, owned by the agent; `healthy.0.1.0` written after 0.25 s |
| first-start manifest while the release is not on GitHub | HTTP 404 logged as a warning, retry after 1 min, then 5, then hourly; one agent start, no crash loop, no revert; About says so |
| remote addresses while unpaired | GitHub's only |
| `--key` with a random, well-formed, unknown key | dialled the production hub, which closed with 4000; "key not accepted; retrying hourly"; key in no log |
| `--hf-base`, `--release-base`, `--approve`, `--hub` to any other host, malformed `--key` | launcher exits 2 each time, no restart |
| signed 0.1.0 beside the install | verified, `committed_seq` 1 |
| signed 0.1.1 on trial, with the real engine and SmolLM2 | gate passed after 0.7 s, `committed_seq` 2; engine files checked, low integrity, model loaded |
| the rollback-test 0.1.2 on trial, real release binary | never healthy; reverted after 120.4 s (`not healthy within 2m0s`); 0.1.1 started at once |

The release build cannot reach a local release server, so 0.1.1 and 0.1.2 were placed on disk with the pointer written by hand, as
the updater leaves them. The fetch from GitHub in a release build was exercised only up to its 404.

Not exercised: the real installer's Start menu shortcut, autostart task and uninstaller (it was compiled, not run, because it writes
outside a scratch folder); `release.yml` on GitHub (its gate script was run locally for every case); `gh release download` of a draft.
