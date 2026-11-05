package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/gguf/gguftest"
	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/hf/hftest"
	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/pairing"
	"github.com/cheetahrunout/cr-agent/internal/status"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

const commitA = "7c1e2a9d40b3f1e8a9c0d2b4e6f8a1c3e5d7f9b2"

type plain struct{}

func (plain) Protect(_ string, b []byte) ([]byte, error)   { return append([]byte("P:"), b...), nil }
func (plain) Unprotect(_ string, b []byte) ([]byte, error) { return b[2:], nil }

type clicker struct {
	mu    sync.Mutex
	allow bool
	asked []models.Prompt
}

func (c *clicker) Ask(_ context.Context, p models.Prompt) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, p)
	return c.allow
}

// world is a fake hub (pairing + socket), a fake Hugging Face, a fake engine and a real agent.
type world struct {
	t       *testing.T
	a       *Agent
	st      *store.Store
	mm      *models.Manager
	hf      *hftest.Server
	fe      *fakeEngine
	hubs    chan *hub
	hubURL  string
	click   *clicker
	model   []byte
	root    string
	started []supervisor.Config
	closes  chan websocket.StatusCode

	mu       sync.Mutex
	codes    map[string]string // pairing code → state: ok | used | expired
	keys     map[string]bool   // machine keys the hub accepts
	pairReqs []map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, hf: hftest.New(), fe: newFakeEngine(t), hubs: make(chan *hub, 8), click: &clicker{},
		codes: map[string]string{"K7M29QWDX3": "ok", "EXP1REDC0D": "expired", "VSEDC0DE11": "used"}, keys: map[string]bool{},
		root: filepath.Join(t.TempDir(), "models"), closes: make(chan websocket.StatusCode, 8)}
	t.Cleanup(w.hf.Close)
	f := gguftest.Default()
	f.DataBytes = 128 << 10
	w.model = gguftest.Build(f)
	w.hf.AddRepo("unsloth/Tiny-GGUF", commitA, false, map[string][]byte{"Tiny-Q4_K_M.gguf": w.model})
	w.hf.AddRepo("wizard-org/Wizard-GGUF", commitA, false, map[string][]byte{"W-Q4_K_M.gguf": w.model})

	srv := httptest.NewServer(http.HandlerFunc(w.serveHub))
	t.Cleanup(srv.Close)
	w.hubURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent"

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	var err error
	if w.st, err = store.OpenWith(t.TempDir(), plain{}); err != nil {
		t.Fatal(err)
	}
	client, err := hf.New(hf.Options{Transport: w.hf.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	w.mm, err = models.New(models.Options{Store: w.st, Dir: w.root, HF: client, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Approver: w.click, ProgressEvery: 10 * time.Millisecond, RetryBase: time.Millisecond,
		DiskSpace: func(string) (uint64, uint64, error) { return 500 << 30, 1000 << 30, nil },
		OnChange:  func(withModels bool) { w.a.ModelsChanged(withModels) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.mm.Close)
	w.a = New(Config{Version: "0.1.0", LlamaTag: "test", LogicalCores: 8, Log: log, Models: w.mm, Store: w.st,
		HFToken:  func() string { return "absent" },
		CheckHub: func(u string) (string, error) { return u, nil }, // the test hub is local
		Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
			w.mu.Lock()
			w.started = append(w.started, c)
			w.mu.Unlock()
			return &fakeHandle{base: w.fe.srv.URL, done: make(chan struct{})}, nil
		},
		TuneLink: func(c *link.Config) {
			c.BaseDelay, c.MaxDelay, c.GoingAway = 10*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
		},
	})
	t.Cleanup(w.a.Shutdown)
	return w
}

func (w *world) serveHub(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/agent/pair":
		var req map[string]string
		json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&req)
		w.mu.Lock()
		w.pairReqs = append(w.pairReqs, req)
		state := w.codes[req["code"]]
		if state == "ok" {
			w.codes[req["code"]] = "used"
			w.keys["crm_"+req["key_id"]+"_"+req["key_secret"]] = true
		}
		w.mu.Unlock()
		// As the real hub: a mistyped, an expired and a used code all answer 404 pairing_code_invalid.
		if state == "ok" && pairing.ValidKey("crm_"+req["key_id"]+"_"+req["key_secret"]) {
			rw.Write([]byte(`{"machine_id":"01JCKZ3Q9X8YB7N6M5P4R3S2T1","slug":"anna-pc"}`))
		} else {
			rw.WriteHeader(http.StatusNotFound)
			rw.Write([]byte(`{"error":{"code":"pairing_code_invalid","type":"hub","message":"hub text"}}`))
		}
	case "/agent":
		w.mu.Lock()
		ok := w.keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		w.mu.Unlock()
		if !ok {
			http.Error(rw, "bad key", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(rw, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(link.MaxPayload)
		w.hubs <- &hub{t: w.t, conn: c, ctx: context.Background()}
		<-r.Context().Done()
	default:
		http.NotFound(rw, r)
	}
}

// connect pairs with the good code and returns the hub's side of the socket after hello and welcome.
func (w *world) connect() (*hub, map[string]any) {
	w.t.Helper()
	if _, err := w.a.PairWith(context.Background(), w.hubURL, "K7M29QWDX3", &pairing.Client{}, nil); err != nil {
		w.t.Fatalf("pair: %v", err)
	}
	return w.accept()
}

func (w *world) accept() (*hub, map[string]any) {
	w.t.Helper()
	var h *hub
	select {
	case h = <-w.hubs:
	case <-time.After(5 * time.Second):
		w.t.Fatal("agent did not connect")
	}
	hello := h.next("hello", 2*time.Second)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{}}`)
	return h, hello
}

// until reads frames until pred accepts one.
func (h *hub) until(what string, d time.Duration, pred func(m map[string]any) bool) map[string]any {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, d)
	defer cancel()
	for {
		_, b, err := h.conn.Read(ctx)
		if err != nil {
			h.t.Fatalf("waiting for %s: %v", what, err)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if pred(m) {
			return m
		}
	}
}

func modelState(m map[string]any, id string) string {
	ms, _ := m["models"].([]any)
	for _, x := range ms {
		if mm := x.(map[string]any); mm["model_id"] == id {
			return mm["state"].(string)
		}
	}
	return ""
}

func TestPairingMessagesAndConnect(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if w.a.ConnectStored() {
		t.Fatal("connected without a pairing")
	}
	for _, code := range []string{"WR0NGC0DE1", "EXP1REDC0D", "VSEDC0DE11"} { // mistyped, expired, used: one answer from the hub
		_, err := w.a.PairWith(ctx, w.hubURL, code, nil, nil)
		var pe *pairing.Error
		if !errors.As(err, &pe) || pe.Problem != pairing.ProblemInvalid || strings.Contains(pe.Message(), "hub text") {
			t.Fatalf("%s: got %v", code, err)
		}
		if w.st.Paired() {
			t.Fatalf("%s: a failed pairing stored a key", code)
		}
	}
	h, hello := w.connect()
	if hello["agent_version"] != "0.1.0" || hello["hf_token"] != "absent" {
		t.Fatalf("hello %v", hello)
	}
	c := w.st.Config()
	if !w.st.Paired() || c.MachineID != "01JCKZ3Q9X8YB7N6M5P4R3S2T1" || c.Slug != "anna-pc" || c.Hub != w.hubURL {
		t.Fatalf("stored pairing %+v", c)
	}
	// The same code again: already used. The working pairing stays.
	_, err := w.a.PairWith(ctx, w.hubURL, "K7M29QWDX3", nil, nil)
	var pe *pairing.Error
	if !errors.As(err, &pe) || pe.Problem != pairing.ProblemInvalid || !strings.Contains(pe.Message(), "already used") {
		t.Fatalf("reused code: %v", err)
	}
	// A PC paired before sends its machine id, so the hub can offer "Replace key".
	w.mu.Lock()
	last := w.pairReqs[len(w.pairReqs)-1]
	w.mu.Unlock()
	if last["prior_machine_id"] != "01JCKZ3Q9X8YB7N6M5P4R3S2T1" {
		t.Fatalf("prior machine id not sent: %v", last)
	}
	h.send(`{"v":1,"t":"ping","ts":7}`)
	h.next("pong", 2*time.Second)

	// After a restart the stored key connects on its own.
	w.a.Disconnect()
	if !w.a.ConnectStored() {
		t.Fatal("stored pairing not used")
	}
	w.accept()
	deadline := time.Now().Add(2 * time.Second)
	for w.a.Snapshot(true).Link != link.PhaseConnected && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if in := w.a.Snapshot(true); in.Link != link.PhaseConnected {
		t.Fatalf("phase %q", in.Link)
	}
	// Unpair forgets the key and the link stops.
	if err := w.a.Unpair(); err != nil || w.st.Paired() || w.a.ConnectStored() {
		t.Fatalf("unpair: %v", err)
	}
	if text, _ := status.Line(w.a.Snapshot(w.st.Paired())); text != "Not paired" {
		t.Fatalf("status %q", text)
	}
}

func TestInspectDownloadLoadDelete(t *testing.T) {
	w := newWorld(t)
	h, hello := w.connect()
	caps, _ := json.Marshal(hello["capabilities"])
	if !strings.Contains(string(caps), "inspect") || len(hello["models"].([]any)) != 0 {
		t.Fatalf("hello %v", hello)
	}

	// inspect: header facts, nothing on disk.
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"inspect","args":{"repo":"unsloth/Tiny-GGUF","commit":"` + commitA + `","file":"Tiny-Q4_K_M.gguf"}}`)
	done := h.next("cmd.done", 5*time.Second)
	res := done["result"].(map[string]any)
	if res["arch"] != "llama" || res["kv"].(map[string]any)["context_length"].(float64) != 8192 || res["split_count"].(float64) != 1 {
		t.Fatalf("inspect result %v", res)
	}

	// load before download: not found.
	load := `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T2","op":"load","args":{"model_id":"tiny-q4_k_m","settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on"}}}`
	h.send(load)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "not_found" {
		t.Fatalf("load before download: %v", e)
	}

	// download: ack, progress as status job frames, done, and the model shows as ready.
	w.hf.Lock()
	w.hf.ChunkDelay = 10 * time.Millisecond
	w.hf.Unlock()
	dl := `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"download","args":{"model_id":"tiny-q4_k_m","repo":"unsloth/Tiny-GGUF","commit":"` + commitA + `","files":["Tiny-Q4_K_M.gguf"],"kind":"gguf"}}`
	h.send(dl)
	sawProgress, sawReady, sawDone := false, false, false
	h.until("download to finish", 15*time.Second, func(m map[string]any) bool {
		switch m["t"] {
		case "status":
			for _, j := range m["jobs"].([]any) {
				jm := j.(map[string]any)
				if jm["model_id"] == "tiny-q4_k_m" && jm["kind"] == "download" && jm["state"] == "running" && jm["bytes_done"] != nil &&
					jm["bytes_done"].(float64) > 0 && jm["bytes_total"].(float64) == float64(len(w.model)) {
					sawProgress = true
				}
			}
			if modelState(m, "tiny-q4_k_m") == "ready" {
				sawReady = true
			}
		case "cmd.done":
			sawDone = m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T3" && m["result"].(map[string]any)["size_bytes"].(float64) == float64(len(w.model))
		case "cmd.err":
			t.Fatalf("download failed: %v", m)
		}
		return sawDone && sawReady
	})
	if !sawProgress {
		t.Fatal("no progress status frame during the download")
	}
	file := filepath.Join(w.root, "unsloth", "Tiny-GGUF", commitA[:12], "Tiny-Q4_K_M.gguf")
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}

	// load: the engine gets the downloaded file and the model id as alias; n_layer and n_ctx_train
	// now come from the file's own header (2 blocks, 8192 tokens).
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T4","op":"load","args":{"model_id":"tiny-q4_k_m","settings":{"ctx":16384,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on"}}}`)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "bad_setting" || e["field"] != "ctx" {
		t.Fatalf("ctx over n_ctx_train: %v", e)
	}
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T5","op":"load","args":{"model_id":"tiny-q4_k_m","settings":{"ctx":4096,"gpu_layers":4,"threads":4,"kv_type":"f16","flash_attn":"on"}}}`)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "bad_setting" || e["field"] != "gpu_layers" {
		t.Fatalf("gpu_layers over n_layer+1: %v", e)
	}
	h.send(load)
	h.until("load done", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T2" })
	w.mu.Lock()
	sc := w.started[len(w.started)-1]
	w.mu.Unlock()
	if sc.ModelPath != file || sc.Alias != "tiny-q4_k_m" {
		t.Fatalf("engine started with %q alias %q", sc.ModelPath, sc.Alias)
	}

	// delete while loaded is refused; a second download of a loaded model too.
	del := `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T6","op":"delete","args":{"model_id":"tiny-q4_k_m"}}`
	h.send(del)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "busy" {
		t.Fatalf("delete while loaded: %v", e)
	}
	h.send(dl)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "busy" {
		t.Fatalf("download while loaded: %v", e)
	}

	// pause from the hub: the model is unloaded, requests and loads are refused, status says so.
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T7","op":"pause","args":{}}`)
	h.until("paused status", 5*time.Second, func(m map[string]any) bool {
		return m["t"] == "status" && m["local_pause"] == true && m["engine"] == "unloaded"
	})
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T8","path":"/v1/chat/completions","stream":true,"body":{"messages":[{"role":"user","content":"x"}]}}`)
	if hd := h.next("res.head", 2*time.Second); hd["status"].(float64) != 503 {
		t.Fatalf("req while paused: %v", hd)
	}
	if d := h.next("res.data", 2*time.Second); !strings.Contains(d["d"].([]any)[0].(string), "machine_paused") {
		t.Fatalf("pause body %v", d)
	}
	h.send(load)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "busy" {
		t.Fatalf("load while paused: %v", e)
	}
	if !w.st.Config().LocalPause {
		t.Fatal("pause not persisted")
	}
	resumed := make(chan struct{}, 1)
	w.a.cfg.ResumedByHub = func() { resumed <- struct{}{} }
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T9","op":"resume","args":{}}`)
	h.until("resumed status", 5*time.Second, func(m map[string]any) bool { return m["t"] == "status" && m["local_pause"] == false })
	select {
	case <-resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("the tray was not told that the hub lifted the pause")
	}

	// delete: files and the index entry go, and a status frame tells the hub.
	h.send(del)
	h.until("delete done", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T6" })
	if _, err := os.Stat(file); !os.IsNotExist(err) || len(w.mm.Models()) != 0 {
		t.Fatalf("delete left the file or the entry: %v", err)
	}
	h.send(del)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "not_found" {
		t.Fatalf("delete twice: %v", e)
	}
	// The golden invalid download stays refused by the decoder, before any of this runs.
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T5","op":"download","args":{"model_id":"evil","repo":"someone/repo","commit":"` + commitA + `","files":["../../Windows/system32/evil.gguf"],"kind":"gguf"}}`)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "bad_args" || e["field"] != "files" {
		t.Fatalf("path traversal: %v", e)
	}
}

func TestApprovalAndCancelOverTheLink(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	wiz := func(id string) string {
		return `{"v":1,"t":"cmd","id":"` + id + `","op":"download","args":{"model_id":"wizard-q4_k_m","repo":"wizard-org/Wizard-GGUF","commit":"` + commitA + `","files":["W-Q4_K_M.gguf"],"kind":"gguf"}}`
	}
	// Deny: the hub sees the approval job waiting for a click, then cmd.err denied and the model as denied.
	h.send(wiz("c-01JCKZ3Q9X8YB7N6M5P4R3S2T1"))
	sawWaiting := false
	e := h.until("denied", 5*time.Second, func(m map[string]any) bool {
		if m["t"] == "status" {
			for _, j := range m["jobs"].([]any) {
				if jm := j.(map[string]any); jm["kind"] == "approval" && jm["state"] == "awaiting_click" && jm["awaiting_click"] == true {
					sawWaiting = true
				}
			}
		}
		return m["t"] == "cmd.err"
	})
	if e["code"] != "denied" || !sawWaiting {
		t.Fatalf("deny: %v (saw waiting: %v)", e, sawWaiting)
	}
	if p := w.click.asked[0]; p.Repo != "wizard-org/Wizard-GGUF" || p.SizeBytes != int64(len(w.model)) {
		t.Fatalf("prompt %+v", p)
	}

	// Allow, then cancel{c-id} mid-transfer: cmd.err cancelled and nothing left on disk.
	w.click.mu.Lock()
	w.click.allow = true
	w.click.mu.Unlock()
	w.hf.Lock()
	w.hf.ChunkDelay = 30 * time.Millisecond
	w.hf.Unlock()
	h.send(wiz("c-01JCKZ3Q9X8YB7N6M5P4R3S2T2"))
	h.until("bytes moving", 5*time.Second, func(m map[string]any) bool {
		if m["t"] != "status" {
			return false
		}
		for _, j := range m["jobs"].([]any) {
			if jm := j.(map[string]any); jm["kind"] == "download" && jm["bytes_done"] != nil && jm["bytes_done"].(float64) > 0 {
				return true
			}
		}
		return false
	})
	h.send(`{"v":1,"t":"cancel","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T2"}`)
	if e := h.next("cmd.err", 5*time.Second); e["code"] != "cancelled" || e["id"] != "c-01JCKZ3Q9X8YB7N6M5P4R3S2T2" {
		t.Fatalf("cancel: %v", e)
	}
	if _, err := os.Stat(filepath.Join(w.root, "wizard-org")); !os.IsNotExist(err) {
		t.Fatal("a cancelled download left files")
	}
}

// The hub URL in config.json is input: a release build connects only to the production host, whatever the file says.
func TestStoredHubIsRevalidated(t *testing.T) {
	w := newWorld(t)
	w.connect()
	w.a.Disconnect()
	w.a.cfg.CheckHub = link.ValidateHubURL // the real rule of this build
	if _, err := link.ValidateHubURL(w.hubURL); err == nil {
		t.Skip("debug build: local hubs are accepted")
	}
	if w.a.ConnectStored() {
		t.Fatal("connected to a stored hub the build does not accept")
	}
	select {
	case <-w.hubs:
		t.Fatal("the agent dialled the stored hub")
	case <-time.After(150 * time.Millisecond):
	}
	for _, evil := range []string{"wss://evil.example/agent", "wss://cr-ai.apps.grovestreet.zip.evil.io/agent", "ws://cr-ai.apps.grovestreet.zip/agent"} {
		w.st.Update(func(c *store.Config) { c.Hub = evil })
		if w.a.ConnectStored() {
			t.Fatalf("connected to %s", evil)
		}
	}
}

// A download keeps running when the socket drops; the next hello carries the job and then the model.
func TestDownloadSurvivesSocketLoss(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	w.hf.Lock()
	w.hf.ChunkDelay = 25 * time.Millisecond
	w.hf.Unlock()
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T3","op":"download","args":{"model_id":"tiny-q4_k_m","repo":"unsloth/Tiny-GGUF","commit":"` + commitA + `","files":["Tiny-Q4_K_M.gguf"],"kind":"gguf"}}`)
	h.next("cmd.ack", 2*time.Second)
	h.until("a running job", 5*time.Second, func(m map[string]any) bool { return m["t"] == "status" && len(m["jobs"].([]any)) == 1 })
	h.conn.CloseNow()
	h2, hello := w.accept()
	jobs := hello["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["model_id"] != "tiny-q4_k_m" || modelState(hello, "tiny-q4_k_m") != "downloading" {
		t.Fatalf("hello after the drop: jobs %v models %v", hello["jobs"], hello["models"])
	}
	h2.until("ready after reconnect", 15*time.Second, func(m map[string]any) bool {
		return m["t"] == "status" && modelState(m, "tiny-q4_k_m") == "ready"
	})
}

// Sleep and wake (ruling B12).
func TestSuspendResume(t *testing.T) {
	w := newWorld(t)
	h, _ := w.connect()
	// A local model keeps this test about sleep, not downloads.
	w.a.cfg.LocalModelID, w.a.cfg.LocalModelPath = "smollm2-360m-q8_0", "local.gguf"
	h.send(loadOK)
	h.until("load done", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	h.send(reqStream)
	h.next("res.data", 2*time.Second)

	t0 := time.Now()
	gone := make(chan struct{})
	go func() { w.a.Suspend(); close(gone) }()
	// The hub sees: the answer cancelled, status{sleeping}, then a close with 1001.
	sawSleeping := false
	var closeCode websocket.StatusCode = -1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for closeCode == -1 {
		_, b, err := h.conn.Read(ctx)
		if err != nil {
			closeCode = websocket.CloseStatus(err)
			if closeCode == -1 {
				t.Fatalf("socket ended without a close frame: %v", err)
			}
			break
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		if m["t"] == "status" && m["sleeping"] == true && m["engine"] == "unloaded" {
			sawSleeping = true
		}
	}
	<-gone
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("suspend took %v; Windows grants about two seconds", el)
	}
	if !sawSleeping || closeCode != websocket.StatusGoingAway {
		t.Fatalf("sleeping status seen: %v, close code %d", sawSleeping, closeCode)
	}
	select {
	case <-w.fe.disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer in flight was not aborted")
	}
	if in := w.a.Snapshot(true); in.Engine != "unloaded" {
		t.Fatalf("engine %q after suspend", in.Engine)
	}
	// Nothing reconnects while asleep.
	select {
	case <-w.hubs:
		t.Fatal("the agent reconnected while asleep")
	case <-time.After(150 * time.Millisecond):
	}
	// Loads are refused while asleep.
	if _, err := w.a.Load(context.Background(), "smollm2-360m-q8_0", "local.gguf", supervisor.DefaultSettings(4)); err == nil {
		t.Fatal("a load ran while asleep")
	}

	t1 := time.Now()
	w.a.Resume()
	_, hello := w.accept()
	if el := time.Since(t1); el > time.Second {
		t.Fatalf("reconnect after wake took %v", el)
	}
	if hello["engine"] != "unloaded" || hello["last_disconnect_reason"] != "sleep" || hello["model_id"] != nil || len(hello["inflight"].([]any)) != 0 {
		t.Fatalf("hello after wake: %v", hello)
	}
	// A second wake signal (Windows sends two) does nothing.
	w.a.Resume()
	select {
	case <-w.hubs:
		t.Fatal("a duplicate wake signal reconnected again")
	case <-time.After(150 * time.Millisecond):
	}
}
