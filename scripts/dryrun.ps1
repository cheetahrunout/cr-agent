# Local dry run of the release path (PLAN ruling B27), with debug builds and the throwaway test key.
# Nothing here touches GitHub, the real install folder or autostart: everything lives under -Work.
#
#   install 0.1.0 into a scratch folder (as the installer leaves it)
#   update to 0.1.1                         -> the launcher restarts into it, it passes the health gate
#   offer 0.1.2, built to never be healthy  -> the launcher reverts to 0.1.1 within 2 minutes
#   offer 0.1.0, signed again               -> refused by committed_seq
#   offer 0.1.3 with one flipped byte       -> refused before any archive is fetched
#
# Usage:  pwsh scripts\dryrun.ps1 -Work C:\some\scratch\folder
param(
    [Parameter(Mandatory = $true)][string]$Work,
    [string]$Go = "go",
    [int]$HubPort = 9000,
    [int]$HfPort = 9001,
    [int]$ReleasePort = 9002
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

function Run($exe, [string[]]$arguments) {
    & $exe @arguments
    if ($LASTEXITCODE -ne 0) { throw "$exe $($arguments -join ' ') failed with exit code $LASTEXITCODE" }
}

$busy = Get-NetTCPConnection -State Listen -LocalPort $HubPort, $HfPort, $ReleasePort -ErrorAction SilentlyContinue
if ($busy) { throw "ports in use: $($busy.LocalPort -join ', ')" }
if (Test-Path $Work) { throw "$Work exists already; give a folder that does not exist yet" }
$bin = Join-Path $Work "bin"; $dist = Join-Path $Work "dist"; $root = Join-Path $Work "install"; $engineSrc = Join-Path $Work "engine-src"
New-Item -ItemType Directory -Force $bin, $dist, $root, $engineSrc | Out-Null

# 1. Debug builds: four agent versions (0.1.2 is the deliberately unhealthy one), launcher, tools.
$t0 = Get-Date
Run $Go @("build", "-tags", "debug", "-o", "$bin\launcher.exe", "./cmd/launcher")
Run $Go @("build", "-tags", "debug", "-o", "$bin\signtool.exe", "./cmd/signtool")
Run $Go @("build", "-tags", "debug", "-o", "$bin\fakehub.exe", "./cmd/fakehub")
foreach ($v in "0.1.0", "0.1.1", "0.1.2", "0.1.3") {
    $ld = "-X main.version=$v"
    if ($v -eq "0.1.2") { $ld += " -X main.sabotage=unhealthy" }
    New-Item -ItemType Directory -Force "$bin\$v" | Out-Null
    Run $Go @("build", "-tags", "debug", "-ldflags", $ld, "-o", "$bin\$v\cragent.exe", "./cmd/cragent")
    Copy-Item "$bin\launcher.exe" "$bin\$v\launcher.exe"
}
"built in {0:n1} s" -f ((Get-Date) - $t0).TotalSeconds

# 2. Releases, signed with the throwaway test key. The engine archive is a small placeholder with the
#    same bytes in every release, so the updater sees it as installed and does not fetch it.
[IO.File]::WriteAllText("$engineSrc\llama-server.exe", "placeholder engine for the dry run")
[IO.File]::WriteAllText("$engineSrc\ggml-cuda.dll", "placeholder")
$prev = $null
foreach ($v in "0.1.0", "0.1.1", "0.1.2", "0.1.3") {
    $d = "$dist\v$v"
    New-Item -ItemType Directory -Force $d | Out-Null
    Run "$bin\signtool.exe" @("pack", "-out", "$d\agent-$v-windows-amd64.zip", "-prefix", "versions/$v", "-dir", "$bin\$v", "cragent.exe", "launcher.exe")
    Run "$bin\signtool.exe" @("pack", "-out", "$d\engine-cuda13-b12125-windows-amd64.zip", "-prefix", "engine/b12125", "-dir", $engineSrc)
    Run "$bin\signtool.exe" @("manifest", "-dist", $d, "-version", $v)
    if ($prev) { Run "$bin\signtool.exe" @("sign", "-dist", $d, "-prev", "$dist\v$prev\manifest.json", "-test-key") }
    else { Run "$bin\signtool.exe" @("sign", "-dist", $d, "-first", "-test-key") }
    Run "$bin\signtool.exe" @("verify", "-dist", $d)
    $prev = $v
}
# 0.1.0 signed again: a fresh, valid signature on the old release_seq 1.
Run "$bin\signtool.exe" @("sign", "-dist", "$dist\v0.1.0", "-first", "-test-key")
# 0.1.3: flip one byte of the signed manifest.
$m = [IO.File]::ReadAllBytes("$dist\v0.1.3\manifest.json")
$i = [int]($m.Length / 2); $m[$i] = $m[$i] -bxor 1
[IO.File]::WriteAllBytes("$dist\v0.1.3\manifest.json", $m)

# 3. The scratch install, as the installer leaves it: launcher, one version, the engine, the pointer.
Copy-Item "$bin\launcher.exe" "$root\launcher.exe"
New-Item -ItemType Directory -Force "$root\versions\0.1.0", "$root\engine\b12125" | Out-Null
Copy-Item "$bin\0.1.0\cragent.exe", "$bin\0.1.0\launcher.exe" "$root\versions\0.1.0\"
Copy-Item "$engineSrc\*" "$root\engine\b12125\"
[IO.File]::WriteAllText("$root\current.json", '{"version":"0.1.0"}', (New-Object Text.UTF8Encoding($false)))

# 4. The fake hub (pairing, socket, release server), then the launcher.
$hub = Start-Process -FilePath "$bin\fakehub.exe" -PassThru -NoNewWindow -RedirectStandardError "$Work\hub.log" -RedirectStandardOutput "$Work\hub.out" `
    -ArgumentList "-scenario", "update", "-releases", $dist, "-addr", "127.0.0.1:$HubPort", "-hf-addr", "127.0.0.1:$HfPort", "-release-addr", "127.0.0.1:$ReleasePort"
Start-Sleep -Milliseconds 800
$launcher = Start-Process -FilePath "$root\launcher.exe" -PassThru -NoNewWindow -RedirectStandardError "$Work\launcher-console.log" -RedirectStandardOutput "$Work\launcher-console.out" `
    -ArgumentList "--no-tray", "--hub", "ws://localhost:$HubPort/agent", "--release-base", "http://127.0.0.1:$ReleasePort/", "--pair", "K7M2-9QWD-X3"
$finished = $hub.WaitForExit(6 * 60 * 1000)
$code = if ($finished) { $hub.ExitCode } else { -1 }
Start-Sleep -Milliseconds 500

# 5. What is left on disk, then stop the launcher (its Job Object takes the agent with it).
""; "---- fake hub"; Get-Content "$Work\hub.log"
""; "---- current.json: $((Get-Content "$root\current.json" -Raw).Trim() -replace '\s+', ' ')"
"---- versions on disk: $((Get-ChildItem "$root\versions" -Directory).Name -join ', ')"
"---- health marks: $((Get-ChildItem "$root" -Filter 'healthy.*').Name -join ', ')"
""; "---- launcher log"
Get-Content "$root\logs\launcher.log" | ForEach-Object {
    $j = $_ | ConvertFrom-Json
    $extra = ($j.PSObject.Properties | Where-Object { $_.Name -notin "time", "level", "msg" } | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join " "
    "{0}  {1}  {2}" -f ([datetime]$j.time).ToString("HH:mm:ss.fff"), $j.msg, $extra
}
if (-not $hub.HasExited) { Stop-Process -Id $hub.Id -Confirm:$false }
if (-not $launcher.HasExited) { Stop-Process -Id $launcher.Id -Confirm:$false }
Start-Sleep -Milliseconds 500
$left = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Work*" })
""; "processes left from the dry run: $($left.Count)"
"dry run exit code: $code"
exit $code
