# Builds the agent, the launcher, the two release zips and the installer for one version.
# The release workflow runs this; so can a developer, locally.
#
#   pwsh scripts\build-installer.ps1 -Version 0.1.0 -EngineDir <folder with llama-server.exe and its DLLs>
#
#   -DebugBuild   debug binaries (they trust the throwaway test key and have a console); never published
#   -Scratch      the layout-check installer: no Start menu entry, no autostart, no uninstall entry, no compression
#   -NoInstaller  only the binaries and the zips (when Inno Setup is not there)
#
# Output, under -Out (default build\release\<version>):
#   stage\                                   the install layout the installer packs
#   dist\agent-<version>-windows-amd64.zip   entries versions/<version>/cragent.exe, versions/<version>/launcher.exe
#   dist\engine-<backend>-<tag>-windows-amd64.zip   entries engine/<tag>/...
#   dist\CRAgent-Setup-<version>.exe
#   dist\manifest.json                       unsigned: release_seq and key_id are set by the signer
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$EngineDir,
    [string]$Out = "",
    [string]$Go = "go",
    [string]$Iscc = "",
    [switch]$DebugBuild,
    [switch]$Scratch,
    [switch]$NoInstaller
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
if ($Version -notmatch '^\d{1,3}\.\d{1,3}\.\d{1,3}$') { throw "version must be x.y.z" }
if (-not (Test-Path (Join-Path $EngineDir "llama-server.exe"))) { throw "no llama-server.exe in $EngineDir" }
if (-not $Out) { $Out = Join-Path $repo "build\release\$Version" }

function Run($exe, [string[]]$arguments) {
    & $exe @arguments
    if ($LASTEXITCODE -ne 0) { throw "$exe failed with exit code $LASTEXITCODE" }
}

$lock = Get-Content (Join-Path $repo "engine.lock") -Raw | ConvertFrom-Json
$tag = $lock.llama_tag
$backend = ($lock.backend -replace '[^a-z0-9]', '' -replace '\d$', '')   # cuda-13.4 -> cuda13
$stage = Join-Path $Out "stage"; $dist = Join-Path $Out "dist"; $tools = Join-Path $Out "tools"
if (Test-Path $Out) { Remove-Item -Recurse -Force $Out }
New-Item -ItemType Directory -Force "$stage\versions\$Version", "$stage\engine\$tag", $dist, $tools | Out-Null

# 1. Binaries. A release build is reproducible (-trimpath), stripped, and has no console window.
$env:CGO_ENABLED = "0"; $env:GOOS = "windows"; $env:GOARCH = "amd64"
if ($DebugBuild) {
    $common = @("build", "-tags", "debug", "-trimpath")
    $ldAgent = "-X main.version=$Version"; $ldLauncher = ""
} else {
    $common = @("build", "-trimpath")
    $ldAgent = "-s -w -H windowsgui -X main.version=$Version"; $ldLauncher = "-s -w -H windowsgui"
}
Run $Go ($common + @("-ldflags", $ldAgent, "-o", "$stage\versions\$Version\cragent.exe", "./cmd/cragent"))
Run $Go ($common + @("-ldflags", $ldLauncher, "-o", "$stage\launcher.exe", "./cmd/launcher"))
Copy-Item "$stage\launcher.exe" "$stage\versions\$Version\launcher.exe"
# The tool that packs and hashes is always the plain console build.
$toolBuild = @("build")
if ($DebugBuild) { $toolBuild += @("-tags", "debug") }
Run $Go ($toolBuild + @("-o", "$tools\signtool.exe", "./cmd/signtool"))

# 2. The engine, as the official zips have it (local notes such as SHA256SUMS are not part of it).
Get-ChildItem -Path $EngineDir -File -Recurse | Where-Object { $_.Name -ne "SHA256SUMS" } | ForEach-Object {
    $rel = $_.FullName.Substring((Resolve-Path $EngineDir).Path.TrimEnd('\').Length + 1)
    $dest = Join-Path "$stage\engine\$tag" $rel
    New-Item -ItemType Directory -Force (Split-Path -Parent $dest) | Out-Null
    Copy-Item $_.FullName $dest
}

# 3. The two zips, with the entry paths the manifest lists.
Run "$tools\signtool.exe" @("pack", "-out", "$dist\agent-$Version-windows-amd64.zip", "-prefix", "versions/$Version", "-dir", "$stage\versions\$Version")
Run "$tools\signtool.exe" @("pack", "-out", "$dist\engine-$backend-$tag-windows-amd64.zip", "-prefix", "engine/$tag", "-dir", "$stage\engine\$tag")

# 4. The installer.
if (-not $NoInstaller) {
    if (-not $Iscc) {
        $Iscc = @("$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe", "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe") |
            Where-Object { Test-Path $_ } | Select-Object -First 1
    }
    if (-not $Iscc) { throw "Inno Setup 6 (ISCC.exe) not found; pass -Iscc or -NoInstaller" }
    $defines = @("/Qp", "/DAppVersion=$Version", "/DLlamaTag=$tag", "/DStage=$stage", "/DOutDir=$dist")
    if ($Scratch) { $defines += "/DScratch" }
    Run $Iscc ($defines + @((Join-Path $repo "installer\cr-agent.iss")))
}

# 5. The unsigned manifest: every archive and every installed file with size and SHA-256.
if (-not $Scratch) {
    Run "$tools\signtool.exe" @("manifest", "-dist", $dist, "-version", $Version)
}
""
Get-ChildItem $dist | ForEach-Object { "{0,14:n0}  {1}" -f $_.Length, $_.Name }
