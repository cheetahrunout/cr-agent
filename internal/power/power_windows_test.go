//go:build windows

package power

import "testing"

// Registration with Windows works, the callback reaches the handler, and stop unregisters.
func TestWatch(t *testing.T) {
	var got []Event
	stop, err := Watch(func(e Event) { got = append(got, e) })
	if err != nil {
		t.Fatal(err)
	}
	dispatch(pbtSuspend)
	dispatch(0x000A) // a power status change: not for us
	dispatch(pbtResumeAutomatic)
	dispatch(pbtResumeSuspend)
	if len(got) != 3 || got[0] != Suspend || got[1] != Resume || got[2] != Resume {
		t.Fatalf("events %v", got)
	}
	stop()
	dispatch(pbtSuspend)
	if len(got) != 3 {
		t.Fatal("handler called after stop")
	}
	// Registering again after a stop works (the agent restarts its watcher on re-pair).
	stop2, err := Watch(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	stop2()
}
