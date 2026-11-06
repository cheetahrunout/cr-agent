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
	Level     Level
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

func prepareSandbox(string, string, string, func(string)) error { return errors.New("Windows only") }
func newSocketPath() (string, error)                            { return "", errors.New("Windows only") }
func socketOwner(string) (uint32, error)                        { return 0, errors.New("Windows only") }

// GrantModelFolder lets the engine's sandbox read a model folder.
func GrantModelFolder(string) (bool, error) { return false, errors.New("Windows only") }

// RevokeModelGrant takes away an entry the agent added.
func RevokeModelGrant(string) error { return errors.New("Windows only") }

// ScopeContainer gives each agent folder its own AppContainer.
func ScopeContainer(string) {}

// ResetSandbox makes the AppContainer's profile anew.
func ResetSandbox() error { return errors.New("Windows only") }

// DeleteContainer removes the AppContainer's profile.
func DeleteContainer() error { return nil }

// CheckModelFolder says why a folder cannot be the model folder.
func CheckModelFolder(string, string) error { return nil }

// IsOwnModelFolder reports whether dir is <home>/models or below it.
func IsOwnModelFolder(string, string) bool { return false }

// CanEngineRead reports whether a model folder already lets the sandbox read it.
func CanEngineRead(string) bool { return false }

func (p *proc) PeakMemory() uint64 { return 0 }
