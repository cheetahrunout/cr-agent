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
	ActInstallUpdate // the verified latest version; arg is the version
	ActSandbox       // the host's choice between the AppContainer and low integrity
	ActPending       // a pending approval was chosen from the menu; arg is its id
	ActNotification  // the notification was clicked
	ActAddModels     // the host wants to add GGUF files from the model folder
)

// Pending is a download waiting for the host's Allow or Deny.
type Pending struct {
	ID    string
	Label string // built by the agent from its own Hugging Face lookup
}

// State is everything the tray shows.
type State struct {
	Status    string
	Icon      status.Icon
	Paired    bool
	Paused    bool
	Pending   []Pending
	ModelsDir string
	HFToken   string // absent | present | invalid
	Version   string
	// UpdateAvailable is the version the agent's own daily check verified, never one the hub named.
	UpdateAvailable string
	SandboxBlocked  bool // the AppContainer does not work here: offer the opt-in
	LowIntegrity    bool // the host chose low integrity: say so, and offer the way back
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
	items := []Item{
		{Label: "CR Agent", Disabled: true},
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
	if s.UpdateAvailable != "" {
		items = append(items, Item{Label: "Install update " + s.UpdateAvailable + "…", Action: ActInstallUpdate, Arg: s.UpdateAvailable})
	}
	switch {
	case s.LowIntegrity:
		items = append(items, Item{Label: "Low integrity: the engine can read your files. Use its sandbox again…", Action: ActSandbox})
	case s.SandboxBlocked:
		items = append(items, Item{Label: "The engine's sandbox does not work on this PC: use low integrity…", Action: ActSandbox})
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
		Item{Label: "Add model files…", Action: ActAddModels},
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
	t := "CR Agent\n" + s.Status
	if r := []rune(t); len(r) > 127 {
		t = string(r[:126]) + "…"
	}
	return t
}
