# Writes the Windows version details (name, description, version) of one program as a resource the Go linker picks up.
# Data only: nothing in the program reads it, and the code is the same with or without it. A program that says what it
# is shows that in Explorer and to antivirus software. The result is ignored by git; the caller removes it after the build.
#
#   pwsh -File scripts\stamp-version.ps1 -Dir cmd\cragent -Version 0.2.5 -Description "CR Agent" -FileName cragent.exe
#
# The tool is the version go.mod and go.sum pin.
param(
    [Parameter(Mandatory = $true)][string]$Dir,
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Description,
    [Parameter(Mandatory = $true)][string]$FileName
)
$ErrorActionPreference = "Stop"
if ($Version -notmatch '^\d{1,3}\.\d{1,3}\.\d{1,3}$') { throw "version must be x.y.z" }
$parts = $Version.Split('.') | ForEach-Object { [int]$_ }
$number = @{ Major = $parts[0]; Minor = $parts[1]; Patch = $parts[2]; Build = 0 }
$info = @{
    FixedFileInfo  = @{ FileVersion = $number; ProductVersion = $number; FileFlagsMask = "3f"; FileFlags = "00"; FileOS = "040004"; FileType = "01"; FileSubType = "00" }
    StringFileInfo = @{
        CompanyName = "CR Local AI Hub"; ProductName = "CR Agent"; FileDescription = $Description
        FileVersion = "$Version.0"; ProductVersion = $Version; InternalName = [IO.Path]::GetFileNameWithoutExtension($FileName)
        OriginalFilename = $FileName; LegalCopyright = "CR Local AI Hub"
    }
    VarFileInfo    = @{ Translation = @{ LangID = "0409"; CharsetID = "04B0" } }
}
$json = Join-Path ([IO.Path]::GetTempPath()) ("cr-version-" + [guid]::NewGuid().ToString("N").Substring(0, 8) + ".json")
$syso = Join-Path (Resolve-Path $Dir).Path "resource_windows_amd64.syso"
try {
    [IO.File]::WriteAllText($json, ($info | ConvertTo-Json -Depth 5))
    & go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo -64 -o $syso $json
    if ($LASTEXITCODE -ne 0) { throw "goversioninfo failed with exit code $LASTEXITCODE" }
} finally {
    Remove-Item $json -ErrorAction SilentlyContinue
}
