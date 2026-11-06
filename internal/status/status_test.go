package status

import (
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// The precedence list, enumerated: each row sets everything below it too, and the higher part wins.
func TestPrecedence(t *testing.T) {
	all := Input{Paired: true, Link: link.PhaseConnected, Mode: "update_required", Paused: true,
		Engine: protocol.EngineError, ModelID: "hub-text", Inflight: 1, Downloading: true, DownloadPct: 50}
	steps := []struct {
		name string
		set  func(*Input)
		want string
		icon Icon
	}{
		{"not paired", func(i *Input) { i.Paired = false; i.Link = link.PhaseRemoved }, "Not paired", IconGrey},
		{"removed", func(i *Input) { i.Link = link.PhaseRemoved }, "removed from the hub", IconGreyBang},
		{"key rejected", func(i *Input) { i.Link = link.PhaseKeyRejected }, "Key not accepted", IconGreyBang},
		{"replaced", func(i *Input) { i.Link = link.PhaseReplaced }, "in use elsewhere", IconGreyBang},
		{"protocol too old", func(i *Input) { i.Link = link.PhaseUpdateRequired }, "Update required", IconGreyBang},
		{"paused beats reconnecting", func(i *Input) { i.Link = link.PhaseDown }, "Paused", IconGrey},
		{"paused beats update and engine error", func(i *Input) {}, "Paused", IconGrey},
		{"reconnecting", func(i *Input) { i.Paused = false; i.Link = link.PhaseDown }, "Reconnecting", IconRing},
		{"connecting", func(i *Input) { i.Paused = false; i.Link = link.PhaseConnecting }, "Connecting", IconRing},
		{"update required beats engine error", func(i *Input) { i.Paused = false }, "Update required", IconGreyBang},
		{"engine error beats occupancy", func(i *Input) { i.Paused = false; i.Mode = "normal" }, "engine error", IconGreyBang},
		{"in use", func(i *Input) { i.Paused = false; i.Mode = "normal"; i.Engine = protocol.EngineLoaded }, "in use · downloading 50 %", IconAmber},
		{"loading", func(i *Input) {
			i.Paused, i.Mode, i.Engine, i.Inflight, i.Downloading = false, "normal", protocol.EngineLoading, 0, false
		}, "loading a model", IconAmber},
		{"loaded", func(i *Input) {
			i.Paused, i.Mode, i.Engine, i.Inflight, i.Downloading = false, "normal", protocol.EngineLoaded, 0, false
		}, "model loaded", IconGreen},
		{"idle", func(i *Input) {
			i.Paused, i.Mode, i.Engine, i.Inflight, i.Downloading = false, "normal", protocol.EngineUnloaded, 0, false
		}, "no model loaded", IconGreen},
	}
	for _, s := range steps {
		in := all
		s.set(&in)
		text, icon := Line(in)
		if !strings.Contains(text, s.want) || icon != s.icon {
			t.Errorf("%s: got %q icon %d, want %q icon %d", s.name, text, icon, s.want, s.icon)
		}
		if strings.Contains(text, "hub-text") { // ModelID comes from the hub (finding L2)
			t.Errorf("%s: hub text in the tray: %q", s.name, text)
		}
		if len([]rune(text)) > 120 {
			t.Errorf("%s: %d characters do not fit a tray tooltip", s.name, len([]rune(text)))
		}
	}
}
