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
		GPUInUse: func(context.Context, uint32) (bool, error) { return false, nil },
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
