# Fetches the pinned llama.cpp engine: the official zips named in engine.lock, from the official
# release of the pinned tag, each checked against its SHA-256 before it is unpacked. A name or a hash
# that does not match stops the build (PLAN §2.5: "CI fails loudly on a hash or name mismatch").
#
#   pwsh scripts\fetch-engine.ps1 -Out <folder>      the unpacked engine, ready for build-installer.ps1
param(
    [Parameter(Mandatory = $true)][string]$Out,
    [string]$Cache = ""
)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$repo = Split-Path -Parent $PSScriptRoot
$lock = Get-Content (Join-Path $repo "engine.lock") -Raw | ConvertFrom-Json
if ($lock.upstream -notmatch '^https://github\.com/ggml-org/llama\.cpp/releases/download/b\d+/$') { throw "engine.lock: upstream is not an official llama.cpp release" }
if (-not $Cache) { $Cache = Join-Path ([IO.Path]::GetTempPath()) "cr-agent-engine-$($lock.llama_tag)" }
New-Item -ItemType Directory -Force $Cache | Out-Null
if (Test-Path $Out) { Remove-Item -Recurse -Force $Out }
New-Item -ItemType Directory -Force $Out | Out-Null

foreach ($a in $lock.archives) {
    $zip = Join-Path $Cache $a.name
    $have = (Test-Path $zip) -and ((Get-FileHash $zip -Algorithm SHA256).Hash.ToLower() -eq $a.sha256)
    if (-not $have) {
        "fetching $($lock.upstream)$($a.name)"
        Invoke-WebRequest -Uri ($lock.upstream + $a.name) -OutFile $zip -UseBasicParsing
    }
    $sum = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
    if ($sum -ne $a.sha256) {
        Remove-Item $zip
        throw "$($a.name): SHA-256 is $sum, engine.lock says $($a.sha256). The asset changed or the pin is wrong."
    }
    "ok  $($a.name)  $sum"
    Expand-Archive -Path $zip -DestinationPath $Out -Force
}
if (-not (Test-Path (Join-Path $Out "llama-server.exe"))) { throw "no llama-server.exe in the official zips: the layout of the release changed" }
"engine $($lock.llama_tag): {0} files, {1:n0} MB in $Out" -f (Get-ChildItem $Out -File -Recurse).Count, ((Get-ChildItem $Out -File -Recurse | Measure-Object Length -Sum).Sum / 1MB)
