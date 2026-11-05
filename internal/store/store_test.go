package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testKey = "crm_abcdefgh_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST"

func open(t *testing.T) *Store {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI: Windows only")
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWith(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ModelsDir() != filepath.Join(dir, "models") {
		t.Fatalf("default models dir %s", s.ModelsDir())
	}
	if err := s.Update(func(c *Config) { c.ModelsDir, c.IdleUnloadMin, c.LocalPause = `D:\Models`, 30, true }); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenWith(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := s2.Config()
	if c.ModelsDir != `D:\Models` || c.IdleUnloadMin != 30 || !c.LocalPause || s2.ModelsDir() != `D:\Models` {
		t.Fatalf("reloaded %+v", c)
	}
	// No temp files left behind by the atomic write.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
	// A damaged config is an error, not a silent reset.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{broken"), 0o600)
	if _, err := OpenWith(dir, nil); err == nil {
		t.Fatal("damaged config accepted")
	}
}

func TestMachineKeyIsProtected(t *testing.T) {
	s := open(t)
	if s.Paired() {
		t.Fatal("paired before pairing")
	}
	if _, err := s.MachineKey(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.SavePairing("wss://cr-ai.apps.grovestreet.zip/agent", "01JCKZ3Q9X8YB7N6M5P4R3S2T1", "anna-pc", "abcdefgh", testKey); err != nil {
		t.Fatal(err)
	}
	got, err := s.MachineKey()
	if err != nil || got != testKey {
		t.Fatalf("key round trip: %v", err)
	}
	// Nothing on disk contains the key or its secret part in the clear.
	secret := []byte(strings.Split(testKey, "_")[2])
	filepath.Walk(s.Dir(), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(path)
			if bytes.Contains(b, secret) {
				t.Errorf("%s holds the secret in the clear", path)
			}
		}
		return nil
	})
	c := s.Config()
	if c.MachineID == "" || c.Slug != "anna-pc" || c.KeyID != "abcdefgh" {
		t.Fatalf("config %+v", c)
	}

	// A blob moved under another name does not open (the name is DPAPI entropy).
	blob, _ := os.ReadFile(filepath.Join(s.Dir(), "state", SecretMachineKey+".bin"))
	os.WriteFile(filepath.Join(s.Dir(), "state", SecretHFToken+".bin"), blob, 0o600)
	if _, err := s.ReadSecret(SecretHFToken); err == nil {
		t.Fatal("a blob opened under another name")
	}
	// A damaged blob is an error that does not echo content.
	os.WriteFile(filepath.Join(s.Dir(), "state", SecretMachineKey+".bin"), []byte("garbage"), 0o600)
	if _, err := s.MachineKey(); err == nil || strings.Contains(err.Error(), "garbage") {
		t.Fatalf("damaged blob: %v", err)
	}

	if err := s.Unpair(); err != nil {
		t.Fatal(err)
	}
	if s.Paired() || s.Config().KeyID != "" || s.Config().MachineID == "" {
		t.Fatalf("after unpair: paired=%v cfg=%+v", s.Paired(), s.Config())
	}
}

func TestStateNames(t *testing.T) {
	s, _ := OpenWith(t.TempDir(), nil)
	for _, bad := range []string{"", "../x", `a\b`, "A", "x.y", "con/x"} {
		if err := s.WriteJSON(bad, 1); err == nil {
			t.Errorf("accepted state name %q", bad)
		}
	}
	if err := s.WriteJSON(StateModels, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	var m map[string]int
	if err := s.ReadJSON(StateModels, &m); err != nil || m["a"] != 1 {
		t.Fatalf("json round trip %v %v", m, err)
	}
	if err := s.ReadJSON("absent", &m); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
