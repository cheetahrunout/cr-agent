//go:build !windows

package keepawake

func (a *Awake) run() {
	for range a.ch {
	}
}
