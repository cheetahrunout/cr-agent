//go:build windows

package supervisor

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sys32(t *testing.T, exe string) string {
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", exe)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("%s not found", p)
	}
	return p
}

func runSpawn(t *testing.T, o spawnOptions) (string, uint32, string) {
	t.Helper()
	p, err := spawn(o)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	integrity := p.Integrity()
	outc := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(p.out); outc <- b }()
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		p.Kill()
		t.Fatal("child did not exit")
	}
	code := p.ExitCode()
	p.Kill()
	out := <-outc
	p.out.Close()
	return string(out), code, integrity
}

// ACTIVE_PROCESS_LIMIT 1: the engine cannot start a child of its own.
func TestJobBlocksGrandchildren(t *testing.T) {
	cmd := sys32(t, "cmd.exe")
	out, _, _ := runSpawn(t, spawnOptions{Exe: cmd, Args: []string{"/c", cmd + " /c echo GRANDCHILD-RAN"}, Dir: os.TempDir()})
	if strings.Contains(out, "GRANDCHILD-RAN") {
		t.Fatalf("a grandchild ran inside the job: %q", out)
	}
	t.Logf("output: %q", strings.TrimSpace(out))
}

// The low-IL flag really yields a low-integrity child.
func TestLowIntegrityChild(t *testing.T) {
	whoami := sys32(t, "whoami.exe")
	out, code, integrity := runSpawn(t, spawnOptions{Exe: whoami, Args: []string{"/groups"}, Dir: os.TempDir(), LowIL: true})
	if code != 0 || !strings.Contains(out, "Low Mandatory Level") || integrity != "low" {
		t.Fatalf("exit %d integrity %s, output %q", code, integrity, out)
	}
	_, _, integrity = runSpawn(t, spawnOptions{Exe: whoami, Args: []string{"/groups"}, Dir: os.TempDir()})
	if integrity == "low" {
		t.Fatal("normal spawn came out low")
	}
}

// LLAMA_* variables never reach the engine (llama-server reads LLAMA_ARG_* as flags).
func TestChildEnvDropsLlamaVars(t *testing.T) {
	t.Setenv("LLAMA_ARG_RPC", "10.0.0.1:50052")
	cmd := sys32(t, "cmd.exe")
	out, _, _ := runSpawn(t, spawnOptions{Exe: cmd, Args: []string{"/c", "set"}, Dir: os.TempDir()})
	if strings.Contains(out, "LLAMA_ARG_RPC") || !strings.Contains(strings.ToUpper(out), "SYSTEMROOT") {
		t.Fatalf("environment wrong: %q", out)
	}
}
