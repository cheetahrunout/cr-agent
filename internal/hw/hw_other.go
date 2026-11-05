//go:build !windows

package hw

import (
	"errors"
	"os/exec"
)

func nvidiaSMIPath() (string, error) { return "", errors.New("nvidia-smi: Windows only in this build") }

func hideWindow(*exec.Cmd) {}

func querySystem(string) (System, error) { return System{}, errors.New("hardware probe: Windows only") }

// DiskSpace is Windows only in this build.
func DiskSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.New("disk probe: Windows only")
}
