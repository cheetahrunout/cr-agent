//go:build debug

package link

import (
	"net/url"
	"strconv"
)

// debugHostAllowed lets a debug build (never signed for release) talk to a local hub (ruling B11).
func debugHostAllowed(u *url.URL) bool {
	if u.Scheme != "ws" && u.Scheme != "wss" {
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
