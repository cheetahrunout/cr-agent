package approve

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/models"
)

type rec struct {
	mu      sync.Mutex
	notes   []string
	changes int
}

func (r *rec) notify(title, _ string) {
	r.mu.Lock()
	r.notes = append(r.notes, title)
	r.mu.Unlock()
}

func (r *rec) changed() {
	r.mu.Lock()
	r.changes++
	r.mu.Unlock()
}

func (r *rec) lastNote() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.notes) == 0 {
		return "none"
	}
	return r.notes[len(r.notes)-1]
}

func waitPending(t *testing.T, q *Queue, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(q.Pending()) != n {
		if time.Now().After(deadline) {
			t.Fatalf("pending = %d, want %d", len(q.Pending()), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQueue(t *testing.T) {
	r := &rec{}
	answers := make(chan bool, 4)
	var dialogs atomic.Int32
	q := &Queue{Notify: r.notify, Changed: r.changed, Dialog: func(ctx context.Context, p models.Prompt) (bool, bool) {
		dialogs.Add(1)
		select {
		case a := <-answers:
			return a, true
		case <-ctx.Done():
			return false, false
		}
	}}
	p := models.Prompt{ModelID: "wizard-x", Repo: "wizard-org/Wizard-X-GGUF", SizeBytes: 8 << 30}

	// Allow from the tray menu item.
	res := make(chan bool, 1)
	go func() { res <- q.Ask(context.Background(), p) }()
	waitPending(t, q, 1)
	if got := q.Pending()[0]; got.ID != "wizard-x" || got.Label != "wizard-org/Wizard-X-GGUF (8.0 GB)" {
		t.Fatalf("pending %+v", got)
	}
	if r.lastNote() != "A download is waiting for you" {
		t.Fatalf("notification %q", r.lastNote())
	}
	answers <- true
	q.Open("wizard-x")
	if !<-res || len(q.Pending()) != 0 || r.lastNote() != "" {
		t.Fatalf("allow: pending %d, last note %q", len(q.Pending()), r.lastNote())
	}

	// Deny from a click on the notification (no id: the oldest).
	go func() { res <- q.Ask(context.Background(), p) }()
	waitPending(t, q, 1)
	answers <- false
	q.Open("")
	if <-res {
		t.Fatal("deny came back as allow")
	}

	// Nobody answers: the deadline is a Deny, and the pending item and the notification go away.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() { res <- q.Ask(ctx, p) }()
	waitPending(t, q, 1)
	if <-res || len(q.Pending()) != 0 || r.lastNote() != "" {
		t.Fatal("an unanswered prompt did not end as a Deny")
	}

	// The deadline passes while the dialog is open: still a Deny, and a double click opens no second dialog.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	before := dialogs.Load()
	go func() { res <- q.Ask(ctx2, p) }()
	waitPending(t, q, 1)
	go q.Open("wizard-x")
	time.Sleep(20 * time.Millisecond)
	q.Open("wizard-x") // the dialog is already up
	if <-res {
		t.Fatal("deadline with the dialog open came back as allow")
	}
	if n := dialogs.Load() - before; n != 1 {
		t.Fatalf("%d dialogs for one prompt", n)
	}
	q.Open("nothing-waits") // harmless
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changes == 0 {
		t.Fatal("the tray was never told the list changed")
	}
}
