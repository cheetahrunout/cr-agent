//go:build windows

package keepawake

import (
	"runtime"

	"golang.org/x/sys/windows"
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

func (a *Awake) run() {
	runtime.LockOSThread() // the execution state dies with its thread: this one never exits
	for on := range a.ch {
		state := uintptr(esContinuous)
		if on {
			state |= esSystemRequired
		}
		procSetThreadExecutionState.Call(state)
	}
}
