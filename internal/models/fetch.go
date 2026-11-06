package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

// run is one job from lookup to ready.
func (m *Manager) run(j *job, e *Entry) {
	out := m.work(j, e)
	if out.Err != nil && context.Cause(j.ctx) == errCancelled {
		out.Err = errf(protocol.ErrCancelled, "cancelled")
	}
	shutdown := out.Err != nil && context.Cause(j.ctx) == errShutdown

	m.mu.Lock()
	dir := m.dir
	switch {
	case shutdown: // the agent is closing: the entry stays as it is and the job restarts at the next start
		out.Err = errf(protocol.ErrCancelled, "the agent is closing")
	case out.Err == nil:
		e.State, e.ETags = StateReady, nil
	case out.Err.Code == protocol.ErrCancelled:
		if m.index[e.ModelID] == e {
			delete(m.index, e.ModelID)
		}
	case out.Err.Code == protocol.ErrDenied:
		e.State = StateDenied
	default:
		e.State = StateError
	}
	delete(m.jobs, e.ModelID)
	m.saveLocked()
	m.mu.Unlock()

	if out.Err != nil && !shutdown && (out.Err.Code == protocol.ErrCancelled || out.Err.Code == protocol.ErrHashMismatch) {
		removeFiles(dir, e, m.log)
	}
	if out.Err != nil {
		m.log.Warn("download ended", "model_id", e.ModelID, "code", out.Err.Code, "why", out.Err.Message)
	} else {
		m.log.Info("download ready", "model_id", e.ModelID, "repo", e.Repo, "bytes", j.total)
	}
	j.mu.Lock()
	waiters := j.waiters
	j.waiters = nil
	j.mu.Unlock()
	close(j.finished)
	for _, w := range waiters {
		w(out)
	}
	m.o.OnChange(true)
}

func (m *Manager) setJob(j *job, kind, state string) {
	j.mu.Lock()
	changed := j.kind != kind || j.state != state
	j.kind, j.state = kind, state
	j.mu.Unlock()
	if changed {
		m.o.OnChange(false)
	}
}

func hfError(err error, what string) *Error {
	var se *hf.StatusError
	switch {
	case errors.As(err, &se) && se.Code == 404:
		return errf(protocol.ErrNotFound, "%s not found on Hugging Face", what)
	case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
		return errf(protocol.ErrDenied, "Hugging Face refused access to %s: the repo is gated or private; accept its licence and set a token in the agent", what)
	default:
		return errf(protocol.ErrDownloadFailed, "Hugging Face lookup failed for %s", what)
	}
}

func (m *Manager) work(j *job, e *Entry) Outcome {
	ctx := j.ctx
	// 1. The agent's own lookup: the id as Hugging Face spells it, the commit, sizes and hashes.
	rev, err := m.o.HF.Revision(ctx, e.Repo, e.Commit)
	if err != nil {
		return Outcome{Err: hfError(err, "the repo")}
	}
	if rev.ID != e.Repo || rev.SHA != e.Commit {
		return Outcome{Err: errf(protocol.ErrNotFound, "repo or commit does not match Hugging Face's own id")}
	}
	tree, err := m.o.HF.Tree(ctx, e.Repo, e.Commit)
	if err != nil {
		return Outcome{Err: hfError(err, "the file listing")}
	}
	files := make([]protocol.LocalFile, 0, len(e.Names))
	var total int64
	for _, n := range e.Names {
		f, ok := tree[n]
		switch {
		case !ok:
			return Outcome{Err: &Error{Code: protocol.ErrNotFound, Field: "files", Message: "a file is not in the repo at this commit"}}
		case f.SHA256 == "" || f.Size <= 0 || f.Size > 1<<40:
			return Outcome{Err: &Error{Code: protocol.ErrUnsupported, Field: "files", Message: "Hugging Face lists no SHA-256 for a file"}}
		}
		files = append(files, protocol.LocalFile{Path: n, Size: f.Size, SHA256: f.SHA256})
		total += f.Size
	}
	m.mu.Lock()
	e.Files = files
	m.saveLocked()
	m.mu.Unlock()
	j.mu.Lock()
	j.total = total
	j.mu.Unlock()

	// 2. Was it the host's choice? (§9.2, rulings B13, B14)
	owner, _, _ := strings.Cut(e.Repo, "/")
	prompt := Prompt{ModelID: e.ModelID, Repo: e.Repo, Commit: e.Commit, Files: e.Names, SizeBytes: total}
	if Allowlisted(owner) || m.approved(e.Repo, e.Commit, e.Names) {
		if m.o.Started != nil {
			m.o.Started(prompt)
		}
	} else {
		prompt.AllowedBefore = m.repoAllowedBefore(e.Repo)
		prompt.Deadline = time.Now().Add(m.o.ApprovalTimeout)
		m.mu.Lock()
		e.State = StateAwaiting
		m.saveLocked()
		m.mu.Unlock()
		m.setJob(j, "approval", "awaiting_click")
		m.o.OnChange(true)
		actx, cancel := context.WithDeadline(ctx, prompt.Deadline)
		ok := m.o.Approver.Ask(actx, prompt)
		cancel()
		if ctx.Err() != nil {
			return Outcome{Err: errf(protocol.ErrCancelled, "cancelled")}
		}
		if !ok {
			return Outcome{Err: errf(protocol.ErrDenied, "the host did not allow this download on the PC")}
		}
		if err := m.remember(e.Repo, e.Commit, e.Names); err != nil {
			m.log.Warn("approval not remembered", "err", err.Error())
		}
		m.mu.Lock()
		e.State = StateDownloading
		m.saveLocked()
		m.mu.Unlock()
		m.o.OnChange(true)
	}

	// 3. Transfer, one job at a time, pausable.
	for {
		m.mu.Lock()
		paused, wake := m.paused, m.wake
		m.mu.Unlock()
		if paused {
			m.setJob(j, "download", "paused")
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return Outcome{Err: errf(protocol.ErrCancelled, "cancelled")}
			}
		}
		m.setJob(j, "download", "running")
		select {
		case m.slot <- struct{}{}:
		case <-wake:
			continue
		case <-ctx.Done():
			return Outcome{Err: errf(protocol.ErrCancelled, "cancelled")}
		}
		tctx, pause := context.WithCancelCause(ctx)
		j.mu.Lock()
		j.pause = pause
		j.mu.Unlock()
		m.mu.Lock()
		if m.paused { // paused between the check and the slot
			pause(errPaused)
		}
		m.mu.Unlock()
		ferr := m.fetchAll(tctx, j, e)
		j.mu.Lock()
		j.pause = nil
		j.mu.Unlock()
		pausedNow := context.Cause(tctx) == errPaused
		pause(nil)
		<-m.slot
		switch {
		case ferr == nil:
			return Outcome{Result: resultOf(e)}
		case ctx.Err() != nil:
			return Outcome{Err: errf(protocol.ErrCancelled, "cancelled")}
		case pausedNow:
			continue
		default:
			return Outcome{Err: ferr}
		}
	}
}

// transient marks an error worth a retry with resume.
type transient struct{ error }

func (m *Manager) fetchAll(ctx context.Context, j *job, e *Entry) *Error {
	m.mu.Lock()
	root := m.dir
	m.mu.Unlock()

	// Progress ticks while bytes move.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(m.o.ProgressEvery)
		defer t.Stop()
		last := int64(-1)
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if d := j.done.Load(); d != last {
					last = d
					m.o.OnChange(false)
				}
			}
		}
	}()

	var base int64
	for _, f := range e.Files {
		var err error
		for attempt := 0; ; attempt++ {
			err = m.fetchFile(ctx, j, e, root, f, base)
			var tr transient
			if err == nil || !errors.As(err, &tr) || ctx.Err() != nil {
				break
			}
			if attempt+1 >= m.o.Retries {
				err = errf(protocol.ErrDownloadFailed, "the download kept failing; the partial file is kept for a later retry")
				break
			}
			m.log.Info("download interrupted; retrying with resume", "model_id", e.ModelID, "attempt", attempt+1)
			if m.o.Sleep(ctx, m.o.RetryBase<<attempt) != nil {
				break
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return errf(protocol.ErrCancelled, "cancelled")
			}
			var me *Error
			if errors.As(err, &me) {
				return me
			}
			return errf(protocol.ErrDownloadFailed, "%v", err)
		}
		base += f.Size
		j.done.Store(base)
	}
	// The files are in place and hashed; the GGUF pre-check is the last gate before "ready".
	for _, f := range e.Files {
		if _, err := checkFile(e.path(root, f.Path)); err != nil {
			removeFiles(root, e, m.log)
			return errf(protocol.ErrUnsupported, "a downloaded file is not a valid GGUF file")
		}
	}
	return nil
}

// spaceFor checks the disk reserve: free ≥ what is still to come + max(20 GiB, 10 %).
func (m *Manager) spaceFor(root string, remaining int64) *Error {
	free, total, err := m.o.DiskSpace(root)
	if err != nil {
		return nil // no probe on this platform: the write itself will fail if the disk fills
	}
	need := uint64(remaining) + m.o.Reserve(total)
	if free < need {
		n, f := int64(need>>20), int64(free>>20)
		return &Error{Code: protocol.ErrNoSpace, Message: "not enough free disk space for this download and the reserve", NeedMB: &n, FreeMB: &f}
	}
	return nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func (m *Manager) fetchFile(ctx context.Context, j *job, e *Entry, root string, f protocol.LocalFile, base int64) error {
	final := e.path(root, f.Path)
	part := final + ".part"
	dir := filepath.Dir(final)
	if err := noReparse(root, dir); err != nil {
		return errf(protocol.ErrDownloadFailed, "%v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errf(protocol.ErrDownloadFailed, "the model folder cannot be written")
	}
	if err := noReparse(root, dir); err != nil {
		return errf(protocol.ErrDownloadFailed, "%v", err)
	}

	// Already in place (a retry after a later file failed, or a lost index): trust it only by its hash.
	if st, err := os.Lstat(final); err == nil {
		if st.Mode().IsRegular() && st.Size() == f.Size {
			if sum, err := hashFile(ctx, final); err == nil && sum == f.SHA256 {
				j.done.Store(base + f.Size)
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := os.Remove(final); err != nil {
			return errf(protocol.ErrDownloadFailed, "an older file is in the way and cannot be removed")
		}
	}

	fh, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return errf(protocol.ErrDownloadFailed, "the partial file cannot be opened")
	}
	defer fh.Close()
	if err := underRoot(fh, root); err != nil {
		return errf(protocol.ErrDownloadFailed, "%v", err)
	}
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return errf(protocol.ErrDownloadFailed, "the partial file is not a regular file")
	}
	have := st.Size()
	if have > f.Size {
		have = 0
	}
	h := sha256.New()
	if have > 0 { // resume: rebuild the running hash from what is on disk
		if _, err := io.CopyN(h, &ctxReader{ctx: ctx, r: fh}, have); err != nil {
			return transient{err}
		}
	}
	if err := fh.Truncate(have); err != nil {
		return errf(protocol.ErrDownloadFailed, "the partial file cannot be resized")
	}
	if _, err := fh.Seek(have, io.SeekStart); err != nil {
		return errf(protocol.ErrDownloadFailed, "the partial file cannot be positioned")
	}
	j.done.Store(base + have)
	if perr := m.spaceFor(root, f.Size-have); perr != nil {
		return perr
	}

	if have < f.Size {
		m.mu.Lock()
		etag := e.ETags[f.Path]
		m.mu.Unlock()
		body, err := m.o.HF.Open(ctx, e.Repo, e.Commit, f.Path, have, 0, etag)
		if err != nil {
			var se *hf.StatusError
			switch {
			case errors.Is(err, hf.ErrRedirect):
				return errf(protocol.ErrDownloadFailed, "Hugging Face redirected the download to a host that is not allowed")
			case errors.As(err, &se) && se.Code == 404:
				return errf(protocol.ErrNotFound, "a file is not on Hugging Face at this commit")
			case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
				return errf(protocol.ErrDenied, "Hugging Face refused the download: the repo is gated or private; accept its licence and set a token in the agent")
			case errors.As(err, &se) && se.Code == 416:
				fh.Truncate(0)
				return transient{err}
			}
			return transient{err}
		}
		defer body.Close()
		if body.Total >= 0 && body.Total != f.Size {
			return errf(protocol.ErrHashMismatch, "the file's size differs from Hugging Face's listing")
		}
		if body.Offset == 0 && have > 0 { // the server started over (stale validator or no Range support)
			if err := fh.Truncate(0); err != nil {
				return errf(protocol.ErrDownloadFailed, "the partial file cannot be reset")
			}
			fh.Seek(0, io.SeekStart)
			h.Reset()
			have = 0
			j.done.Store(base)
		}
		if body.ETag != "" && body.ETag != etag {
			m.mu.Lock()
			if e.ETags == nil {
				e.ETags = map[string]string{}
			}
			e.ETags[f.Path] = body.ETag
			m.saveLocked()
			m.mu.Unlock()
		}

		buf := make([]byte, 1<<20)
		var sinceCheck int64
		for have < f.Size {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := body.Read(buf)
			if n > 0 {
				if have+int64(n) > f.Size {
					return errf(protocol.ErrHashMismatch, "the server sent more bytes than Hugging Face lists")
				}
				if _, werr := fh.Write(buf[:n]); werr != nil {
					return errf(protocol.ErrNoSpace, "writing the file failed (disk full?)")
				}
				h.Write(buf[:n])
				have += int64(n)
				j.done.Store(base + have)
				if sinceCheck += int64(n); sinceCheck >= 1<<30 { // re-check the reserve every GiB
					sinceCheck = 0
					if perr := m.spaceFor(root, f.Size-have); perr != nil {
						return perr
					}
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return transient{rerr}
			}
		}
		if have < f.Size {
			return transient{errors.New("short body")}
		}
	}

	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		fh.Close()
		os.Remove(part)
		return errf(protocol.ErrHashMismatch, "the downloaded file does not match Hugging Face's SHA-256")
	}
	if err := fh.Sync(); err != nil {
		return errf(protocol.ErrDownloadFailed, "the file could not be flushed to disk")
	}
	if err := fh.Close(); err != nil {
		return errf(protocol.ErrDownloadFailed, "the file could not be closed")
	}
	// Only now does the file get its real name: load never sees an unverified file.
	if err := os.Rename(part, final); err != nil {
		return errf(protocol.ErrDownloadFailed, "the verified file could not be moved into place")
	}
	return nil
}

// approvals is the DPAPI-protected list of files the host allowed: one row per (repo, commit, file),
// exactly what the dialog showed (finding M2). Allowing a file of a commit says nothing about the
// repo's other files.
type approvals struct {
	Allowed []approval `json:"allowed"`
}

type approval struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	File   string `json:"file"`
}

func (m *Manager) loadApprovals() approvals {
	var a approvals
	if m.o.Store == nil {
		return a
	}
	b, err := m.o.Store.ReadSecret(store.SecretApprovals)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("approvals unreadable; asking again", "err", err.Error())
		}
		return a
	}
	_ = json.Unmarshal(b, &a)
	return a
}

// approved reports whether the host allowed every one of these files of this commit.
func (m *Manager) approved(repo, commit string, files []string) bool {
	if len(files) == 0 {
		return false
	}
	allowed := m.loadApprovals().Allowed
	for _, f := range files {
		found := false
		for _, x := range allowed {
			if x.Repo == repo && x.Commit == commit && x.File == f {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// trusted is the rule for a model to be used at all: its publisher is on the shipped allowlist, or
// the host allowed exactly these files (finding M3). An entry that only the index names is neither.
func (m *Manager) trusted(e *Entry) bool {
	owner, _, _ := strings.Cut(e.Repo, "/")
	return Allowlisted(owner) || m.approved(e.Repo, e.Commit, e.Names)
}

func (m *Manager) repoAllowedBefore(repo string) bool {
	for _, x := range m.loadApprovals().Allowed {
		if x.Repo == repo {
			return true
		}
	}
	return false
}

func (m *Manager) remember(repo, commit string, files []string) error {
	if m.o.Store == nil {
		return errors.New("no store")
	}
	a := m.loadApprovals()
	for _, f := range files {
		a.Allowed = append(a.Allowed, approval{Repo: repo, Commit: commit, File: f})
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return m.o.Store.WriteSecret(store.SecretApprovals, b)
}
