//go:build !debug

package pairing

import (
	"errors"
	"testing"
)

const prod = "wss://cr-ai.apps.grovestreet.zip/agent"

func TestParseInputRelease(t *testing.T) {
	for _, in := range []string{"K7M2-9QWD-X3", " k7m2 9qwd x3 ", "https://cr-ai.apps.grovestreet.zip/agent#K7M2-9QWD-X3"} {
		hub, code, err := ParseInput(in, prod)
		if err != nil || hub != prod || code != "K7M29QWDX3" {
			t.Errorf("%q → %q %q %v", in, hub, code, err)
		}
	}
	for in, want := range map[string]Problem{
		"https://evil.example/agent#K7M2-9QWD-X3":                       ProblemHost,
		"http://localhost:9000/agent#K7M2-9QWD-X3":                      ProblemHost,
		"https://cr-ai.apps.grovestreet.zip/other#K7M2-9QWD-X3":         ProblemHost,
		"https://cr-ai.apps.grovestreet.zip.evil.io/agent#K7M2-9QWD-X3": ProblemHost,
		"https://cr-ai.apps.grovestreet.zip/agent":                      ProblemInput,
		"https://cr-ai.apps.grovestreet.zip/agent#nope":                 ProblemInput,
		"hello": ProblemInput,
		"":      ProblemInput,
	} {
		_, _, err := ParseInput(in, prod)
		var pe *Error
		if !errors.As(err, &pe) || pe.Problem != want {
			t.Errorf("%q: got %v, want problem %d", in, err, want)
		}
	}
	// A bare code never pairs with a hub this build refuses.
	if _, _, err := ParseInput("K7M2-9QWD-X3", "ws://localhost:9000/agent"); err == nil {
		t.Error("bare code accepted for a local hub in a release build")
	}
}
