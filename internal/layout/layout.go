// Package layout is the install folder of the CR agent (PLAN §9.1, ruling B22), shared by the
// launcher and the agent:
//
//	<root>\launcher.exe               stable; starts the version current.json names
//	<root>\current.json               {version, previous, pending_since}, replaced atomically
//	<root>\versions\<version>\        cragent.exe, launcher.exe (staged), manifest.json, manifest.json.minisig
//	<root>\engine\<llama tag>\        llama-server.exe and its DLLs
//	<root>\healthy.<version>          written by an agent that passed the health gate
//	<root>\downloads\                 partial update archives
//	<root>\models\, state\, logs\     the store of the agent (internal/store)
package layout

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ExitRestart is the exit code that asks the launcher to start the version the pointer names.
const ExitRestart = 75

// HealthGate is how long a new version has to prove itself before the launcher reverts.
const HealthGate = 2 * time.Minute

// Pointer is current.json.
type Pointer struct {
	Version      string     `json:"version"`
	Previous     string     `json:"previous,omitempty"`
	PendingSince *time.Time `json:"pending_since,omitempty"` // set while Version is on trial
}

var reVersion = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`)

// ValidVersion checks a version before it becomes part of a path.
func ValidVersion(v string) bool { return reVersion.MatchString(v) }

// PointerFile is current.json.
func PointerFile(root string) string { return filepath.Join(root, "current.json") }

// VersionDir is the folder of one agent version.
func VersionDir(root, v string) string { return filepath.Join(root, "versions", v) }

// AgentExe is the agent binary of a version.
func AgentExe(root, v string) string { return filepath.Join(root, "versions", v, "cragent.exe") }

// StagedLauncher is the launcher a release ships, before it replaces the running one.
func StagedLauncher(root, v string) string { return filepath.Join(root, "versions", v, "launcher.exe") }

// ManifestFile is the signed manifest kept beside a version, for the launcher.
func ManifestFile(root, v string) string { return filepath.Join(root, "versions", v, "manifest.json") }

// LauncherExe is the stable launcher.
func LauncherExe(root string) string { return filepath.Join(root, "launcher.exe") }

// EngineDir is the folder of one engine build.
func EngineDir(root, tag string) string { return filepath.Join(root, "engine", tag) }

// HealthyFile marks a version that passed the health gate.
func HealthyFile(root, v string) string { return filepath.Join(root, "healthy."+v) }

// DownloadsDir holds partial update archives.
func DownloadsDir(root string) string { return filepath.Join(root, "downloads") }

// ReadPointer reads current.json.
func ReadPointer(root string) (Pointer, error) {
	var p Pointer
	b, err := os.ReadFile(PointerFile(root))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, errors.New("current.json is damaged")
	}
	if !ValidVersion(p.Version) || (p.Previous != "" && !ValidVersion(p.Previous)) {
		return p, errors.New("current.json names a bad version")
	}
	return p, nil
}

// WritePointer replaces current.json atomically and durably: a crash leaves the old or the new file,
// never half of one.
func WritePointer(root string, p Pointer) error {
	if !ValidVersion(p.Version) || (p.Previous != "" && !ValidVersion(p.Previous)) {
		return errors.New("bad version for current.json")
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(PointerFile(root), append(b, '\n'))
}

// WriteAtomic writes a file beside its target, flushes it, and moves it over the target.
func WriteAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = Replace(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

// Find tells whether exe is an installed agent, ...\versions\<version>\cragent.exe beside a
// current.json, and returns the install folder and the version of that folder.
func Find(exe string) (root, version string, ok bool) {
	dir := filepath.Dir(exe)
	version = filepath.Base(dir)
	versions := filepath.Dir(dir)
	if !strings.EqualFold(filepath.Base(exe), "cragent.exe") || !strings.EqualFold(filepath.Base(versions), "versions") || !ValidVersion(version) {
		return "", "", false
	}
	root = filepath.Dir(versions)
	if _, err := os.Stat(PointerFile(root)); err != nil {
		return "", "", false
	}
	return root, version, true
}

// Installed lists the versions on disk.
func Installed(root string) []string {
	ents, err := os.ReadDir(filepath.Join(root, "versions"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && ValidVersion(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}
