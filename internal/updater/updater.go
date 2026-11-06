// Package updater installs a signed release into the versioned layout (PLAN §13, rulings B19–B23).
//
// The hub sends only a version. Everything else is the agent's own: the URL is the fixed GitHub
// prefix plus the tag; the manifest and its signature are fetched and verified against the compiled-in
// trust list before any archive is fetched; archives are hashed before they are unpacked; every
// unpacked file must be listed in the manifest with its hash; and a release older than the one
// committed on this PC is refused. The running version is not touched: the new one is written beside
// it, the launcher's pointer is flipped, and the launcher reverts if the new agent is not healthy.
package updater

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/confine"
	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

// secretRelease is the DPAPI blob holding committed_seq and the installed manifest (ruling B21).
const secretRelease = "release"

// Config configures an updater.
type Config struct {
	Root    string // the install folder
	Version string // the version of the running agent
	Store   *store.Store
	Keys    []trust.Key // the keys a manifest may be signed with
	// Base is empty in every release build (the GitHub prefix). A debug build may name a local server.
	Base       string
	Transport  http.RoundTripper
	Log        *slog.Logger
	HubVersion func() string // the hub's version from welcome, "" when unknown
	StopEngine func()        // called before the pointer is flipped
	Now        func() time.Time
	RetryDelay time.Duration // between download attempts, multiplied by the attempt (1 s)
	StallAfter time.Duration // a download that delivers no bytes for this long is cancelled (60 s)
}

// Error is a refused or failed update, shaped for cmd.err.
type Error struct {
	Code string // update_failed | hub_too_old | busy
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// ErrAlreadyInstalled is returned for the running version when its files are intact: there is
// nothing to do and nothing to restart.
var ErrAlreadyInstalled = errors.New("this version is already installed and intact")

func failf(format string, a ...any) *Error {
	return &Error{Code: "update_failed", Msg: fmt.Sprintf(format, a...)}
}

// Progress reports a phase: downloading, verifying, installing, restarting.
type Progress func(state string, pct float64)

type pending struct {
	Version  string `json:"version"`
	Manifest []byte `json:"manifest"`
	Sig      []byte `json:"sig"`
}

// state is what the DPAPI blob holds.
type state struct {
	CommittedSeq     int64    `json:"committed_seq"`
	CommittedVersion string   `json:"committed_version"`
	Manifest         []byte   `json:"manifest,omitempty"`
	Sig              []byte   `json:"sig,omitempty"`
	Pending          *pending `json:"pending,omitempty"`
	// The newest release the daily check verified (§13, ruling B9): the hub has no say in these.
	LatestVersion string `json:"latest_version,omitempty"`
	LatestSeq     int64  `json:"latest_seq,omitempty"`
}

// Updater installs releases and guards the installed one.
type Updater struct {
	cfg Config
	log *slog.Logger
	f   *fetcher

	mu         sync.Mutex
	busy       bool
	manifest   *release.Manifest // the verified manifest of the running version, when there is one
	raw, sig   []byte
	toCommit   bool   // the running version's manifest is verified but not committed yet
	needFetch  bool   // no manifest for the running version on this PC yet
	rolledBack string // a version the launcher reverted
	engineOK   map[string]error
}

// New builds an updater. A Base other than the GitHub prefix is refused unless this is a debug build.
func New(cfg Config) (*Updater, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = time.Second
	}
	if cfg.StallAfter <= 0 {
		cfg.StallAfter = 60 * time.Second
	}
	if !layout.ValidVersion(cfg.Version) {
		return nil, errors.New("updater: bad running version")
	}
	f, err := newFetcher(cfg.Base, cfg.Transport)
	if err != nil {
		return nil, err
	}
	return &Updater{cfg: cfg, log: cfg.Log, f: f, engineOK: map[string]error{}}, nil
}

// ErrStateUnreadable means the release state exists but cannot be opened or understood. It is not a
// fresh install: treating it as one would forget committed_seq and allow a downgrade.
var ErrStateUnreadable = errors.New("the release state of this PC cannot be read")

// load reads the release state. Only a state that does not exist is a fresh install.
func (u *Updater) load() (state, error) {
	var st state
	b, err := u.cfg.Store.ReadSecret(secretRelease)
	if errors.Is(err, store.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("%w: %v", ErrStateUnreadable, err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return state{}, fmt.Errorf("%w: damaged", ErrStateUnreadable)
	}
	return st, nil
}

// floor is the release a new one must not be older than: the higher of the committed one and the
// one that is running, which may be on trial and not committed yet. The caller holds u.mu.
func (u *Updater) floor(st state) (int64, string) {
	seq, version := st.CommittedSeq, st.CommittedVersion
	if u.manifest != nil && u.manifest.ReleaseSeq > seq {
		seq, version = u.manifest.ReleaseSeq, u.manifest.Version
	}
	return seq, version
}

func (u *Updater) save(st state) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return u.cfg.Store.WriteSecret(secretRelease, b)
}

// Unverified reports that the running version has no checked manifest yet (see Bootstrap).
func (u *Updater) Unverified() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.needFetch
}

// Committed is the release sequence and version this PC is committed to.
func (u *Updater) Committed() (int64, string) {
	st, _ := u.load()
	return st.CommittedSeq, st.CommittedVersion
}

// RolledBack names a version the launcher reverted since the last start, once.
func (u *Updater) RolledBack() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	v := u.rolledBack
	u.rolledBack = ""
	return v
}

func hashFile(path string) (string, int64, error) { return release.HashFile(path) }

// verifyFiles checks the manifest's files on disk. prefix limits the check ("versions/" or "engine/").
func (u *Updater) verifyFiles(m *release.Manifest, prefix string) error {
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		sum, size, err := hashFile(filepath.Join(u.cfg.Root, filepath.FromSlash(f.Path)))
		if err != nil {
			return fmt.Errorf("%s is missing", f.Path)
		}
		if sum != f.SHA256 || size != f.Size {
			return fmt.Errorf("%s does not match the signed release", f.Path)
		}
	}
	return nil
}

// Startup runs at every start of an installed agent (ruling B21): the manifest of the running
// version must carry a valid signature, and the agent's own files must match it. An error means the
// install cannot be trusted and the agent must not go on.
func (u *Updater) Startup() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := u.load()
	if err != nil {
		return err
	}
	v := u.cfg.Version
	var raw, sig []byte
	switch {
	case st.Pending != nil && st.Pending.Version == v: // on trial after an update
		raw, sig, u.toCommit = st.Pending.Manifest, st.Pending.Sig, true
	case st.CommittedVersion == v && len(st.Manifest) > 0:
		raw, sig = st.Manifest, st.Sig
		if st.Pending != nil { // the launcher reverted the pending version
			u.rolledBack = st.Pending.Version
			st.Pending = nil
			if err := u.save(st); err != nil {
				u.log.Warn("release state not saved", "err", err.Error())
			}
		}
	default: // first start after the installer, or a version put here outside the updater
		// manifest.json is written last (writeManifest), so without it there is no manifest, whatever
		// else a crash left behind.
		r, rerr := os.ReadFile(layout.ManifestFile(u.cfg.Root, v))
		s, serr := os.ReadFile(layout.ManifestFile(u.cfg.Root, v) + ".minisig")
		if rerr != nil {
			u.needFetch = true
			u.log.Info("no manifest for this version on this PC yet; it will be fetched and checked", "version", v)
			return nil
		}
		if serr != nil {
			return fmt.Errorf("the manifest of %s has no signature beside it", v)
		}
		raw, sig, u.toCommit = r, s, true
	}
	m, verr := release.Verify(raw, sig, u.cfg.Keys)
	if verr != nil {
		return fmt.Errorf("the installed release does not verify: %w", verr)
	}
	if m.Version != v {
		return fmt.Errorf("the stored manifest is for %s, not for the running %s", m.Version, v)
	}
	if u.toCommit {
		if err := release.CheckSeq(m, st.CommittedSeq, st.CommittedVersion); err != nil {
			return fmt.Errorf("this version is older than the one this PC is committed to: %w", err)
		}
	}
	if err := u.verifyFiles(m, "versions/"); err != nil {
		return fmt.Errorf("the installed agent was changed: %w", err)
	}
	u.manifest, u.raw, u.sig = m, raw, sig
	u.log.Info("installed release verified", "version", m.Version, "release_seq", m.ReleaseSeq, "key_id", m.KeyID, "on_trial", u.toCommit)
	return nil
}

// Bootstrap fetches and checks the manifest of the running version when the PC has none (a fresh
// install: the installer cannot carry the manifest that lists the installer). Until it succeeds the
// install is unverified; updates are still verified in full.
func (u *Updater) Bootstrap(ctx context.Context) error {
	u.mu.Lock()
	need := u.needFetch
	u.mu.Unlock()
	if !need {
		return nil
	}
	v := u.cfg.Version
	m, raw, sig, err := u.fetchVerified(ctx, v)
	if err != nil {
		return fmt.Errorf("the manifest of this version %w", err)
	}
	st, err := u.load()
	if err != nil {
		return err
	}
	if m.Version != v {
		return errors.New("the published manifest is for another version")
	}
	if err := release.CheckSeq(m, st.CommittedSeq, st.CommittedVersion); err != nil {
		return err
	}
	if err := u.verifyFiles(m, "versions/"); err != nil {
		return fmt.Errorf("the installed agent does not match its signed release: %w", err)
	}
	if err := u.writeManifest(m, raw, sig); err != nil {
		return err
	}
	u.mu.Lock()
	u.manifest, u.raw, u.sig, u.needFetch, u.toCommit = m, raw, sig, false, true
	u.mu.Unlock()
	u.log.Info("installed release verified against its published manifest", "version", v, "release_seq", m.ReleaseSeq)
	return u.MarkHealthy()
}

// writeManifest stores the signed pair beside a version so that a crash cannot leave half of it:
// manifest.json is the commit point. It is removed first and written last, so at any moment the
// folder holds a whole pair, or no manifest at all, which simply means "not checked yet".
func (u *Updater) writeManifest(m *release.Manifest, raw, sig []byte) error {
	path := layout.ManifestFile(u.cfg.Root, m.Version)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := layout.WriteAtomic(path+".minisig", sig); err != nil {
		return err
	}
	return layout.WriteAtomic(path, raw)
}

// MarkHealthy is called once the agent passed the health gate: it writes healthy.<version> for the
// launcher and commits the running release, which moves committed_seq forward (ruling B21).
func (u *Updater) MarkHealthy() error {
	if err := os.WriteFile(layout.HealthyFile(u.cfg.Root, u.cfg.Version), []byte(u.cfg.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.toCommit || u.manifest == nil {
		return nil
	}
	st, err := u.load()
	if err != nil {
		return err
	}
	st.CommittedSeq, st.CommittedVersion, st.Manifest, st.Sig, st.Pending = u.manifest.ReleaseSeq, u.manifest.Version, u.raw, u.sig, nil
	if err := u.save(st); err != nil {
		return err
	}
	u.toCommit = false
	u.log.Info("release committed", "version", st.CommittedVersion, "committed_seq", st.CommittedSeq)
	return nil
}

// VerifyEngine checks the engine files of the running release before they are run. The result is
// kept for the life of the process. Without a manifest (a fresh install not yet verified) it passes.
func (u *Updater) VerifyEngine() error {
	u.mu.Lock()
	m := u.manifest
	u.mu.Unlock()
	if m == nil {
		return nil
	}
	u.mu.Lock()
	err, done := u.engineOK[m.LlamaCpp.Tag]
	u.mu.Unlock()
	if done {
		return err
	}
	err = u.verifyFiles(m, "engine/")
	u.mu.Lock()
	u.engineOK[m.LlamaCpp.Tag] = err
	u.mu.Unlock()
	return err
}

// fetchVerified fetches a version's manifest and signature and verifies them. During a key
// rotation a release carries two signatures (ruling B20): manifest.json.minisig by the new key and
// manifest.json.old.minisig by the outgoing one, so an agent that trusts only the outgoing key
// falls back to the second file.
func (u *Updater) fetchVerified(ctx context.Context, version string) (*release.Manifest, []byte, []byte, error) {
	raw, sig, err := u.f.manifest(ctx, version)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not be fetched: %w", err)
	}
	m, err := release.Verify(raw, sig, u.cfg.Keys)
	if errors.Is(err, release.ErrUntrustedKey) {
		if old, oerr := u.f.small(ctx, "v"+version+"/manifest.json.old.minisig", 4096); oerr == nil {
			if m2, err2 := release.Verify(raw, old, u.cfg.Keys); err2 == nil {
				return m2, raw, old, nil
			}
		}
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("was refused: %w", err)
	}
	return m, raw, sig, nil
}

// CheckLatest is the daily check (§13, ruling B9), so a hacked hub cannot hide an update: it fetches
// the manifest of the release GitHub calls latest, from the fixed prefix and never from the hub,
// verifies it the same way as any update, and stores the version. It returns that version.
func (u *Updater) CheckLatest(ctx context.Context) (string, error) {
	raw, err := u.f.latestFile(ctx, "manifest.json", 4<<20)
	if err != nil {
		return "", fmt.Errorf("the latest manifest could not be fetched: %w", err)
	}
	sig, err := u.f.latestFile(ctx, "manifest.json.minisig", 4096)
	if err != nil {
		return "", fmt.Errorf("the latest signature could not be fetched: %w", err)
	}
	m, err := release.Verify(raw, sig, u.cfg.Keys)
	if errors.Is(err, release.ErrUntrustedKey) { // a key rotation: the outgoing key signs too (ruling B20)
		if old, oerr := u.f.latestFile(ctx, "manifest.json.old.minisig", 4096); oerr == nil {
			m, err = release.Verify(raw, old, u.cfg.Keys)
		}
	}
	if err != nil {
		return "", fmt.Errorf("the latest manifest was refused: %w", err)
	}
	if strings.Contains(m.Notes, "ROLLBACK TEST") {
		return "", errors.New("the release marked latest is a rollback test; ignored")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := u.load()
	if err != nil {
		return "", err
	}
	if m.ReleaseSeq < st.LatestSeq { // "latest" went back: keep what was verified before
		return st.LatestVersion, nil
	}
	st.LatestVersion, st.LatestSeq = m.Version, m.ReleaseSeq
	if err := u.save(st); err != nil {
		return "", err
	}
	return m.Version, nil
}

// LatestVerified is the newest version the daily check verified, "" before the first success.
func (u *Updater) LatestVerified() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := u.load()
	if err != nil {
		return ""
	}
	return st.LatestVersion
}

// UpdateAvailable is the verified latest version when it is newer than what this PC runs, else "".
func (u *Updater) UpdateAvailable() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := u.load()
	if err != nil || st.LatestVersion == "" || st.LatestVersion == u.cfg.Version {
		return ""
	}
	floor, _ := u.floor(st)
	if st.LatestSeq <= floor || !semverLess(u.cfg.Version, st.LatestVersion) {
		return ""
	}
	return st.LatestVersion
}

func semverLess(a, b string) bool {
	var x, y [3]int
	fmt.Sscanf(a, "%d.%d.%d", &x[0], &x[1], &x[2])
	fmt.Sscanf(b, "%d.%d.%d", &y[0], &y[1], &y[2])
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

// Update installs a version and flips the launcher's pointer to it. When it returns nil the caller
// exits with layout.ExitRestart and the launcher starts the new version on trial.
func (u *Updater) Update(ctx context.Context, version string, progress Progress) error {
	return u.update(ctx, version, progress, true)
}

// UpdateLocal is Update started by the host on the PC. It skips min_hub_version: the hub's version
// comes from the hub's welcome, so a hub could otherwise use it to block the tray's way past the hub.
func (u *Updater) UpdateLocal(ctx context.Context, version string, progress Progress) error {
	return u.update(ctx, version, progress, false)
}

func (u *Updater) update(ctx context.Context, version string, progress Progress, checkHub bool) error {
	if progress == nil {
		progress = func(string, float64) {}
	}
	if !release.ValidVersion(version) {
		return failf("bad version")
	}
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return &Error{Code: "busy", Msg: "an update is already running"}
	}
	u.busy = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
	}()
	root := u.cfg.Root
	ptr, err := layout.ReadPointer(root)
	if err != nil {
		return failf("this copy of the agent was not installed by the installer")
	}

	// 1. The manifest and its signature, and nothing else, until they are verified.
	progress("verifying", 0)
	m, raw, sig, err := u.fetchVerified(ctx, version)
	if err != nil {
		return failf("the release manifest %v", err)
	}
	if m.Version != version {
		return failf("the manifest is for version %s, not %s", m.Version, version)
	}
	st, err := u.load()
	if err != nil {
		return failf("%v", err)
	}
	u.mu.Lock()
	floorSeq, floorVersion := u.floor(st)
	unverified := u.needFetch
	u.mu.Unlock()
	if err := release.CheckSeq(m, floorSeq, floorVersion); err != nil {
		return failf("the release was refused: %v", err)
	}
	// A running version whose manifest could not be checked yet has no known sequence: until it does,
	// its version number is the floor.
	if unverified && semverLess(version, u.cfg.Version) {
		return failf("the release was refused: %s is older than the running %s, whose signed manifest could not be checked yet", version, u.cfg.Version)
	}
	if hub := ""; checkHub && u.cfg.HubVersion != nil && m.MinHubVersion != "" {
		if hub = u.cfg.HubVersion(); release.ValidVersion(hub) && semverLess(hub, m.MinHubVersion) {
			return &Error{Code: "hub_too_old", Msg: "this release needs hub " + m.MinHubVersion + " or newer"}
		}
	}
	if version == u.cfg.Version { // a reinstall of the running version: only a check is possible
		if err := u.verifyFiles(m, ""); err != nil {
			return failf("the running version cannot be repaired while it runs: %v", err)
		}
		return ErrAlreadyInstalled
	}

	// 2. Archives: only those whose hash differs from the installed release, then any that turn out to be needed.
	installed := map[string]string{}
	if len(st.Manifest) > 0 {
		var cur release.Manifest
		if json.Unmarshal(st.Manifest, &cur) == nil {
			for _, a := range cur.Archives {
				installed[a.Name] = a.SHA256
			}
		}
	}
	var fetch, skipped []release.Archive
	var total int64
	for _, a := range m.Zips() {
		if installed[a.Name] == a.SHA256 {
			skipped = append(skipped, a)
			continue
		}
		fetch = append(fetch, a)
		total += a.Size
	}
	var done int64
	install := func(list []release.Archive) error {
		for _, a := range list {
			path, err := u.download(ctx, version, a, func(n int64) {
				if total > 0 {
					progress("downloading", float64(int64(float64(done+n)/float64(total)*1000))/10)
				}
			})
			if err != nil {
				return err
			}
			done += a.Size
			progress("verifying", 100)
			err = u.extract(path, m)
			os.Remove(path)
			if err != nil {
				return failf("%s: %v", a.Name, err)
			}
		}
		return nil
	}
	if err := install(fetch); err != nil {
		return asError(err)
	}
	if err := u.verifyFiles(m, ""); err != nil && len(skipped) > 0 {
		// A file of an archive that looked installed is missing or changed: fetch that archive after all.
		for _, a := range skipped {
			total += a.Size
		}
		if err := install(skipped); err != nil {
			return asError(err)
		}
	}
	if err := u.verifyFiles(m, ""); err != nil {
		return failf("after unpacking, %v", err)
	}
	if err := u.removeExtras(m); err != nil {
		return failf("%v", err)
	}
	if err := u.writeManifest(m, raw, sig); err != nil {
		return failf("the manifest could not be stored beside the new version")
	}

	// 3. Install: the launcher, the pending record, then the pointer.
	progress("installing", 100)
	if err := u.replaceLauncher(m); err != nil {
		return failf("the launcher could not be replaced: %v", err)
	}
	// The download took a while: a MarkHealthy may have committed meanwhile. Read the state again
	// just before writing it, under the lock, and judge the release against what is there now.
	u.mu.Lock()
	cur, err := u.load()
	if err == nil {
		fs, fv := u.floor(cur)
		err = release.CheckSeq(m, fs, fv)
	}
	if err == nil {
		cur.Pending = &pending{Version: m.Version, Manifest: raw, Sig: sig}
		err = u.save(cur)
	}
	u.mu.Unlock()
	if err != nil {
		return failf("the pending release could not be recorded: %v", err)
	}
	if u.cfg.StopEngine != nil {
		u.cfg.StopEngine()
	}
	now := u.cfg.Now().UTC()
	os.Remove(layout.HealthyFile(root, m.Version)) // a stale marker must not pass the gate
	if err := layout.WritePointer(root, layout.Pointer{Version: m.Version, Previous: ptr.Version, PendingSince: &now}); err != nil {
		return failf("the launcher pointer could not be written")
	}
	u.log.Info("update installed; restarting into it", "version", m.Version, "release_seq", m.ReleaseSeq, "previous", ptr.Version)
	progress("restarting", 100)
	return nil
}

func asError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return failf("%v", err)
}

// download fetches one archive into downloads\, resuming a partial file, and checks its size and
// SHA-256 before anything reads it.
func (u *Updater) download(ctx context.Context, version string, a release.Archive, tick func(int64)) (string, error) {
	dir := layout.DownloadsDir(u.cfg.Root)
	if err := confine.NoReparse(u.cfg.Root, dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	part := filepath.Join(dir, a.Name+".part")
	var lastErr error
	fresh := false // a stale partial was thrown away once already
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", failf("cancelled")
			case <-time.After(time.Duration(attempt) * u.cfg.RetryDelay):
			}
		}
		fh, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return "", err
		}
		if err := confine.UnderRoot(fh, u.cfg.Root); err != nil {
			fh.Close()
			return "", err
		}
		st, _ := fh.Stat()
		have := st.Size()
		if have > a.Size {
			have = 0
		}
		h := sha256.New()
		if have > 0 {
			if _, err := io.CopyN(h, fh, have); err != nil {
				have = 0
				h.Reset()
			}
		}
		fh.Truncate(have)
		fh.Seek(have, io.SeekStart)
		resumed := have > 0
		if have < a.Size {
			// A connection that stays open and delivers nothing must not hold the update for ever.
			actx, cancelAttempt := context.WithCancel(ctx)
			var stalled atomic.Bool
			watchdog := time.AfterFunc(u.cfg.StallAfter, func() { stalled.Store(true); cancelAttempt() })
			stop := func() { watchdog.Stop(); cancelAttempt() }
			body, offset, size, err := u.f.open(actx, "v"+version+"/"+a.Name, have)
			var se *statusError
			if (err == nil && size >= 0 && size != a.Size) || (errors.As(err, &se) && se.code == http.StatusRequestedRangeNotSatisfiable) {
				// The file on the server has another size than the manifest lists: not this release's archive.
				if body != nil {
					body.Close()
				}
				stop()
				fh.Close()
				os.Remove(part)
				return "", failf("%s does not match the manifest (size)", a.Name)
			}
			if err != nil {
				stop()
				fh.Close()
				lastErr = err
				if errors.As(err, &se) && se.code >= 400 && se.code < 500 {
					break // not there: no retry
				}
				continue
			}
			if offset == 0 && have > 0 { // the server started over
				fh.Truncate(0)
				fh.Seek(0, io.SeekStart)
				h.Reset()
				have = 0
			}
			buf := make([]byte, 1<<20)
			for have < a.Size {
				n, rerr := body.Read(buf)
				if n > 0 {
					watchdog.Reset(u.cfg.StallAfter)
					if have+int64(n) > a.Size {
						rerr = errors.New("more bytes than the manifest lists")
						n = 0
					} else {
						fh.Write(buf[:n])
						h.Write(buf[:n])
						have += int64(n)
						tick(have)
					}
				}
				if rerr != nil {
					if rerr != io.EOF {
						lastErr = rerr
					}
					break
				}
			}
			stop()
			body.Close()
			if stalled.Load() {
				lastErr = fmt.Errorf("no bytes for %s", u.cfg.StallAfter)
			}
		}
		fh.Sync()
		fh.Close()
		if have < a.Size {
			if lastErr == nil {
				lastErr = errors.New("the download ended early")
			}
			continue
		}
		if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
			os.Remove(part)
			if resumed && !fresh { // the partial was of another file: once more from byte 0
				fresh = true
				continue
			}
			return "", failf("%s does not match the manifest (SHA-256)", a.Name)
		}
		return part, nil
	}
	return "", failf("%s could not be downloaded: %v", a.Name, lastErr)
}

// extract unpacks an archive whose hash was verified. Every entry must be a regular file the manifest
// lists, with the listed size and hash; anything else refuses the whole archive before a byte is written.
func (u *Updater) extract(zipPath string, m *release.Manifest) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return errors.New("not a zip archive")
	}
	defer zr.Close()
	var entries []*zip.File
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") && e.UncompressedSize64 == 0 {
			continue // a directory entry
		}
		f, ok := m.File(e.Name)
		switch {
		case !ok:
			return fmt.Errorf("entry %q is not in the manifest", e.Name)
		case !e.Mode().IsRegular():
			return fmt.Errorf("entry %q is not a regular file", e.Name)
		case e.UncompressedSize64 != uint64(f.Size):
			return fmt.Errorf("entry %q has another size than the manifest lists", e.Name)
		}
		entries = append(entries, e)
	}
	root := u.cfg.Root
	for _, e := range entries {
		f, _ := m.File(e.Name)
		target := filepath.Join(root, filepath.FromSlash(e.Name))
		dir := filepath.Dir(target)
		if err := confine.NoReparse(root, dir); err != nil {
			return err
		}
		if sum, size, err := hashFile(target); err == nil && sum == f.SHA256 && size == f.Size {
			continue // already in place
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := confine.NoReparse(root, dir); err != nil {
			return err
		}
		tmp := target + ".new"
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if err := confine.UnderRoot(out, root); err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		rc, err := e.Open()
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		h := sha256.New()
		n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, f.Size+1))
		rc.Close()
		if cerr == nil {
			cerr = out.Sync()
		}
		if err := out.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil || n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			os.Remove(tmp)
			return fmt.Errorf("entry %q does not match the manifest", e.Name)
		}
		if err := layout.Replace(tmp, target); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("%q could not be put in place (in use?)", e.Name)
		}
	}
	return nil
}

// removeExtras deletes anything in the new version's folder that the manifest does not list.
func (u *Updater) removeExtras(m *release.Manifest) error {
	dir := layout.VersionDir(u.cfg.Root, m.Version)
	if err := confine.NoReparse(u.cfg.Root, dir); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(u.cfg.Root, path)
		rel = filepath.ToSlash(rel)
		if _, ok := m.File(rel); ok || rel == "versions/"+m.Version+"/manifest.json" || rel == "versions/"+m.Version+"/manifest.json.minisig" {
			return nil
		}
		return os.Remove(path)
	})
}

// replaceLauncher puts a changed launcher in place: the running launcher.exe is renamed to
// launcher.old (Windows allows renaming a running program) and the new one takes its name, to take
// effect at the next start (ruling B22).
func (u *Updater) replaceLauncher(m *release.Manifest) error {
	f, ok := m.File(m.LauncherPath())
	if !ok {
		return nil
	}
	root := u.cfg.Root
	live := layout.LauncherExe(root)
	if sum, _, err := hashFile(live); err == nil && sum == f.SHA256 {
		return nil
	}
	staged := layout.StagedLauncher(root, m.Version)
	b, err := os.ReadFile(staged)
	if err != nil {
		return err
	}
	old := filepath.Join(root, "launcher.old")
	os.Remove(old)
	if _, err := os.Stat(live); err == nil {
		if err := os.Rename(live, old); err != nil {
			return err
		}
	}
	if err := os.WriteFile(live, b, 0o755); err != nil {
		os.Rename(old, live) // put the old one back
		return err
	}
	return nil
}
