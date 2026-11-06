# One run for the owner: check the signing tool, type the passphrase, and get one file to upload on the hub (ruling B69).
# Started by "Sign release.bat", which build-signtool.ps1 puts beside the tool, outside the repository.
#
# It takes the newest release folder under -Root. Signed already, it does not sign again: a run that stopped halfway picks
# up where it stopped. It builds nothing, and the passphrase goes only to signtool.exe, which reads it from the console.
#
#   -DryRun     does not publish (for a test)
#   -ToolArgs   extra arguments for `sign` (for a test with a debug tool and its throwaway key)
param(
    [string]$Root = (Join-Path $env:USERPROFILE "Desktop\AI\signing"),
    [string]$Tool = (Join-Path $PSScriptRoot "signtool.exe"),
    [switch]$DryRun,
    [string[]]$ToolArgs = @()
)
$ErrorActionPreference = "Stop"

function Stop-Here([string]$why) {
    ""
    "STOPPED: $why"
    "Nothing further was done. Tell Claude what this says."
    exit 1
}
function Signed([string]$dir) { Test-Path (Join-Path $dir "manifest.json.minisig") }

# ---------- which release ----------
$folders = @(Get-ChildItem $Root -Directory | Where-Object { $_.Name -match '^\d{1,3}\.\d{1,3}\.\d{1,3}$' -and (Test-Path (Join-Path $_.FullName "manifest.json")) } |
    Sort-Object { [version]$_.Name })
if ($folders.Count -eq 0) { Stop-Here "there is no release folder in $Root." }
$target = $folders[-1]
$version = $target.Name
$dist = $target.FullName
$hubFile = Join-Path $Root "upload-to-hub-$version.json"
$older = @($folders | Where-Object { $_.Name -ne $version })
$unsignedOlder = @($older | Where-Object { -not (Signed $_.FullName) })
if ($unsignedOlder.Count -ne 0) { Stop-Here "older folders are not signed: $($unsignedOlder.Name -join ', ')." }
$prev = if ($older.Count) { Join-Path $older[-1].FullName "manifest.json" } else { $null }

$m = Get-Content (Join-Path $dist "manifest.json") -Raw | ConvertFrom-Json
if ($m.version -ne $version) { Stop-Here "the folder $version holds a manifest for version $($m.version)." }
$commit = ""
if (Test-Path (Join-Path $dist "BUILD.txt")) { $commit = ([regex]::Match((Get-Content (Join-Path $dist "BUILD.txt") -Raw), '(?m)^commit ([0-9a-f]{40})\s*$')).Groups[1].Value }

$publish = Join-Path $PSScriptRoot "publish-release.ps1"
if (-not $DryRun) {
    & gh auth status *> $null
    if ($LASTEXITCODE -ne 0) { Stop-Here "GitHub is not signed in here. Run: gh auth login" }
}

""
"Release $version"
"  built from  $commit"
"  notes       $($m.notes)"
if ($m.notes -match 'ROLLBACK TEST') { "  THIS IS A ROLLBACK TEST RELEASE. It never works on purpose." }
""

# ---------- the tool, and signing ----------
if (-not (Signed $dist)) {
    if (-not (Test-Path $Tool)) { Stop-Here "there is no signing tool at $Tool." }
    # Worked out here by Windows, not by the tool. The value to compare with is the one Claude gave you, not one on this PC.
    $hash = (Get-FileHash $Tool -Algorithm SHA256).Hash
    $grouped = ($hash -split '(.{8})' | Where-Object { $_ }) -join ' '
    "Check the signing tool. Its code is:"
    ""
    "  $grouped"
    ""
    $answer = Read-Host "Is that the code Claude gave you? Type Y to go on, anything else stops"
    if ($answer -ne 'Y' -and $answer -ne 'y') { Stop-Here "you did not confirm the code. Do not type your passphrase." }
    ""
    "Now type your passphrase. Nothing appears while you type: that is correct."
    $signArgs = @("sign", "-dist", $dist) + $(if ($prev) { @("-prev", $prev) } else { @("-first") }) + $ToolArgs
    & $Tool @signArgs
    if ($LASTEXITCODE -ne 0) { Stop-Here "signing did not work. If it says a typo, just run this again." }
} else {
    "This release is signed already, so no passphrase is needed. Going on from there."
}

# ---------- check, publish ----------
""
& $Tool verify -dist $dist
if ($LASTEXITCODE -ne 0) { Stop-Here "the signed folder does not verify." }

""
if ($DryRun) {
    & $publish -Dist $dist -Version $version -Tool $Tool -DryRun
} else {
    & gh release view "v$version" --repo cheetahrunout/cr-agent *> $null
    if ($LASTEXITCODE -eq 0) {
        "v$version is on GitHub already."
    } else {
        "Publishing on GitHub. The files are large, so this can take a few minutes."
        & $publish -Dist $dist -Version $version -Tool $Tool
    }
}
$global:LASTEXITCODE = 0

# ---------- the file for the hub ----------
# The signature covers the manifest's exact bytes: read as they are, carried as a JSON string.
$utf8 = [Text.UTF8Encoding]::new($false)
$upload = [ordered]@{
    format  = "cr-release-1"
    version = $version
    manifest = [IO.File]::ReadAllText((Join-Path $dist "manifest.json"), $utf8)
    minisig  = [IO.File]::ReadAllText((Join-Path $dist "manifest.json.minisig"), $utf8)
}
$old = Join-Path $dist "manifest.json.old.minisig"
if (Test-Path $old) { $upload.old_minisig = [IO.File]::ReadAllText($old, $utf8) }
[IO.File]::WriteAllText($hubFile, ($upload | ConvertTo-Json -Depth 3), $utf8)

""
"Done. Last step: on the hub, go to Admin, Add a release, and upload this file:"
""
"  $hubFile"
""
if (-not $DryRun) { Start-Process explorer.exe "/select,`"$hubFile`"" }
