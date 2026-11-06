# What's new in CR Agent

Add a section here for every release, newest first: scripts\build-release.ps1 refuses a version without one. Its lines also go into the signed release's notes, so write them for the people who use the agent, keep them to plain text, and do not use double quotes.

## 0.2.7

- Installing again into a folder that still holds your models now works.
- Adds this list: choose What's new in the tray.

## 0.2.6

- You can choose where the agent installs, at the first install. A folder outside the default location is made private to you.
- After a reinstall, models the agent has no record of are no longer shown as ready. Files you added can be added again from the tray, and downloads can be fetched again from the hub.

## 0.2.5

- Fixed: an update could fail with Access is denied if the agent had not restarted since an earlier update that also changed the launcher.
- If antivirus software removes one of the agent's files, the agent now says so.
- The agent's programs now show their name and version in Windows, and every build is scanned with Microsoft Defender before it is signed.

## 0.2.4

- Fixed: the engine could leave a Windows error box on screen.
- Fixed: the check for whether a model fits could report far more graphics memory than the model really needs.

## 0.2.3

- Add model files you already have, with Add model files in the tray.
- The engine now runs inside a Windows sandbox, away from your files, the network and your screen.

## 0.2.2

- A deliberately broken test release, to prove that a failed update goes back by itself. It was withdrawn and never offered to anyone else.

## 0.2.1

- A test release, to prove the update path from one signed version to the next.

## 0.2.0

- The first signed release: pair a PC with the hub, download and run models, and take signed updates.
