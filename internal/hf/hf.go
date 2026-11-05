// Package hf is the agent's only way to Hugging Face (§9.2, ruling B17). Requests start at
// https://huggingface.co and nowhere else; redirects are followed by hand, at most three hops, https
// only, to huggingface.co or a host ending in .hf.co; the token goes only to huggingface.co itself.
// The agent takes file sizes and SHA-256 hashes from here, never from the hub.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Host is the one host requests may start at.
const Host = "huggingface.co"

// MaxRedirects is the number of hops followed (ruling B17).
const MaxRedirects = 3

// Options configure a client.
type Options struct {
	// Base is empty in every release build. A debug build may point it at a local stand-in
	// (http://127.0.0.1:<port>); see hosts_debug.go.
	Base string
	// Transport is the HTTP transport (tests route huggingface.co to a local server with it).
	Transport http.RoundTripper
	// Token returns the host's Hugging Face token, or "".
	Token func() string
	// UserAgent identifies the agent.
	UserAgent string
}

// Client talks to Hugging Face.
type Client struct {
	base  *url.URL
	hc    *http.Client
	token func() string
	ua    string
}

// New builds a client. A Base other than Hugging Face is refused unless this is a debug build.
func New(o Options) (*Client, error) {
	raw := o.Base
	if raw == "" {
		raw = "https://" + Host
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("download host must be a bare origin")
	}
	u.Path = ""
	if !(u.Scheme == "https" && u.Host == Host) && !standInAllowed(u) {
		return nil, errors.New("download host not accepted by this build")
	}
	tr := o.Transport
	if tr == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 30 * time.Second
		tr = t
	}
	c := &Client{base: u, token: o.Token, ua: o.UserAgent,
		hc: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if c.token == nil {
		c.token = func() string { return "" }
	}
	if c.ua == "" {
		c.ua = "cr-agent"
	}
	return c, nil
}

// isOrigin reports whether u is the host requests start at (the only host that may see the token).
func (c *Client) isOrigin(u *url.URL) bool { return u.Scheme == c.base.Scheme && u.Host == c.base.Host }

// redirectAllowed is the hop rule: the origin itself, or https to a host ending in .hf.co.
func (c *Client) redirectAllowed(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if c.isOrigin(u) {
		return true
	}
	return u.Scheme == "https" && u.Port() == "" && strings.HasSuffix(u.Hostname(), ".hf.co") && len(u.Hostname()) > len(".hf.co")
}

// StatusError is a non-success answer.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("hugging face answered HTTP %d", e.Code) }

// ErrRedirect is a redirect the hop rule refuses.
var ErrRedirect = errors.New("hugging face redirected to a host that is not allowed")

// do sends one request and follows redirects by the hop rule. Headers in hdr go to every hop;
// Authorization is added only for the origin.
func (c *Client) do(ctx context.Context, method string, u *url.URL, hdr http.Header) (*http.Response, error) {
	if !c.isOrigin(u) {
		return nil, ErrRedirect
	}
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", c.ua)
		req.Header.Del("Authorization")
		if tok := c.token(); tok != "" && c.isOrigin(u) {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, errors.New("request to hugging face failed") // the cause may quote signed URLs
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if hop >= MaxRedirects {
				return nil, ErrRedirect
			}
			next, err := u.Parse(loc)
			if err != nil || loc == "" || !c.redirectAllowed(next) {
				return nil, ErrRedirect
			}
			u = next
			continue
		}
		return resp, nil
	}
}

func (c *Client) apiURL(parts ...string) *url.URL {
	u := *c.base
	esc := make([]string, 0, len(parts))
	for _, p := range parts {
		for _, seg := range strings.Split(p, "/") {
			esc = append(esc, url.PathEscape(seg))
		}
	}
	u.Path = "/" + strings.Join(parts, "/")
	u.RawPath = "/" + strings.Join(esc, "/")
	return &u
}

func (c *Client) getJSON(ctx context.Context, u *url.URL, max int64, v any) (http.Header, error) {
	resp, err := c.do(ctx, http.MethodGet, u, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &StatusError{Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, errors.New("reading hugging face's answer failed")
	}
	if int64(len(b)) > max {
		return nil, errors.New("hugging face's answer is too large")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, errors.New("hugging face's answer is not the expected JSON")
	}
	return resp.Header, nil
}

// Revision is what the model API says about a repo at a commit.
type Revision struct {
	ID      string `json:"id"`  // owner/name in Hugging Face's own case
	SHA     string `json:"sha"` // the commit
	Private bool   `json:"private"`
}

// Revision asks GET /api/models/<repo>/revision/<commit>.
func (c *Client) Revision(ctx context.Context, repo, commit string) (*Revision, error) {
	var r Revision
	if _, err := c.getJSON(ctx, c.apiURL("api/models", repo, "revision", commit), 32<<20, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// File is one file of a repo as the tree API lists it.
type File struct {
	Path   string
	Size   int64
	SHA256 string // lfs.oid; empty for a file that is not in LFS
}

type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

var (
	reSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reLinkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)
)

// Tree asks GET /api/models/<repo>/tree/<commit>?recursive=1 and follows its pages.
func (c *Client) Tree(ctx context.Context, repo, commit string) (map[string]File, error) {
	u := c.apiURL("api/models", repo, "tree", commit)
	u.RawQuery = "recursive=1"
	prefix := u.EscapedPath()
	files := map[string]File{}
	for page := 0; page < 50; page++ {
		var entries []treeEntry
		hdr, err := c.getJSON(ctx, u, 16<<20, &entries)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Type != "file" {
				continue
			}
			f := File{Path: e.Path, Size: e.Size}
			if e.LFS != nil && reSHA256.MatchString(e.LFS.OID) {
				f.SHA256, f.Size = e.LFS.OID, e.LFS.Size
			}
			files[e.Path] = f
		}
		m := reLinkNext.FindStringSubmatch(hdr.Get("Link"))
		if m == nil {
			return files, nil
		}
		next, err := u.Parse(m[1])
		if err != nil || !c.isOrigin(next) || next.EscapedPath() != prefix {
			return nil, errors.New("hugging face's next page link is not on the same listing")
		}
		u = next
	}
	return nil, errors.New("hugging face's file listing has too many pages")
}

// Body is an open download.
type Body struct {
	io.ReadCloser
	Offset int64  // first byte delivered (0 when the server ignored the Range)
	Total  int64  // size of the whole file, -1 when unknown
	ETag   string // validator of the final response, for If-Range on a later resume
}

// Open starts GET /<repo>/resolve/<commit>/<file> at offset. etag, when set, is sent as If-Range so
// a changed file restarts from byte 0 instead of being stitched together. length > 0 limits the range.
func (c *Client) Open(ctx context.Context, repo, commit, file string, offset, length int64, etag string) (*Body, error) {
	hdr := http.Header{"Accept-Encoding": {"identity"}}
	if offset > 0 || length > 0 {
		r := "bytes=" + strconv.FormatInt(offset, 10) + "-"
		if length > 0 {
			r += strconv.FormatInt(offset+length-1, 10)
		}
		hdr.Set("Range", r)
		if etag != "" && offset > 0 {
			hdr.Set("If-Range", etag)
		}
	}
	resp, err := c.do(ctx, http.MethodGet, c.apiURL(repo, "resolve", commit, file), hdr)
	if err != nil {
		return nil, err
	}
	b := &Body{ReadCloser: resp.Body, Total: -1, ETag: resp.Header.Get("ETag")}
	if strings.HasPrefix(b.ETag, "W/") {
		b.ETag = "" // a weak validator may not be used with If-Range
	}
	switch resp.StatusCode {
	case http.StatusOK:
		b.Total = resp.ContentLength
		return b, nil
	case http.StatusPartialContent:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			resp.Body.Close()
			return nil, errors.New("hugging face answered with a range that was not asked for")
		}
		b.Offset, b.Total = start, total
		return b, nil
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode}
	}
}

// parseContentRange reads "bytes <start>-<end>/<total>".
func parseContentRange(s string) (start, total int64, ok bool) {
	rest, found := strings.CutPrefix(s, "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, tot, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	a, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	total, err2 := strconv.ParseInt(tot, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || total <= start {
		return 0, 0, false
	}
	return start, total, true
}

// TokenState is what the hub may learn about the token: present, absent or invalid, never the token.
type TokenState string

// Token states.
const (
	TokenAbsent  TokenState = "absent"
	TokenPresent TokenState = "present"
	TokenInvalid TokenState = "invalid"
)

// CheckToken validates a token against GET /api/whoami-v2.
func (c *Client) CheckToken(ctx context.Context, token string) (TokenState, error) {
	if token == "" {
		return TokenAbsent, nil
	}
	probe := *c
	probe.token = func() string { return token }
	resp, err := probe.do(ctx, http.MethodGet, c.apiURL("api/whoami-v2"), http.Header{"Accept": {"application/json"}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return TokenPresent, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return TokenInvalid, nil
	}
	return "", &StatusError{Code: resp.StatusCode}
}
