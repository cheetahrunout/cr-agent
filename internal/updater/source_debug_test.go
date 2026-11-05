//go:build debug

package updater

import "testing"

func TestDebugSourceHosts(t *testing.T) {
	for _, ok := range []string{"", "http://127.0.0.1:9002/", "http://localhost:9002/releases/"} {
		if _, err := newFetcher(ok, nil); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://example.com:9002/", "http://127.0.0.1/", "https://evil.example/", "http://127.0.0.1:9002"} {
		if _, err := newFetcher(bad, nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
