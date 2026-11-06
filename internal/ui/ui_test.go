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
	p := models.Prompt{Repo: "wizard-org/Wizard-X-GGUF", Commit: "7c1e2a9d40b3f1e8a9c0d2b4e6f8a1c3e5d7f9b2", Files: []string{"Wizard-X-Q4_K_M.gguf"}, SizeBytes: 8 << 30}
	text := ApprovalText(p)
	for _, part := range []string{"not on the built-in list", "wizard-org/Wizard-X-GGUF", "Wizard-X-Q4_K_M.gguf\n8.0 GB · commit 7c1e2a9d40b3", "only if you asked for this in the hub"} {
		if !strings.Contains(text, part) {
			t.Errorf("missing %q in %q", part, text)
		}
	}
	p.AllowedBefore = true
	p.Files = []string{"a.gguf", "b.gguf", "c.gguf", "d.gguf", "e.gguf"}
	text = ApprovalText(p)
	if !strings.Contains(text, "new version of a repo you allowed before") || !strings.Contains(text, "a.gguf\nb.gguf\nc.gguf\nd.gguf\ne.gguf\n") {
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

func TestAddFilesTexts(t *testing.T) {
	long := "Qwen3.6-Very-Long-Model-Name-With-Many-Words-Q4_K_XL.gguf"
	scan := models.Scan{Dir: `C:\\models`,
		Found:   []models.Found{{Key: long, Files: []string{long}, Size: 18 << 30}, {Key: "big-00001-of-00002.gguf", Files: []string{"big-00001-of-00002.gguf", "big-00002-of-00002.gguf"}, Size: 2 << 30}},
		Refused: []models.Refused{{Name: "evil\u202egnp.gguf\nsecond line", Why: "its name has characters the hub cannot carry"}}}
	row := FoundRow(scan.Found[0])
	if !strings.HasPrefix(row, "18.0 GB · ") || !strings.HasSuffix(row, "Q4_K_XL.gguf") || len([]rune(row)) > 40 {
		t.Fatalf("row %q", row)
	}
	if row := FoundRow(scan.Found[1]); row != "2.0 GB · big (2 parts)" {
		t.Fatalf("row %q", row)
	}
	text := RefusedText(scan)
	if strings.ContainsRune(text, 0x202e) || strings.Contains(text, "\nsecond line") {
		t.Fatalf("a file name steered the text: %q", text)
	}
	conf := ConfirmAddText(scan, []string{long})
	if !strings.Contains(conf, long) || !strings.Contains(conf, "18.0 GB") || strings.Contains(conf, "big-") {
		t.Fatalf("confirm %q", conf)
	}
	// Two names that shorten to one row stay two rows.
	a, b := "Same-Start-Of-A-Long-Name-Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-Q4.gguf", "Same-Start-Of-A-Long-Name-Bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-Q4.gguf"
	rows, key := listRows(models.Scan{Found: []models.Found{{Key: a, Files: []string{a}, Size: 1}, {Key: b, Files: []string{b}, Size: 1}}})
	if len(rows) != 2 || len(key) != 2 || key[rows[0]] != a || key[rows[1]] != b {
		t.Fatalf("rows %q", rows)
	}
	res := ImportResultText([]models.ImportResult{{Key: "a.gguf", ModelID: "a"}, {Key: "b.gguf", Err: "cancelled"}})
	if !strings.Contains(res, "a.gguf") || !strings.Contains(res, "b.gguf: cancelled") {
		t.Fatalf("result %q", res)
	}
}

// Two long names that differ only at the end: the confirmation shows each in full, and each list row
// ends as its real name ends.
func TestLongNamesStayApart(t *testing.T) {
	long := strings.Repeat("a", 90)
	safeName, evilName := long+"-safe.gguf", long+"-evil.gguf"
	s := models.Scan{Found: []models.Found{{Key: safeName, Files: []string{safeName}, Size: 1 << 30}, {Key: evilName, Files: []string{evilName}, Size: 1 << 30}}}
	text := ConfirmAddText(s, []string{safeName, evilName})
	if !strings.Contains(text, safeName) || !strings.Contains(text, evilName) {
		t.Fatalf("the confirmation cut a name: %s", text)
	}
	rows, _ := listRows(s)
	if !strings.HasSuffix(rows[0], "-safe.gguf") || !strings.HasSuffix(rows[1], "-evil.gguf") {
		t.Fatalf("rows %q", rows)
	}
	res := ImportResultText([]models.ImportResult{{Key: safeName}, {Key: evilName, Err: "cancelled"}})
	if !strings.Contains(res, safeName) || !strings.Contains(res, evilName) {
		t.Fatalf("the result cut a name: %s", res)
	}
	// Control and formatting characters stay out of every one of them, and a shortened refusal keeps the end.
	bad := "evil\u202egnp\n" + long + "-end.gguf"
	for _, got := range []string{ConfirmAddText(models.Scan{Found: []models.Found{{Key: bad, Files: []string{bad}, Size: 1}}}, []string{bad}), RefusedText(models.Scan{Refused: []models.Refused{{Name: bad, Why: "x"}}})} {
		if strings.ContainsRune(got, 0x202e) || !strings.Contains(got, "-end.gguf") {
			t.Fatalf("%q", got)
		}
	}
}
