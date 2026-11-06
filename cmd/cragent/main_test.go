package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf/gguftest"
	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/updater"
)

// The rollback test's switch names one version: any other value, or the same link flag on another
// version, leaves a healthy build.
func TestNeverHealthy(t *testing.T) {
	sv, vv := sabotage, version
	defer func() { sabotage, version = sv, vv }()
	version = "0.1.2"
	for s, want := range map[string]bool{"": false, "unhealthy": false, "unhealthy-0.1.1": false, "unhealthy-0.1.2": true, "UNHEALTHY-0.1.2": false} {
		sabotage = s
		if neverHealthy() != want {
			t.Errorf("sabotage %q on 0.1.2: %v", s, !want)
		}
	}
	version = "0.1.3"
	sabotage = "unhealthy-0.1.2"
	if neverHealthy() {
		t.Error("a later version linked with the old flag is unhealthy")
	}
}

func TestBootstrapDelay(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, time.Hour, time.Hour}
	for i, d := range want {
		if got := bootstrapDelay(i); got != d {
			t.Errorf("try %d: %v", i, got)
		}
	}
}

func TestDeriveID(t *testing.T) {
	for in, want := range map[string]string{
		"smollm2-360m-q8_0.gguf": "smollm2-360m-q8_0", "Qwen3 14B (Q4_K_M).gguf": "qwen3-14b--q4_k_m-", "_x.gguf": "x",
	} {
		got := deriveID(in)
		if got != want || !protocol.ValidModelID(got) {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if len(deriveID(strings.Repeat("a", 200)+".gguf")) != 80 {
		t.Error("id not capped at 80")
	}
}

func TestSingleInstance(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("named mutex: Windows only")
	}
	home := t.TempDir()
	release, running := singleInstance(home)
	if running {
		t.Fatal("first instance refused")
	}
	if _, again := singleInstance(strings.ToUpper(home)); !again {
		t.Fatal("a second agent on the same folder was allowed")
	}
	if rel2, other := singleInstance(t.TempDir()); other {
		t.Fatal("another folder was refused")
	} else {
		rel2()
	}
	release()
	rel3, again := singleInstance(home)
	if again {
		t.Fatal("the folder stayed locked after release")
	}
	rel3()
}

func TestLogRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	os.WriteFile(path, make([]byte, 11<<20), 0o600)
	w := newLogWriter(path)
	w.Write([]byte("line\n"))
	if st, _ := os.Stat(path); st.Size() != 5 {
		t.Fatalf("new log has %d bytes", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != 11<<20 {
		t.Fatalf("old log not kept: %v", err)
	}
	w.Close()
}

// Finding L4: the log rotates while the agent runs, not only at its start.
func TestLogRotatesInTheWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w := newLogWriter(path)
	defer w.Close()
	line := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 25; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	a, err1 := os.Stat(path)
	b, err2 := os.Stat(path + ".1")
	if err1 != nil || err2 != nil || a.Size() > maxLogSize || b.Size() > maxLogSize || a.Size()+b.Size() > 2*maxLogSize {
		t.Fatalf("log %v (%v), old log %v (%v)", a, err1, b, err2)
	}
}

// Finding L7: every flag that points the agent at another engine, key or model, or weakens the
// sandbox, is named, and a release build refuses to start with any of them.
func TestDebugOnlyFlags(t *testing.T) {
	clean := options{approve: "ask", lowIL: true}
	if set := debugOnlyFlags(clean); len(set) != 0 {
		t.Fatalf("a plain start names %v", set)
	}
	for name, o := range map[string]options{
		"--engine":       {approve: "ask", lowIL: true, engine: "x"},
		"--key":          {approve: "ask", lowIL: true, key: "x"},
		"--model":        {approve: "ask", lowIL: true, model: "x"},
		"--model-id":     {approve: "ask", lowIL: true, modelID: "x"},
		"--load":         {approve: "ask", lowIL: true, load: true},
		"--low-il=false": {approve: "ask"},
		"--no-mem-cap":   {approve: "ask", lowIL: true, noMemCap: true},
		"--hf-base":      {approve: "ask", lowIL: true, hfBase: "x"},
		"--release-base": {approve: "ask", lowIL: true, releaseBase: "x"},
		"--approve":      {approve: "allow", lowIL: true},
	} {
		if set := debugOnlyFlags(o); len(set) != 1 || set[0] != name {
			t.Errorf("%s: %v", name, set)
		}
	}
	t.Setenv("CRAGENT_ENGINE", `C:\evil`)
	t.Setenv("CRAGENT_KEY", "crm_x_y")
	if got := devEnv("CRAGENT_ENGINE"); debugBuild != (got != "") {
		t.Fatalf("devEnv %q in a build with debugBuild=%v", got, debugBuild)
	}
}

func TestLatestDelay(t *testing.T) {
	if d := latestDelay(0); d != 24*time.Hour {
		t.Fatal(d)
	}
	if d := latestDelay(int64(30 * time.Minute)); d != 24*time.Hour+30*time.Minute {
		t.Fatal(d)
	}
}

func TestStartFailure(t *testing.T) {
	passing := errors.New("model folder: the folder cannot be created")
	state := fmt.Errorf("install check: %w: damaged", updater.ErrStateUnreadable)
	for _, c := range []struct {
		name             string
		err              error
		managed, onTrial bool
		dialog, advice   bool
		code             int
	}{
		{"a missing model drive", passing, true, false, false, false, 1},
		{"unreadable release state", state, true, false, true, true, 2},
		{"on trial, whatever the reason", state, true, true, false, false, 1},
		{"no launcher to retry", passing, false, false, true, false, 1},
	} {
		text, code := startFailure(c.err, c.managed, c.onTrial)
		if code != c.code || (text != "") != c.dialog || strings.Contains(strings.ToLower(text), "install") != c.advice {
			t.Errorf("%s: code %d, text %q", c.name, code, text)
		}
	}
	if text := openFailure(`C:\x`, errors.New("config.json is damaged")); !strings.Contains(text, "config.json") || strings.Contains(strings.ToLower(text), "install") {
		t.Errorf("open failure %q", text)
	}
}

type plainProtector struct{}

func (plainProtector) Protect(_ string, b []byte) ([]byte, error)   { return b, nil }
func (plainProtector) Unprotect(_ string, b []byte) ([]byte, error) { return b, nil }

// A cancelled import still tells the host what happened: what finished before the cancel stays added.
func TestImportAlwaysTells(t *testing.T) {
	st, err := store.OpenWith(t.TempDir(), plainProtector{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "models")
	mm, err := models.New(models.Options{Store: st, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mm.Close()
	f := gguftest.Default()
	f.DataBytes = 4096
	os.WriteFile(filepath.Join(dir, "a.gguf"), gguftest.Build(f), 0o644)
	f.DataBytes = 8192
	os.WriteFile(filepath.Join(dir, "b.gguf"), gguftest.Build(f), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var told []string
	closed := false
	importAndTell(ctx, mm, mm.Scan().Picks([]string{"a.gguf", "b.gguf"}), func(file string, _, _ int64) {
		if file == "b.gguf" {
			cancel()
		}
	}, func() { closed = true }, func(s string) { told = append(told, s) })
	if !closed || len(told) != 1 || !strings.Contains(told[0], "Added") || !strings.Contains(told[0], "a.gguf") || !strings.Contains(told[0], "b.gguf: cancelled") {
		t.Fatalf("closed %v, told %q", closed, told)
	}
}
