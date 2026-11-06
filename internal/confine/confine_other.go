//go:build !windows

package confine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func NoReparse(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path leaves the folder")
	}
	cur := root
	for _, p := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		cur = filepath.Join(cur, p)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("the folder contains a link; refusing to write through it")
		}
	}
	return nil
}

func UnderRoot(f *os.File, root string) error {
	got, err := filepath.EvalSymlinks(f.Name())
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(got, strings.TrimRight(want, "/")+"/") {
		return errors.New("the file resolved outside the folder")
	}
	return nil
}

// Links is how many names an open file has.
func Links(f *os.File) (uint32, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint32(s.Nlink), nil
	}
	return 1, nil
}
