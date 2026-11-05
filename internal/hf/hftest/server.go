// Package hftest is a local stand-in for Hugging Face: the model API (revision, tree, whoami) and
// resolve with Range support and a redirect to a CDN host. Tests reach it through Transport(), which
// routes https://huggingface.co and https://*.hf.co to it, so the client under test keeps its real
// host policy. The debug fake hub serves it directly as the agent's --hf-base.
package hftest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server is the fake.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	repos map[string]*repo // by id

	// Knobs, set before use or under Lock().
	CDN            string        // redirect target origin for resolve; "" = https://cdn-lfs.hf.co (tests) — set to the server's own URL for a stand-in
	PageSize       int           // tree entries per page, 0 = all
	Token          string        // when set, gated repos need "Bearer <Token>"
	FailAfter      int64         // cut the first FailTimes blob responses after this many bytes
	FailTimes      int32         // how many responses to cut
	Corrupt        bool          // flip one byte of every blob
	ChunkDelay     time.Duration // sleep between 64 KiB chunks (slow download)
	IgnoreRange    bool          // answer 200 with the whole file whatever the Range
	RedirectTo     string        // when set, resolve redirects here instead (a forbidden host)
	ExtraRedirects int           // extra hops on the CDN before the blob

	// Observations.
	TokenAtOrigin atomic.Int32 // requests to huggingface.co that carried the token
	TokenAtCDN    atomic.Int32 // requests to any other host that carried an Authorization header
	BlobRequests  atomic.Int32
	RangeRequests atomic.Int32
	failed        atomic.Int32
}

type repo struct {
	id     string
	commit string
	gated  bool
	files  map[string][]byte
	sha    map[string]string // by file name, hashed once
}

// New starts the fake on a free local port.
func New() *Server {
	s := &Server{repos: map[string]*repo{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewOn starts the fake on addr as a stand-in the debug agent can be pointed at (--hf-base): the
// "CDN" it redirects to is itself.
func NewOn(addr string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{repos: map[string]*repo{}}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Server.Listener.Close()
	s.Server.Listener = l
	s.Server.Start()
	s.CDN = s.URL
	return s, nil
}

// Lock guards knob changes made while requests are running.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// AddRepo publishes files under id at commit.
func (s *Server) AddRepo(id, commit string, gated bool, files map[string][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := &repo{id: id, commit: commit, gated: gated, files: files, sha: map[string]string{}}
	for n, b := range files {
		rp.sha[n] = SHA256(b)
	}
	s.repos[strings.ToLower(id)] = rp
}

// SHA256 is the hex hash of b.
func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Transport routes every request for huggingface.co or *.hf.co to the fake, keeping the Host header.
func (s *Server) Transport() http.RoundTripper {
	target, _ := url.Parse(s.URL)
	return rt(func(r *http.Request) (*http.Response, error) {
		h := r.URL.Hostname()
		if r.URL.Scheme != "https" || !(h == "huggingface.co" || strings.HasSuffix(h, ".hf.co")) {
			return nil, fmt.Errorf("hftest: request to %s://%s left the allowed hosts", r.URL.Scheme, r.URL.Host)
		}
		out := r.Clone(r.Context())
		out.Host = r.URL.Host
		out.URL.Scheme, out.URL.Host = "http", target.Host
		return http.DefaultTransport.RoundTrip(out)
	})
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *Server) origin(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		h = r.Host
	}
	return h == "huggingface.co" || h == "127.0.0.1" || h == "localhost"
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth != "" {
		if s.origin(r) {
			s.TokenAtOrigin.Add(1)
		} else {
			s.TokenAtCDN.Add(1)
		}
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/api/whoami-v2":
		s.mu.Lock()
		tok := s.Token
		s.mu.Unlock()
		if tok == "" || auth != "Bearer "+tok {
			http.Error(w, `{"error":"Invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"name":"tester"}`))
	case len(p) == 6 && p[0] == "api" && p[1] == "models" && p[4] == "revision":
		rp := s.repo(p[2]+"/"+p[3], p[5])
		if rp == nil {
			http.Error(w, `{"error":"Repository not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": rp.id, "sha": rp.commit, "private": false, "gated": rp.gated})
	case len(p) == 6 && p[0] == "api" && p[1] == "models" && p[4] == "tree":
		s.tree(w, r, s.repo(p[2]+"/"+p[3], p[5]))
	case len(p) >= 5 && p[2] == "resolve":
		s.resolve(w, r, s.repo(p[0]+"/"+p[1], p[3]), strings.Join(p[4:], "/"), auth)
	case len(p) == 3 && p[0] == "hop":
		n, _ := strconv.Atoi(p[1])
		if n > 1 {
			http.Redirect(w, r, fmt.Sprintf("%s/hop/%d/%s", s.cdn(), n-1, p[2]), http.StatusFound)
			return
		}
		http.Redirect(w, r, s.cdn()+"/blob/"+p[2], http.StatusFound)
	case len(p) == 2 && p[0] == "blob":
		s.blob(w, r, p[1])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) cdn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CDN == "" {
		return "https://cdn-lfs.hf.co"
	}
	return s.CDN
}

func (s *Server) repo(id, commit string) *repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.repos[strings.ToLower(id)]
	if rp == nil || rp.commit != commit {
		return nil
	}
	return rp
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request, rp *repo) {
	if rp == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	names := make([]string, 0, len(rp.files))
	for n := range rp.files {
		names = append(names, n)
	}
	sortStrings(names)
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	s.mu.Lock()
	size := s.PageSize
	s.mu.Unlock()
	end := len(names)
	if size > 0 && start+size < end {
		end = start + size
		next := *r.URL
		q := next.Query()
		q.Set("cursor", strconv.Itoa(end))
		next.RawQuery = q.Encode()
		scheme := "https"
		if s.origin(r) && r.Host != "huggingface.co" {
			scheme = "http"
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s://%s%s>; rel="next"`, scheme, r.Host, next.RequestURI()))
	}
	out := []map[string]any{{"type": "directory", "path": "sub", "size": 0}}
	for _, n := range names[start:end] {
		b := rp.files[n]
		e := map[string]any{"type": "file", "path": n, "size": len(b), "oid": "0000"}
		if strings.HasSuffix(n, ".gguf") {
			e["lfs"] = map[string]any{"oid": rp.sha[n], "size": len(b), "pointerSize": 134}
		}
		out = append(out, e)
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request, rp *repo, file, auth string) {
	if rp == nil {
		http.NotFound(w, r)
		return
	}
	_, ok := rp.files[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	tok, to, extra := s.Token, s.RedirectTo, s.ExtraRedirects
	s.mu.Unlock()
	if rp.gated && (tok == "" || auth != "Bearer "+tok) {
		http.Error(w, "gated", http.StatusUnauthorized)
		return
	}
	if to != "" {
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	if extra > 0 {
		http.Redirect(w, r, fmt.Sprintf("%s/hop/%d/%s", s.cdn(), extra, rp.sha[file]), http.StatusFound)
		return
	}
	http.Redirect(w, r, s.cdn()+"/blob/"+rp.sha[file], http.StatusFound)
}

func (s *Server) find(sha string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rp := range s.repos {
		for n, b := range rp.files {
			if rp.sha[n] == sha {
				return b
			}
		}
	}
	return nil
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request, sha string) {
	b := s.find(sha)
	if b == nil {
		http.NotFound(w, r)
		return
	}
	s.BlobRequests.Add(1)
	if r.Header.Get("Range") != "" {
		s.RangeRequests.Add(1)
	}
	s.mu.Lock()
	corrupt, failAfter, failTimes, delay, ignore := s.Corrupt, s.FailAfter, s.FailTimes, s.ChunkDelay, s.IgnoreRange
	s.mu.Unlock()
	if corrupt {
		b = append([]byte(nil), b...)
		b[len(b)/2] ^= 0xff
	}
	if ignore {
		r.Header.Del("Range")
	}
	w.Header().Set("ETag", `"`+sha+`"`)
	out := &shaped{ResponseWriter: w, delay: delay, limit: -1}
	if failAfter > 0 && s.failed.Add(1) <= failTimes {
		out.limit = failAfter
	}
	http.ServeContent(out, r, "", time.Time{}, bytes.NewReader(b))
}

// shaped slows a response down and can cut it short (the connection is then closed mid-body).
type shaped struct {
	http.ResponseWriter
	delay time.Duration
	limit int64
	sent  int64
}

func (s *shaped) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > 64<<10 {
			n = 64 << 10
		}
		if s.limit >= 0 && s.sent+int64(n) > s.limit {
			n = int(s.limit - s.sent)
			if n > 0 {
				s.ResponseWriter.Write(p[:n])
				if f, ok := s.ResponseWriter.(http.Flusher); ok {
					f.Flush()
				}
			}
			panic(http.ErrAbortHandler) // closes the connection without finishing the body
		}
		w, err := s.ResponseWriter.Write(p[:n])
		total += w
		s.sent += int64(w)
		if err != nil {
			return total, err
		}
		if f, ok := s.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		p = p[n:]
	}
	return total, nil
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
