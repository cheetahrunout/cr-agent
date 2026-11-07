package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// fakeEngine is an httptest SSE server standing in for llama-server.
type fakeEngine struct {
	srv          *httptest.Server
	requests     atomic.Int32
	disconnected chan time.Time
	mu           sync.Mutex
	lastBody     map[string]any
}

func newFakeEngine(t *testing.T) *fakeEngine {
	fe := &fakeEngine{disconnected: make(chan time.Time, 8)}
	fe.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/chat/completions":
			fe.requests.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			fe.mu.Lock()
			fe.lastBody = body
			fe.mu.Unlock()
			if body["stream"] != true {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := 0; i < 2000; i++ {
				select {
				case <-r.Context().Done():
					fe.disconnected <- time.Now()
					return
				case <-time.After(10 * time.Millisecond):
				}
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fe.srv.Close)
	return fe
}

type fakeHandle struct {
	base string
	done chan struct{}
	once sync.Once
}

func (h *fakeHandle) Target() (string, string, string) {
	return h.base, "local-key", "smollm2-360m-q8_0"
}
func (h *fakeHandle) PID() uint32           { return 1 }
func (h *fakeHandle) Done() <-chan struct{} { return h.done }
func (h *fakeHandle) Stop()                 { h.once.Do(func() { close(h.done) }) }

// hub is the test's side of the socket.
type hub struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

func (h *hub) send(s string) {
	h.t.Helper()
	if err := h.conn.Write(h.ctx, websocket.MessageText, []byte(s)); err != nil {
		h.t.Fatalf("hub write: %v", err)
	}
}

// next reads frames until one of type t arrives, within d.
func (h *hub) next(t string, d time.Duration) map[string]any {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, d)
	defer cancel()
	for {
		_, b, err := h.conn.Read(ctx)
		if err != nil {
			h.t.Fatalf("waiting for %s: %v", t, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			h.t.Fatalf("bad frame %s", b)
		}
		if m["t"] == t {
			return m
		}
	}
}

func setup(t *testing.T) (*Agent, *fakeEngine, chan *hub, func()) {
	fe := newFakeEngine(t)
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a := New(Config{
		Version: "0.0.1", LlamaTag: "test", LocalModelID: "smollm2-360m-q8_0", LocalModelPath: "local.gguf", Log: log,
		LogicalCores: 4,
		Start: func(ctx context.Context, c supervisor.Config) (Engine, error) {
			if c.Alias != "smollm2-360m-q8_0" || c.ModelPath != "local.gguf" {
				return nil, fmt.Errorf("alias %q path %q", c.Alias, c.ModelPath)
			}
			return &fakeHandle{base: fe.srv.URL, done: make(chan struct{})}, nil
		},
		TuneLink: func(c *link.Config) { c.BaseDelay, c.MaxDelay = 10*time.Millisecond, 50*time.Millisecond },
	})
	hubs := make(chan *hub, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer crm_x_y" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(link.MaxPayload)
		hubs <- &hub{t: t, conn: c, ctx: context.Background()}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	a.Connect("ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", "crm_x_y")
	cancel := a.Disconnect
	t.Cleanup(cancel)
	return a, fe, hubs, cancel
}

const (
	loadOK       = `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T1","op":"load","args":{"model_id":"smollm2-360m-q8_0","settings":{"ctx":4096,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on","parallel":1}}}`
	reqStream    = `{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T3","path":"/v1/chat/completions","stream":true,"body":{"model":"anna-pc/smol","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":512}}`
	cancelStream = `{"v":1,"t":"cancel","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T3"}`
)

func TestHubDrivesAgent(t *testing.T) {
	_, fe, hubs, _ := setup(t)
	var h *hub
	select {
	case h = <-hubs:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect")
	}

	hello := h.next("hello", 2*time.Second)
	if hello["proto"] != "1.0" || hello["engine"] != "unloaded" {
		t.Fatalf("hello %v", hello)
	}
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{"max_frame":262144,"max_body":262144}}`)
	h.send(`{"v":1,"t":"ping","ts":1793870000123}`)
	if p := h.next("pong", 2*time.Second); p["ts"].(float64) != 1793870000123 {
		t.Fatalf("pong %v", p)
	}

	// Settings bounds: refused with cmd.err bad_setting naming the field, never clamped.
	for _, c := range []struct{ settings, field string }{
		{`{"ctx":4096,"gpu_layers":"all","threads":8,"kv_type":"f16","flash_attn":"on"}`, "threads"}, // > logical cores (4)
		{`{"ctx":4096,"gpu_layers":"all","threads":9999,"kv_type":"f16","flash_attn":"on"}`, "threads"},
		{`{"ctx":1024,"gpu_layers":"all","threads":2,"kv_type":"f16","flash_attn":"on"}`, "ctx"},
		{`{"ctx":4096,"gpu_layers":"all","threads":2,"kv_type":"q4_0","flash_attn":"on"}`, "kv_type"},
		{`{"ctx":4096,"gpu_layers":"all","threads":2,"kv_type":"f16","flash_attn":"on","parallel":5}`, "parallel"}, // 1..4 people at once (ruling B74)
	} {
		h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T9","op":"load","args":{"model_id":"smollm2-360m-q8_0","settings":` + c.settings + `}}`)
		e := h.next("cmd.err", 2*time.Second)
		if e["code"] != "bad_setting" || e["field"] != c.field {
			t.Fatalf("%s: got %v", c.settings, e)
		}
	}

	h.send(loadOK)
	h.next("cmd.ack", 2*time.Second)
	if d := h.next("cmd.done", 5*time.Second); d["id"] != "c-01JCKZ3Q9X8YB7N6M5P4R3S2T1" {
		t.Fatalf("done %v", d)
	}

	// Stream, then cancel: the local request must be aborted within 2 s.
	h.send(reqStream)
	head := h.next("res.head", 2*time.Second)
	if head["status"].(float64) != 200 || !strings.HasPrefix(head["ctype"].(string), "text/event-stream") {
		t.Fatalf("head %v", head)
	}
	got := 0
	for got < 5 {
		d := h.next("res.data", 2*time.Second)
		got += len(d["d"].([]any))
	}
	fe.mu.Lock()
	if fe.lastBody["model"] != "smollm2-360m-q8_0" {
		t.Errorf("model not rewritten to the alias: %v", fe.lastBody["model"])
	}
	fe.mu.Unlock()
	t0 := time.Now()
	h.send(cancelStream)
	end := h.next("res.end", 2*time.Second)
	if end["reason"] != "cancelled" {
		t.Fatalf("end %v", end)
	}
	select {
	case at := <-fe.disconnected:
		t.Logf("engine saw the disconnect %v after cancel", at.Sub(t0))
	case <-time.After(2 * time.Second):
		t.Fatal("engine kept generating after cancel")
	}

	// B7: an unknown body field is a local 400 and never reaches the engine.
	before := fe.requests.Load()
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T5","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"x"}],"slot_id":3}}`)
	if hd := h.next("res.head", 2*time.Second); hd["status"].(float64) != 400 {
		t.Fatalf("head %v", hd)
	}
	if e := h.next("res.end", 2*time.Second); e["reason"] != "error" {
		t.Fatalf("end %v", e)
	}
	// Strict req: a path outside the enum is refused by the decoder.
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T6","path":"/slots","stream":false,"body":{}}`)
	if hd := h.next("res.head", 2*time.Second); hd["status"].(float64) != 400 {
		t.Fatalf("bad path head %v", hd)
	}
	if fe.requests.Load() != before {
		t.Fatal("a refused request reached the engine")
	}

	// Non-stream request: one body in res.data.
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T7","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"x"}],"stream":true}}`)
	h.next("res.head", 2*time.Second)
	if d := h.next("res.data", 2*time.Second); !strings.Contains(d["d"].([]any)[0].(string), `"hi"`) {
		t.Fatalf("plain body %v", d)
	}
	if e := h.next("res.end", 2*time.Second); e["reason"] != "done" {
		t.Fatalf("end %v", e)
	}

	// Config stub.
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2TA","op":"config","args":{"keep_awake":true,"idle_unload_min":30}}`)
	h.next("cmd.done", 2*time.Second)
	h.send(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2TB","op":"config","args":{"idle_unload_min":5000}}`)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "bad_setting" || e["field"] != "idle_unload_min" {
		t.Fatalf("config bound %v", e)
	}

	// Socket loss mid-stream: every local request is cancelled.
	h.send(`{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T8","path":"/v1/chat/completions","stream":true,"body":{"messages":[{"role":"user","content":"x"}]}}`)
	h.next("res.data", 2*time.Second)
	t1 := time.Now()
	h.conn.CloseNow()
	select {
	case at := <-fe.disconnected:
		t.Logf("engine saw the disconnect %v after socket loss", at.Sub(t1))
	case <-time.After(2 * time.Second):
		t.Fatal("socket loss did not cancel the local request")
	}

	// The agent reconnects with a fresh hello reporting the loaded model and no inflight ids.
	select {
	case h = <-hubs:
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect")
	}
	hello = h.next("hello", 2*time.Second)
	if hello["engine"] != "loaded" || hello["model_id"] != "smollm2-360m-q8_0" || len(hello["inflight"].([]any)) != 0 {
		t.Fatalf("hello after reconnect %v", hello)
	}
}
