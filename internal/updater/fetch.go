package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/release"
)

// maxRedirects and the two asset hosts are §2.5's rule for GitHub release downloads.
const maxRedirects = 3

var assetHosts = map[string]bool{"objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true}

// fetcher downloads release files. Every URL is the fixed prefix plus a path the agent builds itself
// from a version and a name out of a verified manifest; nothing the hub sends is ever a URL (B23).
type fetcher struct {
	base *url.URL
	hc   *http.Client
}

func newFetcher(base string, tr http.RoundTripper) (*fetcher, error) {
	if base == "" {
		base = release.DownloadPrefix
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/") {
		return nil, errors.New("release source must be a plain URL prefix ending in /")
	}
	if base != release.DownloadPrefix && !standInAllowed(u) {
		return nil, errors.New("release source not accepted by this build")
	}
	if tr == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 30 * time.Second
		tr = t
	}
	return &fetcher{base: u, hc: &http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// redirectAllowed: the source host itself, or https to one of GitHub's two asset hosts.
func (f *fetcher) redirectAllowed(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if u.Scheme == f.base.Scheme && u.Host == f.base.Host {
		return true
	}
	return u.Scheme == "https" && u.Port() == "" && assetHosts[u.Hostname()]
}

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

var errRedirect = errors.New("redirect to a host that is not allowed")

// get follows redirects by the rule above. rel is relative to the prefix.
func (f *fetcher) get(ctx context.Context, rel string, hdr http.Header) (*http.Response, error) {
	u, err := f.base.Parse(rel)
	if err != nil || !strings.HasPrefix(u.Path, f.base.Path) {
		return nil, errors.New("bad release path")
	}
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", "cr-agent-updater")
		resp, err := f.hc.Do(req)
		if err != nil {
			return nil, errors.New("request failed")
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if hop >= maxRedirects {
				return nil, errRedirect
			}
			next, err := u.Parse(loc)
			if err != nil || loc == "" || !f.redirectAllowed(next) {
				return nil, errRedirect
			}
			u = next
			continue
		}
		return resp, nil
	}
}

// small fetches a file of at most max bytes.
func (f *fetcher) small(ctx context.Context, rel string, max int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := f.get(ctx, rel, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &statusError{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, errors.New("read failed")
	}
	if int64(len(b)) > max {
		return nil, errors.New("file larger than expected")
	}
	return b, nil
}

// manifest fetches v<version>/manifest.json and its signature.
func (f *fetcher) manifest(ctx context.Context, version string) (raw, sig []byte, err error) {
	if raw, err = f.small(ctx, "v"+version+"/manifest.json", 4<<20); err != nil {
		return nil, nil, err
	}
	if sig, err = f.small(ctx, "v"+version+"/manifest.json.minisig", 4096); err != nil {
		return nil, nil, fmt.Errorf("signature: %w", err)
	}
	return raw, sig, nil
}

// open starts a download at offset. It returns the offset the server really started at and the
// size of the whole file as the server states it (-1 when it does not say).
func (f *fetcher) open(ctx context.Context, rel string, offset int64) (io.ReadCloser, int64, int64, error) {
	hdr := http.Header{"Accept-Encoding": {"identity"}}
	if offset > 0 {
		hdr.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := f.get(ctx, rel, hdr)
	if err != nil {
		return nil, 0, -1, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, 0, resp.ContentLength, nil
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range")
		if !strings.HasPrefix(cr, "bytes "+strconv.FormatInt(offset, 10)+"-") {
			resp.Body.Close()
			return nil, 0, -1, errors.New("the server answered with another range")
		}
		total := int64(-1)
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				total = n
			}
		}
		return resp.Body, offset, total, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return nil, 0, -1, &statusError{resp.StatusCode}
}
