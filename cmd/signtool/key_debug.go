//go:build debug

package main

import (
	"flag"

	"github.com/cheetahrunout/cr-agent/internal/sign"
)

// keyFlags adds -test-key in a debug build: sign with the throwaway test key, which only debug
// builds trust. It is how the tests and the local dry run sign without a console.
func keyFlags(fs *flag.FlagSet) func() keySource {
	test := fs.Bool("test-key", false, "debug builds only: use the throwaway test key")
	return func() keySource {
		if *test {
			return func(bool) (*sign.Key, error) { return sign.ThrowawayKey(), nil }
		}
		return fromConsole
	}
}
