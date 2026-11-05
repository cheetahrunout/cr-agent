# The GPU smoke test of a downloaded release, on the NVIDIA PC, before it is signed (PLAN §13). Also
# run this at every llama.cpp bump: it includes the sentinel test of ruling B16.
#
#   pwsh -File scripts\smoke-gpu.ps1 -Dist <folder with the release's files>
#
# It unpacks the release's engine archive into a temporary folder and checks that
#   - it is the build engine.lock pins, and it knows every flag and chat template name the agent can emit;
#   - it loads a small model on the GPU under low integrity, streams and cancels;
#   - a prompt with a sentinel in it leaves no trace in the agent's log, also when the engine fails on it.
param(
    [Parameter(Mandatory = $true)][string]$Dist,
    [string]$Model = (Join-Path $env:USERPROFILE "Desktop\AI\models\smollm2-360m-q8_0.gguf")
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { $env:PATH = (Join-Path $env:USERPROFILE "sdk\go\bin") + ";$env:PATH" }
if (-not (Test-Path $Model)) { throw "no model at $Model; give one with -Model <a small .gguf>" }
$zip = Get-ChildItem $Dist -Filter "engine-*.zip" | Select-Object -First 1
if (-not $zip) { throw "no engine-*.zip in $Dist" }
$tag = (Get-Content (Join-Path $repo "engine.lock") -Raw | ConvertFrom-Json).llama_tag
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("cr-agent-smoke-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
try {
    "unpacking $($zip.Name) ..."
    Expand-Archive -Path $zip.FullName -DestinationPath $tmp
    $env:CRAGENT_ENGINE_DIR = Join-Path $tmp "engine\$tag"
    $env:CRAGENT_TEST_GGUF = $Model
    $env:CRAGENT_REQUIRE_GPU = "1"
    & go test -count=1 -run 'TestPinnedEngine' ./internal/supervisor
    if ($LASTEXITCODE -ne 0) { throw "the engine is not what engine.lock pins" }
    & go test -count=1 -run 'TestSentinel' ./internal/agent
    if ($LASTEXITCODE -ne 0) { throw "the engine smoke test failed" }
    "SMOKE TEST OK"
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
