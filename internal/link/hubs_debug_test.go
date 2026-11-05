//go:build debug

package link

import "testing"

func TestDebugHubHosts(t *testing.T) {
	for _, ok := range []string{"wss://cr-ai.apps.grovestreet.zip/agent", "ws://localhost:9000/agent", "ws://127.0.0.1:9000/agent"} {
		if _, err := ValidateHubURL(ok); err != nil {
			t.Errorf("refused %s: %v", ok, err)
		}
	}
	for _, bad := range []string{"ws://localhost/agent", "ws://example.com:9000/agent", "http://localhost:9000/agent"} {
		if _, err := ValidateHubURL(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
