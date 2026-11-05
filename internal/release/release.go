// Package release is the release manifest of the CR agent (PLAN §13, rulings B19–B23): one signed
// manifest.json that lists every installed file and every archive with size and SHA-256.
//
// A release is a folder ("dist") holding
//
//	agent-<version>-windows-amd64.zip     entries versions/<version>/cragent.exe, versions/<version>/launcher.exe
//	engine-<backend>-<tag>-windows-amd64.zip   entries engine/<tag>/…
//	CRAgent-Setup-<version>.exe           the installer (listed, never fetched by the updater)
//	manifest.json, manifest.json.minisig
//
// Zip entries are paths relative to the install folder and are exactly the manifest's files[]:
// unpacking is "write each listed entry where its path says", and anything not listed is refused.
package release

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/sign"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

// Fixed facts of this product.
const (
	Product  = "cr-agent"
	Platform = "windows-amd64"
	// DownloadPrefix is compiled into every agent: releases come from here and nowhere else (B23).
	DownloadPrefix = "https://github.com/cheetahrunout/cr-agent/releases/download/"
	// MaxSeqStep bounds how far release_seq may grow in one release (B20).
	MaxSeqStep = 1000
)

// File is one installed file.
type File struct {
	Path   string `json:"path"` // relative to the install folder, with forward slashes
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Archive is one release asset.
type Archive struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// LlamaCpp names the pinned engine.
type LlamaCpp struct {
	Tag               string            `json:"tag"`
	UpstreamZipSHA256 map[string]string `json:"upstream_zip_sha256"` // official zip name → SHA-256
}

// Manifest is manifest.json.
type Manifest struct {
	Product       string    `json:"product"`
	Platform      string    `json:"platform"`
	Version       string    `json:"version"`
	Protocol      string    `json:"protocol"`
	MinHubVersion string    `json:"min_hub_version"`
	ReleaseSeq    int64     `json:"release_seq"`
	LlamaCpp      LlamaCpp  `json:"llama_cpp"`
	Files         []File    `json:"files"`
	Archives      []Archive `json:"archives"`
	KeyID         string    `json:"key_id"`
	ReleasedAt    string    `json:"released_at"`
	Notes         string    `json:"notes"`
}

var (
	reSemver  = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	reSHA     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reTag     = regexp.MustCompile(`^b\d{3,6}$`)
	reSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)
	reAsset   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.(zip|exe)$`)
	reProto   = regexp.MustCompile(`^\d+\.\d+$`)
)

// ValidVersion checks a version as update{version} carries it (B23).
func ValidVersion(v string) bool { return reSemver.MatchString(v) }

var devices = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func badSegment(seg string) bool {
	if !reSegment.MatchString(seg) || seg == "." || seg == ".." || strings.HasSuffix(seg, ".") {
		return true
	}
	base := strings.ToUpper(seg)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if devices[base] {
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9'
}

// SafePath reports whether p is a manifest path this release may install: under versions/<version>/
// or engine/<tag>/, plain segments, nothing that could leave the install folder.
func (m *Manifest) SafePath(p string) bool {
	segs := strings.Split(p, "/")
	if len(segs) < 3 || len(segs) > 8 {
		return false
	}
	switch {
	case segs[0] == "versions" && segs[1] == m.Version:
	case segs[0] == "engine" && segs[1] == m.LlamaCpp.Tag:
	default:
		return false
	}
	for _, s := range segs[2:] {
		if badSegment(s) {
			return false
		}
	}
	return true
}

// Validate checks the manifest's shape. It says nothing about who signed it.
func (m *Manifest) Validate() error {
	switch {
	case m.Product != Product:
		return fmt.Errorf("manifest: product %q", m.Product)
	case m.Platform != Platform:
		return fmt.Errorf("manifest: platform %q", m.Platform)
	case !reSemver.MatchString(m.Version):
		return errors.New("manifest: bad version")
	case !reProto.MatchString(m.Protocol):
		return errors.New("manifest: bad protocol")
	case m.MinHubVersion != "" && !reSemver.MatchString(m.MinHubVersion):
		return errors.New("manifest: bad min_hub_version")
	case m.ReleaseSeq < 1:
		return errors.New("manifest: release_seq must be at least 1")
	case !reTag.MatchString(m.LlamaCpp.Tag):
		return errors.New("manifest: bad llama_cpp.tag")
	case len(m.Files) == 0 || len(m.Files) > 2000:
		return errors.New("manifest: files missing or too many")
	case len(m.Archives) == 0 || len(m.Archives) > 16:
		return errors.New("manifest: archives missing or too many")
	}
	if _, err := sign.ParseKeyID(m.KeyID); err != nil {
		return errors.New("manifest: bad key_id")
	}
	if _, err := time.Parse(time.RFC3339, m.ReleasedAt); err != nil {
		return errors.New("manifest: bad released_at")
	}
	for name, sha := range m.LlamaCpp.UpstreamZipSHA256 {
		if !reAsset.MatchString(name) || !reSHA.MatchString(sha) {
			return errors.New("manifest: bad llama_cpp.upstream_zip_sha256")
		}
	}
	seen := map[string]bool{}
	hasAgent := false
	for _, f := range m.Files {
		key := strings.ToLower(f.Path)
		if !m.SafePath(f.Path) || f.Size < 0 || f.Size > 4<<30 || !reSHA.MatchString(f.SHA256) || seen[key] {
			return fmt.Errorf("manifest: bad file entry %q", f.Path)
		}
		seen[key] = true
		if f.Path == m.AgentPath() {
			hasAgent = true
		}
	}
	if !hasAgent {
		return errors.New("manifest: the agent binary is not listed")
	}
	names := map[string]bool{}
	for _, a := range m.Archives {
		if !reAsset.MatchString(a.Name) || a.Size <= 0 || !reSHA.MatchString(a.SHA256) || names[strings.ToLower(a.Name)] {
			return fmt.Errorf("manifest: bad archive entry %q", a.Name)
		}
		names[strings.ToLower(a.Name)] = true
	}
	return nil
}

// AgentPath is the agent binary's path in this release.
func (m *Manifest) AgentPath() string { return "versions/" + m.Version + "/cragent.exe" }

// LauncherPath is the staged launcher's path in this release.
func (m *Manifest) LauncherPath() string { return "versions/" + m.Version + "/launcher.exe" }

// Zips are the archives the updater unpacks (the installer is listed but never fetched).
func (m *Manifest) Zips() []Archive {
	var out []Archive
	for _, a := range m.Archives {
		if strings.HasSuffix(a.Name, ".zip") {
			out = append(out, a)
		}
	}
	return out
}

// File looks a path up.
func (m *Manifest) File(path string) (File, bool) {
	for _, f := range m.Files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

// TrustedComment is the signed comment of a manifest's signature (B19).
func TrustedComment(m *Manifest) string {
	return fmt.Sprintf("product=%s version=%s release_seq=%d", m.Product, m.Version, m.ReleaseSeq)
}

var reComment = regexp.MustCompile(`^product=([a-z-]+) version=(\d{1,3}\.\d{1,3}\.\d{1,3}) release_seq=(\d{1,12})$`)

// Marshal writes a manifest the way it is published.
func Marshal(m *Manifest) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// Errors of Verify. Callers show them as "update refused" with the reason.
var (
	ErrUntrustedKey = errors.New("the manifest is signed with a key this build does not trust")
	ErrMismatch     = errors.New("the signed comment does not match the manifest")
)

// Verify is the whole check of ruling B19, in its order: the key id is in the trust list, the
// signature covers the manifest bytes, the global signature covers the trusted comment, and the
// comment's product, version and release_seq equal the manifest's. Only then is the manifest parsed
// for use. Nothing is fetched or unpacked before this passes.
func Verify(manifestJSON, sigFile []byte, keys []trust.Key) (*Manifest, error) {
	if len(manifestJSON) > 4<<20 {
		return nil, errors.New("manifest too large")
	}
	s, err := sign.ParseSignature(sigFile)
	if err != nil {
		return nil, err
	}
	key, ok := trust.Lookup(keys, s.KeyID)
	if !ok {
		return nil, ErrUntrustedKey
	}
	if err := s.Verify(key.Public, manifestJSON); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, errors.New("manifest is not valid JSON")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	c := reComment.FindStringSubmatch(s.TrustedComment)
	if c == nil {
		return nil, ErrMismatch
	}
	seq, _ := strconv.ParseInt(c[3], 10, 64)
	// key_id in the manifest names the key the release was made with. During a rotation the outgoing
	// key signs the same bytes too (ruling B20), so the accepted signature may be by another trusted
	// key than the one named: what counts is that a trusted key signed these exact bytes.
	if c[1] != m.Product || c[2] != m.Version || seq != m.ReleaseSeq {
		return nil, ErrMismatch
	}
	return &m, nil
}

// CheckSeq is the anti-rollback rule of rulings B20 and B21: a release older than the committed one
// is refused; the same sequence is accepted only to reinstall the same version; and the sequence
// may not jump by more than 1000.
func CheckSeq(m *Manifest, committedSeq int64, committedVersion string) error {
	switch {
	case m.ReleaseSeq < committedSeq:
		return fmt.Errorf("release_seq %d is older than the installed %d", m.ReleaseSeq, committedSeq)
	case m.ReleaseSeq == committedSeq && committedSeq > 0 && m.Version != committedVersion:
		return fmt.Errorf("release_seq %d is already taken by version %s", m.ReleaseSeq, committedVersion)
	case m.ReleaseSeq > committedSeq+MaxSeqStep:
		return fmt.Errorf("release_seq %d jumps more than %d past the installed %d", m.ReleaseSeq, MaxSeqStep, committedSeq)
	}
	return nil
}

// HashFile returns a file's SHA-256 and size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// BuildOptions are the fields of a manifest that do not come from the files.
type BuildOptions struct {
	Version       string
	ReleaseSeq    int64
	Protocol      string
	MinHubVersion string
	LlamaCpp      LlamaCpp
	KeyID         string
	ReleasedAt    time.Time
	Notes         string
}

// zipFiles lists a zip's entries as manifest files, hashing each one.
func zipFiles(path string) ([]File, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var files []File
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") {
			continue // directory entry
		}
		if !e.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: entry %q is not a regular file", filepath.Base(path), e.Name)
		}
		rc, err := e.Open()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: entry %q: %w", filepath.Base(path), e.Name, err)
		}
		files = append(files, File{Path: e.Name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	return files, nil
}

// Build hashes a dist folder into a manifest: every *.zip and *.exe becomes an archive, and every
// entry of every zip becomes a file.
func Build(dist string, o BuildOptions) (*Manifest, error) {
	m := &Manifest{Product: Product, Platform: Platform, Version: o.Version, Protocol: o.Protocol, MinHubVersion: o.MinHubVersion,
		ReleaseSeq: o.ReleaseSeq, LlamaCpp: o.LlamaCpp, KeyID: o.KeyID, ReleasedAt: o.ReleasedAt.UTC().Format(time.RFC3339), Notes: o.Notes}
	ents, err := os.ReadDir(dist)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".exe")) {
			continue
		}
		sum, size, err := HashFile(filepath.Join(dist, name))
		if err != nil {
			return nil, err
		}
		m.Archives = append(m.Archives, Archive{Name: name, Size: size, SHA256: sum})
		if strings.HasSuffix(name, ".zip") {
			files, err := zipFiles(filepath.Join(dist, name))
			if err != nil {
				return nil, err
			}
			m.Files = append(m.Files, files...)
		}
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	sort.Slice(m.Archives, func(i, j int) bool { return m.Archives[i].Name < m.Archives[j].Name })
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Pack writes a release zip: every file of dir (or only the named ones) under prefix, with forward
// slashes, sorted, and one fixed timestamp, so the same input gives the same archive byte for byte.
// prefix is versions/<version> or engine/<tag>.
func Pack(out, prefix, dir string, only []string) error {
	var names []string
	if len(only) > 0 {
		names = append(names, only...)
	} else {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("%s is not a regular file", path)
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Strings(names)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range names {
		for _, seg := range strings.Split(n, "/") {
			if badSegment(seg) {
				f.Close()
				os.Remove(out)
				return fmt.Errorf("%q cannot be part of a release (file name)", n)
			}
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + "/" + n, Method: zip.Deflate, Modified: stamp})
		if err == nil {
			var src *os.File
			if src, err = os.Open(filepath.Join(dir, filepath.FromSlash(n))); err == nil {
				_, err = io.Copy(w, src)
				src.Close()
			}
		}
		if err != nil {
			f.Close()
			os.Remove(out)
			return err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	return f.Close()
}

// CheckDist re-hashes a dist folder against its manifest and fails closed: every archive must be
// there with the listed size and hash, every zip entry must be a listed file with the listed hash,
// every listed file must be in some zip, and the folder may hold no other archive.
func CheckDist(dist string, m *Manifest) error {
	listed := map[string]Archive{}
	for _, a := range m.Archives {
		listed[a.Name] = a
	}
	ents, err := os.ReadDir(dist)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() && (strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".exe")) {
			if _, ok := listed[name]; !ok {
				return fmt.Errorf("%s is in the folder but not in the manifest", name)
			}
		}
	}
	inZip := map[string]bool{}
	for _, a := range m.Archives {
		path := filepath.Join(dist, a.Name)
		sum, size, err := HashFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		if sum != a.SHA256 || size != a.Size {
			return fmt.Errorf("%s does not match the manifest", a.Name)
		}
		if !strings.HasSuffix(a.Name, ".zip") {
			continue
		}
		files, err := zipFiles(path)
		if err != nil {
			return err
		}
		for _, f := range files {
			want, ok := m.File(f.Path)
			if !ok || want.SHA256 != f.SHA256 || want.Size != f.Size {
				return fmt.Errorf("%s: entry %q does not match the manifest", a.Name, f.Path)
			}
			inZip[f.Path] = true
		}
	}
	for _, f := range m.Files {
		if !inZip[f.Path] {
			return fmt.Errorf("%q is listed but in no archive", f.Path)
		}
	}
	return nil
}
