# Scans what was just built with Microsoft Defender, and stops if anything is flagged. A signed release cannot be recalled,
# so a flag must come up here and not on a friend's PC. This never adds an exclusion and never changes Defender's settings.
#
#   pwsh -File scripts\scan-defender.ps1 -Path C:\some\folder, C:\some\file.exe
#
# Throws when something is flagged, when Defender removed or changed a file, or when the scan could not run: a scan that
# did not happen is not a clean scan. -Scanner stands in for Defender's program in a test.
param(
    [Parameter(Mandatory = $true)][string[]]$Path,
    [string]$Scanner = ""
)
$ErrorActionPreference = "Stop"

if (-not $Scanner) {
    $platform = Join-Path $env:ProgramData "Microsoft\Windows Defender\Platform"
    $newest = Get-ChildItem $platform -Directory -ErrorAction SilentlyContinue | Sort-Object LastWriteTime -Descending |
        ForEach-Object { Join-Path $_.FullName "MpCmdRun.exe" } | Where-Object { Test-Path $_ } | Select-Object -First 1
    $Scanner = if ($newest) { $newest } else { Join-Path $env:ProgramFiles "Windows Defender\MpCmdRun.exe" }
}
if (-not (Test-Path $Scanner)) { throw "Microsoft Defender's scanner was not found at $Scanner. Nothing was scanned, so this build stops." }

# What is there before the scan: real-time protection may take a file away whatever this scan says.
$before = @{}
$files = foreach ($p in $Path) {
    if (-not (Test-Path $p)) { throw "$p does not exist; Defender may have removed it" }
    Get-ChildItem $p -File -Recurse
    if (Test-Path $p -PathType Leaf) { Get-Item $p }
}
foreach ($f in $files) { $before[$f.FullName] = (Get-FileHash $f.FullName -Algorithm SHA256).Hash }

foreach ($p in $Path) {
    "scanning $p"
    # -DisableRemediation: report only. What is done about a flag is decided here, not by the scan.
    $output = & $Scanner -Scan -ScanType 3 -File (Resolve-Path $p).Path -DisableRemediation 2>&1
    $code = $LASTEXITCODE
    if ($code -eq 2) { throw "Microsoft Defender flagged something in ${p}:`n$($output -join "`n")`nDo not sign this build, and do not add an exclusion." }
    if ($code -ne 0) { throw "the Defender scan of $p did not finish (exit code $code):`n$($output -join "`n")" }
}

foreach ($name in $before.Keys) {
    if (-not (Test-Path $name)) { throw "$name is gone: Microsoft Defender may have removed it. Do not sign this build, and do not add an exclusion." }
    if ((Get-FileHash $name -Algorithm SHA256).Hash -ne $before[$name]) { throw "$name changed during the scan. Do not sign this build." }
}
"clean: Microsoft Defender flagged nothing in $($before.Count) files"
