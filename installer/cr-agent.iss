; Inno Setup 6 script for the CR agent (PLAN §9.1, §13).
;
; Per user, no admin. Installs into %LOCALAPPDATA%\cr-agent:
;   launcher.exe                 the stable launcher (Start menu and autostart point at it)
;   current.json                 {"version": "<this version>"}, written after the files are in place
;   versions\<version>\          cragent.exe and the launcher this release ships
;   engine\<llama tag>\          llama-server.exe and its DLLs, as in the official llama.cpp zips
;
; Built by scripts\build-installer.ps1, which stages the files and passes:
;   /DAppVersion=0.1.0 /DLlamaTag=b12125 /DStage=<folder laid out as above> /DOutDir=<folder>
;   /DScratch   a variant for checking the layout: no Start menu entry, no autostart, no uninstall
;               entry in Windows, no compression. It is never published.

#ifndef AppVersion
  #error AppVersion is required (use scripts\build-installer.ps1)
#endif
#ifndef LlamaTag
  #error LlamaTag is required
#endif
#ifndef Stage
  #error Stage is required
#endif
#ifndef OutDir
  #define OutDir "."
#endif

[Setup]
AppId={{8A2E4D5C-3B1F-4C7A-9E6D-2F0B1A7C5D11}
AppName=CR Agent
AppVersion={#AppVersion}
AppVerName=CR Agent {#AppVersion}
AppPublisher=CR Local AI Hub
DefaultDirName={localappdata}\cr-agent
DisableDirPage=yes
DisableProgramGroupPage=yes
DisableReadyPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
WizardStyle=modern
OutputDir={#OutDir}
CloseApplications=no
RestartApplications=no
UninstallDisplayName=CR Agent
#ifdef Scratch
OutputBaseFilename=CRAgent-Setup-{#AppVersion}-scratch
Compression=none
Uninstallable=no
CreateUninstallRegKey=no
#else
OutputBaseFilename=CRAgent-Setup-{#AppVersion}
Compression=lzma2/max
SolidCompression=yes
LZMANumBlockThreads=4
#endif

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

#ifndef Scratch
[Tasks]
Name: "autostart"; Description: "Start CR Agent when I sign in to Windows"; GroupDescription: "Options:"
#endif

[Files]
Source: "{#Stage}\launcher.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Stage}\versions\{#AppVersion}\*"; DestDir: "{app}\versions\{#AppVersion}"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "{#Stage}\engine\{#LlamaTag}\*"; DestDir: "{app}\engine\{#LlamaTag}"; Flags: ignoreversion recursesubdirs createallsubdirs

#ifndef Scratch
[Icons]
; The AppUserModelID is the identity Windows shows notifications under (ruling B14).
Name: "{userprograms}\CR Agent"; Filename: "{app}\launcher.exe"; WorkingDir: "{app}"; AppUserModelID: "CR.Agent"; Comment: "Shares this PC's GPU with your CR Local AI Hub"

[Registry]
; Autostart for this user only; removed by the uninstaller.
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "CRAgent"; ValueData: """{app}\launcher.exe"""; Flags: uninsdeletevalue; Tasks: autostart

[Run]
Filename: "{app}\launcher.exe"; Description: "Start CR Agent now"; WorkingDir: "{app}"; Flags: nowait postinstall skipifsilent

[UninstallDelete]
; Everything the agent wrote, except the model folder: models are large and the host's own.
Type: filesandordirs; Name: "{app}\versions"
Type: filesandordirs; Name: "{app}\engine"
Type: filesandordirs; Name: "{app}\downloads"
Type: filesandordirs; Name: "{app}\logs"
Type: filesandordirs; Name: "{app}\state"
Type: files; Name: "{app}\current.json"
Type: files; Name: "{app}\config.json"
Type: files; Name: "{app}\launcher.old"
Type: files; Name: "{app}\launcher.old.*"
Type: files; Name: "{app}\healthy.*"
Type: dirifempty; Name: "{app}"
#endif

[Code]
const
  WM_CLOSE = $0010;

// The agent's tray window closes the agent the way its Quit item does; the launcher ends with it.
// Nothing is killed by name: only this product's own window class is asked to close.
procedure CloseRunningAgent;
var
  Wnd: HWND;
  Tries: Integer;
begin
  Tries := 0;
  Wnd := FindWindowByClassName('CRAgentTray');
  while (Wnd <> 0) and (Tries < 40) do
  begin
    if Tries = 0 then
      PostMessage(Wnd, WM_CLOSE, 0, 0);
    Sleep(250);
    Tries := Tries + 1;
    Wnd := FindWindowByClassName('CRAgentTray');
  end;
  // Give the launcher a moment to end after the agent, so launcher.exe can be replaced.
  Sleep(500);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  CloseRunningAgent;
  Result := '';
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  // The pointer is written last: the launcher never sees a version whose files are not all there.
  if CurStep = ssPostInstall then
    SaveStringToFile(ExpandConstant('{app}\current.json'), '{"version":"{#AppVersion}"}' + #13#10, False);
end;

function InitializeUninstall(): Boolean;
begin
  CloseRunningAgent;
  Result := True;
end;
