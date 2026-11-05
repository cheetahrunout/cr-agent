//go:build !debug

package trust

import (
	"errors"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/sign"
)

// A release build never trusts the test key, and refuses to start without a production key.
func TestReleaseBuildTrust(t *testing.T) {
	if Debug() {
		t.Fatal("a release build reports debug trust")
	}
	for _, k := range Keys() {
		if IsThrowaway(k) {
			t.Fatal("a release build trusts the throwaway test key")
		}
	}
	tk := sign.ThrowawayKey()
	if _, ok := Lookup(Keys(), tk.ID); ok {
		t.Fatal("the throwaway key is found in a release build")
	}
	production, err := Production()
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(); len(production) == 0 && !errors.Is(err, ErrNoProductionKey) {
		t.Fatalf("a release build with an empty key list would start: %v", err)
	}
}
