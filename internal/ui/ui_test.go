package ui

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/models"
)

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{8 << 30: "8.0 GB", 9_663_676_416: "9.0 GB", 386 << 20: "386 MB", 1500: "2 KB", 0: "0 KB"} {
		if got := HumanSize(n); got != want {
			t.Errorf("%d → %q, want %q", n, got, want)
		}
	}
}

func TestApprovalText(t *testing.T) {
	p := models.Prompt{Repo: "wizard-org/Wizard-X-GGUF", Files: []string{"Wizard-X-Q4_K_M.gguf"}, SizeBytes: 8 << 30}
	text := ApprovalText(p)
	for _, part := range []string{"not on the built-in list", "wizard-org/Wizard-X-GGUF", "Wizard-X-Q4_K_M.gguf · 8.0 GB", "only if you asked for this in the hub"} {
		if !strings.Contains(text, part) {
			t.Errorf("missing %q in %q", part, text)
		}
	}
	p.AllowedBefore = true
	p.Files = []string{"a.gguf", "b.gguf", "c.gguf", "d.gguf", "e.gguf"}
	text = ApprovalText(p)
	if !strings.Contains(text, "new version of a repo you allowed before") || !strings.Contains(text, "and 2 more files") || strings.Contains(text, "d.gguf") {
		t.Errorf("repeat prompt: %q", text)
	}
	if ApprovalLabel(p) != "wizard-org/Wizard-X-GGUF (8.0 GB)" {
		t.Errorf("label %q", ApprovalLabel(p))
	}
}

// The real dialog: nobody answers, the deadline passes, the dialog closes itself, and that is a Deny.
func TestAllowDownloadDeadline(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native dialog: Windows only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	allow, answered := AllowDownload(ctx, models.Prompt{Repo: "test-org/Test-GGUF", Files: []string{"Test.gguf"}, SizeBytes: 1 << 30})
	if allow || answered {
		t.Fatalf("allow=%v answered=%v for a dialog nobody touched", allow, answered)
	}
	if el := time.Since(t0); el < 300*time.Millisecond || el > 5*time.Second {
		t.Fatalf("dialog lasted %v", el)
	}
}
