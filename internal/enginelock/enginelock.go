// Package enginelock reads the engine pin compiled into the agent (engine.lock and
// supported_archs.txt at the root of the repository).
package enginelock

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	cragent "github.com/cheetahrunout/cr-agent"
)

// Archive is one official llama.cpp zip.
type Archive struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Lock is engine.lock.
type Lock struct {
	Tag         string    `json:"llama_tag"`
	Commit      string    `json:"commit"`
	Backend     string    `json:"backend"`
	Upstream    string    `json:"upstream"`
	Archives    []Archive `json:"archives"`
	StderrAllow []string  `json:"stderr_allow"`

	allow []*regexp.Regexp
	archs []string
}

var (
	reTag = regexp.MustCompile(`^b\d{3,6}$`)
	reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse reads and checks a lock file and its architecture list.
func Parse(lock []byte, archs string) (*Lock, error) {
	var l Lock
	if err := json.Unmarshal(lock, &l); err != nil {
		return nil, fmt.Errorf("engine.lock: %w", err)
	}
	if !reTag.MatchString(l.Tag) {
		return nil, fmt.Errorf("engine.lock: bad tag %q", l.Tag)
	}
	if !strings.HasPrefix(l.Upstream, "https://github.com/ggml-org/llama.cpp/releases/download/"+l.Tag+"/") {
		return nil, fmt.Errorf("engine.lock: upstream is not the official release of %s", l.Tag)
	}
	if len(l.Archives) == 0 {
		return nil, fmt.Errorf("engine.lock: no archives")
	}
	for _, a := range l.Archives {
		if !strings.HasSuffix(a.Name, ".zip") || strings.ContainsAny(a.Name, `/\`) || !reSHA.MatchString(a.SHA256) {
			return nil, fmt.Errorf("engine.lock: bad archive entry %q", a.Name)
		}
	}
	for _, p := range l.StderrAllow {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("engine.lock: stderr_allow: %w", err)
		}
		l.allow = append(l.allow, re)
	}
	for _, line := range strings.Split(archs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		l.archs = append(l.archs, line)
	}
	if len(l.archs) == 0 {
		return nil, fmt.Errorf("supported_archs.txt is empty: run tools/genarchs")
	}
	if !strings.Contains(archs, l.Tag) {
		return nil, fmt.Errorf("supported_archs.txt was not generated for %s: run tools/genarchs", l.Tag)
	}
	return &l, nil
}

// Allowed reports whether a line of engine output may be written to the agent log. Everything else
// is counted, never written (ruling B16).
func (l *Lock) Allowed(line string) bool {
	for _, re := range l.allow {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// Archs is the architecture list of the pinned build.
func (l *Lock) Archs() []string { return append([]string(nil), l.archs...) }

// Supports reports whether the pinned build knows an architecture.
func (l *Lock) Supports(arch string) bool {
	for _, a := range l.archs {
		if a == arch {
			return true
		}
	}
	return false
}

var pinned = func() *Lock {
	l, err := Parse(cragent.EngineLock, cragent.SupportedArchs)
	if err != nil {
		panic(err) // a build with a broken pin must not start
	}
	return l
}()

// Pinned is the lock compiled into this build.
func Pinned() *Lock { return pinned }
