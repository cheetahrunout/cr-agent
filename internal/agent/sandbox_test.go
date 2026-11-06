package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

type bareEngine struct{ done chan struct{} }

func (e *bareEngine) Target() (string, string, string) { return "http://127.0.0.1:1", "k", "m" }
func (e *bareEngine) PID() uint32                      { return 42 }
func (e *bareEngine) Done() <-chan struct{}            { return e.done }
func (e *bareEngine) Stop()                            {}

func sandboxSettings(layers int) protocol.Settings {
	return protocol.Settings{Ctx: 4096, GPULayers: protocol.Layers(layers), Threads: 2, KVType: "f16", FlashAttn: "auto"}
}

// Finding H1: a model that makes the engine exit under the low-integrity token must not get a second
// start at the user's full integrity.
func TestCrashUnderLowILIsNeverRetriedOutside(t *testing.T) {
	var runs []bool
	a := New(Config{Version: "0.1.0", LowIL: true, Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
		runs = append(runs, c.LowIL)
		return nil, fmt.Errorf("%w (exit code 1)", supervisor.ErrEngineExited)
	}})
	defer a.Shutdown()
	_, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(0))
	if err == nil || len(runs) != 1 || !runs[0] {
		t.Fatalf("runs %v err %v", runs, err)
	}
	if in := a.Snapshot(true); in.Engine != protocol.EngineError {
		t.Fatalf("engine %q", in.Engine)
	}
	if a.SandboxBlocked() {
		t.Fatal("a crash was taken for CUDA being blocked: that would offer the opt-in to a model that crashes on purpose")
	}
}

// A failed CUDA check is an engine error with its own code; only the host's click changes the token.
func TestNoCUDAIsAnErrorUntilTheHostOptsIn(t *testing.T) {
	var runs []bool
	a := New(Config{Version: "0.1.0", LowIL: true,
		GPUInUse:  func(context.Context, uint32) (bool, error) { return false, nil },
		ProbeCUDA: func() (bool, error) { return false, nil }, // the engine alone sees no device: it is real
		Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
			runs = append(runs, c.LowIL)
			return &bareEngine{done: make(chan struct{})}, nil
		}})
	defer a.Shutdown()
	_, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1))
	if !errors.Is(err, errNoCUDAUnderLowIL) || len(runs) != 1 || !runs[0] {
		t.Fatalf("runs %v err %v", runs, err)
	}
	if !a.SandboxBlocked() || a.Snapshot(true).Engine != protocol.EngineError {
		t.Fatal("not reported")
	}
	a.mu.Lock()
	code := a.engineErr
	a.mu.Unlock()
	if code != EngineNoCUDALowIL {
		t.Fatalf("code %q", code)
	}
	// Loading again changes nothing without the click.
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1)); err == nil || runs[1] != true {
		t.Fatalf("second load without the opt-in: runs %v err %v", runs, err)
	}
	// The host clicks.
	a.AllowNormalIntegrity(true)
	if a.SandboxBlocked() {
		t.Fatal("still offered after the click")
	}
	a.cfg.GPUInUse = func(context.Context, uint32) (bool, error) { return true, nil }
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1)); err != nil || runs[2] != false {
		t.Fatalf("after the opt-in: runs %v err %v", runs, err)
	}
}

// The check that cannot run says nothing about CUDA: the engine stays in the sandbox.
func TestCheckThatCannotRunKeepsTheSandbox(t *testing.T) {
	var runs []bool
	a := New(Config{Version: "0.1.0", LowIL: true,
		GPUInUse: func(context.Context, uint32) (bool, error) { return false, errors.New("nvml not there") },
		Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
			runs = append(runs, c.LowIL)
			return &bareEngine{done: make(chan struct{})}, nil
		}})
	defer a.Shutdown()
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1)); err != nil || len(runs) != 1 || !runs[0] {
		t.Fatalf("runs %v err %v", runs, err)
	}
	if a.SandboxBlocked() {
		t.Fatal("offered the opt-in on no evidence")
	}
}

// Windows refusing the token, before anything ran, is the one case for a normal-integrity start.
func TestTokenRefusalFallsBack(t *testing.T) {
	var runs []bool
	a := New(Config{Version: "0.1.0", LowIL: true, Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
		runs = append(runs, c.LowIL)
		if c.LowIL {
			return nil, fmt.Errorf("%w: CreateProcessAsUser: privilege not held", supervisor.ErrLowILUnavailable)
		}
		return &bareEngine{done: make(chan struct{})}, nil
	}})
	defer a.Shutdown()
	res, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(0))
	if err != nil || len(runs) != 2 || runs[1] || res["low_il"] != false {
		t.Fatalf("runs %v res %v err %v", runs, res, err)
	}
}

// The chosen GPU reaches the engine's start.
func TestGPUSelectReachesTheEngine(t *testing.T) {
	var got string
	a := New(Config{Version: "0.1.0", Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
		got = c.GPUSelect
		return &bareEngine{done: make(chan struct{})}, nil
	}})
	defer a.Shutdown()
	g := "1"
	a.mu.Lock()
	a.config.GPUSelect = &g
	a.mu.Unlock()
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(0)); err != nil || got != "1" {
		t.Fatalf("gpu_select %q err %v", got, err)
	}
}

// The host's choice is remembered per PC, and no frame from the hub can make it (finding H1).
func TestNormalIntegrityIsTheHostsOwn(t *testing.T) {
	st, err := store.OpenWith(t.TempDir(), plain{})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Config{Version: "0.1.0", LowIL: true, Store: st})
	if a.NormalIntegrity() {
		t.Fatal("on by default")
	}
	a.Shutdown()
	st.Update(func(c *store.Config) { c.EngineNormalIntegrity = true })
	b := New(Config{Version: "0.1.0", LowIL: true, Store: st})
	defer b.Shutdown()
	if !b.NormalIntegrity() {
		t.Fatal("not remembered")
	}
	// The config command has no such field, and the decoder is strict.
	for _, field := range []string{"engine_normal_integrity", "low_il", "sandbox"} {
		_, err := protocol.Decode([]byte(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"config","args":{"` + field + `":true}}`))
		if err == nil {
			t.Errorf("config{%s} was accepted", field)
		}
	}
}

// The hub hears why, with the code.
func TestNoCUDACodeReachesTheHub(t *testing.T) {
	w := newWorldWith(t, func(c *Config) {
		c.LowIL = true
		c.GPUInUse = func(context.Context, uint32) (bool, error) { return false, nil }
	})
	h, _ := w.connect()
	w.a.cfg.LocalModelID, w.a.cfg.LocalModelPath = "smollm2-360m-q8_0", "local.gguf"
	h.send(loadOK)
	var engineErr any
	e := h.until("the refusal", 5*time.Second, func(m map[string]any) bool {
		if m["t"] == "status" && m["engine_error"] != nil {
			engineErr = m["engine_error"]
		}
		return m["t"] == "cmd.err"
	})
	if e["code"] != "engine_failed" || !strings.Contains(e["message"].(string), EngineNoCUDALowIL) || engineErr != EngineNoCUDALowIL {
		t.Fatalf("cmd.err %v, status engine_error %v", e, engineErr)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.started) != 1 || !w.started[0].LowIL {
		t.Fatalf("starts: %d", len(w.started))
	}
}

// A hub can make one load fail its CUDA check (a gpu_select of a GPU the PC does not have). That must
// not raise the offer to leave the sandbox: the agent's own probe, with no hub input, still sees a device.
func TestHubCannotRaiseTheOffer(t *testing.T) {
	probes := 0
	gpuOK := false
	a := New(Config{Version: "0.1.0", LowIL: true,
		GPUInUse:  func(context.Context, uint32) (bool, error) { return gpuOK, nil },
		ProbeCUDA: func() (bool, error) { probes++; return true, nil },
		Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
			return &bareEngine{done: make(chan struct{})}, nil
		}})
	defer a.Shutdown()
	s := "7"
	a.mu.Lock()
	a.config.GPUSelect = &s
	a.mu.Unlock()
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1)); !errors.Is(err, errNoCUDAUnderLowIL) {
		t.Fatal(err)
	}
	if a.SandboxBlocked() || probes != 1 {
		t.Fatalf("offer raised %v, probes %d", a.SandboxBlocked(), probes)
	}
	// A probe that fails is no evidence either, and without a probe nothing is offered.
	a.cfg.ProbeCUDA = func() (bool, error) { return false, errors.New("no answer") }
	a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1))
	if a.SandboxBlocked() {
		t.Fatal("offered on a failed probe")
	}
	// A real "no" raises it; any good load under low integrity clears it.
	a.cfg.ProbeCUDA = func() (bool, error) { return false, nil }
	a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1))
	if !a.SandboxBlocked() {
		t.Fatal("not offered on a real no")
	}
	gpuOK = true
	if _, err := a.Load(context.Background(), "m", "x.gguf", sandboxSettings(1)); err != nil {
		t.Fatal(err)
	}
	if a.SandboxBlocked() {
		t.Fatal("the offer stayed after a good load in the sandbox")
	}
}

// A late first-load hash cannot replace a model a later load brought: the slot is held from the start.
func TestLoadSlotIsHeldFromTheStart(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"download","args":{"model_id":"tiny-q4_k_m","repo":"unsloth/Tiny-GGUF","commit":"` + commitA + `","files":["Tiny-Q4_K_M.gguf"],"kind":"gguf"}}`)
	h.until("download", 15*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	hashing, release := make(chan struct{}), make(chan struct{})
	w.a.testBeforeResolve = func() { close(hashing); <-release }
	load := func(id string) string {
		return `{"v":1,"t":"cmd","id":"` + id + `","op":"load","args":{"model_id":"tiny-q4_k_m","settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on"}}}`
	}
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T4"))
	<-hashing
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T5"))
	e := h.until("the second load's answer", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.err" })
	if e["code"] != "busy" || e["id"] != "c-01JCKZ3Q9X8YB7N6M5P4R3S2T5" {
		t.Fatalf("second load during the first's hash: %v", e)
	}
	close(release)
	h.until("the first load", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T4" })
}
