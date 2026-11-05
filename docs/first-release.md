# The first release: what the owner does

This is the one job only the owner can do: making the signing key and signing the releases. No Go knowledge is needed. It takes
about 10 minutes for the key, and about 30 minutes for the signing once the releases are built.

**How it works in one paragraph.** Your passphrase is the private key. Nothing is stored anywhere. Every agent carries the public half
and refuses any update that was not signed with your passphrase. If the passphrase ever leaks, or everybody forgets it, every PC has
to reinstall, so choose it well and keep it on paper in a safe place.

**Rules for the passphrase.**

- At least six random words. Seven is better. Pick them with dice or a word-list generator, not a sentence you would say.
- Type it only into `signtool`, in a terminal window on this PC. Never in a chat, a browser, a file or a command line.
- Capital letters and extra spaces do not matter: `Amber  COBALT` and `amber cobalt` give the same key.
- Nothing appears on screen while you type. That is correct. Type it and press Enter.

**Where to type the commands.** Start menu → *Terminal*. Every command below goes into that window, one at a time. Do not run them
through Claude or any other tool: the passphrase has to come from your own keyboard.

---

## Part 1: make the key

### Step 1. Build the signing tool

    cd C:\Users\Admin\Desktop\AI\cr-agent
    git pull
    pwsh -File scripts\build-signtool.ps1

**Success looks like:**

    built C:\Users\Admin\Desktop\AI\cr-agent\build\signtool.exe
    keys it trusts: none yet. It can run 'pubkey'; it cannot sign.

That second line is right at this stage. No key exists yet, so this copy of the tool trusts nothing and can sign nothing. Making a key
needs no trust, so `pubkey` works.

### Step 2. Make the key

    .\build\signtool.exe pubkey

It asks for the passphrase twice. Then it takes a few seconds and uses 1 GB of memory, on purpose.

**Success looks like:**

    public key: RWQ...
    key id:     <16 letters and digits>

    Entry for protocol/release-keys.json:
        {
          "added": "2026-11-06",
          "key_id": "<16 letters and digits>",
          "public_key": "RWQ...",
          "status": "current"
        }

| If you see | It means | Do this |
| --- | --- | --- |
| `the two entries differ` | a typo in one of the two | run it again |
| `the passphrase must be at least six words` | too short | choose a longer one |
| `the passphrase is typed at a console` | not in a real terminal | use *Terminal* from the Start menu |

### Step 3. Send the lead that entry

Send the part from `{` to `}`. It is public, so any channel is fine. **Never send the passphrase.**

Write the passphrase on paper now, and give the second admin a copy the same way, because it cannot be recovered.

---

## Part 2: the lead builds the releases

You wait for this part. The lead:

1. adds your entry to `protocol/release-keys.json` and pushes it;
2. pushes the tags `v0.1.0` and `v0.1.1`, one build each;
3. starts the rollback build, which is not a tag:

       gh workflow run release.yml --repo cheetahrunout/cr-agent --ref main -f version=0.1.2 -f rollback_test=true

**About 0.1.2.** Ruling B27 needs a release that fails on purpose, to prove that the launcher takes it back. It is the same source as
0.1.1. Only this one build gets a link flag (`-X main.sabotage=unhealthy-0.1.2`) that keeps it from ever reporting healthy. Nothing
changes on `main`, so it cannot ship by accident. A normal tag build never sets the flag, and the flag names one version, so it has
no effect on any other. The notes of that manifest say `ROLLBACK TEST`, and `signtool` shows them when you sign.

The lead tells you when three drafts exist. You can check for yourself:

    gh release list --repo cheetahrunout/cr-agent

**Success looks like:** three rows that say `Draft`, for v0.1.0, v0.1.1 and v0.1.2. Agents cannot see a draft, and nothing on them is
signed yet.

---

## Part 3: sign and publish

### Step 4. Rebuild the signing tool, now with your key in it

    cd C:\Users\Admin\Desktop\AI\cr-agent
    git pull
    pwsh -File scripts\build-signtool.ps1
    .\build\signtool.exe trust-check

**Success looks like:**

    keys it trusts: 1, from protocol\release-keys.json
    ok: 1 production key(s), no test key

### Step 5. Download the three drafts

About 3.6 GB in all.

    gh release download v0.1.0 --repo cheetahrunout/cr-agent --dir C:\Users\Admin\Desktop\AI\signing\0.1.0
    gh release download v0.1.1 --repo cheetahrunout/cr-agent --dir C:\Users\Admin\Desktop\AI\signing\0.1.1
    gh release download v0.1.2 --repo cheetahrunout/cr-agent --dir C:\Users\Admin\Desktop\AI\signing\0.1.2

**Success looks like:** each folder holds five files: `agent-….zip`, `engine-….zip`, `CRAgent-Setup-….exe`, `manifest.json` and
`SHA256SUMS.txt`.

If `gh` says it cannot find the release, download the five files from the draft on the
[Releases page](https://github.com/cheetahrunout/cr-agent/releases) into those same folders.

### Step 6. Test the engine on this PC's GPU

Once is enough, because all three releases carry the same engine.

    pwsh -File scripts\smoke-gpu.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.1.0

**Success looks like:** the last line is `SMOKE TEST OK`. It takes under a minute. If it says anything else, stop and send the lead
the output.

### Step 7. Sign, in this order

Each command asks for the passphrase once and takes about 25 seconds. It first hashes every file again and refuses if anything
differs from the manifest.

    .\build\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.1.0 -first
    .\build\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.1.1 -prev C:\Users\Admin\Desktop\AI\signing\0.1.0\manifest.json
    .\build\signtool.exe sign -dist C:\Users\Admin\Desktop\AI\signing\0.1.2 -prev C:\Users\Admin\Desktop\AI\signing\0.1.1\manifest.json

**Success looks like** `signed: version 0.1.0, release_seq 1, key <your key id>`, then `0.1.1` with `release_seq 2`, then `0.1.2` with
`release_seq 3`.

**Check the notes.** Only 0.1.2 prints a line `notes: ROLLBACK TEST…`. If 0.1.0 or 0.1.1 prints it, or 0.1.2 does not, **stop**,
upload nothing and tell the lead.

| If you see | It means | Do this |
| --- | --- | --- |
| `this passphrase gives key …, which is not in the trust list … a typo` | a typo. Nothing was signed | run the same command again |
| the same message, three careful tries in a row | this is not the passphrase of the key you sent | stop and tell the lead. Nothing is published yet, so a new key costs only a rebuild |
| `this signtool was built before any key was in …` | Step 4 was skipped | do Step 4, then sign again |
| `the folder is not what the manifest says` | a file is damaged or was changed | delete that version's folder, repeat Step 5 for it. If it happens again, stop and tell the lead |
| `the previous manifest does not verify` | the `-prev` folder is wrong or was not signed | sign the previous version first |
| `version … is already published` | `-prev` names this same version | check the command |

If anything goes wrong before you upload, run the same command again. Signing twice does no harm.

Then check all three:

    .\build\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.1.0
    .\build\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.1.1
    .\build\signtool.exe verify -dist C:\Users\Admin\Desktop\AI\signing\0.1.2

**Success looks like** `ok: version 0.1.0, release_seq 1, signed by <your key id>, 58 files in 3 archives match`. The file count may differ.

### Step 8. Upload the signatures

    gh release upload v0.1.0 C:\Users\Admin\Desktop\AI\signing\0.1.0\manifest.json C:\Users\Admin\Desktop\AI\signing\0.1.0\manifest.json.minisig --clobber --repo cheetahrunout/cr-agent
    gh release upload v0.1.1 C:\Users\Admin\Desktop\AI\signing\0.1.1\manifest.json C:\Users\Admin\Desktop\AI\signing\0.1.1\manifest.json.minisig --clobber --repo cheetahrunout/cr-agent
    gh release upload v0.1.2 C:\Users\Admin\Desktop\AI\signing\0.1.2\manifest.json C:\Users\Admin\Desktop\AI\signing\0.1.2\manifest.json.minisig --clobber --repo cheetahrunout/cr-agent

**Success looks like:** no error. `--clobber` replaces the unsigned manifest with the signed one.

### Step 9. Publish

Publish in this order. 0.1.2 is a prerelease and must never be marked latest.

    gh release edit v0.1.0 --draft=false --repo cheetahrunout/cr-agent
    gh release edit v0.1.1 --draft=false --latest --repo cheetahrunout/cr-agent
    gh release edit v0.1.2 --draft=false --prerelease --latest=false --repo cheetahrunout/cr-agent

Check that the signed manifests can be fetched the way an agent fetches them:

    curl.exe -sIL -o NUL -w "%{http_code}\n" https://github.com/cheetahrunout/cr-agent/releases/download/v0.1.0/manifest.json.minisig
    curl.exe -sIL -o NUL -w "%{http_code}\n" https://github.com/cheetahrunout/cr-agent/releases/download/v0.1.1/manifest.json.minisig
    curl.exe -sIL -o NUL -w "%{http_code}\n" https://github.com/cheetahrunout/cr-agent/releases/download/v0.1.2/manifest.json.minisig

**Success looks like:** `200`, three times. Publishing 0.1.2 creates its tag, and that may start a short workflow run. It ends with
the notice "a release exists already; nothing to build". That is correct.

**After publishing, never sign or upload to these releases again.**

### Step 10. Tell the lead, and share the two facts

Tell the lead the releases are up. Send friends, by a channel other than the hub and GitHub, so they can check their download:

- your key id;
- the SHA-256 of the installer, from the line for `CRAgent-Setup-0.1.1.exe` in
  `C:\Users\Admin\Desktop\AI\signing\0.1.1\SHA256SUMS.txt`.

The `signing` folder holds nothing secret, and you can delete it once the lead has confirmed that the releases work.

---

## Part 4: what the lead does next

Not the owner's job. Listed so that nothing falls between two chairs.

1. Paste the signed manifests of 0.1.0 and 0.1.1 into the hub's admin page. **Paste 0.1.2 only for the rollback test, and only while
   no friend's machine would be offered it:** the hub offers an update to every machine on an older version.
2. Run the B27 test on one PC: install 0.1.0, press *Update* to 0.1.1, then to 0.1.2. Within 2 minutes the launcher should bring 0.1.1
   back, and the hub should show the update as failed. Then ask for `update{0.1.0}`: it must be refused with "release_seq 1 is older
   than the installed 2". This needs no new signature, because the signed 0.1.0 is already an old, valid release.

The real installer's Start menu shortcut, autostart entry and uninstaller were not exercised in rehearsal, so that install is their
first run. Check them there.

## Later releases

A tag `v<x.y.z>` builds a draft. Then repeat Steps 5 to 9 for that one version, with `-prev` naming the newest published manifest,
even if that is 0.1.2. Step 6 is needed only when `engine.lock` changed.

## If the passphrase leaks or is lost

It cannot be rotated away (ruling B20). It means a new key, a new installer, and a reinstall on every PC. Tell the lead at once.
