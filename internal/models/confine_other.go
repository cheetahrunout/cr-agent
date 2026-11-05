//go:build !windows

package models

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func noReparse(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path leaves the model folder")
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
			return errors.New("the model folder contains a link; refusing to write through it")
		}
	}
	return nil
}

func underRoot(f *os.File, root string) error {
	got, err := filepath.EvalSymlinks(f.Name())
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(got, strings.TrimRight(want, "/")+"/") {
		return errors.New("the file resolved outside the model folder")
	}
	return nil
}
