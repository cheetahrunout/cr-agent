//go:build windows

package tray

import (
	"os"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/status"
)

var procSendMessage = user32.NewProc("SendMessageW")

// The real icon: added to the notification area, updated, clicked through its callback message, removed.
func TestTrayLifecycle(t *testing.T) {
	if os.Getenv("CI") == "" && os.Getenv("CRAGENT_TEST_DESKTOP") == "" {
		t.Skip("this puts an icon and a notification on the desktop, so it runs only on CI or with CRAGENT_TEST_DESKTOP=1")
	}
	type choice struct {
		a   Action
		arg string
	}
	acts := make(chan choice, 8)
	tr := New(func(a Action, arg string) { acts <- choice{a, arg} })
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- tr.Run(ready) }()
	select {
	case <-ready:
	case err := <-done:
		t.Skipf("no notification area in this session: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("tray did not start")
	}
	for _, icon := range []status.Icon{status.IconRing, status.IconGreen, status.IconAmber, status.IconGrey, status.IconGreyBang} {
		tr.Update(State{Status: "Connected · sharing · no model loaded", Icon: icon, Paired: true, Version: "test"})
		deadline := time.Now().Add(2 * time.Second) // updates coalesce, so wait for each to be drawn
		for {
			tr.mu.Lock()
			shown := tr.shown
			tr.mu.Unlock()
			if shown == icon {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("icon %d never shown", icon)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	tr.Notify("CR Agent", "A download is waiting for you.")
	tr.Notify("", "") // and take it away again
	time.Sleep(100 * time.Millisecond)
	tr.mu.Lock()
	hwnd, added, icons := tr.hwnd, tr.added, len(tr.icons)
	tr.mu.Unlock()
	if !added || icons != 5 {
		t.Fatalf("icon added=%v, %d icons made", added, icons)
	}

	// A click on the notification arrives as the icon's callback message.
	procSendMessage.Call(uintptr(hwnd), wmTray, 0, ninBalloonUserClick)
	select {
	case c := <-acts:
		if c.a != ActNotification {
			t.Fatalf("got action %d", c.a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification click lost")
	}
	// A timed-out notification is not a click.
	procSendMessage.Call(uintptr(hwnd), wmTray, 0, ninBalloonTimeout)

	// Menu commands map to actions; disabled rows and separators do nothing.
	items := Menu(State{Paired: true, Pending: []Pending{{ID: "m1", Label: "x"}}})
	for i, it := range items {
		tr.choose(items, i+1)
		if it.Separator || it.Disabled {
			continue
		}
		select {
		case c := <-acts:
			if c.a != it.Action || c.arg != it.Arg {
				t.Fatalf("row %q → action %d arg %q", it.Label, c.a, c.arg)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("row %q did nothing", it.Label)
		}
	}
	tr.choose(items, 0)
	tr.choose(items, 99)
	select {
	case c := <-acts:
		t.Fatalf("unexpected action %d", c.a)
	case <-time.After(50 * time.Millisecond):
	}

	tr.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tray did not stop")
	}
	// It can be started again (the same window class).
	ready2 := make(chan struct{})
	go func() { done <- tr.Run(ready2) }()
	select {
	case <-ready2:
		tr.Close()
		<-done
	case err := <-done:
		t.Fatalf("second run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second run did not start")
	}
}
