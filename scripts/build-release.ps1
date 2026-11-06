# Builds one release from the local checkout, on the owner's PC (ruling B62). CI builds nothing that ships.
#
#   pwsh -File scripts\build-release.ps1 -Version 0.2.0 -Out C:\Users\Admin\Desktop\AI\signing\0.2.0
#   pwsh -File scripts\build-release.ps1 -Version 0.2.2 -RollbackTest -Out C:\Users\Admin\Desktop\AI\signing\0.2.2
#
# It refuses a working tree with any uncommitted change and prints the commit it builds, so the
# signature seals a known commit. The engine zips are fetched from the official release and refused
# unless they match engine.lock. The folder ends up holding the two archives, the installer, the
# unsigned manifest.json (its notes name the commit), SHA256SUMS.txt and BUILD.txt. Everything is scanned with Microsoft
# Defender before it reaches that folder: a flag stops the build, so nothing flagged is ever signed.
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Out,
    [switch]$RollbackTest,
    [string]$Iscc = ""
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
if ($Version -notmatch '^\d{1,3}\.\d{1,3}\.\d{1,3}$') { throw "version must be x.y.z" }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { $env:PATH = (Join-Path $env:USERPROFILE "sdk\go\bin") + ";$env:PATH" }
$dirty = @(git status --porcelain)
if ($dirty.Count -ne 0) { throw "the working tree has uncommitted changes; a release is built only from a commit:`n$($dirty -join "`n")" }
$commit = (git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $commit -notmatch '^[0-9a-f]{40}$') { throw "not a git checkout" }
if ((Test-Path $Out) -and @(Get-ChildItem $Out).Count -ne 0) { throw "$Out is not empty; give an empty or new folder" }
"building cr-agent $Version from commit $commit"

& go run ./cmd/signtool trust-check
if ($LASTEXITCODE -ne 0) { throw "protocol\release-keys.json is not fit to ship" }

$work = Join-Path ([IO.Path]::GetTempPath()) ("cr-agent-build-$Version-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
try {
    & (Join-Path $PSScriptRoot "fetch-engine.ps1") -Out (Join-Path $work "engine")
    # What this release changes goes into its signed notes. A release without a section in CHANGELOG.md is not built.
    $section = [regex]::Match((Get-Content (Join-Path $repo "CHANGELOG.md") -Raw), "(?ms)^## $([regex]::Escape($Version))\s*$(.*?)(?=^## |\z)").Groups[1].Value
    $changes = @($section -split "\r?\n" | Where-Object { $_ -match '^- ' } | ForEach-Object { $_.Substring(2).Trim() })
    if ($changes.Count -eq 0 -and -not $RollbackTest) { throw "CHANGELOG.md has no section for $Version. Add one, commit it, and build again." }
    if (($changes -join ' ') -match '"') { throw "CHANGELOG.md: no double quotes in the section for $Version." }
    $notes = ("$($changes -join ' ') Built from commit $commit.").Trim()
    $build = @{ Version = $Version; EngineDir = (Join-Path $work "engine"); Out = (Join-Path $work "release"); Notes = $notes }
    if ($RollbackTest) { $build.RollbackTest = $true }
    if ($Iscc) { $build.Iscc = $Iscc }
    & (Join-Path $PSScriptRoot "build-installer.ps1") @build

    & (Join-Path $PSScriptRoot "scan-defender.ps1") -Path (Join-Path $work "release\stage"), (Join-Path $work "release\dist")

    New-Item -ItemType Directory -Force $Out | Out-Null
    Get-ChildItem (Join-Path $work "release\dist") -File | ForEach-Object { Move-Item $_.FullName (Join-Path $Out $_.Name) }
    # manifest.json is rewritten when it is signed, so it is not listed.
    $sums = Get-ChildItem $Out -File | Where-Object Name -NE "manifest.json" | ForEach-Object {
        "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
    }
    [IO.File]::WriteAllLines((Join-Path $Out "SHA256SUMS.txt"), [string[]]$sums)
    [IO.File]::WriteAllText((Join-Path $Out "BUILD.txt"), "version $Version`ncommit $commit`nrollback_test $([bool]$RollbackTest)`n")
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
""
"release folder: $Out"
"commit:         $commit"
Get-Content (Join-Path $Out "SHA256SUMS.txt")
