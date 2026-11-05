//go:build !windows

package layout

import "os"

// Replace moves from over to.
func Replace(from, to string) error { return os.Rename(from, to) }
