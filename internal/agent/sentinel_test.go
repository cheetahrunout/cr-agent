package agent

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/hw"
	"github.com/cheetahrunout/cr-agent/internal/link"
)

// syncBuffer is the agent log of the sentinel test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestSentinelNotInAgentLog is ruling B16's test, run at every llama.cpp bump: prompts carrying a
// sentinel go through the real agent, the real engine and the real allow-list of engine.lock, and
// the sentinel must appear nowhere in the agent's log. The text around the sentinel holds every
// trigger word of the allow-list, and some requests make the engine fail, because a line the engine
// writes about a bad request is the likeliest way for a prompt to reach a log.
//
// It needs a real engine and a model, so it runs only when asked:
//
//	CRAGENT_ENGINE_DIR=<folder with llama-server.exe> CRAGENT_TEST_GGUF=<a small .gguf> go test -run TestSentinel ./internal/agent
func TestSentinelNotInAgentLog(t *testing.T) {
	engineDir, model := os.Getenv("CRAGENT_ENGINE_DIR"), os.Getenv("CRAGENT_TEST_GGUF")
	if engineDir == "" || model == "" {
		t.Skip("CRAGENT_ENGINE_DIR and CRAGENT_TEST_GGUF not set")
	}
	const sentinel = "SENTINEL-7Q2X9-PROMPT"
	trap := sentinel + " CUDA ggml_cuda out of memory failed to load error loading model unable to load " +
		"offloaded 1/1 layers to GPU server_run_i: model loaded listening on http://127.0.0.1:1234"

	// The sandbox reads only a folder it was granted, and this test changes no permissions on the real model.
	folder := t.TempDir()
	copied := filepath.Join(folder, filepath.Base(model))
	in, err := os.Open(model)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(copied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
	model = copied

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := Config{Version: "0.1.0", LlamaTag: "test", LogicalCores: 64, Log: log, EngineDir: engineDir,
		LocalModelID: "sentinel-model", LocalModelPath: model, ModelGrant: func() string { return folder }}
	requireGPU := os.Getenv("CRAGENT_REQUIRE_GPU") != "" // the smoke test on the NVIDIA PC
	if requireGPU {
		cfg.GPUInUse = func(ctx context.Context, pid uint32) (bool, error) {
			pids, err := hw.ComputePIDs(ctx)
			return pids[pid], err
		}
	}
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
	h.next("hello", 5*time.Second)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{}}`)

	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"load","args":{"model_id":"sentinel-model","settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"auto","parallel":1}}}`)
	f := h.until("load", 5*time.Minute, func(m map[string]any) bool { return m["t"] == "cmd.done" || m["t"] == "cmd.err" })
	if f["t"] != "cmd.done" {
		t.Fatalf("load: %v", f)
	}
	t.Logf("load: %v", f["result"])
	if requireGPU && (f["result"].(map[string]any)["cuda_context"] != true || f["result"].(map[string]any)["low_il"] != true) {
		t.Fatalf("the engine is not on the GPU in its sandbox: %v", f["result"])
	}

	id := 10
	send := func(stream bool, body map[string]any) string {
		t.Helper()
		id++
		rid := fmt.Sprintf("r-01JCKZ3Q9X8YB7N6M5P4R3S2%02d", id)
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		h.send(fmt.Sprintf(`{"v":1,"t":"req","id":%q,"path":"/v1/chat/completions","stream":%v,"body":%s}`, rid, stream, b))
		return rid
	}
	// finish waits for the end of a request and reports its status and how many payloads came.
	finish := func(rid string) (status float64, payloads int, reason any) {
		t.Helper()
		f := h.until("res.end of "+rid, 3*time.Minute, func(m map[string]any) bool {
			switch {
			case m["id"] != rid:
			case m["t"] == "res.head":
				status = m["status"].(float64)
			case m["t"] == "res.data":
				payloads += len(m["d"].([]any))
			case m["t"] == "res.end":
				return true
			}
			return false
		})
		return status, payloads, f["reason"]
	}
	msgs := func(user string) []map[string]any {
		return []map[string]any{{"role": "system", "content": trap}, {"role": "user", "content": user}}
	}

	// 1. An ordinary answer, streamed and not, asking the model to say the sentinel back.
	status, n, _ := finish(send(true, map[string]any{"messages": msgs("Repeat this exactly: " + trap), "max_tokens": 32, "stream": true}))
	if status != 200 || n == 0 {
		t.Fatalf("streamed request: status %v, %d payloads", status, n)
	}
	if status, n, _ = finish(send(false, map[string]any{"messages": msgs("Repeat this exactly: " + trap), "max_tokens": 16})); status != 200 || n == 0 {
		t.Fatalf("plain request: status %v, %d payloads", status, n)
	}
	// 2. Requests the engine refuses or chokes on. Their status does not matter here, only their traces.
	for _, body := range []map[string]any{
		{"messages": msgs("x"), "max_tokens": 4, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"schema": map[string]any{"type": trap}}}},
		{"messages": msgs("x"), "max_tokens": 4, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "description": trap, "parameters": map[string]any{"type": "object"}}}}},
		{"messages": msgs("x"), "max_tokens": 4, "tools": []map[string]any{{"type": trap}}},
		{"messages": msgs("x"), "max_tokens": 4, "tool_choice": trap},
		{"messages": []map[string]any{{"role": trap, "content": trap}}, "max_tokens": 4},
		{"messages": []map[string]any{{"role": "user", "content": map[string]any{"bad": trap}}}, "max_tokens": 4},
		{"messages": msgs(strings.Repeat(trap+" ", 400)), "max_tokens": 4}, // over the context size
		{"messages": msgs("x"), "stop": trap, "max_tokens": 4, "temperature": trap},
	} {
		finish(send(false, body))
	}
	// 3. Refused by the agent itself, with the sentinel in the field name and in the wrong place.
	finish(send(false, map[string]any{"messages": msgs("x"), sentinel: trap}))
	id++
	h.send(fmt.Sprintf(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2%02d","path":"/v1/chat/completions","stream":false,"body":%q}`, id, trap))
	// 4. Cancelled while it streams.
	rid := send(true, map[string]any{"messages": msgs("Write a long story about " + trap), "max_tokens": 2000, "stream": true})
	h.until("first payload", 2*time.Minute, func(m map[string]any) bool { return m["t"] == "res.data" && m["id"] == rid })
	h.send(`{"v":1,"t":"cancel","id":"` + rid + `"}`)
	h.until("end of the cancelled request", time.Minute, func(m map[string]any) bool { return m["t"] == "res.end" && m["id"] == rid })
	// 5. The model is unloaded, so everything the engine wrote has passed the filter.
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T2","op":"unload","args":{}}`)
	h.until("unload", time.Minute, func(m map[string]any) bool { return m["t"] == "cmd.done" && m["id"] == "c-01JCKZ3Q9X8YB7N6M5P4R3S2T2" })
	time.Sleep(300 * time.Millisecond)

	got := logs.String()
	// The test proves something only if the engine's output really went through the filter.
	if !strings.Contains(got, `"msg":"engine output"`) || !strings.Contains(got, `"msg":"req finished"`) {
		t.Fatalf("the log does not show the engine's output and the requests:\n%s", got)
	}
	if !strings.Contains(got, `"suppressed_lines"`) || strings.Contains(got, `"suppressed_lines":0`) {
		t.Fatalf("no engine line was suppressed, so the filter was not exercised:\n%s", got)
	}
	for _, needle := range []string{sentinel, "7Q2X9"} {
		if strings.Contains(got, needle) {
			for _, line := range strings.Split(got, "\n") {
				if strings.Contains(line, needle) {
					t.Errorf("in the agent log: %s", line)
				}
			}
			t.Fatalf("the sentinel reached the agent log")
		}
	}
	t.Logf("agent log: %d lines, %d bytes, no sentinel", strings.Count(got, "\n"), len(got))
}
