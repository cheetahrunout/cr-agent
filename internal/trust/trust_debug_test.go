//go:build debug

package trust

import (
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/sign"
)

func TestDebugBuildTrust(t *testing.T) {
	if !Debug() || Check() != nil {
		t.Fatalf("debug build: Debug()=%v Check()=%v", Debug(), Check())
	}
	if _, ok := Lookup(Keys(), sign.ThrowawayKey().ID); !ok {
		t.Fatal("a debug build must trust the throwaway key")
	}
}
