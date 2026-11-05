// Package status turns the agent's state into the tray's one status line and icon. The order is
// ruling B30's precedence as far as the PC can know it: removed > not connected for a stated reason >
// paused > reconnecting > update required > engine error > occupancy. (Who is using the machine and
// "owner not a host" are known only to the hub.)
package status

import (
	"fmt"

	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Icon is the tray icon's shape (§3.5.6): shapes differ so colour is never the only signal.
type Icon int

// Icons.
const (
	IconGrey     Icon = iota // paused or not paired
	IconGreyBang             // update required, key not accepted, removed, engine error
	IconRing                 // connecting
	IconGreen                // connected, available
	IconAmber                // in use
)

// Input is what the status line is made from.
type Input struct {
	Paired      bool
	Link        link.Phase
	Mode        string // normal | update_required
	Paused      bool
	Engine      string // protocol engine state
	ModelID     string
	Inflight    int
	Downloading bool
	DownloadPct float64
}

// Line returns the status text and icon.
func Line(in Input) (string, Icon) {
	switch {
	case !in.Paired:
		return "Not paired", IconGrey
	case in.Link == link.PhaseRemoved:
		return "This PC was removed from the hub", IconGreyBang
	case in.Link == link.PhaseKeyRejected:
		return "Key not accepted · pair again", IconGreyBang
	case in.Link == link.PhaseReplaced:
		return "Not connected · this PC's key is in use elsewhere", IconGreyBang
	case in.Link == link.PhaseUpdateRequired:
		return "Update required", IconGreyBang
	case in.Paused:
		return "Paused · not sharing", IconGrey
	case in.Link == link.PhaseDown:
		return "Reconnecting…", IconRing
	case in.Link != link.PhaseConnected:
		return "Connecting…", IconRing
	case in.Mode == "update_required":
		return "Update required", IconGreyBang
	case in.Engine == protocol.EngineError:
		return "Connected · engine error", IconGreyBang
	}
	text, icon := "Connected · sharing · no model loaded", IconGreen
	switch {
	case in.Inflight > 0:
		text, icon = "Connected · sharing · in use", IconAmber
	case in.Engine == protocol.EngineLoading:
		text, icon = "Connected · loading "+in.ModelID, IconAmber
	case in.Engine == protocol.EngineLoaded:
		text = "Connected · sharing · " + in.ModelID + " loaded"
	}
	if in.Downloading {
		text += fmt.Sprintf(" · downloading %.0f %%", in.DownloadPct)
	}
	return text, icon
}
