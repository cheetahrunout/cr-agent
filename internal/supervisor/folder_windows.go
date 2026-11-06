//go:build windows

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/cheetahrunout/cr-agent/internal/confine"
)

// Which folders the host may offer the engine's sandbox. A read grant goes to every AppContainer on the
// PC and reaches everything below the folder, so it must never cover the agent's keys or a folder that
// holds the host's own files. Paths are resolved first, so a link or a short name changes nothing, and the
// broad folders come from the system, so a redirected or translated Documents is still Documents.

// resolvePath is the path Windows finally reaches. The part that does not exist yet is kept as given.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; cur = filepath.Dir(cur) {
		final, err := confine.FinalPath(cur)
		if err == nil {
			return filepath.Join(final, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return "", err
		}
		if filepath.Dir(cur) == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

// pathUnder reports whether p is parent or below it. Both are already resolved.
func pathUnder(p, parent string) bool {
	p, parent = strings.ToLower(filepath.Clean(p)), strings.ToLower(filepath.Clean(parent))
	return p == parent || strings.HasPrefix(p, strings.TrimRight(parent, `\`)+`\`)
}

// IsOwnModelFolder reports whether dir is <home>\models or below it, the only part of the agent's folder
// that may hold models.
func IsOwnModelFolder(dir, home string) bool {
	res, err := resolvePath(dir)
	if err != nil {
		return false
	}
	h, err := resolvePath(home)
	if err != nil {
		return false
	}
	return pathUnder(res, filepath.Join(h, "models"))
}

// broadFolders are the folders that hold the host's own things or the system's, in the system's own words.
func broadFolders() []string {
	var out []string
	for _, id := range []*windows.KNOWNFOLDERID{
		windows.FOLDERID_Profile, windows.FOLDERID_Windows,
		windows.FOLDERID_ProgramFiles, windows.FOLDERID_ProgramFilesX86, windows.FOLDERID_ProgramFilesX64,
		windows.FOLDERID_Documents, windows.FOLDERID_Desktop, windows.FOLDERID_Downloads,
		windows.FOLDERID_Pictures, windows.FOLDERID_Music, windows.FOLDERID_Videos,
		windows.FOLDERID_RoamingAppData, windows.FOLDERID_LocalAppData, windows.FOLDERID_LocalAppDataLow,
	} {
		p, err := windows.KnownFolderPath(id, 0)
		if err != nil {
			continue
		}
		if res, err := resolvePath(p); err == nil {
			out = append(out, res)
			if id == windows.FOLDERID_RoamingAppData || id == windows.FOLDERID_LocalAppData {
				out = append(out, filepath.Dir(res)) // AppData itself has no id of its own
			}
		}
	}
	return out
}

const refusedBroad = "This folder holds more than models, so the engine must not be allowed to read it. Make a subfolder just for your models, and choose that."
const refusedHome = "This folder holds CR Agent's own keys and state. Choose its models folder, or a folder of your own just for models."

// CheckModelFolder says why a folder cannot be the model folder, in words for the host; nil when it can.
func CheckModelFolder(dir, home string) error {
	res, err := resolvePath(dir)
	if err != nil {
		return errors.New("This folder cannot be read. Choose another.")
	}
	h, err := resolvePath(home)
	if err != nil {
		return errors.New("CR Agent's own folder cannot be read.")
	}
	if pathUnder(res, filepath.Join(h, "models")) {
		return nil
	}
	if pathUnder(res, h) {
		return errors.New(refusedHome)
	}
	if pathUnder(h, res) {
		return errors.New(refusedBroad)
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if p16, err := windows.UTF16PtrFromString(res); err == nil && windows.GetVolumePathName(p16, &buf[0], uint32(len(buf))) == nil {
		if vol := windows.UTF16ToString(buf); strings.EqualFold(filepath.Clean(vol), filepath.Clean(res)) {
			return errors.New(refusedBroad) // a drive's root, or a volume mounted on a folder
		}
	}
	for _, b := range broadFolders() {
		if pathUnder(b, res) { // the folder itself, or one that contains it
			return errors.New(refusedBroad)
		}
	}
	return nil
}
