package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/release/reltest"
	"github.com/cheetahrunout/cr-agent/internal/sign"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

// unsigned makes a release folder as CI leaves it: zips and a manifest with placeholder fields.
func unsigned(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	key := sign.ThrowawayKey()
	m, err := reltest.MakeDist(dir, reltest.Spec{Version: version, Seq: 1, Agent: []byte("agent " + version), Launcher: []byte("launcher"),
		Engine: map[string][]byte{"llama-server.exe": []byte("engine")}}, key)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "manifest.json.minisig"))
	m.KeyID, m.ReleaseSeq = "0000000000000000", 1
	os.WriteFile(filepath.Join(dir, "manifest.json"), release.Marshal(m), 0o644)
	return dir
}

func verify(t *testing.T, dir string, keys []trust.Key) *release.Manifest {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.json.minisig"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.Verify(raw, sig, keys)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSignSetsSequenceAndKey(t *testing.T) {
	key := sign.ThrowawayKey()
	keys := reltest.Trusting(key)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	first := unsigned(t, "0.1.0")
	m, err := signDist(signOptions{Dist: first, First: true, Now: now}, key, keys)
	if err != nil {
		t.Fatal(err)
	}
	if m.ReleaseSeq != 1 || m.KeyID != key.ID.String() || m.ReleasedAt != "2026-11-05T12:00:00Z" {
		t.Fatalf("first release: %+v", m)
	}
	verify(t, first, keys)

	// The next release takes the previous sequence plus one, read from the published (and verified) manifest.
	second := unsigned(t, "0.1.1")
	m, err = signDist(signOptions{Dist: second, Prev: filepath.Join(first, "manifest.json"), Now: now}, key, keys)
	if err != nil || m.ReleaseSeq != 2 {
		t.Fatalf("second release: %+v %v", m, err)
	}
	if got := verify(t, second, keys); got.ReleaseSeq != 2 || got.Version != "0.1.1" {
		t.Fatalf("verified %+v", got)
	}

	// Neither -first nor -prev; both; the same version again; a previous manifest that was tampered with.
	third := unsigned(t, "0.1.2")
	if _, err := signDist(signOptions{Dist: third, Now: now}, key, keys); err == nil {
		t.Error("signed without knowing the previous release")
	}
	if _, err := signDist(signOptions{Dist: third, First: true, Prev: filepath.Join(first, "manifest.json"), Now: now}, key, keys); err == nil {
		t.Error("-first and -prev together accepted")
	}
	again := unsigned(t, "0.1.1")
	if _, err := signDist(signOptions{Dist: again, Prev: filepath.Join(second, "manifest.json"), Now: now}, key, keys); err == nil {
		t.Error("a published version was signed again")
	}
	raw, _ := os.ReadFile(filepath.Join(second, "manifest.json"))
	os.WriteFile(filepath.Join(second, "manifest.json"), bytes.Replace(raw, []byte(`"release_seq": 2`), []byte(`"release_seq": 7`), 1), 0o644)
	if _, err := signDist(signOptions{Dist: third, Prev: filepath.Join(second, "manifest.json"), Now: now}, key, keys); err == nil || !strings.Contains(err.Error(), "previous manifest") {
		t.Errorf("a tampered previous manifest was believed: %v", err)
	}
}

// A typo in the passphrase gives another key: the tool must refuse, and write nothing.
func TestRefusesAKeyThatIsNotTrusted(t *testing.T) {
	dir := unsigned(t, "0.1.0")
	before, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	typo, _ := sign.KeyFromSeed(bytes.Repeat([]byte{3}, 32))
	_, err := signDist(signOptions{Dist: dir, First: true, Now: time.Now()}, typo, reltest.Trusting(sign.ThrowawayKey()))
	if err == nil || !strings.Contains(err.Error(), "not in the trust list") || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if _, serr := os.Stat(filepath.Join(dir, "manifest.json.minisig")); serr == nil || !bytes.Equal(before, after) {
		t.Fatal("something was written for an untrusted key")
	}
	// With an empty trust list (a release build today) nothing can be signed at all.
	if _, err := signDist(signOptions{Dist: dir, First: true, Now: time.Now()}, sign.ThrowawayKey(), nil); err == nil {
		t.Fatal("signed with an empty trust list")
	}
}

// The signer re-hashes everything and fails closed.
func TestRefusesAFolderThatChanged(t *testing.T) {
	key := sign.ThrowawayKey()
	dir := unsigned(t, "0.1.0")
	reltest.Zip(filepath.Join(dir, reltest.AgentZip("0.1.0")), map[string][]byte{"versions/0.1.0/cragent.exe": []byte("swapped after CI"), "versions/0.1.0/launcher.exe": []byte("launcher")})
	_, err := signDist(signOptions{Dist: dir, First: true, Now: time.Now()}, key, reltest.Trusting(key))
	if err == nil || !strings.Contains(err.Error(), "Nothing was signed") {
		t.Fatalf("got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "manifest.json.minisig")); serr == nil {
		t.Fatal("a signature was written for a changed folder")
	}
}

// Rotation (ruling B20): the outgoing key signs the same bytes into manifest.json.old.minisig.
func TestOldKeySignature(t *testing.T) {
	newKey := sign.ThrowawayKey()
	oldKey, _ := sign.KeyFromSeed(bytes.Repeat([]byte{5}, 32))
	keys := append(reltest.Trusting(newKey), reltest.Trusting(oldKey)...)
	dir := unsigned(t, "0.1.0")
	if _, err := signDist(signOptions{Dist: dir, First: true, Old: true, Now: time.Now()}, oldKey, keys); err != nil {
		// release_seq 1 is also the placeholder, so the old key may sign first here; what matters is below.
		t.Logf("old key first: %v", err)
	}
	if _, err := signDist(signOptions{Dist: dir, First: true, Now: time.Now()}, newKey, keys); err != nil {
		t.Fatal(err)
	}
	signed, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if _, err := signDist(signOptions{Dist: dir, First: true, Old: true, Now: time.Now()}, oldKey, keys); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !bytes.Equal(signed, after) {
		t.Fatal("the old key's signature rewrote the manifest")
	}
	oldSig, err := os.ReadFile(filepath.Join(dir, "manifest.json.old.minisig"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := sign.ParseSignature(oldSig)
	if err != nil || s.Verify(oldKey.Public, after) != nil {
		t.Fatalf("old signature does not verify: %v", err)
	}
	verify(t, dir, keys) // the new key's signature still stands
}

// There is no way to hand the passphrase over other than typing it: no flag of any command takes it.
func TestNoPassphraseFlag(t *testing.T) {
	for _, name := range []string{"pubkey", "sign"} {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		keyFlags(fs)
		fs.VisitAll(func(f *flag.Flag) {
			if strings.Contains(f.Name, "pass") || strings.Contains(f.Name, "secret") || strings.Contains(f.Name, "seed") {
				t.Errorf("%s has a flag %q that could carry key material", name, f.Name)
			}
			if f.Name == "test-key" && !trust.Debug() {
				t.Error("a release build offers -test-key")
			}
		})
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"os.Getenv", "os.Stdin", "LookupEnv"} {
		if bytes.Contains(src, []byte(banned)) {
			t.Errorf("main.go uses %s: the passphrase may only come from the console", banned)
		}
	}
}
