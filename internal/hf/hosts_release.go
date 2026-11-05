//go:build !debug

package hf

import "net/url"

// A release build downloads from Hugging Face and nowhere else.
func standInAllowed(*url.URL) bool { return false }
