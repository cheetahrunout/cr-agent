package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf/gguftest"
	"github.com/cheetahrunout/cr-agent/internal/hf/hftest"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

type countingTransport struct {
	next http.RoundTripper
	on   func()
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.on()
	return c.next.RoundTrip(r)
}

func (e *env) put(rel string, b []byte) string {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) add(keys ...string) []ImportResult {
	e.t.Helper()
	return e.m.Import(context.Background(), e.pick(keys...), nil)
}

// pick is what the dialog hands over for these keys. A key the scan does not offer goes in bare, as a
// caller naming something else would.
func (e *env) pick(keys ...string) []Found {
	e.t.Helper()
	s := e.m.Scan()
	out := s.Picks(keys)
	for _, k := range keys {
		if len(s.Picks([]string{k})) == 0 {
			out = append(out, Found{Key: k, Files: []string{k}})
		}
	}
	return out
}

func (e *env) allowed() []approval {
	e.t.Helper()
	a, err := e.m.loadApprovals()
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Allowed
}

func (e *env) hashes() map[string]string {
	e.t.Helper()
	h, err := e.m.loadHashes()
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) addOK(key string) *protocol.LocalModel {
	e.t.Helper()
	r := e.add(key)
	if len(r) != 1 || r[0].Err != "" {
		e.t.Fatalf("import %s: %+v", key, r)
	}
	return e.listed(r[0].ModelID)
}

func (e *env) listed(id string) *protocol.LocalModel {
	for _, lm := range e.m.Models() {
		if lm.ModelID == id {
			return &lm
		}
	}
	return nil
}

func foundKeys(s Scan) []string {
	out := []string{}
	for _, f := range s.Found {
		out = append(out, f.Key)
	}
	return out
}

func refusedWhy(s Scan, name string) string {
	for _, r := range s.Refused {
		if r.Name == name {
			return r.Why
		}
	}
	return ""
}

// A stranger's file: offered in the dialog, and nothing else until the click. A hub cannot reach it by
// load, inspect or download, under any of the names an import would give it.
func TestStrangersFileGetsNothingBeforeTheClick(t *testing.T) {
	e := newEnv(t)
	e.put("Stranger-Q4_K_M.gguf", e.model)
	if got := foundKeys(e.m.Scan()); len(got) != 1 || got[0] != "Stranger-Q4_K_M.gguf" {
		t.Fatalf("offered %v", got)
	}
	if len(e.m.Models()) != 0 {
		t.Fatalf("listed before the click: %+v", e.m.Models())
	}
	if _, lerr := e.m.Resolve("stranger-q4_k_m"); lerr == nil || lerr.Code != protocol.ErrNotFound {
		t.Fatalf("load: %v", lerr)
	}
	sha := hftest.SHA256(e.model)
	repo := "local-file/Stranger-Q4_K_M"
	if _, ierr := e.m.Inspect(context.Background(), &protocol.InspectArgs{Repo: repo, Commit: sha[:40], File: "Stranger-Q4_K_M.gguf"}); ierr == nil || ierr.Code != protocol.ErrNotFound {
		t.Fatalf("inspect: %v", ierr)
	}
	if o := e.download(args("stranger-q4_k_m", repo, sha[:40], "Stranger-Q4_K_M.gguf")); o.Err == nil || o.Err.Code != protocol.ErrDenied {
		t.Fatalf("download: %+v", o)
	}
	if len(e.m.Models()) != 0 || len(e.allowed()) != 0 || len(e.hashes()) != 0 {
		t.Fatal("a refused command left something behind")
	}
	if n := e.hfCalls.Load(); n != 0 {
		t.Fatalf("%d requests to Hugging Face for the reserved owner", n)
	}
}

// The click brings it in: the frame's fields, the DPAPI records, and a load through the same path as a download.
func TestImportEndsLikeADownload(t *testing.T) {
	e := newEnv(t)
	e.put("Stranger-Q4_K_M.gguf", e.model)
	sha := hftest.SHA256(e.model)
	before := e.changes.Load()
	lm := e.addOK("Stranger-Q4_K_M.gguf")
	if lm.ModelID != "stranger-q4_k_m" || lm.Repo != "local-file/Stranger-Q4_K_M" || lm.Commit != sha[:40] || lm.Kind != "gguf" || lm.State != StateReady {
		t.Fatalf("listed %+v", lm)
	}
	if len(lm.Files) != 1 || lm.Files[0].Path != "Stranger-Q4_K_M.gguf" || lm.Files[0].Size != int64(len(e.model)) || lm.Files[0].SHA256 != sha || *lm.SizeBytes != int64(len(e.model)) {
		t.Fatalf("files %+v", lm.Files)
	}
	if e.changes.Load() == before {
		t.Fatal("the hub was not told")
	}
	if !e.m.approved(lm.Repo, lm.Commit, []string{"Stranger-Q4_K_M.gguf"}) || e.hashes()[lm.Repo+"@"+lm.Commit+"/Stranger-Q4_K_M.gguf"] != sha {
		t.Fatal("the records are not there")
	}
	var hashed int
	e.m.testBeforeHash = func() { hashed++ }
	l, lerr := e.m.Resolve(lm.ModelID)
	if lerr != nil || l.Path != filepath.Join(e.root, "Stranger-Q4_K_M.gguf") {
		t.Fatalf("load: %+v %v", l, lerr)
	}
	if hashed != 1 {
		t.Fatalf("the first load hashed %d times", hashed)
	}
	if got := e.m.Scan(); len(got.Found) != 0 || len(got.Refused) != 0 {
		t.Fatalf("offered again: %+v", got)
	}
	// It lasts: the index and records are read back at the next start.
	e.m.Close()
	e.open()
	if lm := e.listed("stranger-q4_k_m"); lm == nil || lm.State != StateReady {
		t.Fatalf("after a restart: %+v", lm)
	}
	if _, lerr := e.m.Resolve("stranger-q4_k_m"); lerr != nil {
		t.Fatal(lerr)
	}
}

// What the dialog offers, and what it does not.
func TestScanRules(t *testing.T) {
	e := newEnv(t)
	if o := e.download(args("tiny", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	e.put("good.gguf", e.model)
	e.put("sub/nested.gguf", e.model)
	e.put("a/b/too-deep.gguf", e.model)
	e.put("mmproj-F16.gguf", e.model)
	e.put("MMPROJ-other.gguf", e.model)
	e.put("half.gguf.part", e.model)
	e.put("readme.txt", []byte("x"))
	e.put("My Model (1).gguf", e.model)
	e.put("still-copying.gguf", e.model[:len(e.model)/2])
	e.put("not-gguf.gguf", []byte(strings.Repeat("nope", 100)))
	s := e.m.Scan()
	if got := strings.Join(foundKeys(s), " "); got != "good.gguf sub/nested.gguf" {
		t.Fatalf("offered %q", got)
	}
	if !strings.Contains(refusedWhy(s, "My Model (1).gguf"), "rename") {
		t.Fatalf("refused %+v", s.Refused)
	}
	for _, n := range []string{"still-copying.gguf", "not-gguf.gguf"} {
		if !strings.Contains(refusedWhy(s, n), "not a complete GGUF") {
			t.Fatalf("%s: %+v", n, s.Refused)
		}
	}
	for _, r := range s.Refused {
		if strings.Contains(r.Why, e.root) {
			t.Fatalf("a path in the text: %+v", r)
		}
	}
	// Only what is on offer can be imported, whatever the caller names.
	for _, k := range []string{"mmproj-F16.gguf", "half.gguf.part", "a/b/too-deep.gguf", "My Model (1).gguf", "../outside.gguf", "unsloth/Tiny-GGUF/" + commitA[:12] + "/Tiny-Q4_K_M.gguf"} {
		if r := e.add(k); r[0].Err == "" {
			t.Fatalf("%s was imported", k)
		}
	}
}

func split(e *env, count int) []byte {
	f := gguftest.Default()
	f.SplitCount = count
	f.DataBytes = 4096
	return gguftest.Build(f)
}

// A multi-part set is one entry, and refused until every part is there.
func TestMultiPart(t *testing.T) {
	e := newEnv(t)
	part := split(e, 3)
	e.put("big-00001-of-00003.gguf", part)
	e.put("big-00003-of-00003.gguf", part)
	if s := e.m.Scan(); len(s.Found) != 0 || !strings.Contains(refusedWhy(s, "big-00001-of-00003.gguf"), "2 of the parts") {
		t.Fatalf("incomplete: %+v", s)
	}
	e.put("big-00005-of-00003.gguf", part)
	if s := e.m.Scan(); len(s.Found) != 0 {
		t.Fatalf("a wrong part number made it complete: %+v", s)
	}
	os.Remove(filepath.Join(e.root, "big-00005-of-00003.gguf"))
	e.put("big-00002-of-00003.gguf", part)
	s := e.m.Scan()
	if len(s.Found) != 1 || len(s.Found[0].Files) != 3 || s.Found[0].Key != "big-00001-of-00003.gguf" {
		t.Fatalf("complete: %+v", s)
	}
	lm := e.addOK("big-00001-of-00003.gguf")
	if lm.ModelID != "big" || lm.Repo != "local-file/big" || len(lm.Files) != 3 || lm.Files[2].Path != "big-00003-of-00003.gguf" {
		t.Fatalf("listed %+v", lm)
	}
	if len(e.m.Models()) != 1 || len(e.m.Scan().Found) != 0 {
		t.Fatal("a set made more than one entry")
	}
	// A file that says it is a part, under a name that does not.
	e2 := newEnv(t)
	e2.put("renamed.gguf", part)
	if s := e2.m.Scan(); len(s.Found) != 0 || !strings.Contains(refusedWhy(s, "renamed.gguf"), "one part") {
		t.Fatalf("a lone part: %+v", s)
	}
}

// Links leading out of the folder get nothing: a file symlink, a junction, a hard link.
func TestLinksAreRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows links")
	}
	e := newEnv(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.gguf")
	if err := os.WriteFile(target, e.model, 0o644); err != nil {
		t.Fatal(err)
	}
	offered := func(s Scan, key string) bool {
		for _, k := range foundKeys(s) {
			if k == key {
				return true
			}
		}
		return false
	}

	// Junction: the folder inside is a door to another one.
	j := filepath.Join(e.root, "door")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink failed: %v %s", err, out)
	}
	s := e.m.Scan()
	if offered(s, "door/secret.gguf") || refusedWhy(s, "door") == "" {
		t.Fatalf("through a junction: %+v", s)
	}
	if r := e.add("door/secret.gguf"); r[0].Err == "" {
		t.Fatal("imported through a junction")
	}
	if _, _, err := openConfined(e.root, "door/secret.gguf"); err == nil {
		t.Fatal("opened through a junction")
	}

	// Hard link: one more name for a file elsewhere.
	if err := os.Link(target, filepath.Join(e.root, "hard.gguf")); err != nil {
		t.Fatal(err)
	}
	s = e.m.Scan()
	if offered(s, "hard.gguf") || !strings.Contains(refusedWhy(s, "hard.gguf"), "link") {
		t.Fatalf("a hard link: %+v", s)
	}

	// File symlink, where this account may make one (it may need developer mode).
	if err := os.Symlink(target, filepath.Join(e.root, "soft.gguf")); err == nil {
		s = e.m.Scan()
		if offered(s, "soft.gguf") || !strings.Contains(refusedWhy(s, "soft.gguf"), "link") {
			t.Fatalf("a symlink: %+v", s)
		}
		if r := e.add("soft.gguf"); r[0].Err == "" {
			t.Fatal("imported a symlink")
		}
	} else {
		t.Logf("symlink not available: %v", err)
	}
	if len(e.m.Models()) != 0 {
		t.Fatalf("listed %+v", e.m.Models())
	}

	// A file swapped for a link after its import: the next load refuses it.
	e.put("real.gguf", e.model)
	lm := e.addOK("real.gguf")
	os.Remove(filepath.Join(e.root, "real.gguf"))
	if err := os.Symlink(target, filepath.Join(e.root, "real.gguf")); err == nil {
		if _, lerr := e.m.Resolve(lm.ModelID); lerr == nil {
			t.Fatal("loaded through a link")
		}
		if got := e.listed(lm.ModelID); got.State != StateChanged {
			t.Fatalf("state %s", got.State)
		}
	}
}

// A planted index cannot pass as an import: its hash is its own, and the records are DPAPI's.
func TestPlantedImportedEntry(t *testing.T) {
	e := newEnv(t)
	p := e.put("planted.gguf", e.model)
	sha := hftest.SHA256(e.model)
	entry := func(state string) *Entry {
		return &Entry{ModelID: "planted", Repo: "local-file/planted", Commit: sha[:40], Kind: "gguf", Names: []string{"planted.gguf"},
			Files: []protocol.LocalFile{{Path: "planted.gguf", Size: int64(len(e.model)), SHA256: sha}}, State: state}
	}
	j, _ := json.Marshal(indexFile{Version: 1, Models: []*Entry{entry(StateReady)}})
	os.WriteFile(filepath.Join(e.root, indexName), j, 0o644)
	e.m.Close()
	e.open()
	if _, lerr := e.m.Resolve("planted"); lerr == nil || lerr.Code != protocol.ErrNotFound {
		t.Fatalf("load: %v", lerr)
	}
	if _, ierr := e.m.Inspect(context.Background(), &protocol.InspectArgs{Repo: "local-file/planted", Commit: sha[:40], File: "planted.gguf"}); ierr == nil || ierr.Code != protocol.ErrNotFound {
		t.Fatalf("inspect: %v", ierr)
	}
	// Delete of it must not touch the file either.
	if derr := e.m.Delete("planted", false); derr != nil {
		t.Fatal(derr)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	// An entry that would start a download, or disagrees with its own hash, or is another spelling of
	// the owner, does not come out of the index at all.
	bad := map[string]*Entry{"downloading": entry(StateDownloading), "awaiting": entry(StateAwaiting), "commit": entry(StateReady), "spelling": entry(StateReady)}
	bad["commit"].Commit = commitA
	bad["spelling"].Repo = "Local-File/planted"
	for name, x := range bad {
		if x.valid() {
			t.Errorf("%s: accepted", name)
		}
	}
	// Removing an imported entry's files can never reach the host's file, by any path.
	removeFiles(e.root, entry(StateReady), e.m.log)
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

// Inspect reads only an imported file, by (repo, commit, file), from the disk and never from the network.
func TestInspectImported(t *testing.T) {
	e := newEnv(t)
	e.put("mine.gguf", e.model)
	e.put("other.gguf", e.model)
	lm := e.addOK("mine.gguf")
	facts, ierr := e.m.Inspect(context.Background(), &protocol.InspectArgs{Repo: lm.Repo, Commit: lm.Commit, File: "mine.gguf"})
	if ierr != nil || !facts.FromDisk || facts.Header.Arch != "llama" || facts.FileSize != int64(len(e.model)) {
		t.Fatalf("inspect: %+v %v", facts, ierr)
	}
	r := facts.Result()
	if r["arch"] != "llama" || r["split_count"] != int64(1) {
		t.Fatalf("result %+v", r)
	}
	for name, a := range map[string]*protocol.InspectArgs{
		"another file in the folder": {Repo: lm.Repo, Commit: lm.Commit, File: "other.gguf"},
		"another commit":             {Repo: lm.Repo, Commit: commitA, File: "mine.gguf"},
		"another repo":               {Repo: "local-file/other", Commit: lm.Commit, File: "mine.gguf"},
		"another spelling":           {Repo: "LOCAL-FILE/mine", Commit: lm.Commit, File: "mine.gguf"},
		"the index":                  {Repo: lm.Repo, Commit: lm.Commit, File: "cr-agent-index.gguf"},
		"up and out":                 {Repo: lm.Repo, Commit: lm.Commit, File: "../mine.gguf"},
	} {
		if _, ierr := e.m.Inspect(context.Background(), a); ierr == nil {
			t.Errorf("%s: answered", name)
		}
		e.m.mu.Lock()
		e.m.recent = nil // the rate limit is not what is under test
		e.m.mu.Unlock()
	}
	if n := e.hfCalls.Load(); n != 0 {
		t.Fatalf("%d requests to Hugging Face", n)
	}
}

// Delete forgets: index, hash, approval. The file stays, and a later scan offers it again.
func TestDeleteOnlyForgets(t *testing.T) {
	e := newEnv(t)
	p := e.put("sub/keep.gguf", e.model)
	lm := e.addOK("sub/keep.gguf")
	if derr := e.m.Delete(lm.ModelID, true); derr == nil || derr.Code != protocol.ErrBusy {
		t.Fatalf("delete while loaded: %v", derr)
	}
	if derr := e.m.Delete(lm.ModelID, false); derr != nil {
		t.Fatal(derr)
	}
	if b, err := os.ReadFile(p); err != nil || len(b) != len(e.model) {
		t.Fatalf("the file went: %v", err)
	}
	if e.listed(lm.ModelID) != nil || e.m.approved(lm.Repo, lm.Commit, []string{"sub/keep.gguf"}) || len(e.hashes()) != 0 {
		t.Fatal("something was remembered")
	}
	if _, lerr := e.m.Resolve(lm.ModelID); lerr == nil {
		t.Fatal("loadable after delete")
	}
	if got := foundKeys(e.m.Scan()); len(got) != 1 || got[0] != "sub/keep.gguf" {
		t.Fatalf("offered %v", got)
	}
	// A download's delete still removes its files.
	if o := e.download(args("tiny", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	if derr := e.m.Delete("tiny", false); derr != nil {
		t.Fatal(derr)
	}
	if _, err := os.Stat(tinyPath(e.root)); !os.IsNotExist(err) {
		t.Fatal("a download's delete left its file")
	}
}

// A changed file becomes changed, a missing one missing; neither is fetched again. To use a changed
// file the host imports it again, under the same model id.
func TestChangedAndMissing(t *testing.T) {
	e := newEnv(t)
	p := e.put("mine.gguf", e.model)
	lm := e.addOK("mine.gguf")
	if _, lerr := e.m.Resolve(lm.ModelID); lerr != nil {
		t.Fatal(lerr)
	}
	blobs, calls := e.srv.BlobRequests.Load(), e.hfCalls.Load()

	// Changed with the size kept: the first-load hash finds it.
	b := append([]byte(nil), e.model...)
	b[len(b)-1] ^= 0xff
	os.WriteFile(p, b, 0o644)
	os.Chtimes(p, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if _, lerr := e.m.Resolve(lm.ModelID); lerr == nil || lerr.Code != protocol.ErrHashMismatch || strings.Contains(lerr.Message, "download") {
		t.Fatalf("changed: %v", lerr)
	}
	if got := e.listed(lm.ModelID); got.State != StateChanged {
		t.Fatalf("state %s", got.State)
	}
	if _, lerr := e.m.Resolve(lm.ModelID); lerr == nil || lerr.Code != protocol.ErrNotFound {
		t.Fatalf("changed stays changed: %v", lerr)
	}
	if o := e.download(args(lm.ModelID, lm.Repo, lm.Commit, "mine.gguf")); o.Err == nil || o.Err.Code != protocol.ErrDenied {
		t.Fatalf("re-download: %+v", o)
	}

	// Imported again: the same id, the new hash, the old records gone.
	if got := foundKeys(e.m.Scan()); len(got) != 1 || got[0] != "mine.gguf" {
		t.Fatalf("offered %v", got)
	}
	again := e.addOK("mine.gguf")
	if again.ModelID != lm.ModelID || again.Commit == lm.Commit || again.State != StateReady {
		t.Fatalf("again %+v", again)
	}
	if e.m.approved(lm.Repo, lm.Commit, []string{"mine.gguf"}) {
		t.Fatal("the old approval is still there")
	}
	if _, lerr := e.m.Resolve(lm.ModelID); lerr != nil {
		t.Fatal(lerr)
	}

	// Missing at the next start, and back when the file is.
	saved, _ := os.ReadFile(p)
	os.Remove(p)
	e.m.Close()
	e.open()
	if got := e.listed(lm.ModelID); got.State != StateMissing {
		t.Fatalf("state %s", got.State)
	}
	if got := e.m.Scan(); len(got.Found) != 0 {
		t.Fatalf("offered %+v", got)
	}
	os.WriteFile(p, saved, 0o644)
	e.m.Scan()
	if got := e.listed(lm.ModelID); got.State != StateReady {
		t.Fatalf("back: %s", got.State)
	}
	// Another size is changed at once, with no load needed.
	os.WriteFile(p, saved[:len(saved)-32], 0o644)
	e.m.Close()
	e.open()
	if got := e.listed(lm.ModelID); got.State != StateChanged {
		t.Fatalf("state %s", got.State)
	}
	if e.srv.BlobRequests.Load() != blobs || e.hfCalls.Load() != calls {
		t.Fatal("something was fetched")
	}
}

// The hash does not hold the lock, and a cancelled import leaves nothing.
func TestImportDoesNotBlockAndCancels(t *testing.T) {
	e := newEnv(t)
	e.put("mine.gguf", e.model)
	entered, release := make(chan struct{}), make(chan struct{})
	e.m.testDuringImport = func() { close(entered); <-release }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []ImportResult, 1)
	go func() { done <- e.m.Import(ctx, e.pick("mine.gguf"), nil) }()
	<-entered
	free := make(chan struct{})
	go func() {
		e.m.Models()
		e.m.Jobs()
		e.m.Dir()
		e.m.Scan()
		close(free)
	}()
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("the manager is blocked during a hash")
	}
	if err := e.m.SetDir(t.TempDir()); err == nil {
		t.Fatal("the folder changed during an import")
	}
	if r := e.m.Import(context.Background(), e.pick("mine.gguf"), nil); r[0].Err == "" {
		t.Fatal("two imports at once")
	}
	cancel()
	close(release)
	if r := <-done; r[0].Err != "cancelled" {
		t.Fatalf("result %+v", r)
	}
	if len(e.m.Models()) != 0 || len(e.allowed()) != 0 || len(e.hashes()) != 0 {
		t.Fatal("a cancelled import left something")
	}
	// Progress reaches the total.
	e.m.testDuringImport = nil
	var last, total int64
	r := e.m.Import(context.Background(), e.pick("mine.gguf"), func(_ string, d, n int64) { last, total = d, n })
	if r[0].Err != "" || total != int64(len(e.model)) || last != total {
		t.Fatalf("%+v progress %d of %d", r, last, total)
	}
}

// A file that changes during its hash is refused.
func TestChangedDuringTheHash(t *testing.T) {
	e := newEnv(t)
	p := e.put("mine.gguf", e.model)
	e.m.testDuringImport = func() { os.Chtimes(p, time.Now().Add(time.Hour), time.Now().Add(time.Hour)) }
	if r := e.add("mine.gguf"); !strings.Contains(r[0].Err, "changed while") {
		t.Fatalf("%+v", r)
	}
	if len(e.m.Models()) != 0 {
		t.Fatal("listed")
	}
}

// Names and ids: every one passes the schema's own patterns, and an id is unique.
func TestNamesPassTheSchema(t *testing.T) {
	raw, err := os.ReadFile("../../protocol/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	pattern := func(name string) *regexp.Regexp {
		p := schema.Defs[name].Pattern
		if p == "" {
			t.Fatalf("no pattern for %s", name)
		}
		// RE2 has no lookahead: the file pattern's two are checked by hand below.
		p = strings.NewReplacer(`(?!.*\.\.)`, "", `(?![^/]*[. ]$)`, "").Replace(p)
		return regexp.MustCompile(p)
	}
	reR, reC, reM, reF := pattern("repo"), pattern("commit"), pattern("modelId"), pattern("ggufFile")

	e := newEnv(t)
	long := strings.Repeat("a", 200)
	names := []string{"plain.gguf", "UPPER-Case_1.5.gguf", "-leading.gguf", "_x.gguf", ".hidden.gguf", "---.gguf",
		"x." + "y-z.gguf", long + ".gguf", "sub/nested.gguf", "dup.gguf", "DUP.1.gguf", "other/dup.gguf"}
	for _, n := range names {
		e.put(n, e.model)
	}
	ids := map[string]bool{}
	for _, f := range e.m.Scan().Found {
		ids[f.Key] = true
	}
	for _, n := range names {
		if !ids[n] {
			t.Fatalf("%q not offered: %+v", n, e.m.Scan().Refused)
		}
		if r := e.add(n); r[0].Err != "" {
			t.Fatalf("%q: %+v", n, r)
		}
	}
	seen := map[string]bool{}
	for _, lm := range e.m.Models() {
		if seen[lm.ModelID] {
			t.Fatalf("id %s twice", lm.ModelID)
		}
		seen[lm.ModelID] = true
		f := lm.Files[0].Path
		if !reR.MatchString(lm.Repo) || !reC.MatchString(lm.Commit) || !reM.MatchString(lm.ModelID) || !reF.MatchString(f) || strings.Contains(f, "..") {
			t.Errorf("%+v does not fit the schema", lm)
		}
		if !protocol.ValidRepo(lm.Repo) || !protocol.ValidModelID(lm.ModelID) || !protocol.ValidGGUFFile(f) {
			t.Errorf("%+v does not fit the decoder", lm)
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("%d models for %d names", len(seen), len(names))
	}
	if s := localSlug("aux.v2.gguf"); s != "m-aux.v2" || !protocol.ValidRepo(LocalOwner+"/"+s) {
		t.Fatalf("device name slug %q", s)
	}
	if s := localSlug("---.gguf"); s != "model" {
		t.Fatalf("empty slug %q", s)
	}
}

// The frame: encoded as the agent sends it, decoded by the protocol's decoder, equal to the golden
// frame's shape. The golden frame is what the hub's decoder is tested on too.
func TestImportedFrameDecodes(t *testing.T) {
	e := newEnv(t)
	e.put("Stranger-Q4_K_M.gguf", e.model)
	e.addOK("Stranger-Q4_K_M.gguf")
	out, err := protocol.Encode(&protocol.Status{Engine: protocol.EngineUnloaded, Models: e.m.Models()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := protocol.Decode(out)
	if err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	st, ok := f.(*protocol.Status)
	if !ok || len(st.Models) != 1 || st.Models[0].Repo != "local-file/Stranger-Q4_K_M" {
		t.Fatalf("decoded %+v", f)
	}
	golden, err := os.ReadFile("../../protocol/golden/status-local-file.json")
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]any
	if err := json.Unmarshal(golden, &g); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	gm, om := g["models"].([]any)[0].(map[string]any), got["models"].([]any)[0].(map[string]any)
	for k := range om {
		if _, ok := gm[k]; !ok {
			t.Errorf("the golden frame has no %q", k)
		}
	}
	for k := range gm {
		if _, ok := om[k]; !ok {
			t.Errorf("the agent sends no %q", k)
		}
	}
}

// No store means nobody can record the approval, so nothing is imported.
func TestImportNeedsTheStore(t *testing.T) {
	e := newEnv(t)
	e.put("mine.gguf", e.model)
	e.m.o.Store = nil
	if r := e.add("mine.gguf"); r[0].Err == "" {
		t.Fatal("imported with no store")
	}
	if len(e.m.Models()) != 0 {
		t.Fatal("listed")
	}
}

// A cancel keeps what finished before it, and the results say so.
func TestCancelKeepsWhatFinished(t *testing.T) {
	e := newEnv(t)
	other := append([]byte(nil), e.model...)
	other[len(other)-1] ^= 1
	e.put("a.gguf", e.model)
	e.put("b.gguf", other)
	e.put("c.gguf", other)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := e.m.Import(ctx, e.pick("a.gguf", "b.gguf", "c.gguf"), func(file string, _, _ int64) {
		if file == "b.gguf" {
			cancel()
		}
	})
	if len(r) != 3 || r[0].Err != "" || r[0].ModelID != "a" || r[1].Err != "cancelled" || r[2].Err != "cancelled" {
		t.Fatalf("results %+v", r)
	}
	if e.listed("a") == nil || len(e.m.Models()) != 1 {
		t.Fatalf("listed %+v", e.m.Models())
	}
}

// failingProtector fails the read of the named secrets.
type failingProtector struct {
	plain
	fail map[string]bool
}

func (f *failingProtector) Unprotect(name string, b []byte) ([]byte, error) {
	if f.fail[name] {
		return nil, errors.New("DPAPI could not open the blob")
	}
	return f.plain.Unprotect(name, b)
}

// A failed read of the protected records changes nothing: an import or a delete during it refuses to
// write, and what was recorded loads as before once the read works again.
func TestFailedReadLosesNothing(t *testing.T) {
	for _, failing := range [][]string{{store.SecretApprovals}, {store.SecretModelHashes}, {store.SecretApprovals, store.SecretModelHashes}} {
		t.Run(strings.Join(failing, "+"), func(t *testing.T) {
			e := newEnv(t)
			e.ap.answer = true
			if o := e.download(args("wiz", "wizard-org/Wizard-GGUF", commitA, "W-Q4_K_M.gguf")); o.Err != nil {
				t.Fatal(o.Err)
			}
			e.put("mine.gguf", e.model)
			mine := e.addOK("mine.gguf").ModelID
			e.put("gone.gguf", e.model)
			gone := e.addOK("gone.gguf").ModelID
			other := append([]byte(nil), e.model...)
			other[len(other)-1] ^= 1
			e.put("second.gguf", other)
			approvalsBefore, _ := e.st.ReadSecret(store.SecretApprovals)
			hashesBefore, _ := e.st.ReadSecret(store.SecretModelHashes)

			fp := &failingProtector{fail: map[string]bool{}}
			st, err := store.OpenWith(e.st.Dir(), fp)
			if err != nil {
				t.Fatal(err)
			}
			e.m.o.Store = st
			pick := e.pick("second.gguf")
			for _, name := range failing {
				fp.fail[name] = true
			}
			if r := e.m.Import(context.Background(), pick, nil); r[0].Err == "" {
				t.Fatalf("imported while the records could not be read: %+v", r)
			}
			if _, lerr := e.m.Resolve(mine); lerr == nil {
				t.Fatal("loaded while the records could not be read")
			}
			if derr := e.m.Delete(gone, false); derr != nil {
				t.Fatal(derr)
			}
			if err := e.m.remember("wizard-org/x", commitA, []string{"x.gguf"}); (err == nil) == fp.fail[store.SecretApprovals] {
				t.Errorf("remember: %v", err)
			}
			err = e.m.recordHashes(&Entry{Repo: "wizard-org/x", Commit: commitA, Files: []protocol.LocalFile{{Path: "x.gguf", SHA256: "a"}}})
			if (err == nil) == fp.fail[store.SecretModelHashes] {
				t.Errorf("recordHashes: %v", err)
			}
			clear(fp.fail)

			// The blob of a record whose read failed is untouched, whatever was tried above.
			for _, name := range failing {
				want := approvalsBefore
				if name == store.SecretModelHashes {
					want = hashesBefore
				}
				if got, _ := e.st.ReadSecret(name); !bytes.Equal(got, want) {
					t.Errorf("%s was written over: %s", name, got)
				}
			}
			if _, lerr := e.m.Resolve("wiz"); lerr != nil {
				t.Fatalf("the download after the failed read: %v", lerr)
			}
			if _, lerr := e.m.Resolve(mine); lerr != nil {
				t.Fatalf("the added file after the failed read: %v", lerr)
			}
		})
	}
}

// A damaged record is a failed read too, not an empty list.
func TestDamagedRecordIsNotEmpty(t *testing.T) {
	for _, name := range []string{store.SecretApprovals, store.SecretModelHashes} {
		e2 := newEnv(t)
		e2.put("mine.gguf", e2.model)
		if err := e2.st.WriteSecret(name, []byte("not json")); err != nil {
			t.Fatal(err)
		}
		if r := e2.add("mine.gguf"); r[0].Err == "" {
			t.Fatalf("%s: imported over a damaged record", name)
		}
		if b, _ := e2.st.ReadSecret(name); string(b) != "not json" {
			t.Fatalf("%s was written over: %s", name, b)
		}
	}
}

// What the host approved is what the dialog showed: a file that differs from it afterwards is refused.
func TestChangedAfterTheDialog(t *testing.T) {
	e := newEnv(t)
	p := e.put("model.gguf", e.model)
	f := gguftest.Default()
	f.DataBytes = 512 << 10
	bigger := gguftest.Build(f)

	// Another size.
	shown := e.m.Scan().Picks([]string{"model.gguf"})
	os.WriteFile(p, bigger, 0o644)
	r := e.m.Import(context.Background(), shown, nil)
	if !strings.Contains(r[0].Err, "changed after you chose") || len(e.m.Models()) != 0 {
		t.Fatalf("another size: %+v", r)
	}
	// The same size, another modified time: the same bytes for all the dialog can tell.
	shown = e.m.Scan().Picks([]string{"model.gguf"})
	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	r = e.m.Import(context.Background(), shown, nil)
	if !strings.Contains(r[0].Err, "changed after you chose") || len(e.m.Models()) != 0 {
		t.Fatalf("another time: %+v", r)
	}
	// A list of files other than the one offered now, with the right first name.
	shown = e.m.Scan().Picks([]string{"model.gguf"})
	shown[0].Files = append(shown[0].Files, "other.gguf")
	if r = e.m.Import(context.Background(), shown, nil); r[0].Err == "" {
		t.Fatalf("another list: %+v", r)
	}
	// Chosen again, it goes in, with the size it now has.
	if lm := e.addOK("model.gguf"); *lm.SizeBytes != int64(len(bigger)) {
		t.Fatalf("listed %+v", lm)
	}
}

// A hard link that appears after the import, or after a download, stops the next load, also with the
// hash already cached in this process.
func TestSecondNameStopsTheLoad(t *testing.T) {
	e := newEnv(t)
	p := e.put("mine.gguf", e.model)
	lm := e.addOK("mine.gguf")
	if _, lerr := e.m.Resolve(lm.ModelID); lerr != nil {
		t.Fatal(lerr)
	}
	outside := filepath.Join(t.TempDir(), "second-name.gguf")
	if err := os.Link(p, outside); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	_, lerr := e.m.Resolve(lm.ModelID)
	if lerr == nil || lerr.Code != protocol.ErrDenied || !strings.Contains(lerr.Message, "hard link") {
		t.Fatalf("load: %v", lerr)
	}
	if got := e.listed(lm.ModelID); got.State != StateChanged {
		t.Fatalf("state %s", got.State)
	}
	// Removing the link and adding the file again is the way back.
	os.Remove(outside)
	if again := e.addOK("mine.gguf"); again.ModelID != lm.ModelID {
		t.Fatalf("again %+v", again)
	}
	if _, lerr := e.m.Resolve(lm.ModelID); lerr != nil {
		t.Fatal(lerr)
	}

	// A download, the same.
	if o := e.download(args("tiny", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	if _, lerr := e.m.Resolve("tiny"); lerr != nil {
		t.Fatal(lerr)
	}
	if err := os.Link(tinyPath(e.root), filepath.Join(t.TempDir(), "tiny.gguf")); err != nil {
		t.Fatal(err)
	}
	if _, lerr := e.m.Resolve("tiny"); lerr == nil || lerr.Code != protocol.ErrDenied {
		t.Fatalf("a download with a second name: %v", lerr)
	}
}

// The sweep touches only partial files where downloads write. The rest of the folder is the host's.
func TestSweepLeavesTheHostsFiles(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	aged := func(rel string) string {
		p := e.put(rel, []byte("half"))
		os.Chtimes(p, old, old)
		return p
	}
	commit := commitA[:12]
	keep := []string{
		aged("hosts-browser-download.gguf.part"),
		aged("sub/also-the-hosts.gguf.part"),
		aged("a/b/also-the-hosts.gguf.part"),
		aged("a/b/not-a-commit/also-the-hosts.gguf.part"),
		aged("a/b/" + commit + "/x/y/too-deep.gguf.part"),
		aged("local-file/a/" + commit + "/reserved.gguf.part"),
		aged("a/b/" + commit + "/not-a-partial.part"),
	}
	gone := []string{aged("a/b/" + commit + "/old.gguf.part"), aged("a/b/" + commit + "/sub/old.gguf.part")}
	fresh := e.put("a/b/"+commit+"/resuming.gguf.part", []byte("half"))
	keep = append(keep, fresh)

	// Through what the agent really does: opening the folder.
	if err := e.m.SetDir(e.root); err != nil {
		t.Fatal(err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("swept: %s", p)
		}
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("not swept: %s", p)
		}
	}
	if runtime.GOOS != "windows" {
		return
	}
	// A junction is not followed, wherever it sits on the way.
	outside := t.TempDir()
	target := filepath.Join(outside, "b", commit, "outside.gguf.part")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, []byte("half"), 0o644)
	os.Chtimes(target, old, old)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(e.root, "door"), outside).CombinedOutput(); err != nil {
		t.Skipf("mklink failed: %v %s", err, out)
	}
	if err := e.m.SetDir(e.root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("the sweep went through a junction")
	}
}

// An uninstall removes the protected records and keeps the model folder. After a new install nothing is claimed as ready:
// the imported file can be added again by the host, the download fetched again, and neither loads until then.
func TestFolderOutlivesTheRecords(t *testing.T) {
	e := newEnv(t)
	e.put("kept.gguf", e.model)
	found := e.m.Scan().Found
	if len(found) != 1 {
		t.Fatalf("found %+v", found)
	}
	got := e.m.Import(context.Background(), found, nil)
	if len(got) != 1 || got[0].Err != "" {
		t.Fatalf("import %+v", got)
	}
	id := got[0].ModelID
	if _, lerr := e.m.Resolve(id); lerr != nil {
		t.Fatalf("before: %v", lerr)
	}
	if len(e.m.Scan().Found) != 0 {
		t.Fatal("a ready file is offered")
	}

	// Still ready after a plain restart: the records are there.
	e.m.Close()
	e.open()
	if _, lerr := e.m.Resolve(id); lerr != nil {
		t.Fatalf("after a restart: %v", lerr)
	}

	// The records go.
	e.m.Close()
	if err := e.st.WriteSecret(store.SecretModelHashes, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	e.open()
	var state string
	for _, x := range e.m.Models() {
		if x.ModelID == id {
			state = x.State
		}
	}
	if state != StateChanged {
		t.Fatalf("state %q", state)
	}
	if _, lerr := e.m.Resolve(id); lerr == nil {
		t.Fatal("it loads without a record")
	}
	// The host's click brings it back, under the same id.
	found = e.m.Scan().Found
	if len(found) != 1 {
		t.Fatalf("not offered again: %+v", found)
	}
	got = e.m.Import(context.Background(), found, nil)
	if len(got) != 1 || got[0].Err != "" || got[0].ModelID != id {
		t.Fatalf("import again %+v", got)
	}
	if _, lerr := e.m.Resolve(id); lerr != nil {
		t.Fatalf("after adding again: %v", lerr)
	}
}
