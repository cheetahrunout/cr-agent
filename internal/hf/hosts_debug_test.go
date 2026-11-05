//go:build debug

package hf

import "testing"

func TestDebugDownloadHosts(t *testing.T) {
	for _, ok := range []string{"", "http://127.0.0.1:8080", "http://localhost:8080"} {
		if _, err := New(Options{Base: ok}); err != nil {
			t.Errorf("refused %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://example.com:8080", "http://127.0.0.1", "https://hf-mirror.com", "http://10.0.0.1:8080"} {
		if _, err := New(Options{Base: bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
