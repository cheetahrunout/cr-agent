# Publishes one signed release folder on GitHub (ruling B62). GitHub is transport only: nothing here
# builds, and nothing that is not signed goes up.
#
#   pwsh -File scripts\publish-release.ps1 -Dist C:\Users\Admin\Desktop\AI\signing\0.2.0 -Version 0.2.0
#
# It refuses a folder without a valid signature, checked by the signing tool against the compiled-in
# key list, every file re-hashed. A rollback-test release (its signed notes say so) goes up as a
# prerelease and never as latest, so the agents' daily look at "latest" can never see it.
param(
    [Parameter(Mandatory = $true)][string]$Dist,
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$Tool = (Join-Path $env:USERPROFILE "Desktop\AI\signing-tool\signtool.exe"),
    [string]$Repo = "cheetahrunout/cr-agent",
    [switch]$DryRun
)
$ErrorActionPreference = "Stop"
if ($Version -notmatch '^\d{1,3}\.\d{1,3}\.\d{1,3}$') { throw "version must be x.y.z" }
if (-not (Test-Path (Join-Path $Dist "manifest.json.minisig"))) { throw "$Dist has no manifest.json.minisig: this release is not signed. Nothing was published." }
if (-not (Test-Path $Tool)) { throw "no signing tool at $Tool" }
& $Tool verify -dist $Dist
if ($LASTEXITCODE -ne 0) { throw "the folder does not verify. Nothing was published." }
$m = Get-Content (Join-Path $Dist "manifest.json") -Raw | ConvertFrom-Json
if ($m.version -ne $Version) { throw "the manifest is for version $($m.version), not $Version. Nothing was published." }
$rollback = $m.notes -match 'ROLLBACK TEST'
$tag = "v$Version"
$commit = ""
if (Test-Path (Join-Path $Dist "BUILD.txt")) {
    $commit = ([regex]::Match((Get-Content (Join-Path $Dist "BUILD.txt") -Raw), '(?m)^commit ([0-9a-f]{40})\s*$')).Groups[1].Value
}
if (-not $commit) { throw "$Dist has no BUILD.txt with the commit it was built from. Nothing was published." }

$title = "cr-agent $Version"
$notes = "Signed release $Version (release_seq $($m.release_seq), key $($m.key_id)). $($m.notes)"
$flags = @("--latest")
if ($rollback) {
    $title += " ROLLBACK TEST"
    $flags = @("--prerelease", "--latest=false")
}
$files = @(Get-ChildItem $Dist -File | ForEach-Object FullName)
$create = @("release", "create", $tag, "--repo", $Repo, "--target", $commit, "--title", $title, "--notes", $notes) + $flags + $files
if ($DryRun) {
    "would run: gh release create $tag --target $commit --title `"$title`" $($flags -join ' ')"
    "with files: $((Get-ChildItem $Dist -File).Name -join ', ')"
    return
}
& gh release view $tag --repo $Repo *> $null
if ($LASTEXITCODE -eq 0) { throw "a release $tag exists already. Nothing was published." }
$global:LASTEXITCODE = 0
& gh @create
if ($LASTEXITCODE -ne 0) { throw "gh release create failed" }
$code = & curl.exe -sIL -o NUL -w "%{http_code}" "https://github.com/$Repo/releases/download/$tag/manifest.json.minisig"
if ($code -ne "200") { throw "published, but the signature cannot be fetched: HTTP $code" }
"published $tag; the signature can be fetched as an agent fetches it"
