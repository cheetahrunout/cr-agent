// Package reltest builds small signed releases for tests and for the local dry run.
package reltest

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/sign"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

// Spec describes a release.
type Spec struct {
	Version  string
	Seq      int64
	Tag      string            // llama.cpp tag, default b12125
	Agent    []byte            // versions/<version>/cragent.exe
	Launcher []byte            // versions/<version>/launcher.exe
	Engine   map[string][]byte // engine/<tag>/<name>
	MinHub   string
}

// Zip writes a zip of path → content.
func Zip(path string, entries map[string][]byte) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: time.Unix(1793870000, 0)})
		if err != nil {
			return err
		}
		if _, err := w.Write(entries[n]); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// AgentZip and EngineZip are the archive names of a spec.
func AgentZip(version string) string { return "agent-" + version + "-windows-amd64.zip" }

// EngineZip is the engine archive's name for a tag.
func EngineZip(tag string) string { return "engine-cuda13-" + tag + "-windows-amd64.zip" }

// MakeDist writes a release folder for a spec: the two zips, manifest.json and its signature by key.
func MakeDist(dir string, s Spec, key *sign.Key) (*release.Manifest, error) {
	if s.Tag == "" {
		s.Tag = "b12125"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	agent := map[string][]byte{"versions/" + s.Version + "/cragent.exe": s.Agent}
	if s.Launcher != nil {
		agent["versions/"+s.Version+"/launcher.exe"] = s.Launcher
	}
	if err := Zip(filepath.Join(dir, AgentZip(s.Version)), agent); err != nil {
		return nil, err
	}
	engine := map[string][]byte{}
	for n, b := range s.Engine {
		engine["engine/"+s.Tag+"/"+n] = b
	}
	if len(engine) > 0 {
		if err := Zip(filepath.Join(dir, EngineZip(s.Tag)), engine); err != nil {
			return nil, err
		}
	}
	m, err := release.Build(dir, release.BuildOptions{Version: s.Version, ReleaseSeq: s.Seq, Protocol: "1.0", MinHubVersion: s.MinHub,
		LlamaCpp: release.LlamaCpp{Tag: s.Tag}, KeyID: key.ID.String(), ReleasedAt: time.Unix(1793870000, 0), Notes: "test release"})
	if err != nil {
		return nil, err
	}
	return m, Sign(dir, m, key)
}

// Sign writes manifest.json and manifest.json.minisig into dir.
func Sign(dir string, m *release.Manifest, key *sign.Key) error {
	raw := release.Marshal(m)
	sig, err := key.Sign(raw, release.TrustedComment(m))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json.minisig"), sig, 0o644)
}

// Trusting returns a trust list holding exactly one key.
func Trusting(key *sign.Key) []trust.Key {
	return []trust.Key{{ID: key.ID, Public: key.Public, Status: "current"}}
}
