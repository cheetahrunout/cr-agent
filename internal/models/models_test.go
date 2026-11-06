package models

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf/gguftest"
	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/hf/hftest"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

const (
	commitA = "7c1e2a9d40b3f1e8a9c0d2b4e6f8a1c3e5d7f9b2"
	commitB = "1111111111222222222233333333334444444444"
)

// plain stands in for DPAPI so the tests also run where DPAPI is absent; the store tests cover DPAPI.
type plain struct{}

func (plain) Protect(_ string, b []byte) ([]byte, error)   { return append([]byte("P:"), b...), nil }
func (plain) Unprotect(_ string, b []byte) ([]byte, error) { return b[2:], nil }

type approver struct {
	mu      sync.Mutex
	answer  bool
	block   chan struct{} // when set, Ask waits for it (or ctx)
	prompts []Prompt
}

func (a *approver) Ask(ctx context.Context, p Prompt) bool {
	a.mu.Lock()
	a.prompts = append(a.prompts, p)
	ans, block := a.answer, a.block
	a.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false
		}
	}
	return ans
}

func (a *approver) asked() []Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Prompt(nil), a.prompts...)
}

type env struct {
	t       *testing.T
	srv     *hftest.Server
	m       *Manager
	st      *store.Store
	root    string
	ap      *approver
	model   []byte
	started atomic.Int32
	changes atomic.Int32
	free    atomic.Uint64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, srv: hftest.New(), ap: &approver{}, root: filepath.Join(t.TempDir(), "models")}
	t.Cleanup(e.srv.Close)
	f := gguftest.Default()
	f.DataBytes = 256 << 10 // six tensors → about 1.5 MB
	e.model = gguftest.Build(f)
	e.srv.AddRepo("unsloth/Tiny-GGUF", commitA, false, map[string][]byte{
		"Tiny-Q4_K_M.gguf": e.model, "sub/Tiny-Q8_0.gguf": e.model[:len(e.model)-32], "README.md": []byte("x"),
		"junk.gguf": []byte(strings.Repeat("not a gguf file ", 100)),
	})
	e.srv.AddRepo("wizard-org/Wizard-GGUF", commitA, false, map[string][]byte{"W-Q4_K_M.gguf": e.model})
	e.srv.AddRepo("wizard-org/Wizard-GGUF2", commitB, false, map[string][]byte{"W-Q4_K_M.gguf": e.model})
	e.free.Store(500 << 30)
	var err error
	if e.st, err = store.OpenWith(t.TempDir(), plain{}); err != nil {
		t.Fatal(err)
	}
	e.open()
	return e
}

func (e *env) open() {
	e.t.Helper()
	client, err := hf.New(hf.Options{Transport: e.srv.Transport(), Token: func() string { return "hf_secret" }})
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := New(Options{
		Store: e.st, Dir: e.root, HF: client, Approver: e.ap,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Started:         func(Prompt) { e.started.Add(1) },
		OnChange:        func(bool) { e.changes.Add(1) },
		ApprovalTimeout: 300 * time.Millisecond, ProgressEvery: 10 * time.Millisecond, RetryBase: time.Millisecond,
		DiskSpace: func(string) (uint64, uint64, error) { return e.free.Load(), 1000 << 30, nil },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.m = m
	e.t.Cleanup(m.Close)
}

func args(id, repo, commit string, files ...string) *protocol.DownloadArgs {
	return &protocol.DownloadArgs{ModelID: id, Repo: repo, Commit: commit, Files: files, Kind: "gguf"}
}

// download runs a download to its end.
func (e *env) download(a *protocol.DownloadArgs) Outcome {
	e.t.Helper()
	ch := make(chan Outcome, 1)
	e.m.Download(a, func(o Outcome) { ch <- o })
	select {
	case o := <-ch:
		return o
	case <-time.After(20 * time.Second):
		e.t.Fatal("download did not finish")
		return Outcome{}
	}
}

func (e *env) state(id string) string {
	for _, m := range e.m.Models() {
		if m.ModelID == id {
			return m.State
		}
	}
	return ""
}

func (e *env) waitJob(id, state string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range e.m.Jobs() {
			if j.ModelID == id && j.State == state {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("job %s never reached %s (jobs %+v)", id, state, e.m.Jobs())
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func tinyPath(root string) string {
	return filepath.Join(root, "unsloth", "Tiny-GGUF", commitA[:12], "Tiny-Q4_K_M.gguf")
}

func TestDownloadAllowlisted(t *testing.T) {
	e := newEnv(t)
	o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf", "sub/Tiny-Q8_0.gguf"))
	if o.Err != nil {
		t.Fatal(o.Err)
	}
	if o.Result["size_bytes"].(int64) != int64(2*len(e.model)-32) {
		t.Fatalf("result %v", o.Result)
	}
	p := tinyPath(e.root)
	got, err := os.ReadFile(p)
	if err != nil || hftest.SHA256(got) != hftest.SHA256(e.model) {
		t.Fatalf("file wrong: %v", err)
	}
	if !exists(filepath.Join(filepath.Dir(p), "sub", "Tiny-Q8_0.gguf")) || exists(p+".part") {
		t.Fatal("layout wrong or a partial left behind")
	}
	if len(e.ap.asked()) != 0 || e.started.Load() != 1 {
		t.Fatalf("allowlisted publisher: %d prompts, %d notices", len(e.ap.asked()), e.started.Load())
	}
	if e.srv.TokenAtCDN.Load() != 0 {
		t.Fatal("the token reached the CDN host")
	}
	ms := e.m.Models()
	if len(ms) != 1 || ms[0].State != StateReady || ms[0].Repo != "unsloth/Tiny-GGUF" || ms[0].Commit != commitA ||
		len(ms[0].Files) != 2 || ms[0].Files[0].SHA256 != hftest.SHA256(e.model) || len(e.m.Jobs()) != 0 {
		t.Fatalf("models %+v", ms)
	}
	// The same command again is answered at once, without touching the network.
	before := e.srv.BlobRequests.Load()
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf", "sub/Tiny-Q8_0.gguf")); o.Err != nil || e.srv.BlobRequests.Load() != before {
		t.Fatalf("repeat: %v", o.Err)
	}
	// The index survives a restart; a model id cannot be taken over by another repo.
	e.m.Close()
	e.open()
	if e.state("tiny-q4_k_m") != StateReady {
		t.Fatalf("after restart: %q", e.state("tiny-q4_k_m"))
	}
	if o := e.download(args("tiny-q4_k_m", "wizard-org/Wizard-GGUF", commitA, "W-Q4_K_M.gguf")); o.Err == nil || o.Err.Code != protocol.ErrBadArgs {
		t.Fatalf("id takeover: %v", o.Err)
	}
	// Load sees the file only now, after the pre-check.
	l, lerr := e.m.Resolve("tiny-q4_k_m")
	if lerr != nil || l.Path != p || l.Header.BlockCount != 2 || l.Header.ContextLength != 8192 {
		t.Fatalf("resolve %+v %v", l, lerr)
	}
}

func TestResumeAfterCut(t *testing.T) {
	e := newEnv(t)
	e.srv.FailAfter, e.srv.FailTimes = 300_000, 2 // the first two responses die after 300 KB
	o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"))
	if o.Err != nil {
		t.Fatal(o.Err)
	}
	if e.srv.RangeRequests.Load() < 2 {
		t.Fatalf("resumed with %d Range requests", e.srv.RangeRequests.Load())
	}
	got, _ := os.ReadFile(tinyPath(e.root))
	if hftest.SHA256(got) != hftest.SHA256(e.model) {
		t.Fatal("resumed file is wrong")
	}
}

func TestServerIgnoresRange(t *testing.T) {
	e := newEnv(t)
	p := tinyPath(e.root)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p+".part", e.model[:100_000], 0o644)
	e.srv.IgnoreRange = true
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	got, _ := os.ReadFile(p)
	if hftest.SHA256(got) != hftest.SHA256(e.model) {
		t.Fatal("restart from zero produced a wrong file")
	}
}

// A partial file with wrong bytes is caught by the hash, however the transfer went.
func TestHashMismatch(t *testing.T) {
	e := newEnv(t)
	e.srv.Corrupt = true
	o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrHashMismatch {
		t.Fatalf("got %v", o.Err)
	}
	p := tinyPath(e.root)
	if exists(p) || exists(p+".part") {
		t.Fatal("a file with a wrong hash stayed on disk")
	}
	if _, lerr := e.m.Resolve("tiny-q4_k_m"); lerr == nil {
		t.Fatal("a failed download is loadable")
	}
	if e.state("tiny-q4_k_m") != StateError {
		t.Fatalf("state %q", e.state("tiny-q4_k_m"))
	}

	// A tampered partial from an earlier run: resumed, hashed, refused.
	e.srv.Corrupt = false
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p+".part", []byte(strings.Repeat("X", 200_000)), 0o644)
	o = e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrHashMismatch || exists(p) {
		t.Fatalf("tampered partial: %v", o.Err)
	}
	// And a clean retry works.
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
}

func TestLookupRefusals(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		a    *protocol.DownloadArgs
		code string
	}{
		{"repo spelled in another case", args("a", "unsloth/tiny-gguf", commitA, "Tiny-Q4_K_M.gguf"), protocol.ErrNotFound},
		{"unknown repo", args("b", "unsloth/Absent-GGUF", commitA, "x.gguf"), protocol.ErrNotFound},
		{"unknown commit", args("c", "unsloth/Tiny-GGUF", commitB, "Tiny-Q4_K_M.gguf"), protocol.ErrNotFound},
		{"file not in the repo", args("d", "unsloth/Tiny-GGUF", commitA, "Other.gguf"), protocol.ErrNotFound},
		{"valid hash but not GGUF", args("e", "unsloth/Tiny-GGUF", commitA, "junk.gguf"), protocol.ErrUnsupported},
		{"path traversal", args("f", "unsloth/Tiny-GGUF", commitA, "../../x.gguf"), protocol.ErrBadArgs},
		{"three segments", args("g", "unsloth/Tiny-GGUF", commitA, "a/b/c.gguf"), protocol.ErrBadArgs},
		{"device name", args("h", "unsloth/Tiny-GGUF", commitA, "CON.gguf"), protocol.ErrBadArgs},
	}
	for _, c := range cases {
		o := e.download(c.a)
		if o.Err == nil || o.Err.Code != c.code {
			t.Errorf("%s: got %v, want %s", c.name, o.Err, c.code)
		}
	}
	// Nothing reached the disk outside the expected tree, and nothing is loadable.
	if exists(filepath.Join(filepath.Dir(e.root), "x.gguf")) || exists(filepath.Join(e.root, "unsloth", "Tiny-GGUF", commitA[:12], "junk.gguf")) {
		t.Fatal("a refused file is on disk")
	}
	for _, m := range e.m.Models() {
		if m.State == StateReady {
			t.Fatalf("model %s is ready", m.ModelID)
		}
	}
}

func TestApproval(t *testing.T) {
	e := newEnv(t)
	wiz := args("wizard-q4_k_m", "wizard-org/Wizard-GGUF", commitA, "W-Q4_K_M.gguf")

	// Deny.
	o := e.download(wiz)
	if o.Err == nil || o.Err.Code != protocol.ErrDenied || e.state("wizard-q4_k_m") != StateDenied {
		t.Fatalf("deny: %v state %q", o.Err, e.state("wizard-q4_k_m"))
	}
	p := e.ap.asked()[0]
	if p.Repo != "wizard-org/Wizard-GGUF" || p.SizeBytes != int64(len(e.model)) || p.AllowedBefore || p.Files[0] != "W-Q4_K_M.gguf" {
		t.Fatalf("prompt %+v", p)
	}
	if e.srv.BlobRequests.Load() != 0 {
		t.Fatal("bytes were fetched before the host allowed it")
	}

	// No answer within the deadline counts as Deny, and the job shows as waiting meanwhile.
	e.ap.block = make(chan struct{})
	ch := make(chan Outcome, 1)
	e.m.Download(wiz, func(o Outcome) { ch <- o })
	e.waitJob("wizard-q4_k_m", "awaiting_click")
	if j := e.m.Jobs()[0]; j.Kind != "approval" || j.AwaitingClick == nil || !*j.AwaitingClick || e.state("wizard-q4_k_m") != StateAwaiting {
		t.Fatalf("job %+v state %q", j, e.state("wizard-q4_k_m"))
	}
	if o := <-ch; o.Err == nil || o.Err.Code != protocol.ErrDenied {
		t.Fatalf("timeout: %v", o.Err)
	}

	// Allow: downloads, and is remembered for this repo and commit.
	e.ap.block, e.ap.answer = nil, true
	if o := e.download(wiz); o.Err != nil {
		t.Fatal(o.Err)
	}
	asked := len(e.ap.asked())
	if err := e.m.Delete("wizard-q4_k_m", false); err != nil {
		t.Fatal(err)
	}
	if o := e.download(wiz); o.Err != nil || len(e.ap.asked()) != asked {
		t.Fatalf("remembered approval asked again (%d → %d): %v", asked, len(e.ap.asked()), o.Err)
	}
	// The approvals file is a protected secret, not plain JSON in the model folder.
	if b, err := e.st.ReadSecret(store.SecretApprovals); err != nil || !strings.Contains(string(b), commitA) {
		t.Fatalf("approvals not stored: %v", err)
	}

	// Another repo of the same publisher still prompts: Allow is per repo and commit (ruling B13).
	e.ap.answer = false
	o = e.download(args("wizard2-q4_k_m", "wizard-org/Wizard-GGUF2", commitB, "W-Q4_K_M.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrDenied || len(e.ap.asked()) != asked+1 {
		t.Fatalf("other repo: %v, prompts %d", o.Err, len(e.ap.asked()))
	}
}

// A new commit of a repo allowed before prompts again, and says so.
func TestNewCommitPromptsAgain(t *testing.T) {
	e := newEnv(t)
	e.ap.answer = true
	if o := e.download(args("wizard-q4_k_m", "wizard-org/Wizard-GGUF", commitA, "W-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	oldFile := filepath.Join(e.root, "wizard-org", "Wizard-GGUF", commitA[:12], "W-Q4_K_M.gguf")
	e.srv.AddRepo("wizard-org/Wizard-GGUF", commitB, false, map[string][]byte{"W-Q4_K_M.gguf": e.model})
	if o := e.download(args("wizard-q4_k_m", "wizard-org/Wizard-GGUF", commitB, "W-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	ps := e.ap.asked()
	if len(ps) != 2 || ps[0].AllowedBefore || !ps[1].AllowedBefore || ps[1].Commit != commitB {
		t.Fatalf("prompts %+v", ps)
	}
	// Replaced in place: one model under the id, at the new commit, and the old commit's file is gone.
	ms := e.m.Models()
	if len(ms) != 1 || ms[0].Commit != commitB || ms[0].State != StateReady {
		t.Fatalf("models %+v", ms)
	}
	deadline := time.Now().Add(3 * time.Second)
	for exists(oldFile) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exists(oldFile) {
		t.Fatal("the replaced commit's file stayed")
	}
}

func TestNoSpace(t *testing.T) {
	e := newEnv(t)
	e.free.Store(100 << 30) // reserve is max(20 GiB, 10 % of 1000 GiB) = 100 GiB, plus the file
	o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrNoSpace || o.Err.NeedMB == nil || o.Err.FreeMB == nil || *o.Err.FreeMB != 100<<10 || *o.Err.NeedMB <= *o.Err.FreeMB {
		t.Fatalf("got %+v", o.Err)
	}
	if e.srv.BlobRequests.Load() != 0 {
		t.Fatal("the transfer started without space")
	}
}

func TestPauseResumeCancelAttach(t *testing.T) {
	e := newEnv(t)
	e.srv.ChunkDelay = 15 * time.Millisecond // about 1.5 MB in 64 KiB chunks: roughly 350 ms
	a := args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")
	var outs [2]chan Outcome
	for i := range outs {
		outs[i] = make(chan Outcome, 2)
	}
	e.m.Download(a, func(o Outcome) { outs[0] <- o })
	e.waitJob("tiny-q4_k_m", "running")
	// A repeat attaches (ruling B3); different files under the same id are refused.
	e.m.Download(a, func(o Outcome) { outs[1] <- o })
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "sub/Tiny-Q8_0.gguf")); o.Err == nil || o.Err.Code != protocol.ErrBusy {
		t.Fatalf("conflicting download: %v", o.Err)
	}

	// Progress is visible while bytes move.
	deadline := time.Now().Add(5 * time.Second)
	var j protocol.Job
	for time.Now().Before(deadline) {
		if js := e.m.Jobs(); len(js) == 1 && js[0].BytesDone != nil && *js[0].BytesDone > 0 {
			j = js[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if j.BytesTotal == nil || *j.BytesTotal != int64(len(e.model)) || j.Pct == nil || *j.Pct <= 0 || *j.Pct > 100 || j.Kind != "download" {
		t.Fatalf("progress %+v", j)
	}

	e.m.SetPaused(true)
	e.waitJob("tiny-q4_k_m", "paused")
	part := tinyPath(e.root) + ".part"
	st, err := os.Stat(part)
	if err != nil || st.Size() == 0 || st.Size() >= int64(len(e.model)) {
		t.Fatalf("partial after pause: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if st2, _ := os.Stat(part); st2.Size() != st.Size() {
		t.Fatal("bytes kept arriving while paused")
	}
	e.m.SetPaused(false)
	for i, ch := range outs {
		select {
		case o := <-ch:
			if o.Err != nil {
				t.Fatalf("waiter %d: %v", i, o.Err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("waiter %d never heard back", i)
		}
	}
	if e.srv.RangeRequests.Load() == 0 {
		t.Fatal("resume did not use Range")
	}
	select {
	case o := <-outs[0]:
		t.Fatalf("a waiter was called twice: %+v", o)
	default:
	}

	// Cancel mid-transfer: nothing stays.
	b := args("tiny-q8_0", "unsloth/Tiny-GGUF", commitA, "sub/Tiny-Q8_0.gguf")
	ch := make(chan Outcome, 1)
	e.m.Download(b, func(o Outcome) { ch <- o })
	e.waitJob("tiny-q8_0", "running")
	time.Sleep(40 * time.Millisecond)
	if !e.m.Cancel("tiny-q8_0") {
		t.Fatal("cancel found no job")
	}
	if o := <-ch; o.Err == nil || o.Err.Code != protocol.ErrCancelled {
		t.Fatalf("cancel: %v", o.Err)
	}
	sub := filepath.Join(e.root, "unsloth", "Tiny-GGUF", commitA[:12], "sub")
	if exists(sub) || e.state("tiny-q8_0") != "" || e.m.Cancel("tiny-q8_0") {
		t.Fatal("a cancelled download left files, an index entry or a job")
	}
}

// An agent that stops mid-download picks the job up again at the next start.
func TestRestartResumes(t *testing.T) {
	e := newEnv(t)
	e.srv.ChunkDelay = 15 * time.Millisecond
	ch := make(chan Outcome, 1)
	e.m.Download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"), func(o Outcome) { ch <- o })
	e.waitJob("tiny-q4_k_m", "running")
	time.Sleep(80 * time.Millisecond)
	e.m.Close()
	if o := <-ch; o.Err == nil {
		t.Fatal("a download finished during shutdown")
	}
	if !exists(tinyPath(e.root)+".part") || exists(tinyPath(e.root)) {
		t.Fatal("shutdown must keep the partial")
	}
	e.srv.Lock()
	e.srv.ChunkDelay = 0
	e.srv.Unlock()
	e.open() // the new manager finds the entry in "downloading" and resumes on its own
	deadline := time.Now().Add(10 * time.Second)
	for e.state("tiny-q4_k_m") != StateReady && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.state("tiny-q4_k_m") != StateReady || e.srv.RangeRequests.Load() == 0 {
		t.Fatalf("state %q after restart, %d Range requests", e.state("tiny-q4_k_m"), e.srv.RangeRequests.Load())
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	stranger := filepath.Join(e.root, "unsloth", "keep-me.txt")
	os.WriteFile(stranger, []byte("not ours"), 0o644)
	if err := e.m.Delete("tiny-q4_k_m", true); err == nil || err.Code != protocol.ErrBusy {
		t.Fatalf("delete while loaded: %v", err)
	}
	if err := e.m.Delete("nope", false); err == nil || err.Code != protocol.ErrNotFound {
		t.Fatalf("delete unknown: %v", err)
	}
	if err := e.m.Delete("tiny-q4_k_m", false); err != nil {
		t.Fatal(err)
	}
	if exists(tinyPath(e.root)) || exists(filepath.Join(e.root, "unsloth", "Tiny-GGUF")) {
		t.Fatal("files or empty folders stayed")
	}
	if !exists(stranger) || !exists(e.root) {
		t.Fatal("delete removed something it does not own")
	}
	if len(e.m.Models()) != 0 {
		t.Fatal("index entry stayed")
	}
}

// The index lives in a folder other programs can write: entries are re-validated on load.
func TestIndexIsInput(t *testing.T) {
	e := newEnv(t)
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	e.m.Close()
	raw, _ := os.ReadFile(filepath.Join(e.root, indexName))
	var f indexFile
	json.Unmarshal(raw, &f)
	sha := f.Models[0].Files[0].SHA256
	bad := func(id, repo, file string) *Entry {
		return &Entry{ModelID: id, Repo: repo, Commit: commitA, Kind: "gguf", Names: []string{file}, State: StateReady,
			Files: []protocol.LocalFile{{Path: file, Size: 10, SHA256: sha}}}
	}
	f.Models = append(f.Models,
		bad("evil1", "unsloth/Tiny-GGUF", "../../../Windows/system32/evil.gguf"),
		bad("evil2", "../..", "x.gguf"),
		bad("evil3", "unsloth/Tiny-GGUF", "a/b/c.gguf"),
		bad("EVIL4", "unsloth/Tiny-GGUF", "x.gguf"),
		bad("gone", "unsloth/Tiny-GGUF", "Gone.gguf"))
	raw, _ = json.Marshal(f)
	os.WriteFile(filepath.Join(e.root, indexName), raw, 0o644)
	e.open()
	ms := e.m.Models()
	if len(ms) != 2 {
		t.Fatalf("models after tampering: %+v", ms)
	}
	for _, m := range ms {
		switch m.ModelID {
		case "tiny-q4_k_m":
			if m.State != StateReady {
				t.Errorf("real model is %s", m.State)
			}
		case "gone":
			if m.State != StateMissing { // listed, file absent
				t.Errorf("absent model is %s", m.State)
			}
		default:
			t.Errorf("tampered entry kept: %s", m.ModelID)
		}
	}
	// A file that shrank on disk fails the pre-load check.
	os.Truncate(tinyPath(e.root), 5000)
	e.m.Close()
	e.open()
	if e.state("tiny-q4_k_m") != StateMissing {
		t.Fatalf("truncated file: state %q", e.state("tiny-q4_k_m"))
	}
	if _, lerr := e.m.Resolve("tiny-q4_k_m"); lerr == nil {
		t.Fatal("a truncated model is loadable")
	}
}

// A junction planted inside the model folder must not carry a download elsewhere.
func TestRefusesJunction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions: Windows only")
	}
	e := newEnv(t)
	outside := t.TempDir()
	link := filepath.Join(e.root, "unsloth")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink failed: %v %s", err, out)
	}
	defer os.Remove(link)
	o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrDownloadFailed || !strings.Contains(o.Err.Message, "link") {
		t.Fatalf("got %v", o.Err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("the download wrote through the junction: %v", ents)
	}
	// The model folder itself may not be a junction either.
	if _, err := CheckFolder(link); err == nil {
		t.Fatal("a junction was accepted as the model folder")
	}
}

func TestCheckFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "models")
	abs, err := CheckFolder(dir)
	if err != nil || !exists(abs) {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"", `\\server\share\models`, "//server/share"} {
		if _, err := CheckFolder(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	if _, err := CheckFolder(file); err == nil {
		t.Error("a file was accepted as the model folder")
	}
}

func TestSetDirReindexes(t *testing.T) {
	e := newEnv(t)
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := e.m.SetDir(other); err != nil {
		t.Fatal(err)
	}
	if len(e.m.Models()) != 0 || !exists(tinyPath(e.root)) {
		t.Fatal("the new folder must start empty and the old files must stay")
	}
	if err := e.m.SetDir(e.root); err != nil || e.state("tiny-q4_k_m") != StateReady {
		t.Fatalf("going back: %v, state %q", err, e.state("tiny-q4_k_m"))
	}
}

func TestInspect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := &protocol.InspectArgs{Repo: "unsloth/Tiny-GGUF", Commit: commitA, File: "Tiny-Q4_K_M.gguf"}
	f, err := e.m.Inspect(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if f.FromDisk || f.FileSize != int64(len(e.model)) || f.Header.Arch != "llama" || !f.Header.HasChatTemplate || f.Header.ParameterCount <= 0 {
		t.Fatalf("facts %+v", f)
	}
	// Nothing is written by an inspect.
	if ents, _ := os.ReadDir(e.root); len(ents) != 0 {
		t.Fatalf("inspect wrote to the model folder: %v", ents)
	}
	// The answer has exactly the schema's shape.
	raw, _ := json.Marshal(f.Result())
	var got struct {
		Arch string           `json:"arch"`
		KV   map[string]int64 `json:"kv"`
		TB   map[string]int64 `json:"tensors_bytes"`
		SC   int              `json:"split_count"`
	}
	if json.Unmarshal(raw, &got) != nil || got.Arch != "llama" || got.SC != 1 ||
		got.KV["block_count"] != 2 || got.KV["context_length"] != 8192 || got.KV["head_count_kv"] != 2 || got.KV["n_vocab"] != 100 || got.KV["file_type"] != 15 ||
		got.TB["token_embd"] != 256<<10 || got.TB["per_layer_repeating"] != 2*256<<10 || got.TB["total"] != 6*256<<10 {
		t.Fatalf("result %s", raw)
	}
	allowed := map[string]bool{"block_count": true, "context_length": true, "embedding_length": true, "head_count": true, "head_count_kv": true,
		"key_length": true, "value_length": true, "sliding_window": true, "n_vocab": true, "expert_count": true, "expert_used_count": true,
		"ssm_state": true, "file_type": true}
	for k := range got.KV {
		if !allowed[k] {
			t.Errorf("kv field %q is not in the schema", k)
		}
	}

	// Not a GGUF, absent file, bad names.
	for file, code := range map[string]string{"junk.gguf": protocol.ErrUnsupported, "Absent.gguf": protocol.ErrNotFound, "../x.gguf": protocol.ErrBadArgs} {
		if _, err := e.m.Inspect(ctx, &protocol.InspectArgs{Repo: "unsloth/Tiny-GGUF", Commit: commitA, File: file}); err == nil || err.Code != code {
			t.Errorf("%s: got %v, want %s", file, err, code)
		}
	}

	// Once the file is on disk, inspect reads it locally.
	if o := e.download(args("tiny-q4_k_m", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	before := e.srv.BlobRequests.Load()
	f, err = e.m.Inspect(ctx, a)
	if err != nil || !f.FromDisk || e.srv.BlobRequests.Load() != before {
		t.Fatalf("local inspect: %v", err)
	}

	// At most ten a minute.
	var last *Error
	for i := 0; i < inspectPerMinute; i++ {
		_, last = e.m.Inspect(ctx, a)
	}
	if last == nil || last.Code != protocol.ErrBusy {
		t.Fatalf("rate limit: %v", last)
	}
}

// A header larger than the first read is fetched with one more Range request, never the whole file.
func TestInspectLargeHeader(t *testing.T) {
	e := newEnv(t)
	f := gguftest.Default()
	f.Vocab = 500_000 // an 11-byte entry each: a header of about 5.5 MB
	f.DataBytes = 1 << 20
	big := gguftest.Build(f)
	e.srv.AddRepo("unsloth/Big-GGUF", commitA, false, map[string][]byte{"Big.gguf": big})
	facts, err := e.m.Inspect(context.Background(), &protocol.InspectArgs{Repo: "unsloth/Big-GGUF", Commit: commitA, File: "Big.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Header.NVocab != 500_000 || facts.ReadBytes <= inspectFirstRead || facts.ReadBytes > int64(len(big)) || e.srv.BlobRequests.Load() != 2 {
		t.Fatalf("vocab %d, read %d bytes in %d requests", facts.Header.NVocab, facts.ReadBytes, e.srv.BlobRequests.Load())
	}
}

// Finding M2: allowing one file of a commit does not allow another file of that commit.
func TestApprovalIsPerFile(t *testing.T) {
	e := newEnv(t)
	e.srv.AddRepo("wizard-org/Two-GGUF", commitA, false, map[string][]byte{"benign.gguf": e.model, "evil.gguf": e.model[:len(e.model)-64]})
	e.ap.answer = true
	if o := e.download(args("benign", "wizard-org/Two-GGUF", commitA, "benign.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	asked := e.ap.asked()
	if len(asked) != 1 || len(asked[0].Files) != 1 || asked[0].Files[0] != "benign.gguf" {
		t.Fatalf("the host was shown %+v", asked)
	}
	e.ap.answer = false
	o := e.download(args("evil", "wizard-org/Two-GGUF", commitA, "evil.gguf"))
	if o.Err == nil || o.Err.Code != protocol.ErrDenied || len(e.ap.asked()) != 2 || e.ap.asked()[1].Files[0] != "evil.gguf" {
		t.Fatalf("another file of an allowed commit: %v, %d prompts", o.Err, len(e.ap.asked()))
	}
	if _, lerr := e.m.Resolve("evil"); lerr == nil {
		t.Fatal("a file the host never allowed can be loaded")
	}
	// Both files in one request, one of them allowed before: it asks.
	if o := e.download(args("both", "wizard-org/Two-GGUF", commitA, "benign.gguf", "evil.gguf")); o.Err == nil || len(e.ap.asked()) != 3 {
		t.Fatalf("a request with one unapproved file: %v, %d prompts", o.Err, len(e.ap.asked()))
	}
	// The file the host did allow is remembered.
	if err := e.m.Delete("benign", false); err != nil {
		t.Fatal(err)
	}
	if o := e.download(args("benign", "wizard-org/Two-GGUF", commitA, "benign.gguf")); o.Err != nil || len(e.ap.asked()) != 3 {
		t.Fatalf("the allowed file asked again: %v, %d prompts", o.Err, len(e.ap.asked()))
	}
}

// Finding M3: a file that changed on disk is refused at its first load in this process.
func TestLoadHashesTheFile(t *testing.T) {
	e := newEnv(t)
	if o := e.download(args("tiny", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	p := tinyPath(e.root)
	b, _ := os.ReadFile(p)
	flipped := append([]byte(nil), b...)
	flipped[len(flipped)-1] ^= 0xff
	os.WriteFile(p, flipped, 0o644)
	_, lerr := e.m.Resolve("tiny")
	if lerr == nil || lerr.Code != protocol.ErrHashMismatch || e.state("tiny") != StateError {
		t.Fatalf("a changed file of the same size: %v, state %q", lerr, e.state("tiny"))
	}
	// A clean copy is hashed once, and a repeat of the same load in this process does not hash again.
	e2 := newEnv(t)
	if o := e2.download(args("tiny", "unsloth/Tiny-GGUF", commitA, "Tiny-Q4_K_M.gguf")); o.Err != nil {
		t.Fatal(o.Err)
	}
	if _, lerr := e2.m.Resolve("tiny"); lerr != nil {
		t.Fatal(lerr)
	}
	if len(e2.m.hashed) != 1 {
		t.Fatalf("%d files hashed", len(e2.m.hashed))
	}
	e2.m.Close()
	e2.open() // a new process hashes again
	if len(e2.m.hashed) != 0 {
		t.Fatal("a new manager starts with hashes")
	}
	q := tinyPath(e2.root)
	c, _ := os.ReadFile(q)
	c[len(c)-1] ^= 0xff
	os.WriteFile(q, c, 0o644)
	if _, lerr := e2.m.Resolve("tiny"); lerr == nil || lerr.Code != protocol.ErrHashMismatch {
		t.Fatalf("after a restart: %v", lerr)
	}
}

// Finding M3: an entry only the index names is not loadable, and downloading it asks the host.
func TestIndexAloneGrantsNothing(t *testing.T) {
	e := newEnv(t)
	other := filepath.Join(t.TempDir(), "folder")
	file := filepath.Join(other, "wizard-org", "Wizard-GGUF", commitA[:12], "W-Q4_K_M.gguf")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, e.model, 0o644)
	idx := indexFile{Version: 1, Models: []*Entry{{ModelID: "wiz", Repo: "wizard-org/Wizard-GGUF", Commit: commitA, Kind: "gguf", Names: []string{"W-Q4_K_M.gguf"},
		Files: []protocol.LocalFile{{Path: "W-Q4_K_M.gguf", Size: int64(len(e.model)), SHA256: hftest.SHA256(e.model)}}, State: StateReady}}}
	j, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(other, indexName), j, 0o644)
	if err := e.m.SetDir(other); err != nil {
		t.Fatal(err)
	}
	if _, lerr := e.m.Resolve("wiz"); lerr == nil || lerr.Code != protocol.ErrDenied {
		t.Fatalf("an entry only the index names: %v", lerr)
	}
	// The way out is the ordinary one: the host is asked, and on Allow the file on disk is used by its hash.
	e.ap.answer = true
	before := e.srv.BlobRequests.Load()
	if o := e.download(args("wiz", "wizard-org/Wizard-GGUF", commitA, "W-Q4_K_M.gguf")); o.Err != nil || len(e.ap.asked()) != 1 {
		t.Fatalf("download: %v, %d prompts", o.Err, len(e.ap.asked()))
	}
	if e.srv.BlobRequests.Load() != before {
		t.Fatal("the file was fetched again although it is on disk with the right hash")
	}
	if _, lerr := e.m.Resolve("wiz"); lerr != nil {
		t.Fatal(lerr)
	}
}
