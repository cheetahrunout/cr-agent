//go:build windows

// Package confine keeps file writes inside one folder: no path that climbs out of it, and no link
// (junction, symlink, mount point) planted inside it that leads elsewhere.
package confine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// NoReparse refuses a path whose existing components, from root down, include a reparse point
// (junction, symlink, mount point): a planted link must never carry a write outside the folder.
func NoReparse(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path leaves the folder")
	}
	cur := root
	parts := []string{""}
	if rel != "." {
		parts = append(parts, strings.Split(rel, string(filepath.Separator))...)
	}
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		p16, err := windows.UTF16PtrFromString(cur)
		if err != nil {
			return err
		}
		attrs, err := windows.GetFileAttributes(p16)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil // the rest does not exist yet
			}
			return err
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("the folder contains a link (reparse point); refusing to write through it")
		}
	}
	return nil
}

func finalPathOfHandle(h windows.Handle) (string, error) {
	buf := make([]uint16, 1024)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// UnderRoot re-checks an open file: the path Windows finally resolved must lie under the resolved root.
func UnderRoot(f *os.File, root string) error {
	got, err := finalPathOfHandle(windows.Handle(f.Fd()))
	if err != nil {
		return err
	}
	r16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	rh, err := windows.CreateFile(r16, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(rh)
	want, err := finalPathOfHandle(rh)
	if err != nil {
		return err
	}
	want = strings.TrimRight(want, `\`) + `\`
	if len(got) <= len(want) || !strings.EqualFold(got[:len(want)], want) {
		return errors.New("the file resolved outside the folder")
	}
	return nil
}

// Links is how many names an open file has. A file with more than one is also reachable, and
// changeable, by a name that may lie outside the folder.
func Links(f *os.File) (uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return info.NumberOfLinks, nil
}

// FinalPath is the path Windows finally reaches for an existing file or folder: links followed, short
// names expanded, no \\?\ prefix.
func FinalPath(path string) (string, error) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p16, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	got, err := finalPathOfHandle(h)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(got, `\\?\UNC\`) {
		return `\\` + got[len(`\\?\UNC\`):], nil
	}
	return strings.TrimPrefix(got, `\\?\`), nil
}
