//go:build windows

package ui

import "golang.org/x/sys/windows"

// Open opens a file or an https URL with the program Windows associates with it. The agent calls it
// only with its own log path and the validated hub URL.
func Open(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, t, nil, nil, windows.SW_SHOWNORMAL)
}
