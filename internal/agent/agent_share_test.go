package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// Ruling B74: up to 4 people share one engine. The agent says it can in hello, starts the engine with the slots the hub asks
// for, and runs several requests at once, each cancelled on its own.
func TestSharedEngine(t *testing.T) {
	a, fe, hubs, _ := setup(t)
	var started []supervisor.Config
	var mu sync.Mutex
	inner := a.cfg.Start
	a.cfg.Start = func(ctx context.Context, c supervisor.Config) (Engine, error) {
		mu.Lock()
		started = append(started, c)
		mu.Unlock()
		return inner(ctx, c)
	}
	var h *hub
	select {
	case h = <-hubs:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not connect")
	}
	hello := h.next("hello", 2*time.Second)
	caps, _ := json.Marshal(hello["capabilities"])
	if !strings.Contains(string(caps), `"parallel"`) {
		t.Fatalf("capabilities %s: the hub would not offer sharing", caps)
	}
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{"max_frame":262144,"max_body":262144}}`)

	h.send(strings.Replace(loadOK, `"parallel":1`, `"parallel":2`, 1))
	h.next("cmd.ack", 2*time.Second)
	if d := h.next("cmd.done", 5*time.Second); d["id"] != "c-01JCKZ3Q9X8YB7N6M5P4R3S2T1" {
		t.Fatalf("done %v", d)
	}
	mu.Lock()
	if len(started) != 1 || started[0].Settings.Parallel == nil || *started[0].Settings.Parallel != 2 {
		t.Fatalf("engine started with %+v", started)
	}
	mu.Unlock()
	if args := strings.Join(supervisor.BuildArgs(supervisor.Launch{ModelPath: "m.gguf", Alias: "x", Port: 1, APIKey: "k", Settings: started[0].Settings}), " "); !strings.Contains(args, "-np 2 -no-kvu") {
		t.Fatalf("args %s", args)
	}
	if st := a.Hello(); st.SettingsInUse == nil || st.SettingsInUse.Parallel == nil || *st.SettingsInUse.Parallel != 2 {
		t.Fatalf("settings in use %+v", st.SettingsInUse)
	}

	// Two people at once: both stream, and one cancel stops only its own answer.
	one, two := "r-01JCKZ3Q9X8YB7N6M5P4R3S2A1", "r-01JCKZ3Q9X8YB7N6M5P4R3S2A2"
	for _, id := range []string{one, two} {
		h.send(strings.Replace(reqStream, "r-01JCKZ3Q9X8YB7N6M5P4R3S2T3", id, 1))
	}
	data := map[string]int{}
	deadline := time.Now().Add(3 * time.Second)
	for (data[one] < 3 || data[two] < 3) && time.Now().Before(deadline) {
		d := h.next("res.data", 2*time.Second)
		data[d["id"].(string)] += len(d["d"].([]any))
	}
	if data[one] < 3 || data[two] < 3 {
		t.Fatalf("not both at once: %v", data)
	}
	if n := len(a.relay.Inflight()); n != 2 {
		t.Fatalf("%d in flight", n)
	}
	h.send(`{"v":1,"t":"cancel","id":"` + one + `"}`)
	var ended map[string]any
	for ended == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, b, err := h.conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for res.end: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["t"] == "res.end" {
			ended = m
		}
	}
	if ended["id"] != one || ended["reason"] != "cancelled" {
		t.Fatalf("end %v", ended)
	}
	select {
	case <-fe.disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine kept generating the cancelled answer")
	}
	// The other answer goes on.
	for got := 0; got < 3; {
		d := h.next("res.data", 2*time.Second)
		if d["id"] != two {
			t.Fatalf("data for %v after its cancel", d["id"])
		}
		got += len(d["d"].([]any))
	}
	if ids := a.relay.Inflight(); len(ids) != 1 || ids[0] != two {
		t.Fatalf("in flight %v", ids)
	}
}
