//go:build debug

package hf

import (
	"net/url"
	"strconv"
)

// standInAllowed lets a debug build (never signed for release) use a local server in place of
// Hugging Face, for the fake hub's download check.
func standInAllowed(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1":
	default:
		return false
	}
	p, err := strconv.Atoi(u.Port())
	return err == nil && p > 0 && p < 65536
}
