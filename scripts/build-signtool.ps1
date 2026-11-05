# Builds the signing tool into build\signtool.exe, from the repository as it is on disk.
# The tool compiles in protocol\release-keys.json, so build it again after that file changed.
#
#   pwsh -File scripts\build-signtool.ps1
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $sdk = Join-Path $env:USERPROFILE "sdk\go\bin"
    if (-not (Test-Path (Join-Path $sdk "go.exe"))) { throw "Go was not found (looked in PATH and in $sdk)" }
    $env:PATH = "$sdk;$env:PATH"
}
$out = Join-Path $repo "build\signtool.exe"
$env:CGO_ENABLED = "0"
& go build -o $out ./cmd/signtool
if ($LASTEXITCODE -ne 0) { throw "the build failed" }
$keys = @((Get-Content (Join-Path $repo "protocol\release-keys.json") -Raw | ConvertFrom-Json).keys).Count
"built $out"
if ($keys -eq 0) { "keys it trusts: none yet. It can run 'pubkey'; it cannot sign." }
else { "keys it trusts: $keys, from protocol\release-keys.json" }
