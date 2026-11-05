//go:build !debug

package updater

import (
	"net/url"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/release"
)

// The release build takes releases from the fixed GitHub prefix and nothing else (the twin of the
// hub host and download host tests).
func TestReleaseSourceHosts(t *testing.T) {
	for _, ok := range []string{"", release.DownloadPrefix} {
		if _, err := newFetcher(ok, nil); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://127.0.0.1:9002/", "http://localhost:9002/", "https://github.com/evil/cr-agent/releases/download/",
		"https://github.com/cheetahrunout/cr-agent/releases/download", "http://github.com/cheetahrunout/cr-agent/releases/download/",
		"https://github.com.evil.io/cheetahrunout/cr-agent/releases/download/", "https://u:p@github.com/cheetahrunout/cr-agent/releases/download/",
		"https://github.com/cheetahrunout/cr-agent/releases/download/?x=1", "file:///c:/releases/",
	} {
		if _, err := newFetcher(bad, nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	f, _ := newFetcher("", nil)
	for raw, want := range map[string]bool{
		"https://github.com/cheetahrunout/cr-agent/releases/download/v0.1.1/x.zip": true,
		"https://objects.githubusercontent.com/github-production-release-asset/1":  true,
		"https://release-assets.githubusercontent.com/github-production/1":         true,
		"http://objects.githubusercontent.com/x":                                   false,
		"https://objects.githubusercontent.com:8443/x":                             false,
		"https://objects.githubusercontent.com.evil.io/x":                          false,
		"https://raw.githubusercontent.com/x":                                      false,
		"https://evil.example/x":                                                   false,
		"http://127.0.0.1:9002/x":                                                  false,
		"https://u:p@objects.githubusercontent.com/x":                              false,
	} {
		u, _ := url.Parse(raw)
		if got := f.redirectAllowed(u); got != want {
			t.Errorf("redirect to %s: allowed=%v, want %v", raw, got, want)
		}
	}
}
