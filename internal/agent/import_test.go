package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/hw"
	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

const importSettings = `"settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on"}`

func cmdLine(id, op, args string) string {
	return `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2` + id + `","op":"` + op + `","args":{` + args + `}}`
}

func isFor(id string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		return (m["t"] == "cmd.done" || m["t"] == "cmd.err") && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2"+id
	}
}

// Everything a hub can send, before and after the host's click, over the real link.
func TestHubCannotImport(t *testing.T) {
	w := newWorld(t)
	name := "Stranger-Q4_K_M.gguf"
	p := filepath.Join(w.root, name)
	if err := os.WriteFile(p, w.model, 0o644); err != nil {
		t.Fatal(err)
	}
	var blobs = w.hf.BlobRequests.Load()
	h, hello := w.connect()
	if m, _ := hello["models"].([]any); len(m) != 0 {
		t.Fatalf("the hub sees a file nobody added: %v", hello["models"])
	}
	sum := w.mm.Scan().Found
	if len(sum) != 1 {
		t.Fatalf("offered %+v", sum)
	}

	// Refused while it is unimported.
	const id = "stranger-q4_k_m"
	const repo = "local-file/Stranger-Q4_K_M"
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h.send(cmdLine("A1", "load", `"model_id":"`+id+`",`+importSettings))
	if e := h.until("load", 5*time.Second, isFor("A1")); e["t"] != "cmd.err" || e["code"] != "not_found" {
		t.Fatalf("load: %v", e)
	}
	h.send(cmdLine("A2", "inspect", `"repo":"`+repo+`","commit":"`+commit+`","file":"`+name+`"`))
	if e := h.until("inspect", 5*time.Second, isFor("A2")); e["t"] != "cmd.err" || e["code"] != "not_found" {
		t.Fatalf("inspect: %v", e)
	}
	h.send(cmdLine("A3", "download", `"model_id":"`+id+`","repo":"`+repo+`","commit":"`+commit+`","files":["`+name+`"],"kind":"gguf"`))
	if e := h.until("download", 5*time.Second, isFor("A3")); e["t"] != "cmd.err" || e["code"] != "denied" {
		t.Fatalf("download: %v", e)
	}
	h.send(cmdLine("A4", "download", `"model_id":"`+id+`","repo":"LOCAL-FILE/Stranger-Q4_K_M","commit":"`+commit+`","files":["`+name+`"],"kind":"gguf"`))
	if e := h.until("download", 5*time.Second, isFor("A4")); e["t"] != "cmd.err" || e["code"] != "denied" {
		t.Fatalf("download, other spelling: %v", e)
	}
	if n := len(w.click.asked); n != 0 || w.hf.BlobRequests.Load() != blobs {
		t.Fatalf("asked %d times", n)
	}

	// The host clicks. The hub hears of it in a status frame, and can use it as any other model.
	r := w.mm.Import(context.Background(), w.mm.Scan().Picks([]string{name}), nil)
	if r[0].Err != "" {
		t.Fatalf("%+v", r)
	}
	st := h.until("status with the model", 5*time.Second, func(m map[string]any) bool {
		if m["t"] != "status" {
			return false
		}
		ms, _ := m["models"].([]any)
		return len(ms) == 1
	})
	b, _ := json.Marshal(st["models"])
	t.Logf("status.models: %s", b)
	lm := st["models"].([]any)[0].(map[string]any)
	got := lm["commit"].(string)
	if lm["repo"] != repo || lm["model_id"] != id || lm["state"] != "ready" || lm["kind"] != "gguf" {
		t.Fatalf("frame: %v", lm)
	}

	h.send(cmdLine("A5", "inspect", `"repo":"`+repo+`","commit":"`+got+`","file":"`+name+`"`))
	e := h.until("inspect", 5*time.Second, isFor("A5"))
	if e["t"] != "cmd.done" || e["result"].(map[string]any)["arch"] != "llama" {
		t.Fatalf("inspect: %v", e)
	}
	b, _ = json.Marshal(e)
	t.Logf("inspect: %s", b)
	h.send(cmdLine("A6", "load", `"model_id":"`+id+`",`+importSettings))
	if e := h.until("load", 5*time.Second, isFor("A6")); e["t"] != "cmd.done" {
		t.Fatalf("load: %v", e)
	}
	if w.started[len(w.started)-1].ModelPath != p {
		t.Fatalf("loaded %s", w.started[len(w.started)-1].ModelPath)
	}
	h.send(cmdLine("A7", "delete", `"model_id":"`+id+`"`))
	if e := h.until("delete", 5*time.Second, isFor("A7")); e["t"] != "cmd.err" || e["code"] != "busy" {
		t.Fatalf("delete while loaded: %v", e)
	}
	h.send(cmdLine("A8", "unload", ""))
	h.until("unload", 5*time.Second, isFor("A8"))
	h.send(cmdLine("A9", "delete", `"model_id":"`+id+`"`))
	if e := h.until("delete", 5*time.Second, isFor("A9")); e["t"] != "cmd.done" {
		t.Fatalf("delete: %v", e)
	}
	if st, err := os.Stat(p); err != nil || st.Size() != int64(len(w.model)) {
		t.Fatalf("delete touched the host's file: %v", err)
	}
	h.send(cmdLine("B1", "load", `"model_id":"`+id+`",`+importSettings))
	if e := h.until("load", 5*time.Second, isFor("B1")); e["t"] != "cmd.err" || e["code"] != "not_found" {
		t.Fatalf("load after delete: %v", e)
	}
}

// The real run: a scratch home with real DPAPI, the real engine under low integrity, a real GGUF.
//
//	CRAGENT_ENGINE_DIR=<folder with llama-server.exe> CRAGENT_TEST_GGUF=<a small .gguf> go test -v -run TestImportRealEngine ./internal/agent
func TestImportRealEngine(t *testing.T) {
	engineDir, model := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_GGUF")
	if engineDir == "" || model == "" {
		t.Skip("CRAGENT_ENGINE_DIR and CRAGENT_TEST_GGUF not set")
	}
	t.Run("appcontainer", func(t *testing.T) { realImport(t, engineDir, model, supervisor.LevelAppContainer) })
	t.Run("low_integrity", func(t *testing.T) { realImport(t, engineDir, model, supervisor.LevelLowIntegrity) })
}

func realImport(t *testing.T, engineDir, model string, level supervisor.Level) {
	home := t.TempDir()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "models")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(model)
	src, err := os.Open(model)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var a *Agent
	mm, err := models.New(models.Options{Store: st, Dir: root, Log: log, OnChange: func(w bool) {
		if a != nil {
			a.ModelsChanged(w)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer mm.Close()
	a = New(Config{Version: "0.1.0", LlamaTag: "test", LogicalCores: 8, Log: log, EngineDir: engineDir, Models: mm, Store: st, ModelGrant: mm.Dir,
		GPUInUse: func(ctx context.Context, pid uint32) (bool, error) {
			pids, err := hw.ComputePIDs(ctx)
			return pids[pid], err
		}})
	defer a.Shutdown()
	if level == supervisor.LevelLowIntegrity {
		a.AllowLowIntegrity(true) // what the host's click in the tray does
	}
	if a.Level() != level {
		t.Fatalf("level %v", a.Level())
	}
	hubs := make(chan *hub, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(rw, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(link.MaxPayload)
		hubs <- &hub{t: t, conn: c, ctx: context.Background()}
		<-r.Context().Done()
	}))
	defer srv.Close()
	a.Connect("ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", "crm_x_y")
	var h *hub
	select {
	case h = <-hubs:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect")
	}
	h.next("hello", 5*time.Second)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{}}`)

	scan := mm.Scan()
	if len(scan.Found) != 1 {
		t.Fatalf("scan: %+v", scan)
	}
	t0 := time.Now()
	res := mm.Import(context.Background(), scan.Picks([]string{scan.Found[0].Key}), nil)
	if res[0].Err != "" {
		t.Fatalf("import: %+v", res)
	}
	t.Logf("import: %+v in %v", res[0], time.Since(t0).Round(time.Millisecond))
	lm := mm.Models()[0]
	b, _ := json.Marshal(lm)
	t.Logf("models[0]: %s", b)

	h.send(cmdLine("C1", "inspect", `"repo":"`+lm.Repo+`","commit":"`+lm.Commit+`","file":"`+name+`"`))
	e := h.until("inspect", 30*time.Second, isFor("C1"))
	if e["t"] != "cmd.done" {
		t.Fatalf("inspect: %v", e)
	}
	b, _ = json.Marshal(e["result"])
	t.Logf("inspect: %s", b)

	h.send(cmdLine("C2", "load", `"model_id":"`+lm.ModelID+`","settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on"}`))
	f := h.until("load", 5*time.Minute, isFor("C2"))
	if f["t"] != "cmd.done" {
		t.Fatalf("load: %v", f)
	}
	t.Logf("load: %v", f["result"])
	r := f["result"].(map[string]any)
	if r["low_il"] != true || r["cuda_context"] != true {
		t.Fatalf("the engine is not on the GPU in its sandbox: %v", r)
	}
	// Which level really ran shows in how the agent reaches it: only the AppContainer has no port.
	base, _, _, ok := a.target()
	if !ok || strings.Contains(base, "127.0.0.1") != (level == supervisor.LevelLowIntegrity) {
		t.Fatalf("level %v reached at %s", level, base)
	}
	t.Logf("level %v, reached at %s", level, base)
	h.send(cmdLine("C3", "unload", ""))
	h.until("unload", time.Minute, isFor("C3"))
	h.send(cmdLine("C4", "delete", `"model_id":"`+lm.ModelID+`"`))
	if e := h.until("delete", 30*time.Second, isFor("C4")); e["t"] != "cmd.done" {
		t.Fatalf("delete: %v", e)
	}
	if _, err := os.Stat(filepath.Join(root, name)); err != nil {
		t.Fatalf("delete touched the host's file: %v", err)
	}
}
