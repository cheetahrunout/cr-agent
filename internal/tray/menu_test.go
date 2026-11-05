package tray

import (
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/status"
)

func labels(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		switch {
		case it.Separator:
			b.WriteString("---\n")
		case it.Disabled:
			b.WriteString("(" + it.Label + ")\n")
		default:
			b.WriteString(it.Label + "\n")
		}
	}
	return b.String()
}

func TestMenu(t *testing.T) {
	unpaired := labels(Menu(State{Status: "Not paired", ModelsDir: `D:\Models`, Version: "0.1.0", HFToken: "absent"}))
	want := "(CR Agent)\n(Not paired)\n---\n(Pause hosting (frees your GPU))\n---\nPair with the hub…\nHugging Face token…   not set\n" +
		"Model folder…   D:\\Models\n---\nOpen hub\nOpen log\nAbout · 0.1.0\nQuit (machine goes offline)\n"
	if unpaired != want {
		t.Fatalf("unpaired menu:\n%s\nwant:\n%s", unpaired, want)
	}
	s := State{Slug: "anna-tower", Status: "Connected · sharing · in use", Paired: true, ModelsDir: `D:\Models`, Version: "0.1.0", HFToken: "present",
		Pending: []Pending{{ID: "wizard-x", Label: "wizard-org/Wizard-X-GGUF (8.0 GB)"}}}
	items := Menu(s)
	got := labels(items)
	for _, part := range []string{"(CR Agent · anna-tower)", "(Connected · sharing · in use)", "\nPause hosting (frees your GPU)\n",
		"Waiting for you: wizard-org/Wizard-X-GGUF (8.0 GB)…", "Pair again…", "Hugging Face token…   set"} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in\n%s", part, got)
		}
	}
	s.Paused, s.HFToken = true, "invalid"
	got = labels(Menu(s))
	if !strings.Contains(got, "\nResume hosting\n") || strings.Contains(got, "Pause hosting") || !strings.Contains(got, "not accepted") {
		t.Errorf("paused menu:\n%s", got)
	}
	// Every enabled row has an action; the pending row carries its id.
	for _, it := range items {
		if !it.Separator && !it.Disabled && it.Action == ActNone {
			t.Errorf("row %q does nothing", it.Label)
		}
		if it.Action == ActPending && it.Arg != "wizard-x" {
			t.Errorf("pending row arg %q", it.Arg)
		}
	}
}

func TestTooltipFits(t *testing.T) {
	tip := Tooltip(State{Slug: "anna-tower", Status: strings.Repeat("long status ", 20)})
	if n := len([]rune(tip)); n > 127 {
		t.Fatalf("tooltip of %d characters", n)
	}
	if !strings.HasPrefix(tip, "CR Agent · anna-tower\n") {
		t.Fatalf("tooltip %q", tip)
	}
}

// Each state has its own shape, so the icon reads without colour.
func TestIconsDiffer(t *testing.T) {
	kinds := []status.Icon{status.IconGrey, status.IconGreyBang, status.IconRing, status.IconGreen, status.IconAmber}
	seen := map[string]status.Icon{}
	for _, k := range kinds {
		px := iconPixels(k)
		if len(px) != iconSize*iconSize*4 {
			t.Fatalf("icon %d: %d bytes", k, len(px))
		}
		// The alpha channel alone is the shape.
		shape := make([]byte, 0, iconSize*iconSize)
		opaque := 0
		for i := 3; i < len(px); i += 4 {
			shape = append(shape, px[i])
			if px[i] > 128 {
				opaque++
			}
			if px[i-1] > px[i] || px[i-2] > px[i] || px[i-3] > px[i] {
				t.Fatalf("icon %d: colour above alpha (not premultiplied)", k)
			}
		}
		if opaque < 60 || px[3] != 0 {
			t.Fatalf("icon %d: %d opaque pixels, corner alpha %d", k, opaque, px[3])
		}
		if k != status.IconGreyBang { // the exclamation mark differs by colour inside a filled dot, as the green dot does by hue
			if other, dup := seen[string(shape)]; dup {
				t.Fatalf("icons %d and %d have the same shape", k, other)
			}
			seen[string(shape)] = k
		}
	}
}
