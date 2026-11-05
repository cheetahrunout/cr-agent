package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPointerRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadPointer(root); err == nil {
		t.Fatal("read a pointer that does not exist")
	}
	now := time.Now().UTC().Truncate(time.Second)
	want := Pointer{Version: "0.1.1", Previous: "0.1.0", PendingSince: &now}
	if err := WritePointer(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPointer(root)
	if err != nil || got.Version != "0.1.1" || got.Previous != "0.1.0" || got.PendingSince == nil || !got.PendingSince.Equal(now) {
		t.Fatalf("%+v %v", got, err)
	}
	// Replacing an existing pointer, and no temp file left behind.
	if err := WritePointer(root, Pointer{Version: "0.1.1", Previous: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	got, _ = ReadPointer(root)
	if got.PendingSince != nil {
		t.Fatal("pending_since not cleared")
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 {
		t.Fatalf("files left: %v", ents)
	}
	// A pointer is input: a version that is not a version never becomes a path.
	for _, bad := range []string{`{"version":"../../evil"}`, `{"version":"0.1.1","previous":"..\\x"}`, `{"version":""}`, `{broken`} {
		os.WriteFile(PointerFile(root), []byte(bad), 0o644)
		if _, err := ReadPointer(root); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := WritePointer(root, Pointer{Version: "../x"}); err == nil {
		t.Error("wrote a bad version")
	}
}

func TestFind(t *testing.T) {
	root := t.TempDir()
	exe := AgentExe(root, "0.1.1")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("x"), 0o755)
	if _, _, ok := Find(exe); ok {
		t.Fatal("found an install without current.json")
	}
	WritePointer(root, Pointer{Version: "0.1.1"})
	r, v, ok := Find(exe)
	if !ok || r != root || v != "0.1.1" {
		t.Fatalf("%q %q %v", r, v, ok)
	}
	for _, other := range []string{filepath.Join(root, "cragent.exe"), filepath.Join(root, "versions", "latest", "cragent.exe"),
		filepath.Join(root, "versions", "0.1.1", "other.exe"), filepath.Join(root, "build", "0.1.1", "cragent.exe")} {
		if _, _, ok := Find(other); ok {
			t.Errorf("%s taken for an installed agent", strings.TrimPrefix(other, root))
		}
	}
	os.MkdirAll(VersionDir(root, "0.1.0"), 0o755)
	os.MkdirAll(filepath.Join(root, "versions", "junk"), 0o755)
	if got := Installed(root); len(got) != 2 {
		t.Fatalf("installed %v", got)
	}
}
