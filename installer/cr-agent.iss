; Inno Setup 6 script for the CR agent (PLAN §9.1, §13).
;
; Per user, no admin. Installs into %LOCALAPPDATA%\cr-agent, or a folder the host chooses at the first install:
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
; The folder is chosen at the first install. Installing over an existing agent keeps its folder, with its pairing and models.
DisableDirPage=auto
DirExistsWarning=no
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

function GetVolumeInformation(Root, VolumeName: String; VolumeNameSize: Cardinal; var Serial, MaxLength, Flags: Cardinal; FileSystem: String; FileSystemSize: Cardinal): Cardinal;
  external 'GetVolumeInformationW@kernel32.dll stdcall';
function GetDriveType(Root: String): Cardinal;
  external 'GetDriveTypeW@kernel32.dll stdcall';

function IsUnder(Dir, Parent: String): Boolean;
begin
  Dir := Lowercase(AddBackslash(Dir));
  Parent := Lowercase(AddBackslash(Parent));
  Result := Copy(Dir, 1, Length(Parent)) = Parent;
end;

// True when the folder holds nothing but the entries . and ..
function IsEmptyFolder(Dir: String): Boolean;
var
  F: TFindRec;
begin
  Result := True;
  if FindFirst(Dir + '\*', F) then
  begin
    try
      repeat
        if (F.Name <> '.') and (F.Name <> '..') then
          Result := False;
      until (not Result) or not FindNext(F);
    finally
      FindClose(F);
    end;
  end;
end;

// The uninstaller leaves the model folder: models are large and the host's own. So <folder>\models, empty or with the
// agent's own index in it, is what an earlier install left, and installing there again is a reinstall.
function IsLeftByAnAgent(Dir: String; F: TFindRec): Boolean;
begin
  Result := (Lowercase(F.Name) = 'models') and ((F.Attributes and FILE_ATTRIBUTE_DIRECTORY) <> 0)
    and (FileExists(Dir + '\models\cr-agent-index.json') or IsEmptyFolder(Dir + '\models'));
end;

// True when the folder holds anything else. An agent that is already there does not count: that is a reinstall.
function HoldsOtherFiles(Dir: String): Boolean;
var
  F: TFindRec;
begin
  Result := False;
  if FileExists(Dir + '\launcher.exe') then
    Exit;
  if FindFirst(Dir + '\*', F) then
  begin
    try
      repeat
        if (F.Name <> '.') and (F.Name <> '..') and not IsLeftByAnAgent(Dir, F) then
          Result := True;
      until Result or not FindNext(F);
    finally
      FindClose(F);
    end;
  end;
end;

// Why this folder cannot hold the agent, in words for the host; empty when it can. The agent keeps its keys here, updates
// itself here without admin rights, and its engine's sandbox depends on the folder's permissions, which need NTFS or ReFS.
function FolderProblem(Dir: String): String;
var
  Serial, MaxLength, Flags: Cardinal;
  Name, FileSystem, Probe: String;
  Created: Boolean;
begin
  Result := '';
  if Copy(Dir, 1, 2) = '\\' then
  begin
    Result := 'Choose a folder on a drive in this PC, not on a network.';
    Exit;
  end;
  if GetDriveType(ExtractFileDrive(Dir) + '\') <> 3 then
  begin
    Result := 'Choose a folder on a fixed drive in this PC, not on a network or removable drive.';
    Exit;
  end;
  Name := StringOfChar(' ', 64);
  FileSystem := StringOfChar(' ', 64);
  if GetVolumeInformation(ExtractFileDrive(Dir) + '\', Name, 64, Serial, MaxLength, Flags, FileSystem, 64) = 0 then
  begin
    Result := 'This drive cannot be read. Choose another.';
    Exit;
  end;
  FileSystem := Uppercase(Trim(Copy(FileSystem, 1, Pos(#0, FileSystem + #0) - 1)));
  if (FileSystem <> 'NTFS') and (FileSystem <> 'REFS') then
  begin
    Result := 'CR Agent needs a drive formatted as NTFS, because it protects its keys and the engine with file permissions. This drive is ' + FileSystem + '.';
    Exit;
  end;
  if IsUnder(Dir, ExpandConstant('{win}')) or IsUnder(Dir, ExpandConstant('{commonpf32}')) or IsUnder(Dir, ExpandConstant('{commonpf64}')) then
  begin
    Result := 'CR Agent updates itself without administrator rights, so it cannot live in Program Files or Windows. Choose another folder.';
    Exit;
  end;
  if HoldsOtherFiles(Dir) then
  begin
    Result := 'This folder already holds other files. Choose a new or empty folder: CR Agent makes it private to you.';
    Exit;
  end;
  Created := not DirExists(Dir);
  Probe := Dir + '\.cr-agent-write-test';
  if not ForceDirectories(Dir) or not SaveStringToFile(Probe, 'x', False) then
  begin
    Result := 'You cannot write to this folder without administrator rights. Choose another.';
    Exit;
  end;
  DeleteFile(Probe);
  if Created then
    RemoveDir(Dir);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Problem: String;
begin
  Result := True;
  if CurPageID = wpSelectDir then
  begin
    Problem := FolderProblem(WizardDirValue);
    if Problem <> '' then
    begin
      MsgBox(Problem, mbError, MB_OK);
      Result := False;
    end;
  end;
end;

// %LOCALAPPDATA% is private to the person by default. Another drive usually lets every user of this PC change what is on it,
// and the launcher runs at sign-in, so a folder elsewhere gets the same three entries the default one has.
function MakePrivate(Dir: String): Boolean;
var
  Code: Integer;
  Icacls: String;
begin
  Icacls := ExpandConstant('{sys}\icacls.exe');
  Result := ForceDirectories(Dir)
    and Exec(Icacls, '"' + Dir + '" /grant:r "' + ExpandConstant('{username}') + ':(OI)(CI)F" *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F', '', SW_HIDE, ewWaitUntilTerminated, Code)
    and (Code = 0)
    and Exec(Icacls, '"' + Dir + '" /inheritance:r', '', SW_HIDE, ewWaitUntilTerminated, Code)
    and (Code = 0);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Dir: String;
begin
  Dir := ExpandConstant('{app}');
  // Also here, for an install without the wizard page (/DIR= on a silent install).
  Result := FolderProblem(Dir);
  if Result <> '' then
    Exit;
#ifndef Scratch
  CloseRunningAgent;
#endif
  if not IsUnder(Dir, ExpandConstant('{localappdata}')) and not MakePrivate(Dir) then
    Result := 'The permissions of this folder could not be set, so CR Agent was not installed. Choose another folder.';
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  // The pointer is written last: the launcher never sees a version whose files are not all there.
  if CurStep = ssPostInstall then
    SaveStringToFile(ExpandConstant('{app}\current.json'), '{"version":"{#AppVersion}"}' + #13#10, False);
end;

function InitializeUninstall(): Boolean;
begin
#ifndef Scratch
  CloseRunningAgent;
#endif
  Result := True;
end;
