# The first real release: 0.2.0, 0.2.1 and 0.2.2

Release files are built on this PC from the local checkout, the owner signs them here, and GitHub only carries them (ruling B62).

The old `v0.1.x` drafts and tags on GitHub are unsigned leftovers. They are never to be signed or published. The owner may delete them.

## Signing from now on: one double-click (ruling B69)

The sections below describe the first release, signed by hand. For later releases:

1. Double-click `C:\Users\Admin\Desktop\AI\signing-tool\Sign release.bat`.
2. Compare the code it shows with the one the lead gave you, and type Y.
3. Type your passphrase. Nothing appears while you type.
4. It signs, checks and publishes on GitHub, then shows a file, `upload-to-hub-<version>.json`.
5. On the hub: Admin, *Add a release*, *Upload release file*.

If it stops halfway, run it again: a release that is signed already does not ask for the passphrase again.

The code is worked out by that script. If you want the check that does not depend on any file on this PC, run `Get-FileHash` as in (b) below.

## What the owner did for the first release

Four short steps. About 5 minutes.

**(a) Wait for the lead to say the files are ready**, in `C:\Users\Admin\Desktop\AI\signing\0.2.0`, `0.2.1` and `0.2.2`. The lead also tells
you the SHA-256 of the signing tool.

**(b) Check the tool first.** Start menu → *Terminal*. Run this before anything else, and compare the `Hash` it prints with the value the lead gave
you. Capital letters do not matter. If they differ, stop: do not type the passphrase, and tell the lead.

    Get-FileHash C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe

**Sign three times.** In the same window, paste one line at a time. Each asks for the passphrase once and takes about 25 seconds.
Nothing appears on screen while you type: that is correct.

    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.2.0 -first
    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.2.1 -prev C:\Users\Admin\Desktop\AI\signing\0.2.0\manifest.json
    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.2.2 -prev C:\Users\Admin\Desktop\AI\signing\0.2.1\manifest.json

The tool also prints a line `this tool: sha256 …`. That only catches an accident, like the wrong file: a bad tool would print the right number.
The check that counts is `Get-FileHash` above, because Windows computes it, not the tool.

**Success looks like** `signed: version 0.2.0, release_seq 1, key <your key id>`, then `0.2.1` with `release_seq 2`, then `0.2.2` with
`release_seq 3`. Each also prints a `notes:` line with the commit it was built from.

**Check the notes.** Only 0.2.2 says `ROLLBACK TEST`. If 0.2.0 or 0.2.1 says it, or 0.2.2 does not, **stop** and tell the lead.

**(c) Verify.**

    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.2.0
    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.2.1
    C:\Users\Admin\Desktop\AI\signing-tool\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.2.2

**Success looks like** `ok: version 0.2.0, release_seq 1, signed by <your key id>, 58 files in 3 archives match`. The file count may differ.

**(d) Tell the lead.** The lead publishes. You are done.

### If something goes wrong

| If you see | It means | Do this |
| --- | --- | --- |
| `this passphrase gives key …, which is not in the trust list … a typo` | a typo. Nothing was signed | run the same command again |
| the same message, three careful tries in a row | this is not the passphrase of the key in the list | stop and tell the lead |
| `the passphrase must be at least 10 characters` | too short, so certainly a typo | run the same command again |
| `the passphrase is typed at a console` | not in a real terminal | use *Terminal* from the Start menu |
| `the folder is not what the manifest says` | a file was changed after the build. Nothing was signed | stop and tell the lead |
| `the previous manifest does not verify` | the `-prev` version was not signed yet | sign the previous version first |
| `version … is already published` | `-prev` names this same version | check the command |

Before the lead publishes, running a `sign` command again does no harm. After publishing, never sign these versions again.

## The passphrase

**Key rules.**

- At least 10 characters (ruling B61). Longer is stronger: the public key is public, so anyone can try guesses against it on their own
  computers, for as long as they like. Avoid names, dates, song lines and common phrases.
- Type it only into `signtool.exe` in `C:\Users\Admin\Desktop\AI\signing-tool`, in a terminal window on this PC. Never in a chat, a browser,
  a file or a command line. Do not run the commands through Claude or any other tool: the passphrase comes from your own keyboard.
- Capital letters and extra spaces do not matter: `Amber  COBALT` and `amber cobalt` give the same key.
- Make sure the second admin knows it too, because it cannot be recovered.

**What it protects against.** A signed release is the only thing an agent will install. So someone who takes over the hub, the GitHub
repository, CI or the download path cannot put their own program on a friend's PC: without the passphrase they have no signature, and
agents refuse the update. They also cannot send a PC back to an older signed release.

**What it does not protect against.**

- It vouches for what was built on this PC. If this PC or this checkout is compromised when the release is built, the signature seals the
  bad files. That is why the lead reviews the commit and why a release is built only from a clean tree.
- If the program you type into is bad, it has your passphrase. That is why `signtool.exe` sits outside the repository, is built once, and is
  not rebuilt just before signing.
- It does not make models safe. A model file runs inside the engine, whose sandbox limits it but does not make it harmless
  (`docs/engine-sandbox.md`).
- It says nothing about the first install: a friend's first download is checked only by comparing the hash and key id you share.
- A leak cannot be repaired by rotating (ruling B20). A leaked or lost passphrase means a new key, a new installer and a reinstall on
  every PC. Tell the lead at once.

---

## What the lead does

Not the owner's job. In order.

**1. Review and commit.** Everything below runs from a clean tree at the reviewed commit.

**2. The signing tool, once.**

    pwsh -File scripts\build-signtool.ps1

It scans the tool with Microsoft Defender (a flagged tool is deleted, never excluded), prints the commit and SHA-256 and writes them beside the tool, in `signtool.exe.txt`. Tell the owner that value: they compare
it with `Get-FileHash` before running the tool. The line the tool prints about itself is only an accident check. It refuses to replace the tool unless told to: rebuilding is a separate
reviewed step, after reading the diff of `cmd/signtool`, `internal/sign`, `internal/release`, `internal/trust`, `protocol` and `scripts`
(`-Rebuild`). Never rebuild it right before signing: what protects the passphrase is that the owner compares the tool with a hash fixed earlier.

**3. Build the three folders.** Each refuses a dirty tree, prints the commit, and refuses engine zips that do not match `engine.lock`.

    pwsh -File scripts\build-release.ps1 -Version 0.2.0 -Out C:\Users\Admin\Desktop\AI\signing\0.2.0
    pwsh -File scripts\build-release.ps1 -Version 0.2.1 -Out C:\Users\Admin\Desktop\AI\signing\0.2.1
    pwsh -File scripts\build-release.ps1 -Version 0.2.2 -RollbackTest -Out C:\Users\Admin\Desktop\AI\signing\0.2.2

0.2.2 is the same source with one link flag (`-X main.sabotage=unhealthy-0.2.2`) that keeps it from ever reporting healthy, so the launcher
has something to take back (ruling B27). Nothing changes in the source, and the flag has no effect on any other version.

**4. Smoke test the built files.** Run it before the owner signs, in another terminal. That is not isolation: it is the same Windows account, and a
program it runs could change files that account can write. What makes it acceptable is the pin: the engine is hash-checked first, and the owner's
`Get-FileHash` shows whether the signing tool was changed.

    pwsh -File scripts\smoke-gpu.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.2.0

It runs nothing until every file of the engine archive has matched the official zips, which are checked against `engine.lock`.

**5. The owner signs** (above).

**6. Publish, in this order.** Each checks the signature and re-hashes everything first, and refuses an unsigned folder. 0.2.2 goes up as a
prerelease, never as latest, so the agents' daily look at the latest release cannot see it.

    pwsh -File scripts\publish-release.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.2.0 -Version 0.2.0
    pwsh -File scripts\publish-release.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.2.1 -Version 0.2.1
    pwsh -File scripts\publish-release.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.2.2 -Version 0.2.2

A new tag starts `release.yml`, which only checks that it builds and tests on GitHub too. It publishes nothing.

**7. Hub and friends.**

- Paste the signed manifests of 0.2.0 and 0.2.1 into the hub's admin page. Paste 0.2.2 only for the rollback test, and press *Withdraw* on it
  straight after: until then the hub offers it to every machine on an older version and shows it on the download page.
- Share by a channel other than the hub and GitHub: the key id, and the SHA-256 of `CRAgent-Setup-0.2.1.exe` from that folder's
  `SHA256SUMS.txt`.

**8. The B27 test on one PC.** Install 0.2.0, press *Update* to 0.2.1, then to 0.2.2. Within 2 minutes the launcher should bring 0.2.1 back and
the hub should show the update as failed. Then ask for `update{0.2.0}`: it must be refused with "release_seq 1 is older than the installed 2".
This needs no new signature. That install is also the first run of the real installer's Start menu shortcut, autostart entry and
uninstaller, so check them there.

## Later releases

Steps 3 to 7 for that one version, with `-prev` naming the newest published manifest, even if that is 0.2.2. Step 4 is needed only when
`engine.lock` changed. Rebuild the signing tool only when its code changed, after reading the diff.
