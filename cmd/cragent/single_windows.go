//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

// singleInstance takes a named mutex for the home folder, so two agents never share one store, one
// model folder and one machine key. running is true when another agent holds it.
func singleInstance(home string) (release func(), running bool) {
	sum := sha256.Sum256([]byte(strings.ToLower(home)))
	name, err := windows.UTF16PtrFromString(`Local\CRAgent-` + hex.EncodeToString(sum[:8]))
	if err != nil {
		return func() {}, false
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return func() {}, true
	}
	if err != nil || h == 0 {
		return func() {}, false
	}
	return func() { windows.CloseHandle(h) }, false
}
