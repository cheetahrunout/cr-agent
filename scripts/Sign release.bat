@echo off
rem Double-click this. It runs sign-release.ps1 beside it, in this window, so the passphrase goes straight to signtool.exe.
title Sign a CR Agent release
where pwsh >nul 2>nul
if %errorlevel%==0 (
  pwsh -NoProfile -ExecutionPolicy Bypass -File "%~dp0sign-release.ps1" %*
) else (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0sign-release.ps1" %*
)
echo.
pause
