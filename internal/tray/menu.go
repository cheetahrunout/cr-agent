// Package tray is the agent's one tray icon (§3.5.6, §9.1): a status line, Pause/Resume hosting, the
// pending download approvals, the three local inputs (pairing, Hugging Face token, model folder),
// Open hub, Open log, About and Quit. It is written on the Win32 calls of golang.org/x/sys/windows:
// no CGO and no tray library, because the click on a notification must arrive in this process's own
// window (ruling B14: no protocol handler and no COM activator, which a web page could forge).
package tray

import (
	"fmt"

	"github.com/cheetahrunout/cr-agent/internal/status"
)

// Action is something the host chose.
type Action int

// Actions.
const (
	ActNone Action = iota
	ActPauseToggle
	ActPair
	ActHFToken
	ActModelFolder
	ActOpenHub
	ActOpenLog
	ActAbout
	ActQuit
	ActPending      // a pending approval was chosen from the menu; arg is its id
	ActNotification // the notification was clicked
)

// Pending is a download waiting for the host's Allow or Deny.
type Pending struct {
	ID    string
	Label string // built by the agent from its own Hugging Face lookup
}

// State is everything the tray shows.
type State struct {
	Slug      string // empty when not paired
	Status    string
	Icon      status.Icon
	Paired    bool
	Paused    bool
	Pending   []Pending
	ModelsDir string
	HFToken   string // absent | present | invalid
	Version   string
}

// Item is one menu row.
type Item struct {
	Label     string
	Action    Action
	Arg       string
	Disabled  bool
	Separator bool
}

// Menu builds the menu for a state. The wording follows the mockup in §3.5.6.
func Menu(s State) []Item {
	title := "CR Agent"
	if s.Slug != "" {
		title += " · " + s.Slug
	}
	items := []Item{
		{Label: title, Disabled: true},
		{Label: s.Status, Disabled: true},
		{Separator: true},
	}
	if s.Paused {
		items = append(items, Item{Label: "Resume hosting", Action: ActPauseToggle})
	} else {
		items = append(items, Item{Label: "Pause hosting (frees your GPU)", Action: ActPauseToggle, Disabled: !s.Paired})
	}
	for _, p := range s.Pending {
		items = append(items, Item{Label: "Waiting for you: " + p.Label + "…", Action: ActPending, Arg: p.ID})
	}
	items = append(items, Item{Separator: true})
	if s.Paired {
		items = append(items, Item{Label: "Pair again…", Action: ActPair})
	} else {
		items = append(items, Item{Label: "Pair with the hub…", Action: ActPair})
	}
	token := "not set"
	switch s.HFToken {
	case "present":
		token = "set"
	case "invalid":
		token = "not accepted"
	}
	items = append(items,
		Item{Label: "Hugging Face token…   " + token, Action: ActHFToken},
		Item{Label: "Model folder…   " + s.ModelsDir, Action: ActModelFolder},
		Item{Separator: true},
		Item{Label: "Open hub", Action: ActOpenHub},
		Item{Label: "Open log", Action: ActOpenLog},
		Item{Label: fmt.Sprintf("About · %s", s.Version), Action: ActAbout},
		Item{Label: "Quit (machine goes offline)", Action: ActQuit},
	)
	return items
}

// Tooltip is the icon's hover text, within Windows' 127 characters.
func Tooltip(s State) string {
	t := "CR Agent"
	if s.Slug != "" {
		t += " · " + s.Slug
	}
	t += "\n" + s.Status
	if r := []rune(t); len(r) > 127 {
		t = string(r[:126]) + "…"
	}
	return t
}
