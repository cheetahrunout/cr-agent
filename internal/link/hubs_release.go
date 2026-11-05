//go:build !debug

package link

import "net/url"

func debugHostAllowed(*url.URL) bool { return false }
