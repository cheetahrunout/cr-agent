//go:build !windows

package hw

import (
	"errors"
	"os/exec"
)

func nvidiaSMIPath() (string, error) { return "", errors.New("nvidia-smi: Windows only in this build") }

func hideWindow(*exec.Cmd) {}

func querySystem(string) (System, error) { return System{}, errors.New("hardware probe: Windows only") }
