// Package models owns the model folder: the index of downloaded models, the downloader, the local
// approval of unknown publishers, delete, and the GGUF header reads for inspect and the pre-load check.
//
// Security rules (§9.2, rulings B6, B13, B17): the hub names only repo, commit and files; sizes and
// SHA-256 hashes come from Hugging Face's own listing; a file becomes visible to load only after its
// hash matched and it was renamed into place; nothing is written through a link out of the model folder.
package models

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/confine"
	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/hw"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

// Publishers is the allowlist shipped inside the signed agent (§9.2): their files download without
// a prompt. It is compared with the owner exactly as Hugging Face spells it (ruling B17).
var Publishers = []string{"ggml-org", "unsloth", "bartowski", "lmstudio-community", "Qwen", "google", "meta-llama", "mistralai", "microsoft", "openai"}

// Allowlisted reports whether owner is a shipped publisher.
func Allowlisted(owner string) bool {
	for _, p := range Publishers {
		if p == owner {
			return true
		}
	}
	return false
}

// Model states (the localModel enum of the protocol).
const (
	StateDownloading = "downloading"
	StateAwaiting    = "awaiting_approval"
	StateDenied      = "denied"
	StateReady       = "ready"
	StateError       = "error"
	StateMissing     = "missing"
)

// Error is a refused or failed command, shaped for cmd.err.
type Error struct {
	Code    string
	Message string
	Field   string
	NeedMB  *int64
	FreeMB  *int64
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Outcome ends a download command.
type Outcome struct {
	Err    *Error
	Result map[string]any
}

// Prompt is what the host is asked for a publisher that is not on the allowlist. Every field comes
// from the agent's own Hugging Face lookup (ruling B14).
type Prompt struct {
	ModelID       string
	Repo          string
	Commit        string
	Files         []string
	SizeBytes     int64
	AllowedBefore bool // an older commit of this repo was allowed (ruling B13)
	Deadline      time.Time
}

// Approver asks the host. It returns false on Deny, on the deadline, or when ctx ends.
type Approver interface {
	Ask(ctx context.Context, p Prompt) bool
}

// DenyAll is the approver of an agent without a tray: nobody can click, so nothing is allowed.
type DenyAll struct{}

// Ask denies.
func (DenyAll) Ask(context.Context, Prompt) bool { return false }

// Options configure a manager.
type Options struct {
	Store    *store.Store
	Dir      string // the model folder
	HF       *hf.Client
	Log      *slog.Logger
	Approver Approver
	// Started is told about a download that began without a prompt (allowlisted or remembered),
	// so the tray can show a passive notice.
	Started func(p Prompt)
	// OnChange is called when a job or model changed; models is false for a pure progress tick.
	OnChange func(models bool)

	ApprovalTimeout time.Duration                                     // 10 min
	ProgressEvery   time.Duration                                     // 1 s
	RetryBase       time.Duration                                     // 1 s, doubled per attempt
	Retries         int                                               // 6
	Reserve         func(diskTotal uint64) uint64                     // max(20 GiB, 10 %)
	DiskSpace       func(path string) (free, total uint64, err error) // hw.DiskSpace
	Sleep           func(ctx context.Context, d time.Duration) error
}

type job struct {
	modelID  string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	finished chan struct{}

	mu      sync.Mutex
	kind    string // approval | download
	state   string // running | awaiting_click | paused
	total   int64
	waiters []func(Outcome)
	pause   context.CancelCauseFunc // cancels the transfer in flight, not the job

	done atomic.Int64
}

// Manager is the model folder.
type Manager struct {
	o   Options
	log *slog.Logger

	mu               sync.Mutex
	dir              string
	index            map[string]*Entry
	jobs             map[string]*job
	paused           bool
	wake             chan struct{} // closed and replaced when paused changes
	slot             chan struct{} // one download at a time
	inspect          sync.Mutex
	recent           []time.Time            // inspect starts in the last minute
	hashed           map[string]bool        // files whose SHA-256 this process has checked, by path, hash, size and modified time
	hashLocks        map[string]*sync.Mutex // one hash at a time per file
	testBeforeHash   func()                 // tests only
	testDuringImport func()                 // tests only
	importing        bool
	secrets          sync.Mutex // one read-modify-write at a time of the approvals and hashes
}

var (
	errPaused    = errors.New("paused")
	errCancelled = errors.New("cancelled")
	errShutdown  = errors.New("shutdown")
)

// New opens the model folder, reads its index, and restarts downloads that were running.
func New(o Options) (*Manager, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Approver == nil {
		o.Approver = DenyAll{}
	}
	if o.ApprovalTimeout <= 0 {
		o.ApprovalTimeout = 10 * time.Minute
	}
	if o.ProgressEvery <= 0 {
		o.ProgressEvery = time.Second
	}
	if o.RetryBase <= 0 {
		o.RetryBase = time.Second
	}
	if o.Retries <= 0 {
		o.Retries = 6
	}
	if o.Reserve == nil {
		o.Reserve = func(total uint64) uint64 { return max(20<<30, total/10) }
	}
	if o.DiskSpace == nil {
		o.DiskSpace = hw.DiskSpace
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if o.OnChange == nil {
		o.OnChange = func(bool) {}
	}
	m := &Manager{o: o, log: o.Log, hashed: map[string]bool{}, hashLocks: map[string]*sync.Mutex{}, jobs: map[string]*job{}, wake: make(chan struct{}), slot: make(chan struct{}, 1)}
	if err := m.open(o.Dir); err != nil {
		return nil, err
	}
	m.settleUnrecorded()
	return m, nil
}

// open switches to dir and loads its index.
func (m *Manager) open(dir string) error {
	abs, err := CheckFolder(dir)
	if err != nil {
		return err
	}
	idx, err := loadIndex(abs)
	if err != nil {
		return err
	}
	sweepOldPartials(abs, 7*24*time.Hour)
	m.mu.Lock()
	m.dir, m.index = abs, idx
	var restart []*Entry
	for _, e := range idx {
		if e.imported() {
			if e.State == StateReady || e.State == StateMissing {
				e.State = diskState(abs, e)
			}
			continue
		}
		switch e.State {
		case StateReady:
			if !filesPresent(abs, e) {
				e.State = StateMissing
			}
		case StateMissing:
			if filesPresent(abs, e) {
				e.State = StateReady
			}
		case StateDownloading, StateAwaiting:
			restart = append(restart, e)
		}
	}
	m.mu.Unlock()
	for _, e := range restart {
		m.start(e, nil)
	}
	return nil
}

// Dir is the model folder in use.
func (m *Manager) Dir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dir
}

// SetDir moves to another model folder: the new folder is re-indexed and the old files are left
// where they are (ruling B17). Refused while a download is running.
func (m *Manager) SetDir(dir string) error {
	m.mu.Lock()
	busy, importing := len(m.jobs) > 0, m.importing
	m.mu.Unlock()
	if busy {
		return errors.New("a download is running; wait for it or cancel it first")
	}
	if importing {
		return errors.New("model files are being added; wait for that or cancel it first")
	}
	if err := m.open(dir); err != nil {
		return err
	}
	m.settleUnrecorded()
	m.o.OnChange(true)
	return nil
}

// Models lists the index for hello and status.
func (m *Manager) Models() []protocol.LocalModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.LocalModel, 0, len(m.index))
	for _, e := range m.index {
		lm := protocol.LocalModel{ModelID: e.ModelID, Repo: e.Repo, Commit: e.Commit, Kind: e.Kind, State: e.State}
		var size int64
		for _, f := range e.Files {
			lm.Files = append(lm.Files, f)
			size += f.Size
		}
		if len(e.Files) > 0 {
			lm.SizeBytes = &size
		}
		out = append(out, lm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

// Jobs lists the running jobs for hello and status.
func (m *Manager) Jobs() []protocol.Job {
	m.mu.Lock()
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	out := make([]protocol.Job, 0, len(jobs))
	for _, j := range jobs {
		j.mu.Lock()
		pj := protocol.Job{ModelID: j.modelID, Kind: j.kind, State: j.state}
		if j.kind == "approval" {
			t := true
			pj.AwaitingClick = &t
		} else if j.total > 0 {
			done, total := j.done.Load(), j.total
			pct := float64(int64(float64(done)/float64(total)*1000)) / 10
			pj.Pct, pj.BytesDone, pj.BytesTotal = &pct, &done, &total
		}
		j.mu.Unlock()
		out = append(out, pj)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ModelID < out[k].ModelID })
	return out
}

// Busy reports whether any job is running.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs) > 0
}

func sameRequest(e *Entry, a *protocol.DownloadArgs) bool {
	if e.Repo != a.Repo || e.Commit != a.Commit || e.Kind != a.Kind || len(e.Names) != len(a.Files) {
		return false
	}
	for i := range a.Files {
		if e.Names[i] != a.Files[i] {
			return false
		}
	}
	return true
}

// Download starts a download, or attaches to the running job for the same model id (ruling B3).
// onDone is called exactly once, possibly before Download returns.
func (m *Manager) Download(a *protocol.DownloadArgs, onDone func(Outcome)) {
	if !protocol.ValidModelID(a.ModelID) || !protocol.ValidRepo(a.Repo) || len(a.Files) < 1 || len(a.Files) > 16 {
		onDone(Outcome{Err: errf(protocol.ErrBadArgs, "invalid download")})
		return
	}
	if reservedRepo(a.Repo) {
		onDone(Outcome{Err: &Error{Code: protocol.ErrDenied, Field: "repo", Message: "this is a file the host added on the PC; only the host can add it again"}})
		return
	}
	for _, f := range a.Files {
		if !protocol.ValidGGUFFile(f) {
			onDone(Outcome{Err: &Error{Code: protocol.ErrBadArgs, Field: "files", Message: "invalid file name"}})
			return
		}
	}
	m.mu.Lock()
	e := m.index[a.ModelID]
	if j := m.jobs[a.ModelID]; j != nil {
		same := e != nil && sameRequest(e, a)
		if same {
			j.mu.Lock()
			j.waiters = append(j.waiters, onDone)
			j.mu.Unlock()
		}
		m.mu.Unlock()
		if !same {
			onDone(Outcome{Err: errf(protocol.ErrBusy, "another download is running under this model id")})
		}
		return
	}
	if e != nil {
		if e.Repo != a.Repo {
			m.mu.Unlock()
			onDone(Outcome{Err: &Error{Code: protocol.ErrBadArgs, Field: "model_id", Message: "this model id belongs to another repo"}})
			return
		}
		if sameRequest(e, a) && e.State == StateReady && m.trusted(e) && m.recorded(e) {
			res := resultOf(e)
			m.mu.Unlock()
			onDone(Outcome{Result: res})
			return
		}
	}
	// New, a retry (denied, error, missing), or the same repo at another commit (replaced in place).
	var old *Entry
	if e != nil && !sameRequest(e, a) {
		old = e
	}
	ne := &Entry{ModelID: a.ModelID, Repo: a.Repo, Commit: a.Commit, Kind: a.Kind, Names: append([]string(nil), a.Files...), State: StateDownloading}
	if e != nil && sameRequest(e, a) {
		ne.Files, ne.ETags = e.Files, e.ETags
	}
	m.index[a.ModelID] = ne
	m.saveLocked()
	m.mu.Unlock()
	j := m.start(ne, onDone)
	if old != nil {
		go func() { // the replaced commit's files go once the new one is ready
			<-j.finished
			m.mu.Lock()
			ready := m.index[a.ModelID] == ne && ne.State == StateReady
			dir := m.dir
			m.mu.Unlock()
			if ready {
				removeFiles(dir, old, m.log)
			}
		}()
	}
}

func resultOf(e *Entry) map[string]any {
	var size int64
	for _, f := range e.Files {
		size += f.Size
	}
	return map[string]any{"model_id": e.ModelID, "size_bytes": size}
}

func (m *Manager) start(e *Entry, onDone func(Outcome)) *job {
	ctx, cancel := context.WithCancelCause(context.Background())
	j := &job{modelID: e.ModelID, ctx: ctx, cancel: cancel, finished: make(chan struct{}), kind: "download", state: "running"}
	if onDone != nil {
		j.waiters = append(j.waiters, onDone)
	}
	m.mu.Lock()
	m.jobs[e.ModelID] = j
	m.mu.Unlock()
	go m.run(j, e)
	m.o.OnChange(true)
	return j
}

// Cancel stops the job of a model id and removes what it wrote.
func (m *Manager) Cancel(modelID string) bool {
	m.mu.Lock()
	j := m.jobs[modelID]
	m.mu.Unlock()
	if j == nil {
		return false
	}
	j.cancel(errCancelled)
	<-j.finished
	return true
}

// Close stops every job without deleting anything: partial files and the index stay, and the jobs
// start again when the agent does.
func (m *Manager) Close() {
	m.mu.Lock()
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.cancel(errShutdown)
	}
	for _, j := range jobs {
		<-j.finished
	}
}

// SetPaused pauses or resumes every download (the host's "Pause hosting"). Partial files stay.
func (m *Manager) SetPaused(p bool) {
	m.mu.Lock()
	if m.paused == p {
		m.mu.Unlock()
		return
	}
	m.paused = p
	close(m.wake)
	m.wake = make(chan struct{})
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	if p {
		for _, j := range jobs {
			j.mu.Lock()
			if j.pause != nil {
				j.pause(errPaused)
			}
			j.mu.Unlock()
		}
	}
}

// Delete removes a model's files and its index entry. Only files the index lists are touched.
func (m *Manager) Delete(modelID string, loaded bool) *Error {
	m.mu.Lock()
	e := m.index[modelID]
	j := m.jobs[modelID]
	m.mu.Unlock()
	if e == nil {
		return errf(protocol.ErrNotFound, "no such model on this PC")
	}
	if loaded {
		return errf(protocol.ErrBusy, "the model is loaded; unload it first")
	}
	if j != nil {
		j.cancel(errCancelled)
		<-j.finished
	}
	m.mu.Lock()
	if cur := m.index[modelID]; cur != nil {
		e = cur
	}
	delete(m.index, modelID)
	dir := m.dir
	m.saveLocked()
	m.mu.Unlock()
	if e.imported() { // forgets it; the file stays, and a later scan may offer it again
		if err := m.forget(e); err != nil {
			m.log.Warn("the records of a forgotten model could not be removed", "model_id", modelID, "err", err.Error())
		}
		m.log.Info("model forgotten; its file stays", "model_id", modelID)
		m.o.OnChange(true)
		return nil
	}
	removeFiles(dir, e, m.log)
	m.log.Info("model deleted", "model_id", modelID)
	m.o.OnChange(true)
	return nil
}

// Loadable is a model ready for llama-server.
type Loadable struct {
	Path   string
	Size   int64
	Header *gguf.Header
}

// Resolve returns the file to load for a model id after the GGUF pre-check (§9.2).
func (m *Manager) Resolve(modelID string) (*Loadable, *Error) {
	m.refreshImported()
	m.mu.Lock()
	e := m.index[modelID]
	dir := m.dir
	var path, state, kind string
	var files []protocol.LocalFile
	if e != nil && len(e.Files) > 0 {
		path, state, kind = e.path(dir, e.Files[0].Path), e.State, e.Kind
		files = append(files, e.Files...)
	}
	m.mu.Unlock()
	switch {
	case e == nil || path == "":
		return nil, &Error{Code: protocol.ErrNotFound, Field: "model_id", Message: "model not on this PC"}
	case state != StateReady:
		return nil, &Error{Code: protocol.ErrNotFound, Field: "model_id", Message: "model is not ready (" + state + ")"}
	case kind != "gguf":
		return nil, errf(protocol.ErrUnsupported, "a projector cannot be loaded on its own")
	}
	if !m.trusted(e) {
		m.log.Warn("load refused: neither an allowlisted publisher nor a stored approval", "model_id", modelID)
		if e.imported() {
			return nil, errf(protocol.ErrDenied, "this PC's host did not add this file; only the host can, on the PC")
		}
		return nil, errf(protocol.ErrDenied, "this PC's host did not allow this model; download it again to be asked")
	}
	if !m.recorded(e) {
		m.log.Warn("load refused: this agent has no record of downloading this model", "model_id", modelID)
		if e.imported() {
			return nil, errf(protocol.ErrDenied, "this PC has no record of this file; only the host can add it again, on the PC")
		}
		return nil, errf(protocol.ErrDenied, "this PC has no record of downloading this model; download it again")
	}
	if err := noReparse(dir, filepath.Dir(path)); err != nil {
		return nil, errf(protocol.ErrEngineFailed, "%v", err)
	}
	if herr := m.checkHashes(e, dir, files); herr != nil {
		return nil, herr
	}
	h, err := checkFile(path)
	if err != nil {
		m.log.Warn("pre-load check failed", "model_id", modelID, "err", err.Error())
		return nil, errf(protocol.ErrEngineFailed, "the model file failed the GGUF check")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, errf(protocol.ErrEngineFailed, "the model file cannot be read")
	}
	return &Loadable{Path: path, Size: st.Size(), Header: h}, nil
}

// checkHashes hashes a model's files on their first load in this process (finding M3). The index says
// what the hash must be; the file on disk may have changed since it was downloaded.
func (m *Manager) checkHashes(e *Entry, dir string, files []protocol.LocalFile) *Error {
	for _, f := range files {
		path := e.path(dir, f.Path)
		// One hash at a time per file: a second load of it waits and then finds the result.
		m.mu.Lock()
		lock := m.hashLocks[path]
		if lock == nil {
			lock = &sync.Mutex{}
			m.hashLocks[path] = lock
		}
		m.mu.Unlock()
		lock.Lock()
		herr := m.checkHash(e, path, f)
		lock.Unlock()
		if herr != nil {
			return herr
		}
	}
	return nil
}

// checkHash hashes one file unless this process already did, for this size and modified time.
func (m *Manager) checkHash(e *Entry, path string, f protocol.LocalFile) *Error {
	st, serr := os.Stat(path)
	key := ""
	if serr == nil && hasSecondName(path) {
		// The file could be changed through the other name with its time put back, which the cache
		// below would not see. Asked at every load, before the cache.
		m.mu.Lock()
		if m.index[e.ModelID] == e {
			e.State = StateError
			if e.imported() {
				e.State = StateChanged
			}
			m.saveLocked()
		}
		m.mu.Unlock()
		m.log.Error("load refused: a model file has a second name (a hard link)", "model_id", e.ModelID)
		m.o.OnChange(true)
		return errf(protocol.ErrDenied, "a model file has a second name on this PC (a hard link), so it could change unseen; only the host can remove that link")
	}
	if serr == nil {
		key = fmt.Sprintf("%s|%s|%d|%d", path, f.SHA256, st.Size(), st.ModTime().UnixNano())
		m.mu.Lock()
		done := m.hashed[key]
		m.mu.Unlock()
		if done {
			return nil
		}
	}
	if m.testBeforeHash != nil {
		m.testBeforeHash()
	}
	t0 := time.Now()
	sum, err := hashFile(context.Background(), path)
	if serr != nil || err != nil || sum != f.SHA256 {
		m.mu.Lock()
		if m.index[e.ModelID] == e {
			e.State = StateError
			if e.imported() {
				e.State = StateChanged
				if errors.Is(serr, os.ErrNotExist) {
					e.State = StateMissing
				}
			}
			m.saveLocked()
		}
		m.mu.Unlock()
		m.log.Error("load refused: a model file changed since it was downloaded", "model_id", e.ModelID)
		m.o.OnChange(true)
		if e.imported() {
			return errf(protocol.ErrHashMismatch, "a file the host added has changed; only the host can add it again, on the PC")
		}
		return errf(protocol.ErrHashMismatch, "a model file does not match its SHA-256; download it again")
	}
	m.log.Info("model file hashed before its first load", "model_id", e.ModelID, "ms", time.Since(t0).Milliseconds())
	m.mu.Lock()
	m.hashed[key] = true
	m.mu.Unlock()
	return nil
}

// hasSecondName reports a file with more than one name. A file that cannot be opened has none to
// report: the hash that follows fails on it.
func hasSecondName(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	n, err := confine.Links(f)
	return err == nil && n != 1
}

// checkFile is the GGUF pre-check: magic, version, caps, and every tensor inside the file.
func checkFile(path string) (*gguf.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	h, err := gguf.Parse(f, gguf.MaxHeaderBytes, time.Now().Add(2*time.Second))
	if err != nil {
		return nil, err
	}
	return h, h.CheckLayout(uint64(st.Size()))
}

// CheckFolder validates a model folder the host chose and returns its absolute path: a local,
// writable directory that is not itself a link.
func CheckFolder(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("no folder given")
	}
	if strings.HasPrefix(dir, `\\`) || strings.HasPrefix(dir, "//") {
		return "", errors.New("network folders are not supported; choose a folder on a local drive")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("the folder cannot be created: %w", err)
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("the model folder must be a real folder, not a link")
	}
	if err := noReparse(abs, abs); err != nil {
		return "", err
	}
	probe, err := os.CreateTemp(abs, ".cr-agent-write-test-*")
	if err != nil {
		return "", errors.New("the folder is not writable")
	}
	probe.Close()
	os.Remove(probe.Name())
	return abs, nil
}

// sweepOldPartials removes .part files nobody resumed for maxAge (§12.4), only where downloads write:
// <owner>\\<repo>\\<commit[:12]>, and one folder below it. The rest of the model folder is the host's,
// and a file there called .gguf.part is the host's too (ruling B64). No link is followed.
func sweepOldPartials(root string, maxAge time.Duration) {
	cut := time.Now().Add(-maxAge)
	folders := func(dir string) []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		for _, d := range ents {
			if d.Type()&os.ModeType == os.ModeDir {
				out = append(out, d.Name())
			}
		}
		return out
	}
	sweep := func(dir string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range ents {
			if d.Type()&os.ModeType != 0 || !strings.HasSuffix(d.Name(), ".gguf.part") {
				continue
			}
			if info, err := d.Info(); err == nil && info.ModTime().Before(cut) {
				os.Remove(filepath.Join(dir, d.Name()))
			}
		}
	}
	for _, owner := range folders(root) {
		for _, repo := range folders(filepath.Join(root, owner)) {
			if id := owner + "/" + repo; !protocol.ValidRepo(id) || reservedRepo(id) {
				continue
			}
			for _, commit := range folders(filepath.Join(root, owner, repo)) {
				if !reCommitDir.MatchString(commit) {
					continue
				}
				dir := filepath.Join(root, owner, repo, commit)
				sweep(dir)
				for _, sub := range folders(dir) {
					sweep(filepath.Join(dir, sub))
				}
			}
		}
	}
}
