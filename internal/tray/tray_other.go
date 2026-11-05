//go:build !windows

package tray

import "errors"

// Tray is Windows only in this build; the agent runs as a console program elsewhere.
type Tray struct{}

// New makes a tray that cannot run.
func New(func(Action, string)) *Tray { return &Tray{} }

// Run fails: there is no tray on this platform yet.
func (*Tray) Run(chan<- struct{}) error { return errors.New("tray: Windows only") }

// Update does nothing.
func (*Tray) Update(State) {}

// Notify does nothing.
func (*Tray) Notify(string, string) {}

// Close does nothing.
func (*Tray) Close() {}
