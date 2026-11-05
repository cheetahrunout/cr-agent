//go:build linux

package main

import (
	"bufio"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// readPassphrase reads one line from the controlling terminal with echo off. It opens /dev/tty
// itself, so a redirected stdin cannot supply the passphrase.
func readPassphrase(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("the passphrase is typed at a terminal; run signtool from one")
	}
	defer tty.Close()
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", errors.New("the passphrase is typed at a terminal; run signtool from one")
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	tty.WriteString(prompt)
	defer tty.WriteString("\n")
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
