//go:build windows

package layout

import "golang.org/x/sys/windows"

// Replace is MoveFileEx with REPLACE_EXISTING | WRITE_THROUGH (ruling B22).
func Replace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
