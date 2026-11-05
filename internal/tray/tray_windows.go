//go:build windows

package tray

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cheetahrunout/cr-agent/internal/status"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClassEx       = user32.NewProc("RegisterClassExW")
	procCreateWindowEx        = user32.NewProc("CreateWindowExW")
	procDefWindowProc         = user32.NewProc("DefWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetMessage            = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessage       = user32.NewProc("DispatchMessageW")
	procPostMessage           = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenu            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procCreateIconIndirect    = user32.NewProc("CreateIconIndirect")
	procDestroyIcon           = user32.NewProc("DestroyIcon")
	procShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	procCreateBitmap          = gdi32.NewProc("CreateBitmap")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
)

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmNull      = 0x0000
	wmApp       = 0x8000
	wmTray      = wmApp + 1 // the icon's callback message
	wmRefresh   = wmApp + 2 // state changed: redraw icon and tooltip
	wmBalloon   = wmApp + 3 // show the queued notification
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmContext   = 0x007B

	ninBalloonTimeout   = 0x0404
	ninBalloonUserClick = 0x0405

	nimAdd, nimModify, nimDelete = 0, 1, 2
	nifMessage, nifIcon, nifTip  = 0x1, 0x2, 0x4
	nifInfo, nifShowTip          = 0x10, 0x80
	niifInfo, niifRespectQuiet   = 0x1, 0x80

	mfString, mfGrayed, mfSeparator = 0x0, 0x1, 0x800
	tpmRightButton, tpmReturnCmd    = 0x2, 0x100
	tpmNoNotify                     = 0x80
)

// notifyIconData is NOTIFYICONDATAW.
type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GUIDItem         windows.GUID
	HBalloonIcon     windows.Handle
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type point struct{ X, Y int32 }

type msg struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
}

// Tray is the icon. Run owns the window and its message loop on one OS thread; Update, Notify and
// Close may be called from any goroutine.
type Tray struct {
	onAction func(Action, string)

	mu      sync.Mutex
	state   State
	hwnd    windows.Handle
	note    [2]string // queued notification: title, text
	icons   map[status.Icon]windows.Handle
	shown   status.Icon
	added   bool
	taskbar uint32 // the TaskbarCreated message id
}

// The window procedure is one Go callback for the process; it finds its tray here.
var (
	current   *Tray
	currentMu sync.Mutex
	wndProc   uintptr
	wndOnce   sync.Once
)

// New makes a tray. onAction is called on its own goroutine for every choice the host makes.
func New(onAction func(Action, string)) *Tray {
	return &Tray{onAction: onAction, icons: map[status.Icon]windows.Handle{}, shown: -1}
}

// Run shows the icon and runs the message loop until Close. ready, when not nil, is closed once the
// icon is up.
func (t *Tray) Run(ready chan<- struct{}) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	currentMu.Lock()
	if current != nil {
		currentMu.Unlock()
		return errors.New("tray: already running")
	}
	current = t
	currentMu.Unlock()
	defer func() {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
	}()

	wndOnce.Do(func() {
		wndProc = windows.NewCallback(func(hwnd windows.Handle, m uint32, wp, lp uintptr) uintptr {
			currentMu.Lock()
			tr := current
			currentMu.Unlock()
			if tr != nil {
				if handled, res := tr.handle(hwnd, m, wp, lp); handled {
					return res
				}
			}
			r, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(m), wp, lp)
			return r
		})
	})
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		return err
	}
	class, _ := windows.UTF16PtrFromString("CRAgentTray")
	wc := wndClassEx{LpfnWndProc: wndProc, HInstance: inst, LpszClassName: class}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))) // fails harmlessly when already registered
	title, _ := windows.UTF16PtrFromString("CR Agent")
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		return fmt.Errorf("tray: CreateWindowEx: %w", err)
	}
	tb, _ := windows.UTF16PtrFromString("TaskbarCreated")
	id, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(tb)))
	t.mu.Lock()
	t.hwnd, t.taskbar = windows.Handle(hwnd), uint32(id)
	t.mu.Unlock()

	if err := t.refresh(); err != nil {
		procDestroyWindow.Call(hwnd)
		return err
	}
	if ready != nil {
		close(ready)
	}
	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	t.mu.Lock()
	for _, h := range t.icons {
		procDestroyIcon.Call(uintptr(h))
	}
	t.icons, t.hwnd, t.added = map[status.Icon]windows.Handle{}, 0, false
	t.mu.Unlock()
	return nil
}

func (t *Tray) post(m uint32) {
	t.mu.Lock()
	hwnd := t.hwnd
	t.mu.Unlock()
	if hwnd != 0 {
		procPostMessage.Call(uintptr(hwnd), uintptr(m), 0, 0)
	}
}

// Update shows a new state.
func (t *Tray) Update(s State) {
	t.mu.Lock()
	t.state = s
	t.mu.Unlock()
	t.post(wmRefresh)
}

// Notify shows a Windows notification without buttons. A click on it comes back as ActNotification.
// An empty text removes a notification that is still showing.
func (t *Tray) Notify(title, text string) {
	t.mu.Lock()
	t.note = [2]string{title, text}
	t.mu.Unlock()
	t.post(wmBalloon)
}

// Close removes the icon and ends Run.
func (t *Tray) Close() { t.post(wmClose) }

func (t *Tray) act(a Action, arg string) {
	if t.onAction != nil && a != ActNone {
		go t.onAction(a, arg)
	}
}

func (t *Tray) handle(hwnd windows.Handle, m uint32, wp, lp uintptr) (bool, uintptr) {
	t.mu.Lock()
	taskbar := t.taskbar
	t.mu.Unlock()
	switch {
	case m == wmTray:
		switch uint32(lp) & 0xffff {
		case wmRButtonUp, wmContext, wmLButtonUp:
			t.popup(hwnd)
		case ninBalloonUserClick:
			t.act(ActNotification, "")
		}
		return true, 0
	case m == wmRefresh:
		t.refresh()
		return true, 0
	case m == wmBalloon:
		t.balloon()
		return true, 0
	case m == wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return true, 0
	case m == wmDestroy:
		nid := t.nid()
		procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
		procPostQuitMessage.Call(0)
		return true, 0
	case taskbar != 0 && m == taskbar: // Explorer restarted: the icon must be added again
		t.mu.Lock()
		t.added = false
		t.mu.Unlock()
		t.refresh()
		return true, 0
	}
	return false, 0
}

func (t *Tray) nid() notifyIconData {
	t.mu.Lock()
	defer t.mu.Unlock()
	nid := notifyIconData{HWnd: t.hwnd, UID: 1}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	return nid
}

func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		u = []uint16{0}
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func (t *Tray) icon(kind status.Icon) windows.Handle {
	if h, ok := t.icons[kind]; ok {
		return h
	}
	px := iconPixels(kind)
	color, _, _ := procCreateBitmap.Call(iconSize, iconSize, 1, 32, uintptr(unsafe.Pointer(&px[0])))
	maskBits := make([]byte, iconSize*iconSize/8)
	mask, _, _ := procCreateBitmap.Call(iconSize, iconSize, 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	ii := iconInfo{FIcon: 1, HbmMask: windows.Handle(mask), HbmColor: windows.Handle(color)}
	h, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(color)
	procDeleteObject.Call(mask)
	t.icons[kind] = windows.Handle(h)
	return windows.Handle(h)
}

// refresh adds the icon or updates its picture and tooltip. It runs on the window's thread.
func (t *Tray) refresh() error {
	nid := t.nid()
	t.mu.Lock()
	s, added := t.state, t.added
	nid.HIcon = t.icon(s.Icon)
	t.mu.Unlock()
	nid.UFlags = nifMessage | nifIcon | nifTip | nifShowTip
	nid.UCallbackMessage = wmTray
	copyUTF16(nid.SzTip[:], Tooltip(s))
	op := uintptr(nimModify)
	if !added {
		op = nimAdd
	}
	r, _, err := procShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		if added { // the shell lost the icon: add it again next time
			t.mu.Lock()
			t.added = false
			t.mu.Unlock()
			return nil
		}
		return fmt.Errorf("tray: Shell_NotifyIcon: %w", err)
	}
	t.mu.Lock()
	t.added, t.shown = true, s.Icon
	t.mu.Unlock()
	return nil
}

func (t *Tray) balloon() {
	nid := t.nid()
	t.mu.Lock()
	note := t.note
	t.mu.Unlock()
	nid.UFlags = nifInfo
	copyUTF16(nid.SzInfoTitle[:], note[0])
	copyUTF16(nid.SzInfo[:], note[1])
	nid.DwInfoFlags = niifInfo | niifRespectQuiet
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// popup shows the menu at the pointer and acts on the choice.
func (t *Tray) popup(hwnd windows.Handle) {
	t.mu.Lock()
	items := Menu(t.state)
	t.mu.Unlock()
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	for i, it := range items {
		if it.Separator {
			procAppendMenu.Call(menu, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.Disabled {
			flags |= mfGrayed
		}
		label, _ := windows.UTF16PtrFromString(it.Label)
		procAppendMenu.Call(menu, flags, uintptr(i+1), uintptr(unsafe.Pointer(label)))
	}
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(uintptr(hwnd)) // or the menu would not close on a click elsewhere
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd|tpmNoNotify, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	procPostMessage.Call(uintptr(hwnd), wmNull, 0, 0)
	t.choose(items, int(cmd))
}

// choose acts on a menu command id (1-based index into items; 0 = nothing chosen).
func (t *Tray) choose(items []Item, cmd int) {
	if cmd < 1 || cmd > len(items) {
		return
	}
	if it := items[cmd-1]; !it.Disabled && !it.Separator {
		t.act(it.Action, it.Arg)
	}
}
