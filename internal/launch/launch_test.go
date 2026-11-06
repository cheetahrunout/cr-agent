package launch

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/release/reltest"
	"github.com/cheetahrunout/cr-agent/internal/sign"
)

// fakeProc is an agent whose life the test scripts.
type fakeProc struct {
	done   chan struct{}
	code   int
	once   sync.Once
	killed bool
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) ExitCode() int         { return p.code }
func (p *fakeProc) Kill()                 { p.exit(-1); p.killed = true }
func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.code = code
		close(p.done)
	})
}

type world struct {
	t       *testing.T
	root    string
	r       *Runner
	mu      sync.Mutex
	started []string
	procs   []*fakeProc
	// behave is what each version does once started.
	behave map[string]func(p *fakeProc, n int)
}

func version(exe string) string { return filepath.Base(filepath.Dir(exe)) }

func newWorld(t *testing.T) *world {
	w := &world{t: t, root: t.TempDir(), behave: map[string]func(*fakeProc, int){}}
	w.r = &Runner{Root: w.root, Keys: reltest.Trusting(sign.ThrowawayKey()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Gate: 300 * time.Millisecond, Poll: 5 * time.Millisecond, CrashPause: time.Millisecond, MaxCrashes: 3,
		Start: func(exe string, args []string) (Proc, error) {
			if len(args) != 2 || args[0] != "--home" || args[1] != w.root {
				t.Errorf("agent started with %v", args)
			}
			p := &fakeProc{done: make(chan struct{})}
			w.mu.Lock()
			v := version(exe)
			w.started = append(w.started, v)
			w.procs = append(w.procs, p)
			n := 0
			for _, s := range w.started {
				if s == v {
					n++
				}
			}
			fn := w.behave[v]
			w.mu.Unlock()
			if fn != nil {
				go fn(p, n)
			}
			return p, nil
		}}
	return w
}

// install puts a version on disk; signed adds its manifest and signature.
func (w *world) install(v string, signed bool) {
	w.t.Helper()
	agent := []byte("agent " + v)
	if signed {
		dist := w.t.TempDir()
		if _, err := reltest.MakeDist(dist, reltest.Spec{Version: v, Seq: 1, Agent: agent, Engine: map[string][]byte{"x.dll": []byte("e")}}, sign.ThrowawayKey()); err != nil {
			w.t.Fatal(err)
		}
		os.MkdirAll(layout.VersionDir(w.root, v), 0o755)
		for _, f := range []string{"manifest.json", "manifest.json.minisig"} {
			b, _ := os.ReadFile(filepath.Join(dist, f))
			os.WriteFile(filepath.Join(layout.VersionDir(w.root, v), f), b, 0o644)
		}
	}
	os.MkdirAll(layout.VersionDir(w.root, v), 0o755)
	os.WriteFile(layout.AgentExe(w.root, v), agent, 0o755)
}

func (w *world) pointer() layout.Pointer {
	p, err := layout.ReadPointer(w.root)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *world) markHealthy(v string) {
	os.WriteFile(layout.HealthyFile(w.root, v), []byte("ok"), 0o644)
}

func (w *world) run() (int, string) {
	code := w.r.Run()
	w.mu.Lock()
	defer w.mu.Unlock()
	return code, strings.Join(w.started, " ")
}

func pending(v, prev string) layout.Pointer {
	now := time.Now().UTC()
	return layout.Pointer{Version: v, Previous: prev, PendingSince: &now}
}

func TestQuitEndsTheLauncher(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.0", false) // a fresh install: no manifest yet, started as it is
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	os.WriteFile(filepath.Join(w.root, "launcher.old"), []byte("old launcher"), 0o755)
	w.behave["0.1.0"] = func(p *fakeProc, _ int) { p.exit(ExitQuit) }
	if code, started := w.run(); code != 0 || started != "0.1.0" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if _, err := os.Stat(filepath.Join(w.root, "launcher.old")); !os.IsNotExist(err) {
		t.Fatal("launcher.old was not cleaned up")
	}
}

// The whole update path: 75 restarts into the pointer, the new version passes the gate, N-1 is kept.
func TestUpdatePassesTheGate(t *testing.T) {
	w := newWorld(t)
	for _, v := range []string{"0.0.9", "0.1.0", "0.1.1"} {
		w.install(v, true)
	}
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0", Previous: "0.0.9"})
	w.behave["0.1.0"] = func(p *fakeProc, _ int) { // the agent installed 0.1.1 and asks for a restart
		layout.WritePointer(w.root, pending("0.1.1", "0.1.0"))
		p.exit(ExitRestart)
	}
	w.behave["0.1.1"] = func(p *fakeProc, _ int) {
		time.Sleep(60 * time.Millisecond)
		w.markHealthy("0.1.1")
		time.Sleep(60 * time.Millisecond)
		p.exit(ExitQuit)
	}
	code, started := w.run()
	if code != 0 || started != "0.1.0 0.1.1" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if p := w.pointer(); p.Version != "0.1.1" || p.Previous != "0.1.0" || p.PendingSince != nil {
		t.Fatalf("pointer %+v", p)
	}
	got := layout.Installed(w.root)
	if len(got) != 2 || !(got[0] == "0.1.0" && got[1] == "0.1.1") {
		t.Fatalf("versions kept on disk: %v (want the current and the previous one)", got)
	}
}

// A new version that never becomes healthy is killed at the gate and the previous one comes back.
func TestUnhealthyVersionIsReverted(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.behave["0.1.1"] = func(p *fakeProc, _ int) { time.Sleep(30 * time.Millisecond); p.exit(ExitQuit) }
	t0 := time.Now()
	code, started := w.run()
	el := time.Since(t0)
	if code != 0 || started != "0.1.2 0.1.1" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if el < 280*time.Millisecond || el > 3*time.Second {
		t.Fatalf("reverted after %v; the gate is 300 ms here", el)
	}
	if !w.procs[0].killed {
		t.Fatal("the unhealthy agent was not killed")
	}
	if p := w.pointer(); p.Version != "0.1.1" || p.Previous != "" || p.PendingSince != nil {
		t.Fatalf("pointer after the revert %+v", p)
	}
	if _, err := os.Stat(layout.HealthyFile(w.root, "0.1.2")); !os.IsNotExist(err) {
		t.Fatal("healthy.0.1.2 exists after the revert")
	}
}

// Exiting before it is healthy fails the gate at once, without waiting two minutes.
func TestExitBeforeHealthyIsReverted(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.behave["0.1.2"] = func(p *fakeProc, _ int) { p.exit(1) }
	w.behave["0.1.1"] = func(p *fakeProc, _ int) { p.exit(ExitQuit) }
	t0 := time.Now()
	code, started := w.run()
	if code != 0 || started != "0.1.2 0.1.1" || time.Since(t0) > 250*time.Millisecond {
		t.Fatalf("exit %d, started %q, after %v", code, started, time.Since(t0))
	}
	if p := w.pointer(); p.Version != "0.1.1" {
		t.Fatalf("pointer %+v", p)
	}
}

// A version whose binary is not the signed one is never started.
func TestTamperedVersionIsNotStarted(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	os.WriteFile(layout.AgentExe(w.root, "0.1.2"), []byte("another program"), 0o755)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.behave["0.1.1"] = func(p *fakeProc, _ int) { p.exit(ExitQuit) }
	if code, started := w.run(); code != 0 || started != "0.1.1" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	// Not on trial and nothing to go back to: the launcher refuses and ends.
	w2 := newWorld(t)
	w2.install("0.1.2", true)
	os.WriteFile(layout.AgentExe(w2.root, "0.1.2"), []byte("another program"), 0o755)
	layout.WritePointer(w2.root, layout.Pointer{Version: "0.1.2"})
	if code, started := w2.run(); code != 1 || started != "" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	// A manifest signed by a key the launcher does not trust.
	w3 := newWorld(t)
	w3.install("0.1.2", true)
	other, _ := sign.KeyFromSeed(bytes.Repeat([]byte{6}, 32))
	w3.r.Keys = reltest.Trusting(other)
	layout.WritePointer(w3.root, layout.Pointer{Version: "0.1.2"})
	if code, started := w3.run(); code != 1 || started != "" {
		t.Fatalf("untrusted signature: exit %d, started %q", code, started)
	}
	// A pointer to a version that is not on disk.
	w4 := newWorld(t)
	w4.install("0.1.1", true)
	layout.WritePointer(w4.root, pending("0.1.9", "0.1.1"))
	w4.behave["0.1.1"] = func(p *fakeProc, _ int) { p.exit(ExitQuit) }
	if code, started := w4.run(); code != 0 || started != "0.1.1" {
		t.Fatalf("missing version: exit %d, started %q", code, started)
	}
}

func TestCrashesAndRefusals(t *testing.T) {
	// A crashing agent is restarted, a limited number of times.
	w := newWorld(t)
	w.install("0.1.0", true)
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	w.behave["0.1.0"] = func(p *fakeProc, _ int) { p.exit(3) }
	if code, started := w.run(); code != 1 || started != "0.1.0 0.1.0 0.1.0" {
		t.Fatalf("crash loop: exit %d, started %q", code, started)
	}
	// One crash, then a normal life.
	w = newWorld(t)
	w.install("0.1.0", true)
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	w.behave["0.1.0"] = func(p *fakeProc, n int) {
		if n == 1 {
			p.exit(3)
		} else {
			p.exit(ExitQuit)
		}
	}
	if code, started := w.run(); code != 0 || started != "0.1.0 0.1.0" {
		t.Fatalf("one crash: exit %d, started %q", code, started)
	}
	// An agent that refuses to start (another copy is running) is not restarted.
	w = newWorld(t)
	w.install("0.1.0", true)
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	w.behave["0.1.0"] = func(p *fakeProc, _ int) { p.exit(ExitRefused) }
	if code, started := w.run(); code != ExitRefused || started != "0.1.0" {
		t.Fatalf("refusal: exit %d, started %q", code, started)
	}
	// No pointer at all.
	w = newWorld(t)
	if code, _ := w.run(); code != 1 {
		t.Fatalf("no pointer: exit %d", code)
	}
}

// Finding L8. The launcher agrees with the updater on what a crash can leave of the manifest pair.
func TestVerifyManifestPairStates(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.0", true)
	if err := Verify(w.root, "0.1.0", w.r.Keys); err != nil {
		t.Fatal(err)
	}
	m := layout.ManifestFile(w.root, "0.1.0")
	raw, _ := os.ReadFile(m)
	os.Remove(m)
	if err := Verify(w.root, "0.1.0", w.r.Keys); err != nil {
		t.Fatalf("a signature without a manifest is what a crash leaves: %v", err)
	}
	os.WriteFile(m, raw, 0o644)
	os.Remove(m + ".minisig")
	if err := Verify(w.root, "0.1.0", w.r.Keys); err == nil {
		t.Fatal("a manifest without a signature was accepted")
	}
}

// The mark is written, and the release committed, in the last moment before the deadline, and even
// between the last poll and the deadline: the launcher must not revert past that commit.
func TestGateJudgesTheMarkAfterTheKill(t *testing.T) {
	w := newWorld(t)
	w.r.defaults()
	w.r.Gate, w.r.Poll = 2*time.Second, time.Hour // no poll sees it: only the check at the deadline can; wide, for a busy CI machine
	p := &fakeProc{done: make(chan struct{})}
	go func() {
		time.Sleep(200 * time.Millisecond)
		w.markHealthy("0.1.1")
	}()
	// The first check runs at once, before the mark; then only the deadline.
	if ok, why := w.r.gate(p, "0.1.1", time.Now()); !ok {
		t.Fatalf("failed: %s", why)
	}
	if !p.killed {
		t.Fatal("the mark was judged while the process could still write")
	}
}

// Healthy, then ended in any way: the gate is passed, and the version is not reverted.
func TestExitAfterHealthyIsNotReverted(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.behave["0.1.2"] = func(p *fakeProc, n int) {
		if n == 1 {
			w.markHealthy("0.1.2")
			p.exit(3) // a crash right after the commit
			return
		}
		p.exit(ExitQuit)
	}
	code, started := w.run()
	if code != 0 || started != "0.1.2 0.1.2" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if q := w.pointer(); q.Version != "0.1.2" || q.PendingSince != nil {
		t.Fatalf("pointer %+v", q)
	}
}

// A reboot during a trial: the mark from that trial stands, so a committed release is not reverted.
func TestMarkFromAnInterruptedTrialStands(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.markHealthy("0.1.2")
	w.behave["0.1.2"] = func(p *fakeProc, _ int) { p.exit(ExitQuit) }
	if code, started := w.run(); code != 0 || started != "0.1.2" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if q := w.pointer(); q.Version != "0.1.2" {
		t.Fatalf("pointer %+v", q)
	}
}

// A revert never leaves a pointer to a version the launcher would refuse.
func TestRevertChecksThePreviousVersion(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.1", true)
	w.install("0.1.2", true)
	os.WriteFile(layout.AgentExe(w.root, "0.1.1"), []byte("another program"), 0o755)
	layout.WritePointer(w.root, pending("0.1.2", "0.1.1"))
	w.behave["0.1.2"] = func(p *fakeProc, _ int) { p.exit(1) }
	code, started := w.run()
	if code != 1 || started != "0.1.2" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if q := w.pointer(); q.Version != "0.1.2" || q.PendingSince == nil {
		t.Fatalf("the pointer moved to a version that cannot start: %+v", q)
	}
}

// The launcher tells the host once when it gives up, without advising a reinstall.
func TestGivingUpTellsTheHost(t *testing.T) {
	w := newWorld(t)
	w.install("0.1.0", true)
	layout.WritePointer(w.root, layout.Pointer{Version: "0.1.0"})
	w.behave["0.1.0"] = func(p *fakeProc, _ int) { p.exit(1) }
	var said []string
	w.r.Notify = func(s string) { said = append(said, s) }
	if code, started := w.run(); code != 1 || started != "0.1.0 0.1.0 0.1.0" {
		t.Fatalf("exit %d, started %q", code, started)
	}
	if len(said) != 1 || strings.Contains(strings.ToLower(said[0]), "install") {
		t.Fatalf("told the host %q", said)
	}
	// A refusal (another copy is running) is not worth a message.
	w2 := newWorld(t)
	w2.install("0.1.0", true)
	layout.WritePointer(w2.root, layout.Pointer{Version: "0.1.0"})
	w2.behave["0.1.0"] = func(p *fakeProc, _ int) { p.exit(ExitRefused) }
	said = nil
	w2.r.Notify = func(s string) { said = append(said, s) }
	if code, _ := w2.run(); code != ExitRefused || len(said) != 0 {
		t.Fatalf("exit %d, told %q", code, said)
	}
}
