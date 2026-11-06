package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/release/reltest"
	"github.com/cheetahrunout/cr-agent/internal/sign"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/updater/relsrv"
)

type plain struct{}

func (plain) Protect(_ string, b []byte) ([]byte, error)   { return append([]byte("P:"), b...), nil }
func (plain) Unprotect(_ string, b []byte) ([]byte, error) { return b[2:], nil }

var engineFiles = map[string][]byte{"llama-server.exe": []byte("the engine"), "ggml-cuda.dll": bytes.Repeat([]byte{7}, 300_000)}

// noise is content that does not compress, so an archive is as large as its files.
func noise(seed string, n int) []byte {
	b := make([]byte, 0, n+32)
	h := sha256.Sum256([]byte(seed))
	for len(b) < n {
		b = append(b, h[:]...)
		h = sha256.Sum256(h[:])
	}
	return b[:n]
}

func spec(version string, seq int64) reltest.Spec {
	return reltest.Spec{Version: version, Seq: seq, Agent: append([]byte("agent "+version+" "), noise(version, 300_000)...),
		Launcher: []byte("launcher of " + version), Engine: engineFiles}
}

// world is a release server, an install folder at 0.1.0 and its store.
type world struct {
	t    *testing.T
	srv  *relsrv.Server
	root string
	st   *store.Store
	key  *sign.Key
}

// publish puts a signed release on the server.
func (w *world) publish(s reltest.Spec) *release.Manifest {
	w.t.Helper()
	m, err := reltest.MakeDist(filepath.Join(w.srv.Dir, "v"+s.Version), s, w.key)
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *world) updater(version string) *Updater {
	w.t.Helper()
	u, err := New(Config{Root: w.root, Version: version, Store: w.st, Keys: reltest.Trusting(w.key), Transport: w.srv.Transport(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HubVersion: func() string { return "1.0.12" }, RetryDelay: time.Millisecond})
	if err != nil {
		w.t.Fatal(err)
	}
	return u
}

// newWorld installs 0.1.0 the way the installer leaves it (files and pointer, no manifest), and lets
// the first start fetch and commit its manifest.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, srv: relsrv.New(t.TempDir()), root: filepath.Join(t.TempDir(), "cr-agent"), key: sign.ThrowawayKey()}
	t.Cleanup(w.srv.Close)
	var err error
	if w.st, err = store.OpenWith(w.root, plain{}); err != nil {
		t.Fatal(err)
	}
	s := spec("0.1.0", 1)
	w.publish(s)
	put := func(rel string, b []byte) {
		p := filepath.Join(w.root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put("versions/0.1.0/cragent.exe", s.Agent)
	put("versions/0.1.0/launcher.exe", s.Launcher)
	put("launcher.exe", s.Launcher)
	for n, b := range engineFiles {
		put("engine/b12125/"+n, b)
	}
	if err := layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	if err := u.Bootstrap(context.Background()); err != nil {
		t.Fatalf("first-start bootstrap: %v", err)
	}
	if seq, v := u.Committed(); seq != 1 || v != "0.1.0" {
		t.Fatalf("committed %d %s after the first start", seq, v)
	}
	w.srv.Reset()
	return w
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func refused(t *testing.T, err error, part string) {
	t.Helper()
	var ue *Error
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, part) {
		t.Fatalf("got %v, want an update error containing %q", err, part)
	}
}

func TestUpdateThenCommit(t *testing.T) {
	w := newWorld(t)
	s := spec("0.1.1", 2)
	w.publish(s)
	var states []string
	u := w.updater("0.1.0")
	stopped := false
	u.cfg.StopEngine = func() { stopped = true }
	if err := u.Update(context.Background(), "0.1.1", func(state string, pct float64) {
		if len(states) == 0 || states[len(states)-1] != state {
			states = append(states, state)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(states, ",") != "verifying,downloading,verifying,installing,restarting" {
		t.Fatalf("progress %v", states)
	}
	// The new version is beside the old one, with its signed manifest, and the old one is untouched.
	if read(t, layout.AgentExe(w.root, "0.1.1")) != string(s.Agent) || read(t, layout.AgentExe(w.root, "0.1.0")) != string(spec("0.1.0", 1).Agent) {
		t.Fatal("agent files wrong")
	}
	for _, f := range []string{layout.ManifestFile(w.root, "0.1.1"), layout.ManifestFile(w.root, "0.1.1") + ".minisig"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	// The engine archive has the hash of the installed one: it was not fetched.
	if w.srv.Count(reltest.EngineZip("b12125")) != 0 || w.srv.Count(reltest.AgentZip("0.1.1")) != 2 { // the redirect and the asset
		t.Fatalf("requests %v", w.srv.Requests())
	}
	// The launcher changed: the old one is kept as launcher.old (ruling B22).
	if read(t, layout.LauncherExe(w.root)) != "launcher of 0.1.1" || read(t, filepath.Join(w.root, "launcher.old")) != "launcher of 0.1.0" {
		t.Fatal("launcher not replaced by rename")
	}
	// The pointer is flipped with the trial mark; the engine was stopped first.
	p, err := layout.ReadPointer(w.root)
	if err != nil || p.Version != "0.1.1" || p.Previous != "0.1.0" || p.PendingSince == nil || !stopped {
		t.Fatalf("pointer %+v, engine stopped %v", p, stopped)
	}
	// Nothing is committed until the new agent is healthy.
	if seq, v := u.Committed(); seq != 1 || v != "0.1.0" {
		t.Fatalf("committed %d %s before the health gate", seq, v)
	}
	if _, err := os.Stat(filepath.Join(layout.DownloadsDir(w.root), reltest.AgentZip("0.1.1")+".part")); !os.IsNotExist(err) {
		t.Fatal("the downloaded archive was left behind")
	}

	// The new agent starts: it verifies its signature and files, and commits once healthy.
	n := w.updater("0.1.1")
	if err := n.Startup(); err != nil {
		t.Fatal(err)
	}
	if seq, _ := n.Committed(); seq != 1 {
		t.Fatal("committed before MarkHealthy")
	}
	if err := n.MarkHealthy(); err != nil {
		t.Fatal(err)
	}
	if seq, v := n.Committed(); seq != 2 || v != "0.1.1" {
		t.Fatalf("committed %d %s after the health gate", seq, v)
	}
	if _, err := os.Stat(layout.HealthyFile(w.root, "0.1.1")); err != nil {
		t.Fatal("healthy.0.1.1 not written")
	}
	if err := n.VerifyEngine(); err != nil {
		t.Fatal(err)
	}

	// A re-signed 0.1.0 (its old sequence 1) is now refused, before any archive is fetched.
	w.srv.Reset()
	err = n.Update(context.Background(), "0.1.0", nil)
	refused(t, err, "older than the installed")
	if w.srv.Count(".zip") != 0 {
		t.Fatalf("archives were fetched for a refused release: %v", w.srv.Requests())
	}
	// The running version again: intact, nothing to do.
	if err := n.Update(context.Background(), "0.1.1", nil); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("reinstall of the running version: %v", err)
	}
}

// Every refusal below happens before any archive is requested.
func TestManifestRefusals(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	ctx := context.Background()
	dir := func(v string) string { return filepath.Join(w.srv.Dir, "v"+v) }

	// One flipped byte in a published manifest.
	w.publish(spec("0.1.1", 2))
	raw, _ := os.ReadFile(filepath.Join(dir("0.1.1"), "manifest.json"))
	raw[len(raw)/2] ^= 0x01
	os.WriteFile(filepath.Join(dir("0.1.1"), "manifest.json"), raw, 0o644)
	refused(t, u.Update(ctx, "0.1.1", nil), "refused")

	// A manifest of another version served under this version's tag.
	w.publish(spec("0.1.2", 3))
	os.RemoveAll(dir("0.1.3"))
	os.Rename(dir("0.1.2"), dir("0.1.3"))
	refused(t, u.Update(ctx, "0.1.3", nil), "is for version 0.1.2")

	// Signed by a key this build does not trust.
	other, _ := sign.KeyFromSeed(bytes.Repeat([]byte{4}, 32))
	if _, err := reltest.MakeDist(dir("0.1.4"), spec("0.1.4", 4), other); err != nil {
		t.Fatal(err)
	}
	refused(t, u.Update(ctx, "0.1.4", nil), "does not trust")

	// No signature at all; no such release; a hub too old for the release; a jump of the sequence.
	w.publish(spec("0.1.5", 5))
	os.Remove(filepath.Join(dir("0.1.5"), "manifest.json.minisig"))
	refused(t, u.Update(ctx, "0.1.5", nil), "could not be fetched")
	refused(t, u.Update(ctx, "0.9.9", nil), "could not be fetched")
	s := spec("0.1.6", 6)
	s.MinHub = "2.0.0"
	w.publish(s)
	var ue *Error
	if err := u.Update(ctx, "0.1.6", nil); !errors.As(err, &ue) || ue.Code != "hub_too_old" {
		t.Fatalf("min_hub_version: %v", err)
	}
	w.publish(spec("0.1.7", 2000))
	refused(t, u.Update(ctx, "0.1.7", nil), "jumps more than")
	// Versions that are not versions never reach the network.
	before := len(w.srv.Requests())
	for _, bad := range []string{"", "latest", "../0.1.1", "0.1.1/../../x", "v0.1.1", "0.1"} {
		refused(t, u.Update(ctx, bad, nil), "bad version")
	}
	if len(w.srv.Requests()) != before {
		t.Fatal("a bad version was sent to the release server")
	}

	if n := w.srv.Count(".zip"); n != 0 {
		t.Fatalf("%d archive requests for refused manifests: %v", n, w.srv.Requests())
	}
	if p, _ := layout.ReadPointer(w.root); p.Version != "0.1.0" || p.PendingSince != nil {
		t.Fatalf("pointer moved: %+v", p)
	}
	if got := layout.Installed(w.root); len(got) != 1 {
		t.Fatalf("versions on disk after refusals: %v", got)
	}
}

func TestArchiveRefusals(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	ctx := context.Background()
	dir := filepath.Join(w.srv.Dir, "v0.1.1")
	agentZip := filepath.Join(dir, reltest.AgentZip("0.1.1"))

	// The archive on the server is not the one the manifest lists.
	w.publish(spec("0.1.1", 2))
	reltest.Zip(agentZip, map[string][]byte{"versions/0.1.1/cragent.exe": []byte("swapped on the server"), "versions/0.1.1/launcher.exe": []byte("x")})
	refused(t, u.Update(ctx, "0.1.1", nil), "does not match the manifest")
	// The same size, other bytes: caught by the hash.
	w.publish(spec("0.1.1", 2))
	zb, _ := os.ReadFile(agentZip)
	zb[len(zb)/2] ^= 0x55
	os.WriteFile(agentZip, zb, 0o644)
	refused(t, u.Update(ctx, "0.1.1", nil), "does not match the manifest (SHA-256)")
	if _, err := os.Stat(layout.VersionDir(w.root, "0.1.1")); !os.IsNotExist(err) {
		t.Fatal("files were unpacked from an archive with a wrong hash")
	}
	if ents, _ := os.ReadDir(layout.DownloadsDir(w.root)); len(ents) != 0 {
		t.Fatalf("a partial of a wrong archive stayed: %v", ents)
	}

	// Correctly signed and hashed archives that still must not be unpacked: an entry the manifest does
	// not list, an entry that climbs out of the folder, an entry with other content than listed.
	outside := filepath.Join(filepath.Dir(w.root), "evil.exe")
	for name, entries := range map[string]map[string][]byte{
		"an unlisted entry": {"versions/0.1.1/cragent.exe": []byte("agent"), "versions/0.1.1/extra.dll": []byte("not listed")},
		"a climbing entry":  {"versions/0.1.1/cragent.exe": []byte("agent"), "../evil.exe": []byte("zip slip")},
		"an absolute entry": {"versions/0.1.1/cragent.exe": []byte("agent"), "C:/Windows/evil.exe": []byte("zip slip")},
	} {
		m := w.publish(spec("0.1.1", 2))
		reltest.Zip(agentZip, entries)
		sum, size, _ := release.HashFile(agentZip)
		for i := range m.Archives {
			if m.Archives[i].Name == reltest.AgentZip("0.1.1") {
				m.Archives[i].SHA256, m.Archives[i].Size = sum, size
			}
		}
		// The manifest lists only the agent binary of the new archive: the extra entry is unlisted.
		var files []release.File
		for _, f := range m.Files {
			if strings.HasPrefix(f.Path, "engine/") {
				files = append(files, f)
			}
		}
		asum, asize := sha(entries["versions/0.1.1/cragent.exe"])
		m.Files = append(files, release.File{Path: "versions/0.1.1/cragent.exe", Size: asize, SHA256: asum})
		if err := reltest.Sign(dir, m, w.key); err != nil {
			t.Fatal(err)
		}
		refused(t, u.Update(ctx, "0.1.1", nil), "not in the manifest")
		if _, err := os.Stat(layout.VersionDir(w.root, "0.1.1")); !os.IsNotExist(err) {
			t.Fatalf("%s: something was unpacked", name)
		}
		if _, err := os.Stat(outside); err == nil {
			t.Fatalf("%s: a file was written outside the install folder", name)
		}
	}
	if p, _ := layout.ReadPointer(w.root); p.Version != "0.1.0" {
		t.Fatalf("pointer moved: %+v", p)
	}
}

func sha(b []byte) (string, int64) {
	f, _ := os.CreateTemp("", "sha")
	defer os.Remove(f.Name())
	f.Write(b)
	f.Close()
	s, n, _ := release.HashFile(f.Name())
	return s, n
}

// A download that breaks is resumed with Range; a redirect to a host that is not GitHub is refused.
func TestResumeAndRedirectRule(t *testing.T) {
	w := newWorld(t)
	w.publish(spec("0.1.1", 2))
	w.srv.CutAfter, w.srv.CutTimes = 100_000, 2
	u := w.updater("0.1.0")
	if err := u.Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatal(err)
	}
	if n := w.srv.Count("/cdn/"); n != 3 {
		t.Fatalf("%d asset requests, want 3 (two cut, one resumed to the end)", n)
	}
	if read(t, layout.AgentExe(w.root, "0.1.1")) != string(spec("0.1.1", 2).Agent) {
		t.Fatal("resumed archive unpacked wrong")
	}

	w2 := newWorld(t)
	w2.publish(spec("0.1.1", 2))
	for _, to := range []string{"https://evil.example/agent.zip", "http://objects.githubusercontent.com/x.zip", "https://raw.githubusercontent.com/x.zip"} {
		w2.srv.RedirectTo = to
		refused(t, w2.updater("0.1.0").Update(context.Background(), "0.1.1", nil), "could not be downloaded")
	}
	if _, err := os.Stat(layout.VersionDir(w2.root, "0.1.1")); !os.IsNotExist(err) {
		t.Fatal("unpacked after a refused redirect")
	}
}

// Key rotation (ruling B20): a release signed by a new key the agent does not know yet also carries
// manifest.json.old.minisig by the key it does know.
func TestRotationSignature(t *testing.T) {
	w := newWorld(t)
	newKey, _ := sign.KeyFromSeed(bytes.Repeat([]byte{11}, 32))
	dir := filepath.Join(w.srv.Dir, "v0.1.1")
	m, err := reltest.MakeDist(dir, spec("0.1.1", 2), newKey) // signed by the new key only
	if err != nil {
		t.Fatal(err)
	}
	u := w.updater("0.1.0") // trusts only the old (throwaway) key
	refused(t, u.Update(context.Background(), "0.1.1", nil), "does not trust")
	// The outgoing key signs the very same bytes.
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	old, err := w.key.Sign(raw, release.TrustedComment(m))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.json.old.minisig"), old, 0o644)
	// Now the agent that knows only the outgoing key can update, on that key's signature.
	if err := u.Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatalf("the outgoing key's signature was not accepted: %v", err)
	}
	if got, _ := os.ReadFile(layout.ManifestFile(w.root, "0.1.1") + ".minisig"); !bytes.Equal(got, old) {
		t.Fatal("the signature stored beside the version is not the one that verified")
	}
	// The stored pair verifies again at the next start.
	if err := w.updater("0.1.1").Startup(); err != nil {
		t.Fatal(err)
	}
	// A second signature by a key nobody trusts changes nothing.
	stranger, _ := sign.KeyFromSeed(bytes.Repeat([]byte{12}, 32))
	dir2 := filepath.Join(w.srv.Dir, "v0.1.2")
	m2, _ := reltest.MakeDist(dir2, spec("0.1.2", 3), newKey)
	raw2, _ := os.ReadFile(filepath.Join(dir2, "manifest.json"))
	bad, _ := stranger.Sign(raw2, release.TrustedComment(m2))
	os.WriteFile(filepath.Join(dir2, "manifest.json.old.minisig"), bad, 0o644)
	refused(t, w.updater("0.1.1").Update(context.Background(), "0.1.2", nil), "does not trust")
}

// At every start the signature and the agent's own files are checked (ruling B21).
func TestStartupChecks(t *testing.T) {
	w := newWorld(t)
	if err := w.updater("0.1.0").Startup(); err != nil {
		t.Fatalf("an intact install was refused: %v", err)
	}
	// The agent binary was changed on disk.
	exe := layout.AgentExe(w.root, "0.1.0")
	orig, _ := os.ReadFile(exe)
	os.WriteFile(exe, append([]byte("patched "), orig...), 0o755)
	if err := w.updater("0.1.0").Startup(); err == nil || !strings.Contains(err.Error(), "was changed") {
		t.Fatalf("a changed agent binary: %v", err)
	}
	os.WriteFile(exe, orig, 0o755)
	// The engine is checked before it is run.
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(layout.EngineDir(w.root, "b12125"), "llama-server.exe"), []byte("another program"), 0o755)
	if err := u.VerifyEngine(); err == nil {
		t.Fatal("a changed engine binary passed")
	}
	// A build that no longer trusts the key that signed the installed release refuses to run it.
	stranger, _ := sign.KeyFromSeed(bytes.Repeat([]byte{8}, 32))
	u2 := w.updater("0.1.0")
	u2.cfg.Keys = reltest.Trusting(stranger)
	if err := u2.Startup(); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("an untrusted installed release: %v", err)
	}
}

// After the launcher reverted a bad version, the old agent drops the pending record and says so.
func TestRollbackIsNoticed(t *testing.T) {
	w := newWorld(t)
	w.publish(spec("0.1.1", 2))
	if err := w.updater("0.1.0").Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatal(err)
	}
	// The launcher gives up on 0.1.1 and points back at 0.1.0.
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	if got := u.RolledBack(); got != "0.1.1" {
		t.Fatalf("rolled back %q", got)
	}
	if u.RolledBack() != "" {
		t.Fatal("the rollback is reported twice")
	}
	if seq, v := u.Committed(); seq != 1 || v != "0.1.0" {
		t.Fatalf("committed %d %s after a rollback", seq, v)
	}
	// 0.1.1 can be offered again later (its sequence was never committed).
	if err := u.Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatalf("second attempt at the reverted version: %v", err)
	}
}

// Finding M4. A fresh install whose manifest could not be checked yet has committed_seq 0, and a hub
// must not be able to send it back to an older signed release.
func TestUnverifiedInstallIsNotDowngraded(t *testing.T) {
	w := &world{t: t, srv: relsrv.New(t.TempDir()), root: filepath.Join(t.TempDir(), "cr-agent"), key: sign.ThrowawayKey()}
	t.Cleanup(w.srv.Close)
	var err error
	if w.st, err = store.OpenWith(w.root, plain{}); err != nil {
		t.Fatal(err)
	}
	w.publish(spec("0.1.0", 1))
	w.publish(spec("0.3.0", 9))
	cur := spec("0.2.0", 5)
	for rel, b := range map[string][]byte{"versions/0.2.0/cragent.exe": cur.Agent, "versions/0.2.0/launcher.exe": cur.Launcher, "launcher.exe": cur.Launcher} {
		p := filepath.Join(w.root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o755)
	}
	layout.WritePointer(w.root, layout.Pointer{Version: "0.2.0"})
	u := w.updater("0.2.0")
	if err := u.Startup(); err != nil || !u.Unverified() {
		t.Fatalf("startup %v, unverified %v", err, u.Unverified())
	}
	refused(t, u.Update(context.Background(), "0.1.0", nil), "older than the running")
	if p, _ := layout.ReadPointer(w.root); p.Version != "0.2.0" {
		t.Fatalf("pointer %+v", p)
	}
	// A newer release is still allowed: it is verified in full, and it is the way out of this state.
	if err := u.Update(context.Background(), "0.3.0", nil); err != nil {
		t.Fatal(err)
	}
}

// Finding M4. The version on trial is not committed yet, but it is the floor already.
func TestRunningVersionOnTrialIsTheFloor(t *testing.T) {
	w := newWorld(t) // committed 0.1.0, seq 1
	w.publish(spec("0.1.1", 5))
	w.publish(spec("0.1.2", 3)) // signed, newer than the committed one, older than the running one
	if err := w.updater("0.1.0").Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatal(err)
	}
	u := w.updater("0.1.1")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	if seq, _ := u.Committed(); seq != 1 {
		t.Fatalf("committed %d before the health gate", seq)
	}
	refused(t, u.Update(context.Background(), "0.1.2", nil), "older than the installed 5")
}

// Finding M4. A commit that lands during the download must survive the update's write.
func TestCommitDuringUpdateIsKept(t *testing.T) {
	w := newWorld(t)
	w.publish(spec("0.1.1", 2))
	w.publish(spec("0.1.2", 3))
	if err := w.updater("0.1.0").Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatal(err)
	}
	u := w.updater("0.1.1")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	marked := false
	err := u.Update(context.Background(), "0.1.2", func(state string, _ float64) {
		if state == "downloading" && !marked {
			marked = true
			if err := u.MarkHealthy(); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || !marked {
		t.Fatalf("update %v, marked %v", err, marked)
	}
	if seq, v := u.Committed(); seq != 2 || v != "0.1.1" {
		t.Fatalf("committed %d %s after the update: the commit was lost", seq, v)
	}
}

// Finding M4. An unreadable state is an error, never a fresh install.
func TestUnreadableStateIsAnError(t *testing.T) {
	w := newWorld(t)
	w.publish(spec("0.1.1", 2))
	path := filepath.Join(w.st.Dir(), "state", secretRelease+".bin")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"damaged json": []byte("P:{not json"), "too short for the protector": []byte("P:")} {
		os.WriteFile(path, content, 0o600)
		u := w.updater("0.1.0")
		if err := u.Startup(); !errors.Is(err, ErrStateUnreadable) {
			t.Errorf("%s: startup %v", name, err)
		}
		refused(t, u.Update(context.Background(), "0.1.1", nil), "cannot be read")
	}
	os.WriteFile(path, orig, 0o600)
	if err := w.updater("0.1.0").Startup(); err != nil {
		t.Fatal(err)
	}
	// No state at all is a fresh install.
	os.Remove(path)
	if err := w.updater("0.1.0").Startup(); err != nil {
		t.Fatalf("no state: %v", err)
	}
}

// Finding L8. Whatever a crash leaves of the manifest pair, the next start is sane.
func TestManifestPairAfterACrash(t *testing.T) {
	w := newWorld(t)
	path := layout.ManifestFile(w.root, "0.1.0")
	raw, _ := os.ReadFile(path)
	sig, _ := os.ReadFile(path + ".minisig")
	if len(raw) == 0 || len(sig) == 0 {
		t.Fatal("no pair was written")
	}
	// The crash between the two writes: a signature, no manifest. Without the committed state this is "not checked yet".
	os.Remove(path)
	st := filepath.Join(w.st.Dir(), "state", secretRelease+".bin")
	os.Remove(st)
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil || !u.Unverified() {
		t.Fatalf("signature only: %v, unverified %v", err, u.Unverified())
	}
	if err := u.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
		t.Fatal("the pair was not written again")
	}
	// A manifest without a signature is not what a crash leaves, and it is refused.
	os.Remove(st)
	os.Remove(path + ".minisig")
	if err := w.updater("0.1.0").Startup(); err == nil {
		t.Fatal("a manifest without a signature was accepted")
	}
}

// Finding M1: the daily check finds a release the hub never mentioned, verified like any update.
func TestDailyCheckOfTheLatestRelease(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	// No latest release yet.
	if _, err := u.CheckLatest(ctx); err == nil || u.UpdateAvailable() != "" || u.LatestVerified() != "" {
		t.Fatalf("no release: %v, available %q", err, u.UpdateAvailable())
	}
	// The latest is the running version: nothing to offer, but it is verified and reported.
	w.srv.Latest = "0.1.0"
	if v, err := u.CheckLatest(ctx); err != nil || v != "0.1.0" || u.UpdateAvailable() != "" || u.LatestVerified() != "0.1.0" {
		t.Fatalf("same version: %q %v, available %q", v, err, u.UpdateAvailable())
	}
	// A newer one.
	w.publish(spec("0.1.1", 2))
	w.srv.Latest = "0.1.1"
	if v, err := u.CheckLatest(ctx); err != nil || v != "0.1.1" || u.UpdateAvailable() != "0.1.1" {
		t.Fatalf("newer: %q %v, available %q", v, err, u.UpdateAvailable())
	}
	// It survives a restart, and it installs.
	u2 := w.updater("0.1.0")
	if err := u2.Startup(); err != nil || u2.UpdateAvailable() != "0.1.1" {
		t.Fatalf("after a restart: %v, %q", err, u2.UpdateAvailable())
	}
	if err := u2.Update(ctx, u2.UpdateAvailable(), nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := layout.ReadPointer(w.root); p.Version != "0.1.1" {
		t.Fatalf("pointer %+v", p)
	}
	// The new version has nothing to offer.
	u3 := w.updater("0.1.1")
	if err := u3.Startup(); err != nil || u3.UpdateAvailable() != "" {
		t.Fatalf("on the new version: %v, %q", err, u3.UpdateAvailable())
	}
}

func TestDailyCheckRefuses(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	u := w.updater("0.1.0")
	if err := u.Startup(); err != nil {
		t.Fatal(err)
	}
	// One flipped byte in the manifest.
	w.publish(spec("0.1.1", 2))
	w.srv.Latest = "0.1.1"
	path := filepath.Join(w.srv.Dir, "v0.1.1", "manifest.json")
	raw, _ := os.ReadFile(path)
	bad := append([]byte(nil), raw...)
	bad[len(bad)/2] ^= 1
	os.WriteFile(path, bad, 0o644)
	if _, err := u.CheckLatest(ctx); err == nil || u.UpdateAvailable() != "" {
		t.Fatalf("flipped byte: %v, %q", err, u.UpdateAvailable())
	}
	os.WriteFile(path, raw, 0o644)
	// Another key.
	other, _ := sign.KeyFromSeed(bytes.Repeat([]byte{5}, 32))
	if _, err := reltest.MakeDist(filepath.Join(w.srv.Dir, "v0.1.2"), spec("0.1.2", 3), other); err != nil {
		t.Fatal(err)
	}
	w.srv.Latest = "0.1.2"
	if _, err := u.CheckLatest(ctx); !errors.Is(err, release.ErrUntrustedKey) || u.UpdateAvailable() != "" {
		t.Fatalf("other key: %v, %q", err, u.UpdateAvailable())
	}
	// The rollback test's release, marked latest by mistake, is never offered.
	s := spec("0.1.3", 4)
	m, err := reltest.MakeDist(filepath.Join(w.srv.Dir, "v0.1.3"), s, w.key)
	if err != nil {
		t.Fatal(err)
	}
	m.Notes = "ROLLBACK TEST: this agent never passes the health gate."
	if err := reltest.Sign(filepath.Join(w.srv.Dir, "v0.1.3"), m, w.key); err != nil {
		t.Fatal(err)
	}
	w.srv.Latest = "0.1.3"
	if _, err := u.CheckLatest(ctx); err == nil || u.UpdateAvailable() != "" {
		t.Fatalf("rollback test as latest: %v, %q", err, u.UpdateAvailable())
	}
	// A release older than the running one is verified but not offered, and "latest" going back keeps the newer one.
	w.publish(spec("0.1.1", 2))
	w.srv.Latest = "0.1.1"
	if v, err := u.CheckLatest(ctx); err != nil || v != "0.1.1" {
		t.Fatal(err)
	}
	w.srv.Latest = "0.1.0"
	if v, err := u.CheckLatest(ctx); err != nil || v != "0.1.1" || u.UpdateAvailable() != "0.1.1" {
		t.Fatalf("latest went back: %q %v, %q", v, err, u.UpdateAvailable())
	}
	// Redirects out of GitHub are refused, as for any release file.
	w2 := newWorld(t)
	w2.srv.RedirectLatestTo = "https://evil.example/manifest.json"
	u4 := w2.updater("0.1.0")
	if _, err := u4.CheckLatest(ctx); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("redirect: %v", err)
	}
}

// A download whose connection stays open and delivers nothing is cancelled, and resumed.
func TestStalledDownloadIsCancelled(t *testing.T) {
	w := newWorld(t)
	w.publish(spec("0.1.1", 2))
	w.srv.StallAfter, w.srv.StallTimes = 100_000, 1
	u := w.updater("0.1.0")
	u.cfg.StallAfter = 200 * time.Millisecond
	t0 := time.Now()
	if err := u.Update(context.Background(), "0.1.1", nil); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el > 20*time.Second {
		t.Fatalf("took %v", el)
	}
	if n := w.srv.Count("/cdn/"); n != 2 {
		t.Fatalf("%d asset requests, want 2 (one stalled, one resumed)", n)
	}
}

// The tray's install ignores min_hub_version: that value is compared with what the hub says it is.
func TestLocalInstallIgnoresTheHubsVersion(t *testing.T) {
	w := newWorld(t)
	s := spec("0.1.1", 2)
	s.MinHub = "9.0.0"
	w.publish(s)
	u := w.updater("0.1.0")
	var ue *Error
	if err := u.Update(context.Background(), "0.1.1", nil); !errors.As(err, &ue) || ue.Code != "hub_too_old" {
		t.Fatalf("from the hub: %v", err)
	}
	if err := u.UpdateLocal(context.Background(), "0.1.1", nil); err != nil {
		t.Fatalf("from the tray: %v", err)
	}
}
