// Package power tells the agent when Windows is about to sleep and when it wakes (ruling B12),
// through PowerRegisterSuspendResumeNotification with a callback, so it works with or without a tray.
package power

// Event is a power transition.
type Event int

// Events.
const (
	Suspend Event = iota + 1 // the PC is about to sleep or hibernate; the handler has about two seconds
	Resume                   // the PC woke up
)

// Windows power broadcast types.
const (
	pbtSuspend         = 0x0004 // PBT_APMSUSPEND
	pbtResumeSuspend   = 0x0007 // PBT_APMRESUMESUSPEND: woke with a person present
	pbtResumeAutomatic = 0x0012 // PBT_APMRESUMEAUTOMATIC: woke (always sent)
)

// eventFor maps a broadcast type to an event. Windows sends RESUMEAUTOMATIC on every wake and
// RESUMESUSPEND as well when a person woke the PC, so a handler sees Resume once or twice.
func eventFor(typ uint32) (Event, bool) {
	switch typ {
	case pbtSuspend:
		return Suspend, true
	case pbtResumeSuspend, pbtResumeAutomatic:
		return Resume, true
	}
	return 0, false
}
