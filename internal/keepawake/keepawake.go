// Package keepawake stops Windows from going to sleep on its own while the agent is working (§12.4):
// SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED), never ES_DISPLAY_REQUIRED, so the
// screen may still turn off. The state belongs to the calling thread, so one goroutine locked to its
// OS thread owns it for the life of the process.
package keepawake

import "sync"

// Awake is the switch.
type Awake struct {
	once sync.Once
	ch   chan bool
	mu   sync.Mutex
	on   bool
}

// New makes a switch that is off.
func New() *Awake { return &Awake{ch: make(chan bool, 1)} }

// Set turns keep-awake on or off. It is cheap to call with the value it already has.
func (a *Awake) Set(on bool) {
	a.mu.Lock()
	if a.on == on {
		a.mu.Unlock()
		return
	}
	a.on = on
	a.mu.Unlock()
	a.once.Do(func() { go a.run() })
	select { // keep only the latest wish
	case <-a.ch:
	default:
	}
	a.ch <- on
}

// On reports the last value set.
func (a *Awake) On() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.on
}
