# The engine's sandbox (finding H2)

From 0.2.3 the engine runs in an AppContainer with no capabilities, inside the same Job Object as before, and the agent talks to it over a Unix socket. This replaces the low-integrity token alone, which stopped the engine writing to the host's files but not reading them, opening DPAPI blobs or connecting out.

Everything below was measured on one PC: Windows 11 Pro 10.0.26200, RTX 5090, driver 610.88, a user in the Administrators group running unelevated, llama.cpp b12125. The private files were in a scratch folder with the same permissions as the user's profile.

## What each candidate allowed

`deny-only` is a restricted token with the user's SID and Administrators deny-only, plus low integrity, with read access given to the logon SID.

| The engine tried to | Low integrity (before) | Deny-only token | AppContainer (chosen) |
|---|---|---|---|
| Read a private file or list the profile's folders | allowed | blocked | blocked |
| Read a DPAPI blob in a private folder | allowed | blocked | blocked |
| Open a DPAPI blob it was able to read | allowed | **allowed** | **allowed** |
| List `D:\` (Users and Authenticated Users have access) | allowed | **allowed** | blocked |
| Read HKCU `Software\Microsoft` | allowed | blocked | **allowed** |
| Connect out, or resolve a name | allowed | **allowed** | blocked |
| Be reached by the agent on 127.0.0.1 | allowed | allowed | blocked (hence the socket) |
| Use CUDA | yes | yes | yes |
| Write to the engine folder, `C:\Users\Public`, `D:\` | blocked | blocked | blocked |

DPAPI itself still opens a blob for the engine, because the token's user is still the host. What protects the machine key and the Hugging Face token is that the engine cannot read the files that hold them. So a secret must never be stored in the engine folder or a model folder.

Low-privilege AppContainer (LPAC) and the capabilities `lpacCom` and `registryRead` also listed the GPU. They are not used.

## Speed

Real answers from the real engine at temperature 0, three runs per start, alternating starts.

| Model | Low integrity over TCP | AppContainer over the socket |
|---|---|---|
| Qwen3.6 Q4_K_XL (18 GB), load | 6.27 to 6.37 s | 6.36 to 6.37 s |
| Qwen3.6 Q4_K_XL, 200 tokens | 75.0 to 76.4 t/s | 75.6 to 76.4 t/s |
| SmolLM2 (386 MB), 256 tokens | 800 to 815 t/s | 789 to 831 t/s |

Both start the same text. There is no measurable difference.

## How it works

- **Profile:** `CreateAppContainerProfile`, named `cr-agent-engine-<hash of the agent's folder>`. It is per user and needs no administrator rights. It stays behind after an uninstall (under `%LOCALAPPDATA%\Packages`), empty and harmless.
- **Reset:** the profile is the one place the container can write, so a model can rename or fill it. It is deleted and made anew (the SID comes from the name and stays the same) at every agent start, and again whenever the socket cannot be made there. So a model cannot break the sandbox for later loads, and the offer of low integrity goes up only if that retry fails and the agent's own probe also finds no CUDA device.
- **UI:** at both levels the Job Object has every UI limit on (`JobObjectBasicUIRestrictions`, 0xFF) and the child gets an empty desktop of its own, never shown. Without them `BitBlt` and window enumeration work from inside, and the answers leave through the relay. The limits alone are not enough: with them on, measured, 227 titled windows fell to 1 but a copy of the screen still held all 1087 colours. On its own desktop it sees no other window and copies nothing. Win32k lockdown (`PROCESS_CREATION_MITIGATION_POLICY_WIN32K_SYSTEM_CALL_DISABLE_ALWAYS_ON`) would be stronger, but the engine then fails to start: a DLL it needs cannot initialize.
- **Socket:** an AppContainer cannot be reached on loopback without `CheckNetIsolation LoopbackExempt` or `NetworkIsolationSetAppContainerConfig`, which are documented as needing an administrator; that route was not tried. llama-server listens on a Unix socket when `--host` ends in `.sock`, and that needs no exemption and no capability. The socket is `e-<random>.sock` in the container's own `AC\Temp`, which the container can write. B15's check that the answer comes from the engine uses `SIO_AF_UNIX_GETPEERPID` in place of the TCP table. Every request takes a new connection: on a kept-alive connection over this socket, about 1 request in 50 never got an answer, while a new connection to the same engine answered at once.
- **Read access:** one read-only entry for ALL APPLICATION PACKAGES (`S-1-15-2-1`) on the engine folder and the model folder. A file moved in from elsewhere keeps its own permissions, so each load also checks the `.gguf` files beside the model.
- **No silent weakening (B63):** a sandbox that cannot be set up is an engine error, and the old start at normal integrity after a token refusal is gone.

## When the AppContainer does not work on a PC

There are two levels and nothing weaker. No code path in the agent starts the engine at normal integrity, in a release or a debug build.

If the AppContainer cannot be set up, or the agent's own probe (`--list-devices` in the AppContainer, with no model and no hub input) shows no CUDA device, the load is an engine error and the tray offers low integrity. A hub cannot raise this offer: a `gpu_select` of a missing GPU or a model that crashes still leaves the probe seeing a device.

The offer is local, stored per PC as `engine_low_integrity`, and no hub frame can set it. Its dialog says what it gives up: the engine still cannot change the host's files, but a malicious model could read them, including the agent's keys, and connect to the internet. The boundary test records this. This level is reached on loopback, as before 0.2.3.

If low integrity also fails, it is an engine error with its message and no further offer. An older version's `engine_normal_integrity` is not read, so that host starts again from the AppContainer.

`low_il` in `load` results is true at both levels, which is accurate: an AppContainer is low integrity too. The schema has no field for which one, so the true level is in the agent log (`isolation`: `appcontainer` or `low_integrity`), the tray and the About window.

## Why ALL APPLICATION PACKAGES and not the container's own SID

With the container's own SID on a file, a low-integrity process stops getting that file, though the entry for the user is untouched. On DLLs, llama-server hangs while loading them, one thread waiting in `LpcReply`. On a model, it fails with `failed to open GGUF file`. The cause is not known. It matters because an older agent after a rollback runs under the low-integrity token and uses the same engine folder. With ALL APPLICATION PACKAGES a low-integrity start works, measured on both the engine and an 18 GB model.

**Known cost, accepted:** any other AppContainer on the PC can read those two folders. They hold the signed engine and models, and no secrets.

## A model folder the host chose

This is **not possible without changing that folder's permissions**. An AppContainer can read only what names it or ALL APPLICATION PACKAGES, and a normal folder on any drive names neither. llama-server opens the model by path, so the agent cannot hand it an open file instead.

So `<home>\models` and below are granted at load. For another folder, choosing it under *Model folder…* asks the host, and the host's **Allow** adds the one read-only entry.

Some folders are refused outright, because a grant reaches everything below it and any AppContainer on the PC: the rest of `<home>` (it holds the keys), any folder that contains `<home>`, drive roots, the profile, `Windows`, `Program Files`, and Documents, Desktop, Downloads, Pictures, Music, Videos and `AppData` themselves, taken from the system's known-folder paths. Their subfolders are allowed after **Allow**. The path is resolved first, so a link or short name changes nothing, and no grant is made through a reparse point. A stored folder that is now refused gives an engine error (`model_folder_refused`) and a tray notice, and no grant.

The agent records each entry it adds (`engine_grants`) and takes those away when the model folder changes, so an entry the host made is never touched. A folder chosen before 0.2.3 has no such click: the tray says at start that the engine cannot read it, a load fails as an engine error, and choosing the folder again is the way out. Nothing is granted silently.

The deny-only token would have read `D:\` without any change, but only because everything else on `D:\` is readable to it as well.

## On the first friend's PC

Before anything else, open the tray's About window and load a model: About must say `AppContainer`, the log's `model loaded` line must say `"isolation":"appcontainer"` and `"cuda_context":true`, an answer must come at the speed that GPU should give, and the tray must not offer low integrity; write down the driver version and whether the PC has hybrid graphics.

## What could not be checked

There is one PC. A failure of the sandbox on any of these is an engine error, not a silent weaker start, and the host's way out is the opt-in to low integrity.

- Older NVIDIA drivers, and other GPUs.
- Laptops with hybrid graphics (Optimus).
- A standard user account, and domain or policy-managed PCs (AppLocker, or policy blocking AppContainers).
- Antivirus products.
- Windows 10 before 1803, which has no Unix sockets. A non-ASCII socket file name worked here, but a real non-ASCII user name was not tried.
- A model folder on exFAT, which has no permissions.

## Not protected

The engine can still read what ALL APPLICATION PACKAGES can read, such as `C:\Windows` and `Program Files`. It can use the GPU and the memory its job allows.

**The clipboard.** A private desktop shares its window station's clipboard, so what closes it is the job's `READCLIPBOARD` and `WRITECLIPBOARD` limits, on the data calls. `OpenClipboard` proves nothing: at low integrity it succeeds whenever nobody else holds the clipboard, and in an AppContainer the system denies it. Measured once on a free clipboard with the limits on: low integrity opened it, and `GetClipboardData` and `SetClipboardData` got nothing. The test holds this on every run where it can reach the clipboard; if another program holds it, the test skips and says so.

**HKCU is readable.** Measured from inside: `Environment`, `Software`, `Software\Classes`, Explorer's settings and `Software\Microsoft\Windows NT\CurrentVersion\Winlogon`; `Volatile Environment` and `Network` are not. So a secret in a user environment variable is open to a model. The agent therefore has no `HF_TOKEN` way in any more: a token is stored through the tray only, in DPAPI state the sandbox cannot read. **Next to try:** Low-privilege AppContainer (LPAC), which does not get ALL APPLICATION PACKAGES' access. It listed the GPU here, but its load and speed were not measured, and it would need its own grants instead of the ALL APPLICATION PACKAGES entry.

**Named pipes and services are a known limit.** From inside, the engine can still reach local named pipes and services whose own permissions admit an AppContainer. Nothing is done about them in this release.
