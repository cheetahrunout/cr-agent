// Package relsrv is a local stand-in for the GitHub release downloads of cr-agent. It serves a
// folder laid out like the release URLs: <dir>/v<version>/manifest.json and so on.
//
// Tests reach it through Transport(), which routes the real GitHub prefix (and GitHub's asset hosts,
// to which archives are redirected, as GitHub does) to it, so the updater under test keeps its real
// URL building and redirect rule. The debug fake hub serves it directly as the agent's --release-base.
package relsrv

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const githubPrefix = "/cheetahrunout/cr-agent/releases/download"

// Server is the fake.
type Server struct {
	*httptest.Server
	Dir string

	mu       sync.Mutex
	requests []string

	// Knobs.
	Direct     bool   // serve archives without a redirect (stand-in mode)
	RedirectTo string // redirect archives here instead (a host that is not allowed)
	CutAfter   int64  // cut the first CutTimes archive responses after this many bytes
	CutTimes   int32
	cut        atomic.Int32
}

// New serves dir on a free local port, for use through Transport().
func New(dir string) *Server {
	s := &Server{Dir: dir}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewOn serves dir on addr directly, as a debug agent's --release-base.
func NewOn(addr, dir string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Dir: dir, Direct: true}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Server.Listener.Close()
	s.Server.Listener = l
	s.Server.Start()
	return s, nil
}

// Requests lists what was asked for, as "<host> <path>".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Count is the number of requests whose path contains part.
func (s *Server) Count(part string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r, part) {
			n++
		}
	}
	return n
}

// Reset forgets the request log.
func (s *Server) Reset() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
	s.cut.Store(0)
}

// Transport routes https://github.com/cheetahrunout/cr-agent/releases/download/… and GitHub's two
// asset hosts to the fake.
func (s *Server) Transport() http.RoundTripper {
	target, _ := url.Parse(s.URL)
	return rt(func(r *http.Request) (*http.Response, error) {
		h := r.URL.Hostname()
		out := r.Clone(r.Context())
		out.Host = r.URL.Host
		switch {
		case r.URL.Scheme == "https" && h == "github.com" && strings.HasPrefix(r.URL.Path, githubPrefix+"/"):
			out.URL.Path = strings.TrimPrefix(r.URL.Path, githubPrefix)
		case r.URL.Scheme == "https" && (h == "objects.githubusercontent.com" || h == "release-assets.githubusercontent.com"):
		default:
			return nil, fmt.Errorf("relsrv: request to %s://%s%s left the allowed hosts", r.URL.Scheme, r.URL.Host, r.URL.Path)
		}
		out.URL.Scheme, out.URL.Host = "http", target.Host
		return http.DefaultTransport.RoundTrip(out)
	})
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Host+" "+r.URL.Path)
	direct, to := s.Direct, s.RedirectTo
	s.mu.Unlock()
	path := r.URL.Path
	cdn := strings.HasPrefix(path, "/cdn/")
	if cdn {
		path = strings.TrimPrefix(path, "/cdn")
	}
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(s.Dir, filepath.FromSlash(path))
	st, err := os.Stat(file)
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(path, ".zip") && !cdn && !direct {
		if to == "" {
			to = "https://objects.githubusercontent.com/cdn" + path
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	out := http.ResponseWriter(w)
	if strings.HasSuffix(path, ".zip") && s.CutAfter > 0 && s.cut.Add(1) <= s.CutTimes {
		out = &cutter{ResponseWriter: w, limit: s.CutAfter}
	}
	http.ServeContent(out, r, "", time.Time{}, f)
}

// cutter closes the connection after limit bytes of body.
type cutter struct {
	http.ResponseWriter
	limit, sent int64
}

func (c *cutter) Write(p []byte) (int, error) {
	if c.sent+int64(len(p)) > c.limit {
		if n := c.limit - c.sent; n > 0 {
			c.ResponseWriter.Write(p[:n])
			if f, ok := c.ResponseWriter.(http.Flusher); ok {
				f.Flush()
			}
		}
		panic(http.ErrAbortHandler)
	}
	n, err := c.ResponseWriter.Write(p)
	c.sent += int64(n)
	return n, err
}
