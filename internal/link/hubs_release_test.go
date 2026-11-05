//go:build !debug

package link

import "testing"

// The release host list is exactly the production hub (ruling B11). This is the check CI should run:
// grepping the binary for "localhost" always hits Go's own net package.
func TestReleaseHubHosts(t *testing.T) {
	if _, err := ValidateHubURL("wss://cr-ai.apps.grovestreet.zip/agent"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"ws://localhost:9000/agent", "ws://127.0.0.1:9000/agent", "ws://cr-ai.apps.grovestreet.zip/agent",
		"wss://cr-ai-dev.apps.grovestreet.zip/agent", "wss://cr-ai.apps.grovestreet.zip/agent?x=1",
		"wss://u:p@cr-ai.apps.grovestreet.zip/agent", "wss://cr-ai.apps.grovestreet.zip:444/agent",
		"wss://cr-ai.apps.grovestreet.zip/other", "wss://cr-ai.apps.grovestreet.zip.evil.com/agent",
	} {
		if _, err := ValidateHubURL(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
