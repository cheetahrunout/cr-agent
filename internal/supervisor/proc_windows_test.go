//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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

// Both levels really yield a low-integrity child, and no other level starts anything.
func TestEveryLevelIsLowIntegrity(t *testing.T) {
	whoami := sys32(t, "whoami.exe")
	for _, level := range []Level{LevelAppContainer, LevelLowIntegrity} {
		out, code, integrity := runSpawn(t, spawnOptions{Exe: whoami, Args: []string{"/groups"}, Dir: os.TempDir(), Level: level})
		if code != 0 || !strings.Contains(out, "Low Mandatory Level") || integrity != "low" {
			t.Fatalf("%v: exit %d integrity %s, output %q", level, code, integrity, out)
		}
	}
	for _, level := range []Level{-1, 2, 99} {
		if p, err := spawn(spawnOptions{Exe: whoami, Dir: os.TempDir(), Level: level}); err == nil {
			p.Kill()
			t.Fatalf("level %d started a process at integrity %s", level, p.Integrity())
		}
	}
}

// The engine's environment is an allowlist (finding L3): nothing else on the PC reaches it.
func TestChildEnvIsAnAllowlist(t *testing.T) {
	for k, v := range map[string]string{"LLAMA_ARG_RPC": "10.0.0.1:50052", "HF_TOKEN": "hf_secret", "CRAGENT_KEY": "crm_secret",
		"GGML_BACKEND_PATH": `C:\evil\x.dll`, "CUDA_INJECTION64_PATH": `C:\evil\y.dll`, "SOME_OTHER_APP_TOKEN": "s", "PATH": `C:\evil;C:\other`,
		"CUDA_CACHE_MAXSIZE": "1000", "CUDA_VISIBLE_DEVICES": "5"} {
		t.Setenv(k, v)
	}
	cmd := sys32(t, "cmd.exe")
	out, _, _ := runSpawn(t, spawnOptions{Exe: cmd, Args: []string{"/c", "set"}, Dir: os.TempDir()})
	up := strings.ToUpper(out)
	for _, name := range []string{"LLAMA_ARG_RPC", "HF_TOKEN", "CRAGENT_KEY", "GGML_BACKEND_PATH", "CUDA_INJECTION64_PATH", "SOME_OTHER_APP_TOKEN", `C:\EVIL`, "CUDA_VISIBLE_DEVICES"} {
		if strings.Contains(up, name) {
			t.Errorf("%s reached the engine:\n%s", name, out)
		}
	}
	for _, name := range []string{"SYSTEMROOT=", "TEMP=", "LOCALAPPDATA=", "CUDA_CACHE_MAXSIZE=1000", "PATH="} {
		if !strings.Contains(up, name) {
			t.Errorf("%s is missing:\n%s", name, out)
		}
	}
	// gpu_select limits the engine to one GPU; "all" leaves it alone.
	out, _, _ = runSpawn(t, spawnOptions{Exe: cmd, Args: []string{"/c", "set"}, Dir: os.TempDir(), GPUSelect: "1"})
	if !strings.Contains(out, "CUDA_VISIBLE_DEVICES=1") {
		t.Errorf("gpu_select 1 did not reach the engine:\n%s", out)
	}
	out, _, _ = runSpawn(t, spawnOptions{Exe: cmd, Args: []string{"/c", "set"}, Dir: os.TempDir(), GPUSelect: "all"})
	if strings.Contains(out, "CUDA_VISIBLE_DEVICES") {
		t.Errorf("gpu_select all set a device:\n%s", out)
	}
}

// Only a failure of the token itself counts as the sandbox being unavailable.
func TestTokenErrors(t *testing.T) {
	if !tokenError(windows.ERROR_PRIVILEGE_NOT_HELD) || !tokenError(fmt.Errorf("x: %w", windows.ERROR_BAD_TOKEN_TYPE)) {
		t.Error("a token error was not recognised")
	}
	for _, err := range []error{windows.ERROR_FILE_NOT_FOUND, windows.ERROR_ACCESS_DENIED, windows.ERROR_BAD_EXE_FORMAT, windows.ERROR_NOT_ENOUGH_MEMORY, errors.New("exited")} {
		if tokenError(err) {
			t.Errorf("%v taken for a token error", err)
		}
	}
	// A real start in the sandbox that fails for another reason is no token error.
	_, err := spawn(spawnOptions{Exe: filepath.Join(t.TempDir(), "absent.exe"), Dir: os.TempDir()})
	if err == nil || errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("a missing file in the sandbox: %v", err)
	}
}

// ProbeCUDA on the real engine, under the real low-integrity token.
func TestProbeCUDA(t *testing.T) {
	dir := os.Getenv("CRAGENT_ENGINE_DIR")
	if dir == "" {
		t.Skip("CRAGENT_ENGINE_DIR not set")
	}
	found, err := ProbeCUDA(dir)
	if err != nil || !found {
		t.Fatalf("found %v err %v", found, err)
	}
	if _, err := ProbeCUDA(t.TempDir()); err == nil {
		t.Fatal("a folder without an engine gave an answer")
	}
}

func TestCUDADevicePattern(t *testing.T) {
	yes := "Available devices:\n  CUDA0: NVIDIA GeForce RTX 5090 (32606 MiB, 30991 MiB free)\n"
	if !reCUDADevice.MatchString(yes) || reCUDADevice.MatchString("Available devices:\n") || reCUDADevice.MatchString("load_backend: no CUDA0: found\n") {
		t.Fatal("the device pattern is wrong")
	}
}
