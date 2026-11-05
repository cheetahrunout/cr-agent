//go:build !debug

package hf

import (
	"net/url"
	"testing"
)

// The release build accepts Hugging Face as the download host and nothing else (the twin of the hub
// host test). CI runs this instead of grepping the binary.
func TestReleaseDownloadHosts(t *testing.T) {
	for _, ok := range []string{"", "https://huggingface.co", "https://huggingface.co/"} {
		if _, err := New(Options{Base: ok}); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://huggingface.co", "http://127.0.0.1:8080", "http://localhost:8080", "https://localhost:8080",
		"https://hf-mirror.com", "https://huggingface.co.evil.io", "https://evil.hf.co", "https://huggingface.co:444",
		"https://user:pw@huggingface.co", "https://huggingface.co/api", "https://huggingface.co?x=1", "file:///c:/x",
	} {
		if _, err := New(Options{Base: bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c, _ := New(Options{})
	for raw, want := range map[string]bool{
		"https://huggingface.co/x":                 true,
		"https://cdn-lfs.hf.co/repos/ab/cd":        true,
		"https://cas-bridge.xethub.hf.co/xet/blob": true,
		"http://cdn-lfs.hf.co/x":                   false,
		"https://hf.co/x":                          false,
		"https://.hf.co/x":                         false,
		"https://evilhf.co/x":                      false,
		"https://cdn-lfs.hf.co.evil.io/x":          false,
		"https://cdn-lfs.hf.co:8443/x":             false,
		"https://u:p@cdn-lfs.hf.co/x":              false,
		"http://127.0.0.1:9000/x":                  false,
		"http://localhost/x":                       false,
		"https://github.com/x":                     false,
	} {
		u, _ := url.Parse(raw)
		if got := c.redirectAllowed(u); got != want {
			t.Errorf("redirect to %s: allowed=%v, want %v", raw, got, want)
		}
	}
}
