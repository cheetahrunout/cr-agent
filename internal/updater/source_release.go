//go:build !debug

package updater

import "net/url"

// A release build fetches releases from the GitHub prefix and nowhere else.
func standInAllowed(*url.URL) bool { return false }
