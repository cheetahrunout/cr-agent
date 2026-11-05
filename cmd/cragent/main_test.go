package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

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
