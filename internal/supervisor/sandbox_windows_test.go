//go:build windows

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sandboxChild runs inside the sandbox: this test binary, started again by TestSandboxBoundary.
func sandboxChild(args []string) {
	var c struct {
		UserFile, Blob, BlobBeside, Model, EngineDir, Loopback, Socket string
		OtherDrive                                                     string
	}
	if err := json.Unmarshal([]byte(args[0]), &c); err != nil {
		fmt.Println("BAD", err)
		os.Exit(9)
	}
	say := func(name string, err error) {
		v := "allowed"
		if err != nil {
			v = "blocked"
		}
		fmt.Printf("RESULT %s %s\n", name, v)
	}
	read := func(p string) error {
		b, err := os.ReadFile(p)
		if err == nil && len(b) == 0 {
			err = errors.New("empty")
		}
		return err
	}
	unprotect := func(p string) error {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
		var out windows.DataBlob
		return windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out)
	}
	say("user_file", read(c.UserFile))
	say("user_blob", unprotect(c.Blob))
	say("model", read(c.Model))
	_, err := os.ReadDir(os.Getenv("USERPROFILE"))
	say("profile", err)
	if c.OtherDrive != "" {
		_, err := os.ReadDir(c.OtherDrive)
		say("other_drive", err)
	}
	f := filepath.Join(c.EngineDir, "write-test.tmp")
	err = os.WriteFile(f, []byte("x"), 0o644)
	if err == nil {
		os.Remove(f)
	}
	say("write_engine", err)
	for name, addr := range map[string]string{"outbound_a": "1.1.1.1:443", "outbound_b": "8.8.8.8:53", "loopback": c.Loopback} {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			conn.Close()
		}
		say(name, err)
	}
	_, err = net.LookupHost("example.com")
	say("dns", err)
	// Serves on the socket, as the engine does, until the test ends it.
	l, err := net.Listen("unix", c.Socket)
	say("socket", err)
	if err != nil {
		os.Exit(0)
	}
	fmt.Println("SERVING")
	http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "from the sandbox") }))
}

func TestMain(m *testing.M) {
	if i := indexOf(os.Args, "--sandbox-child"); i >= 0 {
		sandboxChild(os.Args[i+1:])
		return
	}
	os.Exit(m.Run())
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// The real boundary at each level: this needs no GPU, so it runs everywhere. At low integrity the test
// records what the host's opt-in gives up, as its dialog says.
func TestSandboxBoundary(t *testing.T) {
	t.Run("appcontainer", func(t *testing.T) {
		boundary(t, LevelAppContainer, map[string]string{"user_file": "blocked", "user_blob": "blocked", "profile": "blocked", "model": "allowed", "write_engine": "blocked",
			"outbound_a": "blocked", "outbound_b": "blocked", "loopback": "blocked", "dns": "blocked", "socket": "allowed"})
	})
	t.Run("low_integrity", func(t *testing.T) {
		boundary(t, LevelLowIntegrity, map[string]string{"user_file": "allowed", "user_blob": "allowed", "profile": "allowed", "model": "allowed", "write_engine": "blocked", "loopback": "allowed"})
	})
}

func boundary(t *testing.T, level Level, want map[string]string) {
	engineDir, modelDir, private := t.TempDir(), t.TempDir(), t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(engineDir, "child.exe")
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(modelDir, "m.gguf")
	os.WriteFile(model, []byte("GGUF"), 0o644)
	userFile := filepath.Join(private, "private.txt")
	os.WriteFile(userFile, []byte("private"), 0o644)
	secret := []byte("secret")
	in := windows.DataBlob{Size: uint32(len(secret)), Data: &secret[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(private, "secret.blob")
	os.WriteFile(blob, unsafe2bytes(out), 0o644)

	if err := prepareSandbox(engineDir, modelDir, model); err != nil {
		t.Fatal(err)
	}
	if !CanEngineRead(modelDir) || CanEngineRead(private) {
		t.Fatalf("CanEngineRead: model folder %v, private folder %v", CanEngineRead(modelDir), CanEngineRead(private))
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sock, err := newSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	defer forgetSocket(sock)
	other := ""
	if _, err := os.ReadDir(`D:\`); err == nil {
		other = `D:\`
	}
	arg, _ := json.Marshal(map[string]string{"UserFile": userFile, "Blob": blob, "Model": model, "EngineDir": engineDir, "Loopback": l.Addr().String(), "Socket": sock, "OtherDrive": other})
	p, err := spawn(spawnOptions{Exe: exe, Args: []string{"--sandbox-child", string(arg)}, Dir: engineDir, Level: level})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	if got := p.Integrity(); got != "low" {
		t.Errorf("integrity %s", got)
	}
	lines := make(chan string, 64)
	go func() {
		buf := make([]byte, 64<<10)
		var rest string
		for {
			n, err := p.out.Read(buf)
			rest += string(buf[:n])
			for {
				i := strings.IndexByte(rest, '\n')
				if i < 0 {
					break
				}
				lines <- strings.TrimSpace(rest[:i])
				rest = rest[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	got := map[string]string{}
wait:
	for {
		select {
		case s, ok := <-lines:
			if !ok || s == "SERVING" {
				break wait
			}
			if f := strings.Fields(s); len(f) == 3 && f[0] == "RESULT" {
				got[f[1]] = f[2]
			} else {
				t.Log(s)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the child said only %v", got)
		}
	}
	t.Logf("results: %v", got)
	if other != "" && level == LevelAppContainer {
		want["other_drive"] = "blocked"
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	if level != LevelAppContainer {
		return
	}
	// The agent reaches it over the socket, sees who owns it, and a plain base URL works.
	owner, err := socketOwner(sock)
	if err != nil || owner != p.PID() {
		t.Fatalf("socket owner %d (child %d): %v", owner, p.PID(), err)
	}
	base := registerSocket(sock)
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: DialEngine}, Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "from the sandbox" {
		t.Fatalf("answer %q", body)
	}
}

func unsafe2bytes(b windows.DataBlob) []byte {
	return append([]byte(nil), unsafe.Slice(b.Data, b.Size)...)
}

// A model folder the agent may grant: a file moved in with its own permissions is brought in too.
func TestPrepareModelFindsAMovedFile(t *testing.T) {
	folder, elsewhere := t.TempDir(), t.TempDir()
	first := filepath.Join(folder, "a.gguf")
	os.WriteFile(first, []byte("x"), 0o644)
	if err := prepareModel(folder, first); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(elsewhere, "moved.gguf")
	os.WriteFile(src, []byte("x"), 0o644)
	moved := filepath.Join(folder, "moved.gguf")
	if err := os.Rename(src, moved); err != nil {
		t.Fatal(err)
	}
	sid, err := allPackages()
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := allowsRead(moved, sid); ok {
		t.Skip("this file system gave the moved file the folder's entry by itself")
	}
	if err := prepareModel(folder, moved); err != nil {
		t.Fatal(err)
	}
	if ok, err := allowsRead(moved, sid); !ok || err != nil {
		t.Fatalf("moved file: %v %v", ok, err)
	}
	// No folder to grant: nothing is changed.
	other := t.TempDir()
	if err := prepareModel("", filepath.Join(other, "x.gguf")); err != nil {
		t.Fatal(err)
	}
	if CanEngineRead(other) {
		t.Fatal("a folder the agent may not grant was changed")
	}
}

// Rollback: an older agent runs under a low-integrity token. With the container's own SID on a file that
// token loses it, so every grant must go to ALL APPLICATION PACKAGES.
func TestGrantsAreForAllPackages(t *testing.T) {
	engine, models := t.TempDir(), t.TempDir()
	file := filepath.Join(models, "m.gguf")
	os.WriteFile(file, []byte("x"), 0o644)
	if err := prepareSandbox(engine, models, file); err != nil {
		t.Fatal(err)
	}
	own, err := engineContainer()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := allPackages()
	for _, p := range []string{engine, models, file} {
		if ok, _ := allowsRead(p, own); ok {
			t.Errorf("%s carries the container's own SID", p)
		}
		if ok, err := allowsRead(p, all); !ok || err != nil {
			t.Errorf("%s: %v %v", p, ok, err)
		}
	}
}

// The real engine, with a model in a folder the sandbox was not given: an engine error, no other start.
func TestRealEngineCannotReadAnUngrantedModel(t *testing.T) {
	dir, model := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_GGUF")
	if dir == "" || model == "" {
		t.Skip("CRAGENT_ENGINE_DIR and CRAGENT_TEST_GGUF not set")
	}
	folder := t.TempDir()
	copied := filepath.Join(folder, "m.gguf")
	b, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Start(context.Background(), Config{EngineDir: dir, ModelPath: copied, Alias: "m", Deadline: time.Minute,
		Settings: DefaultSettings(4)})
	if !errors.Is(err, ErrEngineExited) {
		t.Fatalf("start: %v", err)
	}
	t.Logf("error: %v", err)
	if CanEngineRead(folder) {
		t.Fatal("the folder was changed")
	}
}

// A model on another drive, in a folder this test makes and removes (CRAGENT_TEST_OTHER_DRIVE=D:\\): the
// AppContainer cannot read it until the folder is granted, and then answers from it. Only the file and
// folder this test made are removed, one by one, and only after the engine has let go of them.
func TestRealEngineModelOnAnotherDrive(t *testing.T) {
	dir, model, drive := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_GGUF"), os.Getenv("CRAGENT_TEST_OTHER_DRIVE")
	if dir == "" || model == "" || drive == "" {
		t.Skip("CRAGENT_ENGINE_DIR, CRAGENT_TEST_GGUF and CRAGENT_TEST_OTHER_DRIVE not set")
	}
	folder, err := os.MkdirTemp(drive, "cr-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(folder, "m.gguf")
	t.Cleanup(func() {
		for _, p := range []string{copied, folder} {
			var rerr error
			for i := 0; i < 25; i++ {
				if rerr = os.Remove(p); rerr == nil || os.IsNotExist(rerr) {
					rerr = nil
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if rerr != nil {
				t.Errorf("left behind, remove it by hand: %s: %v", p, rerr)
			}
		}
	})
	b, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if CanEngineRead(folder) {
		t.Fatal("a new folder on that drive already lets the sandbox read it")
	}
	cfg := Config{EngineDir: dir, ModelPath: copied, Alias: "m", Deadline: time.Minute, Settings: DefaultSettings(4)}
	if _, err := Start(context.Background(), cfg); !errors.Is(err, ErrEngineExited) {
		t.Fatalf("without the grant: %v", err)
	}
	if err := GrantModelFolder(folder); err != nil {
		t.Fatal(err)
	}
	e, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("with the grant: %v", err)
	}
	defer e.Stop()
	base, key, _ := e.Target()
	req, _ := http.NewRequest(http.MethodPost, base+"/completion", strings.NewReader(`{"prompt":"Hello","n_predict":8,"temperature":0}`))
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: DialEngine}, Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "content") {
		t.Fatalf("answer %d: %.200s", resp.StatusCode, body)
	}
	t.Logf("answered from %s", drive)
}

// A sandbox that cannot be set up is an error of its own kind, and Start does not fall through to a port.
func TestStartInSandboxNeedsTheSandbox(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "llama-server.exe"), []byte("not a program"), 0o755)
	_, err := Start(context.Background(), Config{EngineDir: dir, ModelPath: filepath.Join(dir, "m.gguf"), Deadline: 5 * time.Second})
	if err == nil {
		t.Fatal("started")
	}
	socketsMu.Lock()
	n := len(sockets)
	socketsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d sockets left registered", n)
	}
}
