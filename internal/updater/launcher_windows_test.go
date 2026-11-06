package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The real case: launcher 1 was renamed to launcher.old by an update and is still running, so Windows refuses to
// replace that file. A second launcher update must still go through. Found on a real install, 0.2.3 to 0.2.4.
func TestAsideNameWhenLauncherOldIsRunning(t *testing.T) {
	root := t.TempDir()
	ping, err := os.ReadFile(filepath.Join(os.Getenv("WINDIR"), "System32", "ping.exe"))
	if err != nil {
		t.Skip(err)
	}
	live := filepath.Join(root, "launcher.exe")
	if err := os.WriteFile(live, ping, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(live, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	running := filepath.Join(root, "launcher.old")
	if err := os.Rename(live, running); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("second launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(live, running); err == nil {
		t.Fatal("this test expects Windows to refuse a rename onto a running program")
	}
	aside := asideName(root)
	if filepath.Base(aside) != "launcher.old.1" {
		t.Fatalf("aside %s", aside)
	}
	if err := os.Rename(live, aside); err != nil {
		t.Fatalf("the launcher could still not be moved: %v", err)
	}
}
