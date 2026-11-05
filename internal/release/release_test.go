package release_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/release/reltest"
	"github.com/cheetahrunout/cr-agent/internal/sign"
)

func spec(version string, seq int64) reltest.Spec {
	return reltest.Spec{Version: version, Seq: seq, Agent: []byte("agent " + version), Launcher: []byte("launcher"),
		Engine: map[string][]byte{"llama-server.exe": []byte("engine"), "ggml-cuda.dll": bytes.Repeat([]byte{1}, 5000)}}
}

func dist(t *testing.T, version string, seq int64) (string, *release.Manifest, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	m, err := reltest.MakeDist(dir, spec(version, seq), sign.ThrowawayKey())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	sig, _ := os.ReadFile(filepath.Join(dir, "manifest.json.minisig"))
	return dir, m, raw, sig
}

func TestBuildAndVerify(t *testing.T) {
	dir, m, raw, sig := dist(t, "0.1.1", 2)
	if len(m.Files) != 4 || len(m.Archives) != 2 || m.AgentPath() != "versions/0.1.1/cragent.exe" || m.KeyID != sign.ThrowawayKeyID {
		t.Fatalf("manifest %+v", m)
	}
	if f, ok := m.File("engine/b12125/ggml-cuda.dll"); !ok || f.Size != 5000 {
		t.Fatalf("engine file %+v", f)
	}
	if err := release.CheckDist(dir, m); err != nil {
		t.Fatal(err)
	}
	keys := reltest.Trusting(sign.ThrowawayKey())
	got, err := release.Verify(raw, sig, keys)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.1.1" || got.ReleaseSeq != 2 || len(got.Zips()) != 2 {
		t.Fatalf("verified %+v", got)
	}
}

func TestVerifyRefuses(t *testing.T) {
	_, m, raw, sig := dist(t, "0.1.1", 2)
	key := sign.ThrowawayKey()
	keys := reltest.Trusting(key)
	other, _ := sign.KeyFromSeed(bytes.Repeat([]byte{9}, 32))

	// One flipped byte anywhere in the manifest.
	for _, i := range []int{0, len(raw) / 2, len(raw) - 2} {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		if _, err := release.Verify(bad, sig, keys); !errors.Is(err, sign.ErrBadSignature) {
			t.Errorf("flipped byte %d: %v", i, err)
		}
	}
	// Signed by a key that is not trusted; and a build that trusts nothing.
	osig, _ := other.Sign(raw, release.TrustedComment(m))
	if _, err := release.Verify(raw, osig, keys); !errors.Is(err, release.ErrUntrustedKey) {
		t.Errorf("untrusted key: %v", err)
	}
	if _, err := release.Verify(raw, sig, nil); !errors.Is(err, release.ErrUntrustedKey) {
		t.Errorf("empty trust list: %v", err)
	}
	// A valid signature whose signed comment names another version or sequence.
	for _, comment := range []string{"product=cr-agent version=0.1.2 release_seq=2", "product=cr-agent version=0.1.1 release_seq=3",
		"product=other version=0.1.1 release_seq=2", "version=0.1.1"} {
		s, _ := key.Sign(raw, comment)
		if _, err := release.Verify(raw, s, keys); !errors.Is(err, release.ErrMismatch) {
			t.Errorf("comment %q: %v", comment, err)
		}
	}
	// Correctly signed manifests that the agent must still refuse to use.
	sign2 := func(change func(*release.Manifest)) error {
		c := *m
		c.Files = append([]release.File(nil), m.Files...)
		c.Archives = append([]release.Archive(nil), m.Archives...)
		change(&c)
		b := release.Marshal(&c)
		s, _ := key.Sign(b, release.TrustedComment(&c))
		_, err := release.Verify(b, s, keys)
		return err
	}
	sha := m.Files[0].SHA256
	for name, change := range map[string]func(*release.Manifest){
		"path leaves the folder":      func(c *release.Manifest) { c.Files[0].Path = "versions/0.1.1/../../evil.exe" },
		"absolute path":               func(c *release.Manifest) { c.Files[0].Path = "C:/Windows/evil.exe" },
		"backslash path":              func(c *release.Manifest) { c.Files[0].Path = `versions\0.1.1\x.exe` },
		"another version's folder":    func(c *release.Manifest) { c.Files[0].Path = "versions/0.0.9/cragent.exe" },
		"outside versions and engine": func(c *release.Manifest) { c.Files[0].Path = "state/machine-key.bin" },
		"models folder":               func(c *release.Manifest) { c.Files[0].Path = "models/x/y.gguf" },
		"device name":                 func(c *release.Manifest) { c.Files[0].Path = "versions/0.1.1/NUL.exe" },
		"duplicate differing in case": func(c *release.Manifest) {
			c.Files = append(c.Files, release.File{Path: "versions/0.1.1/CRAGENT.exe", Size: 1, SHA256: sha})
		},
		"no agent binary":          func(c *release.Manifest) { c.Files = c.Files[:2] }, // sorted: the two engine files
		"another product":          func(c *release.Manifest) { c.Product = "other" },
		"another platform":         func(c *release.Manifest) { c.Platform = "linux-amd64" },
		"sequence zero":            func(c *release.Manifest) { c.ReleaseSeq = 0 },
		"key id that is no key id": func(c *release.Manifest) { c.KeyID = "not-a-key-id" },
		"archive with path":        func(c *release.Manifest) { c.Archives[0].Name = "../x.zip" },
		"short hash":               func(c *release.Manifest) { c.Files[0].SHA256 = "abc" },
	} {
		if err := sign2(change); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := sign2(func(*release.Manifest) {}); err != nil {
		t.Fatalf("an unchanged manifest was refused: %v", err)
	}
}

func TestCheckSeq(t *testing.T) {
	m := func(v string, seq int64) *release.Manifest { return &release.Manifest{Version: v, ReleaseSeq: seq} }
	cases := []struct {
		name      string
		m         *release.Manifest
		committed int64
		version   string
		ok        bool
	}{
		{"first install", m("0.1.0", 1), 0, "", true},
		{"next release", m("0.1.1", 2), 1, "0.1.0", true},
		{"reinstall of the same version", m("0.1.1", 2), 2, "0.1.1", true},
		{"an older release, signed again", m("0.1.0", 1), 2, "0.1.1", false},
		{"another version with the installed sequence", m("0.1.9", 2), 2, "0.1.1", false},
		{"a jump of 1000", m("0.2.0", 1002), 2, "0.1.1", true},
		{"a jump of 1001", m("0.2.0", 1003), 2, "0.1.1", false},
	}
	for _, c := range cases {
		if err := release.CheckSeq(c.m, c.committed, c.version); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// What the signing tool checks before it signs: the folder is exactly what the manifest says.
func TestCheckDistFailsClosed(t *testing.T) {
	dir, m, _, _ := dist(t, "0.1.1", 2)
	agent := filepath.Join(dir, reltest.AgentZip("0.1.1"))
	orig, _ := os.ReadFile(agent)

	// A zip rebuilt with other content.
	reltest.Zip(agent, map[string][]byte{"versions/0.1.1/cragent.exe": []byte("tampered"), "versions/0.1.1/launcher.exe": []byte("launcher")})
	if err := release.CheckDist(dir, m); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("tampered archive: %v", err)
	}
	os.WriteFile(agent, orig, 0o644)
	// An archive nobody listed.
	os.WriteFile(filepath.Join(dir, "extra.zip"), orig, 0o644)
	if err := release.CheckDist(dir, m); err == nil {
		t.Error("an unlisted archive passed")
	}
	os.Remove(filepath.Join(dir, "extra.zip"))
	// A missing archive.
	os.Remove(filepath.Join(dir, reltest.EngineZip("b12125")))
	if err := release.CheckDist(dir, m); err == nil {
		t.Error("a missing archive passed")
	}
	// A listed file that is in no archive.
	dir2, m2, _, _ := dist(t, "0.1.2", 3)
	m2.Files = append(m2.Files, release.File{Path: "versions/0.1.2/ghost.dll", Size: 1, SHA256: m2.Files[0].SHA256})
	if err := release.CheckDist(dir2, m2); err == nil {
		t.Error("a listed file without an archive passed")
	}
}

// Pack makes the zips a release is built from: entries under the prefix, forward slashes, and the
// same bytes for the same input.
func TestPack(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "llama-server.exe"), []byte("engine"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "x.dll"), []byte("dll"), 0o644)
	out := t.TempDir()
	a, b := filepath.Join(out, "engine-cuda13-b12125-windows-amd64.zip"), filepath.Join(out, "again.zip")
	if err := release.Pack(a, "engine/b12125", src, nil); err != nil {
		t.Fatal(err)
	}
	if err := release.Pack(b, "engine/b12125", src, nil); err != nil {
		t.Fatal(err)
	}
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ba, bb) {
		t.Fatal("two packs of the same folder differ")
	}
	os.Remove(b)
	agentDir := t.TempDir()
	os.WriteFile(filepath.Join(agentDir, "cragent.exe"), []byte("agent"), 0o755)
	os.WriteFile(filepath.Join(agentDir, "launcher.exe"), []byte("launcher"), 0o755)
	os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte("not shipped"), 0o644)
	if err := release.Pack(filepath.Join(out, "agent-0.1.1-windows-amd64.zip"), "versions/0.1.1", agentDir, []string{"cragent.exe", "launcher.exe"}); err != nil {
		t.Fatal(err)
	}
	m, err := release.Build(out, release.BuildOptions{Version: "0.1.1", ReleaseSeq: 1, Protocol: "1.0", LlamaCpp: release.LlamaCpp{Tag: "b12125"},
		KeyID: sign.ThrowawayKeyID, ReleasedAt: time.Unix(1793870000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range m.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, " "); got != "engine/b12125/llama-server.exe engine/b12125/sub/x.dll versions/0.1.1/cragent.exe versions/0.1.1/launcher.exe" {
		t.Fatalf("files %s", got)
	}
	os.WriteFile(filepath.Join(src, "NUL.dll"), []byte("x"), 0o644)
	if err := release.Pack(filepath.Join(out, "bad.zip"), "engine/b12125", src, []string{"NUL.dll"}); err == nil {
		t.Fatal("a device name was packed")
	}
}
