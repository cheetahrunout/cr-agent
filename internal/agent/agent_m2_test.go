package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func (h *fakeHandle) stopped() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// lastEngine is the handle of the engine started last.
func (w *world) lastEngine() *fakeHandle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handles[len(w.handles)-1]
}

func (w *world) engineState() string { return w.a.Snapshot(true).Engine }

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// loadLocal loads the development model so a test can be about something other than downloads.
func (w *world) loadLocal(h *hub) {
	w.t.Helper()
	w.a.cfg.LocalModelID, w.a.cfg.LocalModelPath = "smollm2-360m-q8_0", "local.gguf"
	h.send(loadOK)
	h.until("load done", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
}

// Sleep and wake as ruling B12 has it: the engine is kept across the sleep and checked on wake.
func TestSuspendKeepsEngineAndWakeVerifies(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	w.loadLocal(h)
	eng := w.lastEngine()
	h.send(reqStream)
	h.next("res.data", 2*time.Second)

	t0 := time.Now()
	gone := make(chan struct{})
	go func() { w.a.Suspend(); close(gone) }()
	// The hub sees: status{sleeping} with the engine still loaded, then a close with 1001.
	var sleeping map[string]any
	var closeCode websocket.StatusCode = -1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := h.conn.Read(ctx)
		if err != nil {
			if closeCode = websocket.CloseStatus(err); closeCode == -1 {
				t.Fatalf("socket ended without a close frame: %v", err)
			}
			break
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["t"] == "status" && m["sleeping"] == true {
			sleeping = m
		}
	}
	<-gone
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("suspend took %v; Windows grants about two seconds", el)
	}
	if sleeping == nil || sleeping["engine"] != "loaded" || sleeping["model_id"] != "smollm2-360m-q8_0" || closeCode != websocket.StatusGoingAway {
		t.Fatalf("sleeping status %v, close code %d", sleeping, closeCode)
	}
	select {
	case <-w.fe.disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer in flight was not aborted")
	}
	if eng.stopped() || w.engineState() != "loaded" {
		t.Fatalf("the engine must be kept across the sleep (stopped=%v, state %q)", eng.stopped(), w.engineState())
	}
	select {
	case <-w.hubs:
		t.Fatal("the agent reconnected while asleep")
	case <-time.After(150 * time.Millisecond):
	}

	// Wake: reconnect at once; the kept engine shows as loading until it passed /health and one token.
	checks := w.fe.requests.Load()
	release := make(chan struct{})
	real := w.a.cfg.WakeCheck
	w.a.cfg.WakeCheck = func(ctx context.Context, base, key, alias string) error {
		<-release // hold the check so the hello below is read before it finishes
		return real(ctx, base, key, alias)
	}
	t1 := time.Now()
	w.a.Resume()
	h2, hello := w.accept()
	if el := time.Since(t1); el > time.Second {
		t.Fatalf("reconnect after wake took %v", el)
	}
	if hello["engine"] != "loading" || hello["model_id"] != "smollm2-360m-q8_0" || hello["last_disconnect_reason"] != "sleep" || len(hello["inflight"].([]any)) != 0 {
		t.Fatalf("hello after wake: %v", hello)
	}
	// Not usable before the check has passed.
	h2.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T5","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"x"}]}}`)
	if hd := h2.next("res.head", 2*time.Second); hd["status"].(float64) != 503 {
		t.Fatalf("a request before the wake check got %v", hd["status"])
	}
	close(release)
	h2.until("loaded after the check", 5*time.Second, func(m map[string]any) bool { return m["t"] == "status" && m["engine"] == "loaded" })
	if w.fe.requests.Load() != checks+1 {
		t.Fatalf("the one-token completion was sent %d times", w.fe.requests.Load()-checks)
	}
	w.fe.mu.Lock()
	body := w.fe.lastBody
	w.fe.mu.Unlock()
	if body["max_tokens"].(float64) != 1 || body["model"] != "smollm2-360m-q8_0" || body["stream"] != false {
		t.Fatalf("wake check body %v", body)
	}
	if eng.stopped() {
		t.Fatal("a healthy engine was stopped on wake")
	}
	h2.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T6","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"x"}]}}`)
	if hd := h2.next("res.head", 2*time.Second); hd["status"].(float64) != 200 {
		t.Fatalf("a request after the wake check got %v", hd["status"])
	}
	// A second wake signal (Windows sends two) does nothing.
	w.a.Resume()
	select {
	case <-w.hubs:
		t.Fatal("a duplicate wake signal reconnected again")
	case <-time.After(150 * time.Millisecond):
	}
}

// An engine that does not survive the sleep is stopped and reported unloaded with the reason.
func TestWakeCheckFailureStopsEngine(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	w.loadLocal(h)
	eng := w.lastEngine()
	w.a.Suspend()
	w.a.cfg.WakeCheck = func(context.Context, string, string, string) error { return errors.New("/health did not answer") }
	w.a.Resume()
	h2, _ := w.accept()
	st := h2.until("unloaded after a failed check", 5*time.Second, func(m map[string]any) bool { return m["t"] == "status" && m["engine"] == "unloaded" })
	if st["engine_error"] != "sleep_check_failed" || st["model_id"] != nil {
		t.Fatalf("status %v", st)
	}
	waitFor(t, "the engine to be stopped", 2*time.Second, eng.stopped)
	// The hub can load again.
	h2.send(loadOK)
	h2.until("load after the failed check", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	if w.lastEngine() == eng || w.engineState() != "loaded" {
		t.Fatal("no fresh engine after the failed check")
	}
}

// Ruling B10: a dead hub never holds the host's VRAM.
func TestOfflineUnload(t *testing.T) {
	w := newWorldWith(t, func(c *Config) { c.Tick, c.OfflineUnload = 20*time.Millisecond, 250*time.Millisecond })
	h, _ := w.connect()
	w.loadLocal(h)
	eng := w.lastEngine()
	// Connected: the model stays, however long.
	time.Sleep(400 * time.Millisecond)
	if eng.stopped() {
		t.Fatal("the model was unloaded while the hub was connected")
	}
	// The hub goes away and stays away.
	w.mu.Lock()
	w.keys = map[string]bool{}
	w.mu.Unlock()
	t0 := time.Now()
	h.conn.CloseNow()
	waitFor(t, "the offline unload", 3*time.Second, eng.stopped)
	if el := time.Since(t0); el < 200*time.Millisecond {
		t.Fatalf("unloaded after only %v offline", el)
	}
	if w.engineState() != "unloaded" {
		t.Fatalf("state %q", w.engineState())
	}
}

func TestIdleUnload(t *testing.T) {
	w := newWorldWith(t, func(c *Config) { c.Tick, c.IdleUnit = 20*time.Millisecond, 100*time.Millisecond })
	h, _ := w.connect()
	// Three units of idle time (300 ms here), with a request in between that restarts the clock.
	// (No idle time set means never: TestOfflineUnload keeps a model loaded that way.)
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2TA","op":"config","args":{"idle_unload_min":3}}`)
	h.until("config", 2*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	w.loadLocal(h)
	first := w.lastEngine()
	time.Sleep(200 * time.Millisecond)
	if first.stopped() {
		t.Fatal("unloaded before the idle time had passed")
	}
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T5","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"x"}]}}`)
	t0 := time.Now()
	st := h.until("idle unload", 3*time.Second, func(m map[string]any) bool { return m["t"] == "status" && m["engine"] == "unloaded" })
	if el := time.Since(t0); el < 250*time.Millisecond {
		t.Fatalf("unloaded %v after the last request; the idle time is 300 ms", el)
	}
	if st["model_id"] != nil || !first.stopped() {
		t.Fatalf("status %v", st)
	}
	if w.st.Config().IdleUnloadMin != 3 {
		t.Fatal("idle_unload_min not persisted")
	}
}

// §12.4: awake while a request, a load or a download runs; with keep_awake on, while a model is loaded.
func TestKeepAwake(t *testing.T) {
	var mu sync.Mutex
	awake := false
	isAwake := func() bool { mu.Lock(); defer mu.Unlock(); return awake }
	w := newWorldWith(t, func(c *Config) {
		c.Tick = 20 * time.Millisecond
		c.KeepAwake = func(on bool) { mu.Lock(); awake = on; mu.Unlock() }
	})
	h, _ := w.connect()
	if isAwake() {
		t.Fatal("awake while idle")
	}
	w.loadLocal(h)
	waitFor(t, "sleep to be allowed with a model loaded and keep_awake off", 2*time.Second, func() bool { return !isAwake() })
	h.send(reqStream)
	h.next("res.data", 2*time.Second)
	waitFor(t, "awake during an answer", 2*time.Second, isAwake)
	h.send(cancelStream)
	h.next("res.end", 2*time.Second)
	waitFor(t, "sleep to be allowed after the answer", 2*time.Second, func() bool { return !isAwake() })
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2TA","op":"config","args":{"keep_awake":true}}`)
	waitFor(t, "awake with keep_awake on and a model loaded", 2*time.Second, isAwake)
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2TB","op":"unload","args":{}}`)
	waitFor(t, "sleep to be allowed with nothing loaded", 2*time.Second, func() bool { return !isAwake() })
	// A PC that is going to sleep is never held awake by the agent.
	w.loadLocal(h)
	waitFor(t, "awake again", 2*time.Second, isAwake)
	w.a.Suspend()
	if isAwake() {
		t.Fatal("still asking Windows to stay awake while suspending")
	}
}

// Ruling B6: a load that does not fit is refused with the numbers; nothing is adjusted.
func TestPreLoadVRAMCheck(t *testing.T) {
	var mu sync.Mutex
	free, freeErr := int64(100<<20), error(nil)
	w := newWorldWith(t, func(c *Config) {
		c.GPUFree = func(context.Context) (int64, error) { mu.Lock(); defer mu.Unlock(); return free, freeErr }
	})
	h, _ := w.connect()
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"download","args":{"model_id":"tiny-q4_k_m","repo":"unsloth/Tiny-GGUF","commit":"` + commitA + `","files":["Tiny-Q4_K_M.gguf"],"kind":"gguf"}}`)
	h.until("download", 15*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	load := func(id, layers string) string {
		return `{"v":1,"t":"cmd","id":"` + id + `","op":"load","args":{"model_id":"tiny-q4_k_m","settings":{"ctx":4096,"gpu_layers":` + layers + `,"threads":4,"kv_type":"f16","flash_attn":"on"}}}`
	}
	started := func() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.started) }

	// 100 MB free: the compute buffer alone does not fit.
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T4", `"all"`))
	e := h.next("cmd.err", 3*time.Second)
	if e["code"] != "insufficient_vram" || e["free_mb"].(float64) != 100 || e["need_mb"].(float64) <= 100 {
		t.Fatalf("refusal %v", e)
	}
	if started() != 0 || w.engineState() != "unloaded" {
		t.Fatal("the engine was started for a load that does not fit")
	}
	// The same model with nothing on the GPU needs no VRAM.
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T5", `0`))
	h.until("CPU-only load", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T5" })
	// Enough free memory: it loads, with the settings as sent (no automatic change).
	mu.Lock()
	free = 8 << 30
	mu.Unlock()
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T6", `"all"`))
	h.until("load that fits", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T6" })
	w.mu.Lock()
	sc := w.started[len(w.started)-1]
	w.mu.Unlock()
	if !sc.Settings.GPULayers.All || sc.Settings.Ctx != 4096 {
		t.Fatalf("settings were changed: %+v", sc.Settings)
	}
	// Free memory unknown: the load is not blocked by the check.
	mu.Lock()
	free, freeErr = 0, errors.New("no NVML")
	mu.Unlock()
	h.send(load("c-01JCKZ3Q9X8YB7N6M5P4R3S2T7", `"all"`))
	h.until("load without a reading", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T7" })
}
