//go:build windows

package power

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modPowrprof    = windows.NewLazySystemDLL("powrprof.dll")
	procRegister   = modPowrprof.NewProc("PowerRegisterSuspendResumeNotification")
	procUnregister = modPowrprof.NewProc("PowerUnregisterSuspendResumeNotification")

	handler  atomic.Pointer[func(Event)]
	callback uintptr
	once     sync.Once
)

const deviceNotifyCallback = 2

// subscribeParameters is DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS.
type subscribeParameters struct {
	Callback uintptr
	Context  uintptr
}

func dispatch(typ uint32) {
	if ev, ok := eventFor(typ); ok {
		if h := handler.Load(); h != nil {
			(*h)(ev)
		}
	}
}

// Watch calls fn for every suspend and resume until stop is called. fn runs on a Windows thread-pool
// thread and holds up the suspend while it runs, so it must return within about two seconds.
func Watch(fn func(Event)) (stop func(), err error) {
	once.Do(func() { // a Go callback is never freed: make exactly one
		callback = windows.NewCallback(func(context uintptr, typ uint32, setting uintptr) uintptr {
			dispatch(typ)
			return 0
		})
	})
	if err := procRegister.Find(); err != nil {
		return nil, err
	}
	handler.Store(&fn)
	params := subscribeParameters{Callback: callback}
	var reg uintptr
	if r, _, _ := procRegister.Call(deviceNotifyCallback, uintptr(unsafe.Pointer(&params)), uintptr(unsafe.Pointer(&reg))); r != 0 {
		handler.Store(nil)
		return nil, fmt.Errorf("PowerRegisterSuspendResumeNotification: error %d", r)
	}
	return func() {
		procUnregister.Call(reg)
		handler.Store(nil)
	}, nil
}
