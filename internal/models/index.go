package models

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// indexName is the index kept inside the model folder, so a folder the host moves or picks again
// brings its models with it (ruling B17: changing the folder re-indexes the new one).
const indexName = "cr-agent-index.json"

// Entry is one model of the index.
type Entry struct {
	ModelID string               `json:"model_id"`
	Repo    string               `json:"repo"`
	Commit  string               `json:"commit"`
	Kind    string               `json:"kind"`
	Names   []string             `json:"names"`           // the files the hub named, in its order
	Files   []protocol.LocalFile `json:"files,omitempty"` // filled from Hugging Face's listing
	State   string               `json:"state"`
	ETags   map[string]string    `json:"etags,omitempty"` // validators for If-Range on resume
}

type indexFile struct {
	Version int      `json:"version"`
	Models  []*Entry `json:"models"`
}

var (
	reCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reSHA    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// reCommitDir is the folder a download's files go in: Entry.dir's commit[:12].
	reCommitDir = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// valid re-checks an entry read from disk with the same rules as a command: the index sits in a
// folder other programs can write, so it is input, not truth.
func (e *Entry) valid() bool {
	if e == nil || !protocol.ValidModelID(e.ModelID) || !protocol.ValidRepo(e.Repo) || !reCommit.MatchString(e.Commit) {
		return false
	}
	if e.Kind != "gguf" && e.Kind != "mmproj" {
		return false
	}
	if len(e.Names) < 1 || len(e.Names) > 16 || len(e.Files) > 16 {
		return false
	}
	for _, n := range e.Names {
		if !protocol.ValidGGUFFile(n) {
			return false
		}
	}
	if len(e.Files) != 0 && len(e.Files) != len(e.Names) {
		return false
	}
	for i, f := range e.Files {
		// Names is what was approved and Files[0] is what loads: they must be the same files.
		if f.Path != e.Names[i] || !protocol.ValidGGUFFile(f.Path) || f.Size <= 0 || !reSHA.MatchString(f.SHA256) {
			return false
		}
	}
	if reservedRepo(e.Repo) {
		return e.validImported() // the index is input: nothing may come from it that starts a download
	}
	switch e.State {
	case StateDownloading, StateAwaiting, StateDenied, StateReady, StateError, StateMissing:
		return e.State != StateReady || len(e.Files) == len(e.Names)
	}
	return false
}

// dir is <models>\<owner>\<repo>\<commit[:12]>; an imported file stays where the host put it, and its
// path in the index is relative to the model folder itself.
func (e *Entry) dir(root string) string {
	if e.imported() {
		return root
	}
	owner, name, _ := strings.Cut(e.Repo, "/")
	return filepath.Join(root, owner, name, e.Commit[:12])
}

func (e *Entry) path(root, file string) string {
	return filepath.Join(e.dir(root), filepath.FromSlash(file))
}

func loadIndex(root string) (map[string]*Entry, error) {
	idx := map[string]*Entry{}
	b, err := os.ReadFile(filepath.Join(root, indexName))
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		// A damaged index must not block the agent: the files stay, the models are simply unknown.
		os.Rename(filepath.Join(root, indexName), filepath.Join(root, indexName+".damaged"))
		return idx, nil
	}
	for _, e := range f.Models {
		if e.valid() && idx[e.ModelID] == nil {
			idx[e.ModelID] = e
		}
	}
	return idx, nil
}

// saveLocked writes the index; the caller holds m.mu.
func (m *Manager) saveLocked() {
	f := indexFile{Version: 1}
	for _, e := range m.index {
		f.Models = append(f.Models, e)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		tmp := filepath.Join(m.dir, indexName+".tmp")
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, filepath.Join(m.dir, indexName))
		}
	}
	if err != nil {
		m.log.Error("model index not saved", "err", err.Error())
	}
}

func filesPresent(root string, e *Entry) bool {
	if len(e.Files) == 0 {
		return false
	}
	for _, f := range e.Files {
		st, err := os.Lstat(e.path(root, f.Path))
		if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
			return false
		}
	}
	return true
}

// removeFiles deletes an entry's files and partials, then the folders they leave empty, never
// anything above the model folder and never through a link.
func removeFiles(root string, e *Entry, log *slog.Logger) {
	if e.imported() {
		return // the host's files are the host's (ruling B64)
	}
	names := map[string]bool{}
	for _, n := range e.Names {
		names[n] = true
	}
	for _, f := range e.Files {
		names[f.Path] = true
	}
	dir := e.dir(root)
	if err := noReparse(root, dir); err != nil {
		log.Warn("delete skipped", "model_id", e.ModelID, "err", err.Error())
		return
	}
	for n := range names {
		if !protocol.ValidGGUFFile(n) {
			continue
		}
		p := e.path(root, n)
		if err := noReparse(root, filepath.Dir(p)); err != nil {
			continue
		}
		for _, target := range []string{p, p + ".part"} {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("file not deleted", "model_id", e.ModelID, "err", "remove failed")
			}
		}
		if sub := filepath.Dir(p); sub != dir {
			os.Remove(sub) // only succeeds when empty
		}
	}
	for d := dir; d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // not empty: another model lives here
			break
		}
	}
}
