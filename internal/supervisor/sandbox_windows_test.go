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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cheetahrunout/cr-agent/internal/fit"
	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
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
	if i := indexOf(os.Args, "--sabotage-child"); i >= 0 {
		sabotageChild(os.Args[i+1])
		return
	}
	if i := indexOf(os.Args, "--clip-child"); i >= 0 {
		clipChild(os.Args[i+1], os.Args[i+2], os.Args[i+3])
		return
	}
	if i := indexOf(os.Args, "--ui-child"); i >= 0 {
		uiChild(os.Args[i+1])
		return
	}
	// These tests reset the profile, so they use one of their own, and leave none behind.
	setContainerName(fmt.Sprintf("%s-test-%d", containerName, os.Getpid()))
	code := m.Run()
	DeleteContainer()
	os.Exit(code)
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

	if err := prepareSandbox(engineDir, modelDir, model, nil); err != nil {
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
	if err := prepareModel(folder, first, nil); err != nil {
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
	if err := prepareModel(folder, moved, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := allowsRead(moved, sid); !ok || err != nil {
		t.Fatalf("moved file: %v %v", ok, err)
	}
	// No folder to grant: nothing is changed.
	other := t.TempDir()
	if err := prepareModel("", filepath.Join(other, "x.gguf"), nil); err != nil {
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
	if err := prepareSandbox(engine, models, file, nil); err != nil {
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
	if _, err := GrantModelFolder(folder); err != nil {
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

// What a model can do inside the container: take the socket's folder away and leave a file in its place.
func sabotageChild(temp string) {
	fmt.Println("rename:", os.Rename(temp, filepath.Join(filepath.Dir(temp), "Temp.gone")))
	fmt.Println("file:", os.WriteFile(temp, []byte("x"), 0o644))
}

// startChild runs this test binary at a level, with the given flag, and returns what it printed.
func startChild(t *testing.T, level Level, args ...string) string {
	t.Helper()
	return startChildWith(t, spawnOptions{Level: level}, args...)
}

func startChildWith(t *testing.T, o spawnOptions, args ...string) string {
	t.Helper()
	engineDir := t.TempDir()
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
	if err := prepareSandbox(engineDir, "", "", nil); err != nil {
		t.Fatal(err)
	}
	o.Exe, o.Args, o.Dir = exe, args, engineDir
	p, err := spawn(o)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	defer p.out.Close()
	out, _ := io.ReadAll(p.out)
	return string(out)
}

// A model cannot break the sandbox for later loads: the next start makes the profile anew, with the same SID.
func TestModelCannotBreakTheSandbox(t *testing.T) {
	sid, err := engineContainer()
	if err != nil {
		t.Fatal(err)
	}
	before := sid.String()
	temp, err := containerTemp(sid)
	if err != nil {
		t.Fatal(err)
	}
	out := startChild(t, LevelAppContainer, "--sabotage-child", temp)
	if !strings.Contains(out, "rename: <nil>") || !strings.Contains(out, "file: <nil>") {
		t.Skipf("the container could not do it here: %s", out)
	}
	if st, err := os.Stat(temp); err != nil || st.IsDir() {
		t.Fatalf("the sabotage did not take: %v", err)
	}
	p, err := newSocketPath()
	if err != nil {
		t.Fatalf("the sandbox stayed broken: %v", err)
	}
	if st, err := os.Stat(temp); err != nil || !st.IsDir() {
		t.Fatalf("Temp after the reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(temp), "Temp.gone")); err == nil {
		t.Fatal("what the model left is still there")
	}
	after, err := engineContainer()
	if err != nil || after.String() != before {
		t.Fatalf("SID %v, was %s: %v", after, before, err)
	}
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// A link in the socket's place is not followed: the agent writes nothing through it.
func TestSocketIsNotMadeThroughALink(t *testing.T) {
	sid, err := engineContainer()
	if err != nil {
		t.Fatal(err)
	}
	temp, _ := containerTemp(sid)
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", temp, outside).CombinedOutput(); err != nil {
		t.Skipf("mklink failed: %v %s", err, out)
	}
	if _, err := trySocketPath(); err == nil {
		t.Fatal("a socket path through a link was accepted")
	}
	p, err := newSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(p, outside) {
		t.Fatalf("socket %s", p)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("written through the link: %v", ents)
	}
}

// The reset at each agent start clears whatever a model left between loads.
func TestResetClearsTheProfile(t *testing.T) {
	sid, err := engineContainer()
	if err != nil {
		t.Fatal(err)
	}
	temp, _ := containerTemp(sid)
	planted := filepath.Join(temp, "planted.bin")
	if err := os.WriteFile(planted, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ResetSandbox(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("still there after the reset")
	}
	if _, err := newSocketPath(); err != nil {
		t.Fatal(err)
	}
}

// Each agent folder has its own container, so a reset cannot reach another agent's engine.
func TestContainersAreScopedByFolder(t *testing.T) {
	saved := currentContainer
	defer setContainerName(saved)
	ScopeContainer(`C:\\one`)
	a := currentContainer
	ScopeContainer(`C:\\ONE`)
	b := currentContainer
	ScopeContainer(`C:\\two`)
	if a != b || a == currentContainer || !strings.HasPrefix(a, containerName+"-") {
		t.Fatalf("%q %q %q", a, b, currentContainer)
	}
}

// uiProbe is what a process can see of the desktop: the titled windows of other processes, how many colours
// a copy of the screen holds (one means it got nothing), and a global atom of this session.
func uiProbe(atomName string) (others, colours int, atom bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	u := windows.NewLazySystemDLL("user32.dll")
	g := windows.NewLazySystemDLL("gdi32.dll")
	self := windows.GetCurrentProcessId()
	cb := syscall.NewCallback(func(h, _ uintptr) uintptr {
		var pid uint32
		u.NewProc("GetWindowThreadProcessId").Call(h, uintptr(unsafe.Pointer(&pid)))
		b := make([]uint16, 200)
		if n, _, _ := u.NewProc("GetWindowTextW").Call(h, uintptr(unsafe.Pointer(&b[0])), 200); n > 0 && pid != self {
			others++
		}
		return 1
	})
	u.NewProc("EnumWindows").Call(cb, 0)
	seen := map[uintptr]bool{}
	if dc, _, _ := u.NewProc("GetDC").Call(0); dc != 0 {
		w, _, _ := u.NewProc("GetSystemMetrics").Call(0)
		hh, _, _ := u.NewProc("GetSystemMetrics").Call(1)
		if w == 0 || hh == 0 {
			w, hh = 256, 256
		}
		mem, _, _ := g.NewProc("CreateCompatibleDC").Call(dc)
		bm, _, _ := g.NewProc("CreateCompatibleBitmap").Call(dc, w, hh)
		g.NewProc("SelectObject").Call(mem, bm)
		if ok, _, _ := g.NewProc("BitBlt").Call(mem, 0, 0, w, hh, dc, 0, 0, 0x00CC0020); ok != 0 {
			for x := uintptr(0); x < w; x += w/40 + 1 {
				for y := uintptr(0); y < hh; y += hh/40 + 1 {
					c, _, _ := g.NewProc("GetPixel").Call(mem, x, y)
					seen[c] = true
				}
			}
		}
	}
	// The session's global atoms: only the job's limits give the child a table of its own.
	if atomName != "" {
		name, _ := windows.UTF16PtrFromString(atomName)
		a, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFindAtomW").Call(uintptr(unsafe.Pointer(name)))
		atom = a != 0
	}
	return others, len(seen), atom
}

func uiChild(atomName string) {
	others, colours, atom := uiProbe(atomName)
	fmt.Printf("others=%d colours=%d atom=%v\n", others, colours, atom)
}

// The screen and other windows' titles are closed at both levels. The clipboard has its own test.
func TestSandboxCannotSeeTheDesktop(t *testing.T) {
	others, colours, _ := uiProbe("")
	t.Logf("this test itself: others=%d colours=%d", others, colours)
	atomName := fmt.Sprintf("cr-agent-test-atom-%d", os.Getpid())
	name, _ := windows.UTF16PtrFromString(atomName)
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	added, _, _ := kernel.NewProc("GlobalAddAtomW").Call(uintptr(unsafe.Pointer(name)))
	if added == 0 {
		t.Fatal("no global atom")
	}
	defer kernel.NewProc("GlobalDeleteAtom").Call(added)
	for _, level := range []Level{LevelAppContainer, LevelLowIntegrity} {
		out := strings.TrimSpace(startChild(t, level, "--ui-child", atomName))
		t.Logf("%v: %s", level, out)
		var o, c int
		var atom bool
		if _, err := fmt.Sscanf(out, "others=%d colours=%d atom=%v", &o, &c, &atom); err != nil {
			t.Fatalf("%v: %q: %v", level, out, err)
		}
		if o != 0 {
			t.Errorf("%v: sees %d windows of other processes", level, o)
		}
		if c > 1 {
			t.Errorf("%v: a copy of the screen holds %d colours", level, c)
		}
		if atom {
			t.Errorf("%v: finds a global atom of this session: the job's UI limits are off", level)
		}
	}
	if others == 0 || colours <= 1 {
		t.Log("this session has no desktop to see, so this run proves little")
	}
}

// A forgotten engine host is never dialled out: a prompt must not reach whatever answers to that name.
func TestForgottenEngineIsNeverLookedUp(t *testing.T) {
	base := registerSocket("gone.sock")
	host := strings.TrimPrefix(base, "http://") + ":80"
	forgetSocket("gone.sock")
	for _, addr := range []string{host, strings.ToUpper(host), "cr-engine-anything:80"} {
		if _, err := DialEngine(context.Background(), "tcp", addr); !errors.Is(err, ErrEngineGone) {
			t.Errorf("%s: %v", addr, err)
		}
	}
	// Any other address is dialled as it was.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := DialEngine(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func known(t *testing.T, id *windows.KNOWNFOLDERID) string {
	t.Helper()
	p, err := windows.KnownFolderPath(id, 0)
	if err != nil {
		t.Skipf("no such known folder here: %v", err)
	}
	return p
}

// Refused: the agent's folder but for models, what contains it, roots, and the broad folders themselves.
func TestCheckModelFolder(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "a", "home")
	os.MkdirAll(filepath.Join(home, "models", "sub"), 0o755)
	os.MkdirAll(filepath.Join(home, "state"), 0o755)
	docs := known(t, windows.FOLDERID_Documents)
	roaming := known(t, windows.FOLDERID_RoamingAppData)
	refused := []string{home, filepath.Join(home, "state"), filepath.Join(home, "logs"), filepath.Join(home, "engine"), filepath.Join(base, "a"), base[:3], docs,
		known(t, windows.FOLDERID_Desktop), known(t, windows.FOLDERID_Downloads), known(t, windows.FOLDERID_Pictures), known(t, windows.FOLDERID_Music), known(t, windows.FOLDERID_Videos),
		known(t, windows.FOLDERID_Profile), filepath.Dir(known(t, windows.FOLDERID_Profile)), known(t, windows.FOLDERID_Windows), known(t, windows.FOLDERID_ProgramFiles), known(t, windows.FOLDERID_ProgramFilesX86),
		filepath.Dir(roaming), roaming, known(t, windows.FOLDERID_LocalAppData)}
	for _, p := range refused {
		if err := CheckModelFolder(p, home); err == nil {
			t.Errorf("%s was accepted", p)
		} else if !strings.Contains(err.Error(), "older") {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join(home, "models"), filepath.Join(home, "models", "sub"), filepath.Join(base, "other"), filepath.Join(docs, "cr-agent-no-such-folder"), filepath.Join(known(t, windows.FOLDERID_Downloads), "x", "y")} {
		if err := CheckModelFolder(p, home); err != nil {
			t.Errorf("%s was refused: %v", p, err)
		}
	}
	// Every other drive's root, which nothing else in the list covers.
	for _, d := range "DEFGHIJ" {
		root := string(d) + `:\`
		if _, err := os.ReadDir(root); err == nil && CheckModelFolder(root, home) == nil {
			t.Errorf("the drive root %s was accepted", root)
		}
	}
	if !IsOwnModelFolder(filepath.Join(home, "models", "sub"), home) || IsOwnModelFolder(filepath.Join(home, "state"), home) || IsOwnModelFolder(home, home) {
		t.Error("what counts as the agent's own model folder is wrong")
	}
	// A link or a short name changes nothing.
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, docs).CombinedOutput(); err != nil {
		t.Logf("mklink failed: %v %s", err, out)
	} else {
		defer os.Remove(link)
		if CheckModelFolder(link, home) == nil {
			t.Error("a junction to Documents was accepted")
		}
		if CheckModelFolder(filepath.Join(link), filepath.Join(home)) == nil {
			t.Error("a junction to Documents was accepted")
		}
	}
	link2 := filepath.Join(base, "link2")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link2, filepath.Join(home, "state")).CombinedOutput(); err == nil {
		defer os.Remove(link2)
		if CheckModelFolder(link2, home) == nil {
			t.Error("a junction into the agent's state was accepted")
		}
	} else {
		t.Logf("mklink failed: %v %s", err, out)
	}
	if short := shortName(known(t, windows.FOLDERID_ProgramFiles)); short != "" {
		if CheckModelFolder(short, home) == nil {
			t.Errorf("the short name %s was accepted", short)
		}
	}
}

func shortName(p string) string {
	p16, _ := windows.UTF16PtrFromString(p)
	buf := make([]uint16, 300)
	n, err := windows.GetShortPathName(p16, &buf[0], 300)
	if err != nil || n == 0 {
		return ""
	}
	s := windows.UTF16ToString(buf)
	if strings.EqualFold(s, p) {
		return ""
	}
	return s
}

// The grant never lands on a folder or file through a link, and does not cross one below the folder.
func TestGrantDoesNotFollowLinks(t *testing.T) {
	target, parent := t.TempDir(), t.TempDir()
	link := filepath.Join(parent, "door")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink failed: %v %s", err, out)
	}
	defer os.Remove(link)
	if _, err := GrantModelFolder(link); err == nil {
		t.Fatal("a grant through a junction was made")
	}
	if _, err := GrantModelFolder(filepath.Join(link, "sub")); err == nil {
		t.Fatal("a grant below a junction was made")
	}
	os.WriteFile(filepath.Join(target, "m.gguf"), []byte("x"), 0o644)
	for _, model := range []string{filepath.Join(link, "m.gguf"), filepath.Join(target, "m.gguf")} {
		if err := prepareModel(link, model, nil); err == nil {
			t.Fatalf("prepareModel took a junction as the folder, for %s", model)
		}
	}
	if CanEngineRead(target) {
		t.Fatal("the target gained the entry")
	}
	// A junction inside a granted folder: the entry stays on this side of it.
	if _, err := GrantModelFolder(parent); err != nil {
		t.Fatal(err)
	}
	if CanEngineRead(target) {
		t.Fatal("the grant crossed a junction below the folder")
	}
}

// Added is reported only when the entry is new, and the revoke takes it from what inherited it too.
func TestGrantAndRevoke(t *testing.T) {
	folder := t.TempDir()
	file := filepath.Join(folder, "m.gguf")
	os.WriteFile(file, []byte("x"), 0o644)
	added, err := GrantModelFolder(folder)
	if err != nil || !added || !CanEngineRead(file) {
		t.Fatalf("first grant: %v %v, file readable %v", added, err, CanEngineRead(file))
	}
	if again, err := GrantModelFolder(folder); err != nil || again {
		t.Fatalf("second grant: %v %v", again, err)
	}
	var reported []string
	moved := filepath.Join(folder, "moved.gguf")
	elsewhere := t.TempDir()
	src := filepath.Join(elsewhere, "moved.gguf")
	os.WriteFile(src, []byte("x"), 0o644)
	if err := os.Rename(src, moved); err != nil {
		t.Fatal(err)
	}
	if err := prepareModel(folder, moved, func(p string) { reported = append(reported, p) }); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || reported[0] != moved {
		t.Fatalf("reported %v", reported)
	}
	for _, p := range []string{folder, moved} {
		if err := RevokeModelGrant(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{folder, file, moved} {
		if CanEngineRead(p) {
			t.Errorf("%s still has the entry", p)
		}
	}
	// The real agent reports only new entries: a folder it just granted is reported once.
	reported = nil
	if err := prepareModel(folder, file, func(p string) { reported = append(reported, p) }); err != nil || len(reported) != 1 || reported[0] != folder {
		t.Fatalf("after the revoke: %v %v", reported, err)
	}
}

var (
	clipUser   = windows.NewLazySystemDLL("user32.dll")
	clipKernel = windows.NewLazySystemDLL("kernel32.dll")
)

const cfUnicodeText = 13

func clipFormat(name string) uintptr {
	n, _ := windows.UTF16PtrFromString(name)
	f, _, _ := clipUser.NewProc("RegisterClipboardFormatW").Call(uintptr(unsafe.Pointer(n)))
	return f
}

func clipAlloc(s string) uintptr {
	b, _ := windows.UTF16FromString(s)
	const gmemMoveable = 2
	h, _, _ := clipKernel.NewProc("GlobalAlloc").Call(gmemMoveable, uintptr(len(b)*2))
	p, _, _ := clipKernel.NewProc("GlobalLock").Call(h)
	copy(unsafe.Slice(*(**uint16)(unsafe.Pointer(&p)), len(b)), b)
	clipKernel.NewProc("GlobalUnlock").Call(h)
	return h
}

// clipRead is the text held under a format on an open clipboard, or "" when the call gives nothing.
func clipRead(format uintptr) string {
	h, _, _ := clipUser.NewProc("GetClipboardData").Call(format)
	if h == 0 {
		return ""
	}
	p, _, _ := clipKernel.NewProc("GlobalLock").Call(h)
	if p == 0 {
		return ""
	}
	defer clipKernel.NewProc("GlobalUnlock").Call(h)
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&p)))
}

func clipOpen(tries int) (bool, error) {
	var last error
	for i := 0; i < tries; i++ {
		ok, _, e := clipUser.NewProc("OpenClipboard").Call(0)
		if ok != 0 {
			return true, nil
		}
		last = e
		time.Sleep(50 * time.Millisecond)
	}
	return false, last
}

// clipChild tries the clipboard's data calls. It prints only whether it got anything, never what it got,
// and it never empties the clipboard or writes text: if a limit failed, the host's data must survive this.
func clipChild(formatName, known, written string) {
	runtime.LockOSThread()
	opened, err := clipOpen(30)
	fmt.Printf("open=%v\n", opened)
	if !opened {
		fmt.Printf("open_error=%q\n", fmt.Sprint(err))
		return
	}
	defer clipUser.NewProc("CloseClipboard").Call()
	fmt.Printf("read_planted=%v\n", clipRead(clipFormat(formatName)) == known)
	fmt.Printf("read_text=%v\n", clipRead(cfUnicodeText) != "")
	h, _, _ := clipUser.NewProc("SetClipboardData").Call(clipFormat(written), clipAlloc("written by the sandbox"))
	fmt.Printf("write=%v\n", h != 0)
}

// The clipboard's data is closed at both levels, on the session's own clipboard, the one a real engine
// shares. What counts is the data calls: OpenClipboard alone answers true at low integrity whenever
// nobody else holds the clipboard. Only a private format is added, plus known text if the clipboard holds no
// text, so the host's data stays; they go with the host's next copy.
//
// A control with the job's UI limits off shows what the limits stop. A clipboard this test cannot open, or that a
// child could not get open, proves nothing, so that is a skip with its reason, never a pass.
func TestSandboxCannotUseTheClipboard(t *testing.T) {
	if os.Getenv("CI") == "" && os.Getenv("CRAGENT_TEST_DESKTOP") == "" {
		t.Skip("this adds to the session's clipboard, so it runs only on CI or with CRAGENT_TEST_DESKTOP=1")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const known = "known text the sandbox must not read"
	format := fmt.Sprintf("CR-Agent-Test-%d", os.Getpid())
	opened, err := clipOpen(200)
	if !opened {
		t.Skipf("for 10 s this test itself could not open the session's clipboard, so it says nothing about the sandbox: %v", err)
	}
	h, _, e := clipUser.NewProc("SetClipboardData").Call(clipFormat(format), clipAlloc(known))
	if avail, _, _ := clipUser.NewProc("IsClipboardFormatAvailable").Call(cfUnicodeText); avail == 0 {
		clipUser.NewProc("SetClipboardData").Call(cfUnicodeText, clipAlloc(known))
	}
	planted := clipRead(clipFormat(format)) == known && clipRead(cfUnicodeText) != ""
	clipUser.NewProc("CloseClipboard").Call()
	if h == 0 || !planted {
		t.Skipf("this test could not plant on the session's clipboard, so it says nothing about the sandbox: %v", e)
	}

	run := func(o spawnOptions, written string) string {
		t.Helper()
		out := ""
		for try := 0; try < 3; try++ {
			out = startChildWith(t, o, "--clip-child", format, known, written)
			if strings.Contains(out, "open=true") {
				break
			}
		}
		return strings.Join(strings.Fields(out), " ")
	}
	type label struct {
		level Level
		name  string
	}
	for _, c := range []label{{LevelAppContainer, "appcontainer"}, {LevelLowIntegrity, "low_integrity"}} {
		control := run(spawnOptions{Level: c.level, noUILimits: true}, format+"-control-"+c.name)
		t.Logf("%s, limits off: %s", c.name, control)
		written := format + "-written-" + c.name
		out := run(spawnOptions{Level: c.level}, written)
		t.Logf("%s, as shipped: %s", c.name, out)

		// Held by someone else all the while? Then a child that never got it open proves nothing.
		if opened, err := clipOpen(40); !opened {
			t.Skipf("%s: the clipboard was held by another program around the check, so it says nothing: %v", c.name, err)
		}
		avail, _, _ := clipUser.NewProc("IsClipboardFormatAvailable").Call(clipFormat(written))
		still := clipRead(clipFormat(format))
		clipUser.NewProc("CloseClipboard").Call()
		for _, bad := range []string{"read_planted=true", "read_text=true", "write=true"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %s", c.name, bad)
			}
		}
		if avail != 0 {
			t.Errorf("%s: what the sandbox wrote is on the clipboard", c.name)
		}
		if still != known {
			t.Skipf("%s: another program replaced the clipboard during the check, so it says nothing more", c.name)
		}
		if c.level == LevelLowIntegrity && !strings.Contains(out, "open=true") {
			t.Skipf("%s: the child never got the clipboard open, so the data calls were not tried: %s", c.name, out)
		}
	}
}

// The engine inherits the mode that keeps Windows from putting up a system error box, at both levels. The
// child must not be a Go program: Go's runtime sets this mode for itself at start, which would hide a failure.
// So the parent asks Windows what a plain system program was started with.
func TestChildCannotPutUpASystemErrorBox(t *testing.T) {
	whoami := sys32(t, "whoami.exe")
	query := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryInformationProcess")
	const processDefaultHardErrorMode = 12
	for _, level := range []Level{LevelAppContainer, LevelLowIntegrity} {
		p, err := spawn(spawnOptions{Exe: whoami, Dir: os.TempDir(), Level: level})
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, p.out)
		<-p.Done()
		var raw, n uint32
		r, _, _ := query.Call(uintptr(p.handle), processDefaultHardErrorMode, uintptr(unsafe.Pointer(&raw)), 4, uintptr(unsafe.Pointer(&n)))
		p.Kill()
		p.out.Close()
		if r != 0 {
			t.Fatalf("%v: NtQueryInformationProcess %#x", level, r)
		}
		mode := raw ^ windows.SEM_FAILCRITICALERRORS // the kernel holds this one bit inverted, as GetErrorMode shows
		t.Logf("%v: error mode %#x", level, mode)
		if mode&engineErrorMode != engineErrorMode {
			t.Errorf("%v: error mode %#x, want %#x set", level, mode, uint32(engineErrorMode))
		}
	}
}

// systemPopups is how many system error boxes Windows has logged (System log, id 26).
func systemPopups(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("wevtutil", "qe", "System", "/q:*[System[(EventID=26)]]", "/f:text", "/c:100000").Output()
	if err != nil {
		t.Skipf("the System log cannot be read here: %v", err)
	}
	return strings.Count(string(out), "Event[")
}

// An engine without its DLLs: a quick engine error, no system error box, at both levels. It needs the real
// engine's program, without which this cannot show a missing DLL.
func TestRealEngineWithoutItsDLLs(t *testing.T) {
	src := os.Getenv("CRAGENT_ENGINE_DIR")
	if src == "" {
		t.Skip("CRAGENT_ENGINE_DIR not set")
	}
	b, err := os.ReadFile(filepath.Join(src, "llama-server.exe"))
	if err != nil {
		t.Fatal(err)
	}
	before := systemPopups(t)
	for _, level := range []Level{LevelAppContainer, LevelLowIntegrity} {
		dir, models := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "llama-server.exe"), b, 0o755); err != nil {
			t.Fatal(err)
		}
		model := filepath.Join(models, "m.gguf")
		os.WriteFile(model, []byte("x"), 0o644)
		t0 := time.Now()
		_, err := Start(context.Background(), Config{EngineDir: dir, ModelPath: model, ModelGrant: models, Alias: "m", Level: level, Deadline: 30 * time.Second, Settings: DefaultSettings(4)})
		el := time.Since(t0)
		t.Logf("%v: %v in %v", level, err, el.Round(time.Millisecond))
		if !errors.Is(err, ErrEngineExited) {
			t.Errorf("%v: %v", level, err)
		}
		if el > 5*time.Second {
			t.Errorf("%v: took %v, as if waiting on a box", level, el)
		}
	}
	if after := systemPopups(t); after != before {
		t.Errorf("%d system error boxes", after-before)
	}
}

// fitCase is a real load measured against the estimate.
type fitCase struct {
	file   string
	layers protocol.GPULayers
	ctx    int
}

// usedMB is the GPU memory in use, by nvidia-smi.
func usedMB(t *testing.T) int64 {
	t.Helper()
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatal(err)
	}
	var mb int64
	fmt.Sscan(strings.TrimSpace(string(out)), &mb)
	return mb
}

func restAfterStop(t *testing.T, want int64) int64 {
	t.Helper()
	var now int64
	for i := 0; i < 40; i++ {
		now = usedMB(t)
		if now <= want+150 {
			return now
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("the GPU is at %d MiB, was %d: other programs use it too, so this alone proves nothing", now, want)
	return now
}

// The estimate against reality, on the owner's models, read in place at low integrity.
// CRAGENT_TEST_FIT=<model folder> CRAGENT_ENGINE_DIR=<engine> go test -v -run TestFitAgainstTheRealEngine ./internal/supervisor
func TestFitAgainstTheRealEngine(t *testing.T) {
	dir, models := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_FIT")
	if dir == "" || models == "" {
		t.Skip("CRAGENT_TEST_FIT and CRAGENT_ENGINE_DIR not set")
	}
	for _, c := range []fitCase{
		{"Qwen3.8-27B-UD-Q5_K_XL.gguf", protocol.AllLayers(), 32768},
		{"Qwen3.8-27B-UD-Q5_K_XL.gguf", protocol.AllLayers(), 8192},
		{"Qwen3.6Q4_K_XL.gguf", protocol.AllLayers(), 32768},
		{"Qwen3.6Q6_K.gguf", protocol.Layers(30), 16384},
		{"Qwen3.6Q6_K.gguf", protocol.Layers(20), 8192},
	} {
		path := filepath.Join(models, c.file)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := f.Stat()
		h, err := gguf.Parse(f, gguf.MaxHeaderBytes, time.Now().Add(5*time.Second))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		sizes, _ := h.TensorBytes(uint64(st.Size()))
		s := DefaultSettings(8)
		s.Ctx, s.GPULayers, s.KVType = c.ctx, c.layers, "f16"
		e := fit.For(h, sizes, s)
		need := (e.Total() + fit.Reserve) >> 20
		rest := usedMB(t)
		eng, err := Start(context.Background(), Config{EngineDir: dir, ModelPath: path, Alias: "m", Level: LevelLowIntegrity, Deadline: 4 * time.Minute, Settings: s})
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		time.Sleep(1500 * time.Millisecond)
		measured := usedMB(t) - rest
		eng.Stop()
		restAfterStop(t, rest)
		ratio := float64(need) / float64(measured)
		t.Logf("%s, %+v, ctx %d: weights %d + KV %d + compute %d + reserve %d = %d MiB; measured %d MiB; ratio %.3f", c.file, c.layers, c.ctx, e.Weights>>20, e.KV>>20, e.Compute>>20, fit.Reserve>>20, need, measured, ratio)
		if ratio < 0.95 || ratio > 1.15 {
			t.Errorf("%s at %d: ratio %.3f", c.file, c.ctx, ratio)
		}
	}
}

// A load that truly cannot get its memory ends quickly as out of memory, and nothing stays behind. On this driver a
// GPU that is merely full does not give that: CUDA spills to system memory and the load succeeds, slowly (see
// docs/fit-measurements.md). What does is the job's memory cap, which every real engine has.
func TestRealEngineOutOfMemoryEndsCleanly(t *testing.T) {
	dir, models := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_FIT")
	if dir == "" || models == "" {
		t.Skip("CRAGENT_TEST_FIT and CRAGENT_ENGINE_DIR not set")
	}
	rest := usedMB(t)
	s := DefaultSettings(8)
	s.Ctx, s.GPULayers, s.KVType = 32768, protocol.AllLayers(), "f16"
	t0 := time.Now()
	eng, err := Start(context.Background(), Config{EngineDir: dir, ModelPath: filepath.Join(models, "Qwen3.6Q6_K.gguf"), Alias: "m", Level: LevelLowIntegrity, MemLimit: 2 << 30, Deadline: 4 * time.Minute, Settings: s})
	el := time.Since(t0)
	if err == nil {
		eng.Stop()
		t.Fatalf("it loaded in %v", el)
	}
	t.Logf("in %v: %v", el.Round(time.Millisecond), err)
	if !errors.Is(err, ErrEngineOutOfMemory) {
		t.Errorf("not recognised as out of memory: %v", err)
	}
	if el > time.Minute {
		t.Errorf("took %v", el)
	}
	t.Logf("GPU at rest %d MiB, now %d MiB", rest, restAfterStop(t, rest))
}
