package models

// Files the host put in the model folder themselves (ruling B64). Only the host's click in the
// agent's own dialog brings one in: nothing here is reachable from a hub frame, and a file that only
// lies in the folder gets nothing. An import runs the checks of a download and ends in the same two
// DPAPI records, so Resolve treats it like any other model and the first-load hash still applies.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/confine"
	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

// LocalOwner is the owner of an imported model's repo. It is reserved: nothing is ever fetched for it.
const LocalOwner = "local-file"

// maxScan bounds one scan: a folder full of dropped files must not keep the agent reading headers.
const maxScan = 256

// StateChanged is an imported file that is no longer what the host approved. It is never fetched
// again: the host imports it again to use it.
const StateChanged = "changed"

// reservedRepo reports a repo of the reserved owner, in any spelling: Windows folders ignore case.
func reservedRepo(repo string) bool {
	owner, _, _ := strings.Cut(repo, "/")
	return strings.EqualFold(owner, LocalOwner)
}

func (e *Entry) imported() bool { return strings.HasPrefix(e.Repo, LocalOwner+"/") }

// validImported is what an imported entry read from the index must also satisfy.
func (e *Entry) validImported() bool {
	if !e.imported() || e.Kind != "gguf" || len(e.Files) == 0 {
		return false
	}
	switch e.State {
	case StateReady, StateMissing, StateChanged, StateError:
	default:
		return false // nothing is ever downloaded for it
	}
	return e.Commit == importCommit(e.Files)
}

// importCommit is the first 40 hex of the file's SHA-256. A multi-part set takes it from the list of
// its parts' hashes, so that changing any part changes it.
func importCommit(files []protocol.LocalFile) string {
	if len(files) == 1 {
		return files[0].SHA256[:40]
	}
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.SHA256 + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:40]
}

// Found is a file, or a complete multi-part set, the host can choose to add.
type Found struct {
	Key   string   // the first file's path in the folder, slash-separated: what Import takes
	Files []string // every part, in order
	Size  int64
	Seen  []Stamp // each part as the scan saw it
}

// Stamp is a file's size and modified time at the moment it was looked at.
type Stamp struct {
	Size    int64
	ModTime time.Time
}

func (s Stamp) same(st os.FileInfo) bool { return s.Size == st.Size() && s.ModTime.Equal(st.ModTime()) }

// Picks turns the keys chosen in the dialog into the rows the dialog showed.
func (s Scan) Picks(keys []string) []Found {
	out := make([]Found, 0, len(keys))
	for _, k := range keys {
		for _, f := range s.Found {
			if f.Key == k {
				out = append(out, f)
			}
		}
	}
	return out
}

// Refused is a file the dialog shows as not usable, and why. The name is the file's own: it is
// shown to the host, never sent to the hub.
type Refused struct {
	Name string
	Why  string
}

// Scan is what a look through the model folder found.
type Scan struct {
	Dir     string
	Found   []Found
	Refused []Refused
}

var (
	reSplit  = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)
	reNotID  = regexp.MustCompile(`[^a-z0-9._-]+`)
	reDots   = regexp.MustCompile(`\.{2,}`)
	errName  = errors.New("its name has characters the hub cannot carry; rename it using only letters, digits and . _ -")
	errLink  = errors.New("it is a link, not a file in this folder")
	errGGUF  = errors.New("it is not a complete GGUF file (still being copied?)")
	errParts = errors.New("it is one part of a multi-part set, and the other parts are not here")
)

// schemaFile adds to ValidGGUFFile what the schema's ggufFile pattern says of a folder name: at most
// 200 characters. ValidGGUFFile allows 205, which is right for a last segment ending in .gguf.
func schemaFile(rel string) bool {
	if !protocol.ValidGGUFFile(rel) {
		return false
	}
	dir, _, nested := strings.Cut(rel, "/")
	return !nested || len(dir) <= 200
}

// openConfined opens a file of the model folder for reading, with every rule a download has for where
// its file ends up: a name the protocol allows, no link anywhere on the way, a plain file that Windows
// itself says lies under the folder, and no second name for it elsewhere.
func openConfined(root, rel string) (*os.File, os.FileInfo, error) {
	if !schemaFile(rel) {
		return nil, nil, errName
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := noReparse(root, p); err != nil {
		return nil, nil, errLink
	}
	lst, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, errLink
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || !os.SameFile(lst, st)) {
		err = errLink
	}
	if err == nil && underRoot(f, root) != nil {
		err = errLink
	}
	if err == nil {
		if n, lerr := confine.Links(f); lerr != nil || n != 1 {
			err = errLink
		}
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// readHeader is the GGUF check of a download, on an open file.
func readHeader(f *os.File, size int64) (*gguf.Header, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h, err := gguf.Parse(f, gguf.MaxHeaderBytes, time.Now().Add(inspectParseTime))
	if err != nil {
		return nil, errGGUF
	}
	if h.CheckLayout(uint64(size)) != nil {
		return nil, errGGUF
	}
	return h, nil
}

// checkPart opens, checks and closes one file; count is the set's size, or 1 for a file of its own.
func checkPart(root, rel string, count int64) (Stamp, error) {
	f, st, err := openConfined(root, rel)
	if err != nil {
		return Stamp{}, err
	}
	defer f.Close()
	h, err := readHeader(f, st.Size())
	if err != nil {
		return Stamp{}, err
	}
	if max(h.SplitCount, 1) != count {
		return Stamp{}, errParts
	}
	return Stamp{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// Scan looks through the model folder and one level of folders below it. It skips projectors,
// partial downloads and anything the index already holds. It only reads.
func (m *Manager) Scan() Scan {
	m.refreshImported()
	m.mu.Lock()
	root := m.dir
	known := map[string]bool{}
	for _, e := range m.index {
		if e.imported() && e.State != StateReady && e.State != StateMissing {
			continue // changed: the host may add the file again
		}
		for _, n := range e.Names {
			if rel, err := filepath.Rel(root, e.path(root, n)); err == nil {
				known[strings.ToLower(filepath.ToSlash(rel))] = true
			}
		}
	}
	m.mu.Unlock()

	out := Scan{Dir: root}
	var names []string
	add := func(rel string) {
		base := strings.ToLower(path.Base(rel))
		if strings.HasSuffix(base, ".gguf") && !strings.HasPrefix(base, "mmproj") && !known[strings.ToLower(rel)] && len(names) < maxScan {
			names = append(names, rel)
		}
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		m.log.Warn("the model folder cannot be read", "err", err.Error())
		return out
	}
	for _, d := range ents {
		if !d.IsDir() && d.Type()&os.ModeType == 0 {
			add(d.Name())
			continue
		}
		p := filepath.Join(root, d.Name())
		if noReparse(root, p) != nil {
			if strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
				out.Refused = append(out.Refused, Refused{d.Name(), errLink.Error()})
			} else {
				out.Refused = append(out.Refused, Refused{d.Name(), "it is a link to another folder, so nothing in it can be added"})
			}
			continue
		}
		if !d.IsDir() {
			continue
		}
		subs, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, s := range subs {
			if s.IsDir() {
				continue
			}
			if s.Type()&os.ModeType == 0 {
				add(d.Name() + "/" + s.Name())
			} else if strings.HasSuffix(strings.ToLower(s.Name()), ".gguf") {
				out.Refused = append(out.Refused, Refused{d.Name() + "/" + s.Name(), errLink.Error()})
			}
		}
	}

	// One entry per file, or per set of parts.
	type set struct {
		total int64
		parts map[int64]string
	}
	sets := map[string]*set{}
	var singles []string
	for _, rel := range names {
		g := reSplit.FindStringSubmatch(path.Base(rel))
		if g == nil {
			singles = append(singles, rel)
			continue
		}
		var no, total int64
		fmt.Sscan(g[2], &no)
		fmt.Sscan(g[3], &total)
		key := path.Dir(rel) + "/" + g[1] + "/" + g[3]
		s := sets[key]
		if s == nil {
			s = &set{total: total, parts: map[int64]string{}}
			sets[key] = s
		}
		s.parts[no] = rel
	}
	for _, rel := range singles {
		seen, err := checkPart(root, rel, 1)
		if err != nil {
			out.Refused = append(out.Refused, Refused{rel, reason(err)})
			continue
		}
		out.Found = append(out.Found, Found{Key: rel, Files: []string{rel}, Size: seen.Size, Seen: []Stamp{seen}})
	}
	for _, s := range sets {
		var nos []int64
		for no := range s.parts {
			nos = append(nos, no)
		}
		sort.Slice(nos, func(i, j int) bool { return nos[i] < nos[j] })
		first := s.parts[nos[0]]
		if int64(len(nos)) != s.total || nos[0] != 1 || nos[len(nos)-1] != s.total {
			out.Refused = append(out.Refused, Refused{first, fmt.Sprintf("it is part of a set of %d and only %d of the parts are here", s.total, len(nos))})
			continue
		}
		if s.total > 16 {
			out.Refused = append(out.Refused, Refused{first, "it is part of a set of more than 16 files"})
			continue
		}
		f := Found{Key: first}
		var bad error
		for _, no := range nos {
			seen, err := checkPart(root, s.parts[no], s.total)
			if err != nil {
				bad = fmt.Errorf("part %d: %s", no, reason(err))
				break
			}
			f.Files = append(f.Files, s.parts[no])
			f.Seen = append(f.Seen, seen)
			f.Size += seen.Size
		}
		if bad != nil {
			out.Refused = append(out.Refused, Refused{first, bad.Error()})
			continue
		}
		out.Found = append(out.Found, f)
	}
	sort.Slice(out.Found, func(i, j int) bool { return out.Found[i].Key < out.Found[j].Key })
	sort.Slice(out.Refused, func(i, j int) bool { return out.Refused[i].Name < out.Refused[j].Name })
	return out
}

// reason keeps a dialog's text to the agent's own strings: a raw system error carries a full path.
func reason(err error) string {
	switch {
	case errors.Is(err, errName), errors.Is(err, errLink), errors.Is(err, errGGUF), errors.Is(err, errParts):
		return err.Error()
	}
	return "it cannot be read"
}

// ImportResult is what happened to one chosen file or set.
type ImportResult struct {
	Key     string
	ModelID string
	Err     string // the agent's own words; empty when it worked
}

// ImportProgress is told how far an import's hashing got, over everything chosen.
type ImportProgress func(file string, done, total int64)

// Import adds the files the host chose in the agent's dialog. Only the agent's tray calls this.
// It never holds the manager's lock while it hashes, so the link and the tray go on working, and
// nothing of a file or set is recorded until every part of it is hashed. A cancel stops the file in
// progress and the ones after it: those finished before it stay added, and the results say which is which.
// Each pick is what the dialog showed: a file that differs from it now, in size or modified time, is refused.
func (m *Manager) Import(ctx context.Context, picks []Found, progress ImportProgress) []ImportResult {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	m.mu.Lock()
	if m.importing {
		m.mu.Unlock()
		return []ImportResult{{Err: "another import is running"}}
	}
	m.importing = true
	root := m.dir
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.importing = false
		m.mu.Unlock()
	}()

	// Chosen from a list a moment ago: every rule is applied again, and only what is offered now counts.
	offered := map[string]Found{}
	for _, f := range m.Scan().Found {
		offered[f.Key] = f
	}
	var total, base int64
	for _, f := range picks {
		total += f.Size
	}
	out := make([]ImportResult, 0, len(picks))
	for _, f := range picks {
		k := f.Key
		if now, ok := offered[k]; !ok || !slices.Equal(now.Files, f.Files) || len(f.Seen) != len(f.Files) {
			out = append(out, ImportResult{Key: k, Err: "it can no longer be added"})
			continue
		}
		id, err := m.importOne(ctx, root, f, func(file string, done int64) { progress(file, base+done, total) })
		base += f.Size
		if err != nil {
			why := reason(err)
			if ctx.Err() != nil {
				why = "cancelled"
			} else if errors.Is(err, errImportStopped) {
				why = strings.TrimPrefix(err.Error(), errImportStopped.Error()+": ")
			}
			m.log.Warn("import failed", "file", k, "why", why)
			out = append(out, ImportResult{Key: k, Err: why})
			continue
		}
		m.log.Info("model files added by the host", "model_id", id, "files", len(f.Files), "bytes", f.Size)
		out = append(out, ImportResult{Key: k, ModelID: id})
	}
	return out
}

var errImportStopped = errors.New("import stopped")

func stopped(why string) error { return fmt.Errorf("%w: %s", errImportStopped, why) }

func (m *Manager) importOne(ctx context.Context, root string, f Found, progress func(string, int64)) (string, error) {
	count := int64(len(f.Files))
	files := make([]protocol.LocalFile, 0, count)
	var done int64
	for i, rel := range f.Files {
		sum, size, err := m.hashPart(ctx, root, rel, count, f.Seen[i], func(n int64) { progress(rel, done+n) })
		if err != nil {
			return "", err
		}
		done += size
		files = append(files, protocol.LocalFile{Path: rel, Size: size, SHA256: sum})
	}

	e := &Entry{Repo: LocalOwner + "/" + localSlug(f.Key), Commit: importCommit(files), Kind: "gguf", Names: append([]string(nil), f.Files...), Files: files, State: StateReady}
	if !protocol.ValidRepo(e.Repo) {
		return "", stopped("no valid name can be made for it")
	}
	m.mu.Lock()
	if m.dir != root {
		m.mu.Unlock()
		return "", stopped("the model folder changed")
	}
	var old *Entry
	for _, x := range m.index {
		if x.imported() && x.Names[0] == f.Key {
			old = x
		}
	}
	if old != nil {
		e.ModelID = old.ModelID // a changed file added again keeps its model id
	} else {
		e.ModelID = m.uniqueIDLocked(f.Key)
	}
	if !e.valid() {
		m.mu.Unlock()
		return "", stopped("no valid entry can be made for it")
	}
	if old != nil {
		if err := m.forget(old); err != nil {
			m.mu.Unlock()
			return "", stopped("the agent's protected records cannot be read or written")
		}
	}
	// The host's click is the approval: the same two records a download ends with.
	err := m.remember(e.Repo, e.Commit, e.Names)
	if err == nil {
		err = m.recordHashes(e)
	}
	if err != nil {
		m.forget(e)
		m.mu.Unlock()
		m.log.Warn("an import could not be recorded", "err", err.Error())
		return "", stopped("the agent's protected records cannot be read or written")
	}
	m.index[e.ModelID] = e
	m.saveLocked()
	m.mu.Unlock()
	m.o.OnChange(true)
	return e.ModelID, nil
}

// hashPart checks one file again and hashes it through the same open handle, so what was checked is
// what was hashed; a file that changed meanwhile is refused.
func (m *Manager) hashPart(ctx context.Context, root, rel string, count int64, seen Stamp, progress func(int64)) (string, int64, error) {
	f, st, err := openConfined(root, rel)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if !seen.same(st) { // what the host approved is what the dialog showed
		return "", 0, stopped("it changed after you chose it; choose it again")
	}
	h, err := readHeader(f, st.Size())
	if err != nil {
		return "", 0, err
	}
	if max(h.SplitCount, 1) != count {
		return "", 0, errParts
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if m.testDuringImport != nil {
		m.testDuringImport()
	}
	sum := sha256.New()
	var n int64
	buf := make([]byte, 4<<20)
	last := time.Now()
	progress(0)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		r, rerr := f.Read(buf)
		sum.Write(buf[:r])
		n += int64(r)
		if time.Since(last) > 250*time.Millisecond {
			last = time.Now()
			progress(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	progress(n)
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if n != st.Size() || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
		return "", 0, stopped("it changed while it was being checked")
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}

// FileStem is a file's name without folder, suffix or part number.
func FileStem(rel string) string {
	base := path.Base(rel)
	if g := reSplit.FindStringSubmatch(base); g != nil {
		return g[1]
	}
	return strings.TrimSuffix(base, ".gguf")
}

// localSlug makes the repo's second half from a file name. The name already passed the file rules;
// what remains is the first character and the Windows device names, which the repo rule refuses.
func localSlug(rel string) string {
	s := strings.TrimLeft(reDots.ReplaceAllString(FileStem(rel), "."), "._-")
	if len(s) > 96 {
		s = s[:96]
	}
	s = strings.TrimRight(s, "._-")
	if s == "" {
		return "model"
	}
	if !protocol.ValidRepo(LocalOwner + "/" + s) {
		s = "m-" + s
		if len(s) > 96 {
			s = strings.TrimRight(s[:96], "._-")
		}
	}
	return s
}

// uniqueIDLocked makes a model id from a file name, by the id rules, unlike any other in the index.
func (m *Manager) uniqueIDLocked(rel string) string {
	base := reNotID.ReplaceAllString(strings.ToLower(FileStem(rel)), "-")
	base = strings.TrimLeft(base, "._-")
	if len(base) > 72 {
		base = base[:72]
	}
	base = strings.TrimRight(base, "._-")
	if base == "" {
		base = "model"
	}
	id := base
	for n := 2; m.index[id] != nil; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// diskState compares an imported entry with the folder. The hash is left to the first load.
func diskState(root string, e *Entry) string {
	state := StateReady
	for _, f := range e.Files {
		st, err := os.Lstat(e.path(root, f.Path))
		switch {
		case errors.Is(err, os.ErrNotExist):
			state = StateMissing
		case err != nil, !st.Mode().IsRegular(), st.Size() != f.Size:
			return StateChanged
		}
	}
	return state
}

// refreshImported brings imported entries in step with the folder. A file back at its size is ready
// again, subject to the first-load hash; changed stays changed until the host imports it again.
func (m *Manager) refreshImported() {
	m.mu.Lock()
	dirty := false
	for _, e := range m.index {
		if !e.imported() || (e.State != StateReady && e.State != StateMissing) {
			continue
		}
		if s := diskState(m.dir, e); s != e.State {
			e.State = s
			dirty = true
		}
	}
	if dirty {
		m.saveLocked()
	}
	m.mu.Unlock()
	if dirty {
		m.o.OnChange(true)
	}
}

// forget removes an entry's approval and hash records (ruling B64: delete only forgets).
func (m *Manager) forget(e *Entry) error {
	if m.o.Store == nil {
		return errors.New("no store")
	}
	m.secrets.Lock()
	defer m.secrets.Unlock()
	names := map[string]bool{}
	for _, n := range e.Names {
		names[n] = true
	}
	// Both are read before either is written: a failed read changes nothing.
	a, err := m.loadApprovals()
	if err != nil {
		return err
	}
	rec, err := m.loadHashes()
	if err != nil {
		return err
	}
	kept := a.Allowed[:0]
	for _, x := range a.Allowed {
		if !(x.Repo == e.Repo && x.Commit == e.Commit && names[x.File]) {
			kept = append(kept, x)
		}
	}
	a.Allowed = kept
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := m.o.Store.WriteSecret(store.SecretApprovals, b); err != nil {
		return err
	}
	for n := range names {
		delete(rec, hashKey(e, n))
	}
	if b, err = json.Marshal(rec); err != nil {
		return err
	}
	return m.o.Store.WriteSecret(store.SecretModelHashes, b)
}

// inspectImported reads an imported file's header: only a file of an entry the host approved, found by
// (repo, commit, file) in the index, never a path the hub names and never Hugging Face.
func (m *Manager) inspectImported(root string, e *Entry, file string) (*Facts, *Error) {
	refused := errf(protocol.ErrNotFound, "no such file was added on this PC")
	if e == nil || !m.trusted(e) || !m.recorded(e) {
		return nil, refused
	}
	var want int64 = -1
	for _, x := range e.Files {
		if x.Path == file {
			want = x.Size
		}
	}
	if want < 0 {
		return nil, refused
	}
	f, st, err := openConfined(root, file)
	if err != nil || st.Size() != want {
		if f != nil {
			f.Close()
		}
		m.refreshImported()
		return nil, refused
	}
	defer f.Close()
	h, err := gguf.Parse(f, inspectMaxRead, time.Now().Add(inspectParseTime))
	if err != nil {
		return nil, errf(protocol.ErrUnsupported, "not a GGUF file this agent can read")
	}
	facts := &Facts{Header: h, FileSize: st.Size(), FromDisk: true, ReadBytes: int64(h.HeaderSize)}
	facts.Sizes, _ = h.TensorSizes(uint64(st.Size()))
	return facts, nil
}
