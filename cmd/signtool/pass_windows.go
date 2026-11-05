//go:build windows

package main

import (
	"errors"
	"os"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// readPassphrase reads one line from the console with echo off. It opens the console itself, so a
// redirected stdin (a file, a pipe) cannot supply the passphrase.
func readPassphrase(prompt string) (string, error) {
	name, _ := windows.UTF16PtrFromString("CONIN$")
	in, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return "", errors.New("the passphrase is typed at a console; run signtool from a terminal window")
	}
	defer windows.CloseHandle(in)
	var mode uint32
	if err := windows.GetConsoleMode(in, &mode); err != nil {
		return "", errors.New("the passphrase is typed at a console; run signtool from a terminal window")
	}
	quiet := (mode &^ windows.ENABLE_ECHO_INPUT) | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(in, quiet); err != nil {
		return "", err
	}
	defer windows.SetConsoleMode(in, mode)
	os.Stderr.WriteString(prompt)
	defer os.Stderr.WriteString("\n")
	buf := make([]uint16, 1024)
	var n uint32
	if err := windows.ReadConsole(in, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return "", err
	}
	line := string(utf16.Decode(buf[:n]))
	for i := range buf {
		buf[i] = 0
	}
	return strings.TrimRight(line, "\r\n"), nil
}
