package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
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
	f := openLog(path)
	if f == nil {
		t.Fatal("log not opened")
	}
	logWriter{f}.Write([]byte("line\n"))
	f.Close()
	if st, _ := os.Stat(path); st.Size() != 5 {
		t.Fatalf("new log has %d bytes", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != 11<<20 {
		t.Fatalf("old log not kept: %v", err)
	}
}
