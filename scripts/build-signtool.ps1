# Builds the signing tool once, from the reviewed local checkout, and puts it outside the repository
# (ruling B62). Nobody pulls and rebuilds right before typing a passphrase: what is typed into must be
# a file that was looked at earlier.
#
#   pwsh -File scripts\build-signtool.ps1
#
# It refuses a working tree with uncommitted changes, and it refuses to replace an existing tool
# unless told to. Replacing it is a separate reviewed step: first read the diff since the last build,
#
#   git diff <commit it printed>..HEAD -- cmd/signtool internal/sign internal/release internal/trust protocol scripts
#
# then run this with -Rebuild. The tool compiles in protocol\release-keys.json.
param(
    [string]$Out = (Join-Path $env:USERPROFILE "Desktop\AI\signing-tool\signtool.exe"),
    [switch]$Rebuild
)
$ErrorActionPreference = "Stop"
$repo = (Resolve-Path (Split-Path -Parent $PSScriptRoot)).Path
Set-Location $repo
if ([IO.Path]::GetFullPath($Out).StartsWith($repo + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "the signing tool goes outside the repository"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $sdk = Join-Path $env:USERPROFILE "sdk\go\bin"
    if (-not (Test-Path (Join-Path $sdk "go.exe"))) { throw "Go was not found (looked in PATH and in $sdk)" }
    $env:PATH = "$sdk;$env:PATH"
}
$dirty = @(git status --porcelain)
if ($dirty.Count -ne 0) { throw "the working tree has uncommitted changes; the signing tool is built only from a reviewed commit:`n$($dirty -join "`n")" }
if ((Test-Path $Out) -and -not $Rebuild) {
    throw "$Out exists. Replacing it is a separate reviewed step: read the diff (see the top of this script), then run this with -Rebuild."
}
$commit = (git rev-parse HEAD).Trim()
New-Item -ItemType Directory -Force (Split-Path -Parent $Out) | Out-Null
$env:CGO_ENABLED = "0"
& go build -trimpath -o $Out ./cmd/signtool
if ($LASTEXITCODE -ne 0) { throw "the build failed" }
$sha = (Get-FileHash $Out -Algorithm SHA256).Hash.ToLower()
[IO.File]::WriteAllText("$Out.txt", "built from commit $commit`nsha256 $sha`n")
"built  $Out"
"commit $commit"
"sha256 $sha"
& $Out trust-check
