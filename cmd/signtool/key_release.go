//go:build !debug

package main

import "flag"

// keyFlags adds no flag in a release build: the key comes from the console, and only from there.
func keyFlags(*flag.FlagSet) func() keySource {
	return func() keySource { return fromConsole }
}
