//go:build !windows

package supervisor

import (
	"errors"
	"os"
)

type proc struct {
	out  *os.File
	done chan struct{}
}

type spawnOptions struct {
	Exe       string
	Args      []string
	Dir       string
	LowIL     bool
	MemLimit  uint64
	GPUSelect string
}

func spawn(spawnOptions) (*proc, error) { return nil, errors.New("engine supervisor: Windows only") }
func (p *proc) PID() uint32             { return 0 }
func (p *proc) Done() <-chan struct{}   { return p.done }
func (p *proc) ExitCode() uint32        { return 0 }
func (p *proc) Kill()                   {}
func portOwner(int) (uint32, error)     { return 0, errors.New("Windows only") }

func (p *proc) Integrity() string { return "unknown" }

func (p *proc) PeakMemory() uint64 { return 0 }
