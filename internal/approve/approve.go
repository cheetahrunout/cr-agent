// Package approve is the tray build's models.Approver (§9.2, rulings B13 and B14): a download from a
// publisher outside the allowlist raises a notification without buttons and a pending item in the tray
// menu; clicking either opens the agent's own Allow/Deny dialog; no answer by the deadline is a Deny.
package approve

import (
	"context"
	"sync"

	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/tray"
	"github.com/cheetahrunout/cr-agent/internal/ui"
)

type ask struct {
	id      string
	prompt  models.Prompt
	ctx     context.Context
	decided chan bool
	open    bool
}

// Queue holds the approvals waiting for the host.
type Queue struct {
	// Notify shows a notification; an empty text takes it away.
	Notify func(title, text string)
	// Changed is called when the pending list changed (the tray rebuilds its menu).
	Changed func()
	// Dialog asks the host; answered is false when the dialog went away unanswered.
	Dialog func(ctx context.Context, p models.Prompt) (allow, answered bool)

	mu      sync.Mutex
	pending []*ask
}

// New makes a queue with the native dialog.
func New(notify func(title, text string), changed func()) *Queue {
	return &Queue{Notify: notify, Changed: changed, Dialog: ui.AllowDownload}
}

// Ask implements models.Approver. It blocks until the host answers or ctx ends.
func (q *Queue) Ask(ctx context.Context, p models.Prompt) bool {
	a := &ask{id: p.ModelID, prompt: p, ctx: ctx, decided: make(chan bool, 1)}
	q.mu.Lock()
	q.pending = append(q.pending, a)
	q.mu.Unlock()
	q.Changed()
	q.Notify("A download is waiting for you", ui.ApprovalLabel(p)+"\nClick here, or use the CR Agent tray menu, to allow or deny it.")
	allowed := false
	select {
	case allowed = <-a.decided:
	case <-ctx.Done():
	}
	q.mu.Lock()
	for i, x := range q.pending {
		if x == a {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			break
		}
	}
	left := len(q.pending)
	q.mu.Unlock()
	if left == 0 {
		q.Notify("", "") // nothing waits any more: take the notification away
	}
	q.Changed()
	return allowed
}

// Pending lists the waiting approvals for the tray menu.
func (q *Queue) Pending() []tray.Pending {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]tray.Pending, 0, len(q.pending))
	for _, a := range q.pending {
		out = append(out, tray.Pending{ID: a.id, Label: ui.ApprovalLabel(a.prompt)})
	}
	return out
}

// Open shows the Allow/Deny dialog for a pending approval (id "" = the oldest). A second click while
// the dialog is up does nothing.
func (q *Queue) Open(id string) {
	q.mu.Lock()
	var a *ask
	for _, x := range q.pending {
		if (id == "" || x.id == id) && !x.open {
			a = x
			break
		}
	}
	if a != nil {
		a.open = true
	}
	q.mu.Unlock()
	if a == nil {
		return
	}
	allow, answered := q.Dialog(a.ctx, a.prompt)
	if answered {
		a.decided <- allow
		return
	}
	q.mu.Lock()
	a.open = false
	q.mu.Unlock()
}
