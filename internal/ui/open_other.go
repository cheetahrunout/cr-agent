//go:build !windows

package ui

import "os/exec"

// Open opens a file or URL with the default program of the desktop.
func Open(target string) error { return exec.Command("xdg-open", target).Start() }
