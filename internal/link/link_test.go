package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

type nopHandler struct {
	mu     sync.Mutex
	discon []string
}

func (h *nopHandler) Hello() *protocol.Hello {
	return &protocol.Hello{AgentVersion: "0.0.1", Proto: protocol.Proto, Platform: "windows-amd64", Backend: "cuda",
		Capabilities: []string{}, SupportedArchs: []string{}, Engine: protocol.EngineUnloaded,
		Hardware: protocol.Hardware{Pools: []protocol.Pool{}}, Models: []protocol.LocalModel{}, Jobs: []protocol.Job{}, Inflight: []string{}}
}
func (h *nopHandler) OnWelcome(*Session, *protocol.Welcome) {}
func (h *nopHandler) OnFrame(*Session, protocol.Frame)      {}
func (h *nopHandler) OnDisconnect(r string) {
	h.mu.Lock()
	h.discon = append(h.discon, r)
	h.mu.Unlock()
}

// hubServer runs fn for each accepted socket after checking the bearer key.
func hubServer(t *testing.T, fn func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent" || r.Header.Get("Authorization") != "Bearer crm_x_y" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		fn(r.Context(), c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent" }

func readT(ctx context.Context, c *websocket.Conn) (map[string]any, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func TestBackoffBounds(t *testing.T) {
	max := func(n int64) int64 { return n - 1 }
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for n, w := range want {
		if d := Backoff(n, time.Second, 30*time.Second, max); d != w {
			t.Fatalf("n=%d: max delay %v, want %v", n, d, w)
		}
	}
	if d := Backoff(100, time.Second, 30*time.Second, max); d != 30*time.Second {
		t.Fatalf("overflow: %v", d)
	}
	if d := Backoff(3, time.Second, 30*time.Second, func(int64) int64 { return 0 }); d != 0 {
		t.Fatalf("min delay %v", d)
	}
}

// TestHelloPongAndCloseCodes: hello first, pong echoes ts, and each close code picks its retry policy.
func TestHelloPongAndCloseCodes(t *testing.T) {
	codes := []websocket.StatusCode{websocket.StatusServiceRestart, CloseBadKey, CloseReplaced, CloseUpgradeRequired, websocket.StatusInternalError, CloseRevoked}
	var mu sync.Mutex
	i := 0
	srv := hubServer(t, func(ctx context.Context, c *websocket.Conn) {
		m, err := readT(ctx, c)
		if err != nil || m["t"] != "hello" {
			t.Errorf("first frame %v %v", m, err)
			return
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{}}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"ping","ts":4242}`))
		m, err = readT(ctx, c)
		if err != nil || m["t"] != "pong" || m["ts"].(float64) != 4242 {
			t.Errorf("pong %v %v", m, err)
		}
		mu.Lock()
		code := codes[i]
		i++
		mu.Unlock()
		c.Close(code, "")
	})
	var sleeps []time.Duration
	h := &nopHandler{}
	cl := New(Config{URL: wsURL(srv), Key: "crm_x_y", Handler: h,
		Rand:  func(n int64) int64 { return n - 1 }, // always the ceiling
		Sleep: func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := cl.Run(ctx)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("Run = %v, want ErrRevoked", err)
	}
	want := []time.Duration{3 * time.Second, time.Hour, time.Hour, time.Hour, time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps %v, want %v", sleeps, want)
	}
	for k := range want {
		if sleeps[k] != want[k] {
			t.Fatalf("sleep %d = %v, want %v (all %v)", k, sleeps[k], want[k], sleeps)
		}
	}
}

func TestUnauthorizedUpgradeIsSlowRetry(t *testing.T) {
	srv := hubServer(t, func(ctx context.Context, c *websocket.Conn) {})
	var got time.Duration
	cl := New(Config{URL: wsURL(srv), Key: "crm_bad_key", Handler: &nopHandler{},
		Sleep: func(ctx context.Context, d time.Duration) error { got = d; return context.Canceled }})
	cl.Run(context.Background())
	if got != time.Hour {
		t.Fatalf("delay after 401 = %v, want 1h", got)
	}
}

// A refused upgrade with HTTP 429 means "wait Retry-After, then retry": not a bad key, not a removal.
func TestTooManyRequestsWaitsRetryAfter(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"error":{"code":"rate_limit_exceeded"}}`, http.StatusTooManyRequests)
		case 2:
			http.Error(w, "slow down", http.StatusTooManyRequests) // no Retry-After: a minute
		default:
			w.Header().Set("Retry-After", "99999") // capped at the slow retry
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	var sleeps []time.Duration
	var phases []Phase
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := New(Config{URL: wsURL(srv), Key: "crm_x_y", Handler: &nopHandler{},
		Rand:    func(n int64) int64 { return n - 1 },
		OnPhase: func(p Phase) { phases = append(phases, p) },
		Sleep:   func(context.Context, time.Duration) error { return nil }})
	cl.cfg.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := cl.Run(ctx); errors.Is(err, ErrRevoked) {
		t.Fatal("429 was treated as a removal")
	}
	want := []time.Duration{7*time.Second + 3*time.Second, time.Minute + 3*time.Second, time.Hour + 3*time.Second}
	if len(sleeps) != 3 || sleeps[0] != want[0] || sleeps[1] != want[1] || sleeps[2] != want[2] {
		t.Fatalf("sleeps %v, want %v", sleeps, want)
	}
	for _, p := range phases {
		if p == PhaseKeyRejected || p == PhaseRemoved {
			t.Fatalf("429 produced phase %q", p)
		}
	}
}

// TestDeadAfterSilence: no frame for DeadAfter means the socket is dead and the session context ends.
func TestDeadAfterSilence(t *testing.T) {
	srv := hubServer(t, func(ctx context.Context, c *websocket.Conn) {
		readT(ctx, c) // hello, then silence
		<-ctx.Done()
	})
	h := &nopHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := New(Config{URL: wsURL(srv), Key: "crm_x_y", Handler: h, DeadAfter: 300 * time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { cancel(); return context.Canceled }})
	t0 := time.Now()
	cl.Run(ctx)
	if el := time.Since(t0); el > 3*time.Second {
		t.Fatalf("dead socket noticed after %v", el)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.discon) != 1 || !strings.Contains(h.discon[0], "no frame") {
		t.Fatalf("disconnect reasons %v", h.discon)
	}
}

func TestUnknownTypeDroppedAndBadCmdAnswered(t *testing.T) {
	done := make(chan map[string]any, 1)
	srv := hubServer(t, func(ctx context.Context, c *websocket.Conn) {
		readT(ctx, c)
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"future.thing","x":1}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T4","op":"load","args":{"model_id":"m","settings":{"ctx":8192,"gpu_layers":"all","threads":4,"kv_type":"f16","flash_attn":"on","lora":"x"}}}`))
		m, _ := readT(ctx, c)
		done <- m
		c.Close(websocket.StatusNormalClosure, "")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := New(Config{URL: wsURL(srv), Key: "crm_x_y", Handler: &nopHandler{},
		Sleep: func(context.Context, time.Duration) error { cancel(); return context.Canceled }})
	cl.Run(ctx)
	m := <-done
	if m["t"] != "cmd.err" || m["code"] != "bad_args" || m["id"] != "c-01JCKZ3Q9X8YB7N6M5P4R3S2T4" {
		t.Fatalf("answer %v", m)
	}
}

// Finding L1: the upgrade carries the machine key, and it goes to the validated host or nowhere.
func TestUpgradeDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if c, err := websocket.Accept(w, r, nil); err == nil {
			c.Close(websocket.StatusNormalClosure, "")
		}
	}))
	defer other.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/agent", http.StatusFound)
	}))
	defer hub.Close()
	h := &nopHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slept := make(chan struct{}, 1)
	c := New(Config{URL: wsURL(hub), Key: "crm_x_y", Handler: h, Sleep: func(context.Context, time.Duration) error {
		cancel()
		slept <- struct{}{}
		return context.Canceled
	}})
	c.Run(ctx)
	<-slept
	if reached.Load() != 0 {
		t.Fatal("the upgrade followed a redirect to another host")
	}
}

// Finding L4: what the hub sends is shortened before it reaches the log.
func TestHubStringsAreClippedInTheLog(t *testing.T) {
	long := strings.Repeat("Z", 5000)
	srv := hubServer(t, func(ctx context.Context, c *websocket.Conn) {
		readT(ctx, c)
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"`+long+`"}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T4","op":"`+long+`","args":{}}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"welcome","proto":"`+long+`","mode":"normal","ping_interval_s":25,"limits":{}}`))
		c.Close(websocket.StatusNormalClosure, "")
	})
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := New(Config{URL: wsURL(srv), Key: "crm_x_y", Handler: &nopHandler{}, Log: slog.New(slog.NewTextHandler(&buf, nil)),
		Sleep: func(context.Context, time.Duration) error { cancel(); return context.Canceled }})
	cl.Run(ctx)
	if strings.Contains(buf.String(), strings.Repeat("Z", 200)) {
		t.Fatalf("a hub string reached the log whole: %d bytes of log", buf.Len())
	}
	if !strings.Contains(buf.String(), "unknown frame type dropped") {
		t.Fatalf("log: %s", buf.String())
	}
	if got := Clip(strings.Repeat("é", 200)); !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || len(got) > 130 {
		t.Fatalf("clip %q", got)
	}
}
