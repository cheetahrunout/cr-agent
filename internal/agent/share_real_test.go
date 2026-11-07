package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// TestSharedEngineReal is ruling B74 on the real engine, in its real sandbox: a load with parallel 2 gives two slots of half the
// context each, two answers stream at once, and a prompt too large for one slot is refused by the engine while the other
// answer goes on. It needs a real engine and a small model, so it runs only when asked:
//
//	CRAGENT_ENGINE_DIR=<folder with llama-server.exe> CRAGENT_TEST_GGUF=<a small .gguf> go test -v -run TestSharedEngineReal ./internal/agent
func TestSharedEngineReal(t *testing.T) {
	engineDir, model := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_GGUF")
	if engineDir == "" || model == "" {
		t.Skip("CRAGENT_ENGINE_DIR and CRAGENT_TEST_GGUF not set")
	}
	// Its own container and a copy of the model, as the sentinel test: no permission changes on the real model.
	folder := t.TempDir()
	supervisor.ScopeContainer(folder)
	t.Cleanup(func() { supervisor.DeleteContainer() })
	copied := filepath.Join(folder, filepath.Base(model))
	in, err := os.Open(model)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(copied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	in.Close()
	out.Close()

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{Version: "0.2.8", LlamaTag: "test", LogicalCores: 64, Log: log, EngineDir: engineDir,
		LocalModelID: "share-model", LocalModelPath: copied, ModelGrant: func() string { return folder },
		GPUInUse: func(ctx context.Context, pid uint32) (bool, error) {
			pids, err := hw.ComputePIDs(ctx)
			return pids[pid], err
		}}
	a := New(cfg)
	hubs := make(chan *hub, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(link.MaxPayload)
		hubs <- &hub{t: t, conn: c, ctx: context.Background()}
		<-r.Context().Done()
	}))
	defer srv.Close()
	a.Connect("ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", "crm_x_y")
	defer a.Shutdown()
	var h *hub
	select {
	case h = <-hubs:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect")
	}
	hello := h.next("hello", 5*time.Second)
	if caps, _ := json.Marshal(hello["capabilities"]); !strings.Contains(string(caps), `"parallel"`) {
		t.Fatalf("capabilities %s", caps)
	}
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{}}`)

	t0 := time.Now()
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"load","args":{"model_id":"share-model","settings":{"ctx":8192,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"auto","parallel":2}}}`)
	f := h.until("load", 5*time.Minute, func(m map[string]any) bool { return m["t"] == "cmd.done" || m["t"] == "cmd.err" })
	if f["t"] != "cmd.done" {
		t.Fatalf("load: %v", f)
	}
	res := f["result"].(map[string]any)
	t.Logf("loaded with 2 slots in %v: %v", time.Since(t0).Round(time.Millisecond), res)
	if res["cuda_context"] != true || res["low_il"] != true {
		t.Fatalf("the engine is not on the GPU in its sandbox: %v", res)
	}
	if lvl := a.Level(); lvl != supervisor.LevelAppContainer {
		t.Fatalf("isolation %v", lvl)
	}

	id := 10
	send := func(body map[string]any) string {
		t.Helper()
		id++
		rid := fmt.Sprintf("r-01JCKZ3Q9X8YB7N6M5P4R3S2%02d", id)
		b, _ := json.Marshal(body)
		h.send(fmt.Sprintf(`{"v":1,"t":"req","id":%q,"path":"/v1/chat/completions","stream":true,"body":%s}`, rid, b))
		return rid
	}
	long := func(topic string) map[string]any {
		return map[string]any{"messages": []map[string]any{{"role": "user", "content": "Count from 1 to 1000 in words, one number per line, starting with " + topic + "."}}, "max_tokens": 2000, "stream": true}
	}
	a1, a2 := send(long("one")), send(long("two"))
	// Both stream at the same time: each has payloads before either ends.
	seen := map[string]int{}
	ended := map[string]bool{}
	status := map[string]float64{}
	var tooLong string
	afterRefusal := -1
	for len(ended) < 3 {
		m := h.until("answers", 3*time.Minute, func(map[string]any) bool { return true })
		rid, _ := m["id"].(string)
		switch m["t"] {
		case "res.head":
			status[rid] = m["status"].(float64)
		case "res.data":
			seen[rid] += len(m["d"].([]any))
			if ended[tooLong] {
				afterRefusal += len(m["d"].([]any))
			}
			if tooLong == "" && seen[a1] > 2 && seen[a2] > 2 {
				if len(ended) > 0 {
					t.Fatalf("one ended before both streamed: %v", ended)
				}
				// About 5000 tokens: more than a slot's 4096, less than the 8192 of the whole.
				tooLong = send(map[string]any{"messages": []map[string]any{{"role": "user", "content": strings.Repeat("lighthouse keeper ", 2500)}}, "max_tokens": 8, "stream": true})
			}
		case "res.end":
			ended[rid] = true
			// Refused at once, and nobody else's answer is touched: both were still running.
			if rid == tooLong {
				if ended[a1] && ended[a2] {
					t.Fatalf("both answers ended before the oversized prompt was refused: %v", ended)
				}
				afterRefusal = 0
			}
		}
		if tooLong == "" && len(ended) == 2 {
			t.Fatal("both answers ended before the oversized prompt was sent")
		}
	}
	t.Logf("payloads %v, statuses %v", seen, status)
	if status[a1] != 200 || status[a2] != 200 || seen[a1] < 5 || seen[a2] < 5 {
		t.Fatalf("the two answers: statuses %v, payloads %v", status, seen)
	}
	// Refused at once, and the answer still running went on streaming after it: nobody else is touched.
	t.Logf("payloads after the refusal: %d", afterRefusal)
	if afterRefusal < 3 {
		t.Fatalf("no answer went on after the refusal: %d payloads", afterRefusal)
	}
	if status[tooLong] != 400 {
		t.Fatalf("a prompt larger than its slot: status %v, want 400 from the engine", status[tooLong])
	}
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T2","op":"unload","args":{}}`)
	h.until("unload", time.Minute, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T2" })
}
