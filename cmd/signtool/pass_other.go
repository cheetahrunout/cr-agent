//go:build !windows && !linux

package main

import "errors"

func readPassphrase(string) (string, error) {
	return "", errors.New("signtool reads the passphrase only on Windows and Linux consoles")
}
