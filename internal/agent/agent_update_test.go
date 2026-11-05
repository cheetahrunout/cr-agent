package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/updater"
)

type fakeUpdater struct {
	mu     sync.Mutex
	err    error
	called []string
}

func (f *fakeUpdater) Update(_ context.Context, version string, progress updater.Progress) error {
	f.mu.Lock()
	f.called = append(f.called, version)
	err := f.err
	f.mu.Unlock()
	progress("downloading", 0)
	progress("downloading", 40.2)
	progress("downloading", 40.9) // the same whole percent: no second frame
	progress("verifying", 100)
	if err != nil {
		return err
	}
	progress("installing", 100)
	progress("restarting", 100)
	return nil
}

func updateCmd(id, version string) string {
	return `{"v":1,"t":"cmd","id":"` + id + `","op":"update","args":{"version":"` + version + `"}}`
}

func TestUpdateCommand(t *testing.T) {
	fu := &fakeUpdater{}
	restarts := make(chan struct{}, 4)
	w := newWorldWith(t, func(c *Config) {
		c.Updater = fu
		c.Restart = func() { restarts <- struct{}{} }
	})
	h, _ := w.connect()

	// A good update: progress as status frames, then cmd.done, then the restart.
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T1", "0.1.1"))
	var seen []string
	done := h.until("update done", 5*time.Second, func(m map[string]any) bool {
		if m["t"] == "status" {
			if u, ok := m["update"].(map[string]any); ok {
				seen = append(seen, u["state"].(string))
				if u["version"] != "0.1.1" {
					t.Errorf("update status for %v", u["version"])
				}
			}
		}
		return m["t"] == "cmd.done"
	})
	if got := len(seen); got != 5 || seen[0] != "downloading" || seen[1] != "downloading" || seen[2] != "verifying" || seen[4] != "restarting" {
		t.Fatalf("update status frames %v", seen)
	}
	if r := done["result"].(map[string]any); r["restarting"] != true || r["version"] != "0.1.1" {
		t.Fatalf("result %v", r)
	}
	select {
	case <-restarts:
	case <-time.After(2 * time.Second):
		t.Fatal("no restart after an installed update")
	}

	// A refused release: cmd.err with the reason, a failed update in status, no restart.
	fu.mu.Lock()
	fu.err = &updater.Error{Code: "update_failed", Msg: "the release was refused: release_seq 1 is older than the installed 2"}
	fu.mu.Unlock()
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T2", "0.1.0"))
	failed := false
	e := h.until("update refused", 5*time.Second, func(m map[string]any) bool {
		if u, ok := m["update"].(map[string]any); ok && m["t"] == "status" && u["state"] == "failed" && u["version"] == "0.1.0" {
			failed = true
		}
		return m["t"] == "cmd.err"
	})
	if e["code"] != "update_failed" || e["message"] != "the release was refused: release_seq 1 is older than the installed 2" || !failed {
		t.Fatalf("refusal %v (failed status seen: %v)", e, failed)
	}
	fu.mu.Lock()
	fu.err = &updater.Error{Code: "hub_too_old", Msg: "this release needs hub 2.0.0 or newer"}
	fu.mu.Unlock()
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T3", "0.2.0"))
	if e := h.next("cmd.err", 5*time.Second); e["code"] != "hub_too_old" {
		t.Fatalf("hub too old: %v", e)
	}
	// The running version, intact: done, nothing to restart.
	fu.mu.Lock()
	fu.err = updater.ErrAlreadyInstalled
	fu.mu.Unlock()
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T4", "0.1.0"))
	d := h.until("already installed", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	if d["result"].(map[string]any)["already_installed"] != true {
		t.Fatalf("result %v", d["result"])
	}
	select {
	case <-restarts:
		t.Fatal("a restart without an installed update")
	case <-time.After(100 * time.Millisecond):
	}
	// A version that is not a version is refused by the decoder and never reaches the updater.
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T5", "https://evil.example/x"))
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "bad_args" || e["field"] != "version" {
		t.Fatalf("a URL as version: %v", e)
	}
	fu.mu.Lock()
	defer fu.mu.Unlock()
	if len(fu.called) != 4 {
		t.Fatalf("updater called with %v", fu.called)
	}
}

func TestUpdateWithoutAnInstall(t *testing.T) {
	w := newWorld(t) // no updater: a copy that was not installed
	h, _ := w.connect()
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T1", "0.1.1"))
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "unsupported" {
		t.Fatalf("got %v", e)
	}
}

// mode update_required: no loads, no requests; the update command still works.
func TestUpdateRequiredMode(t *testing.T) {
	fu := &fakeUpdater{}
	w := newWorldWith(t, func(c *Config) { c.Updater, c.Restart = fu, func() {} })
	if _, err := w.a.PairWith(context.Background(), w.hubURL, "K7M29QWDX3", nil, nil); err != nil {
		t.Fatal(err)
	}
	var h *hub
	select {
	case h = <-w.hubs:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
	}
	h.next("hello", 2*time.Second)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"update_required","ping_interval_s":25,"limits":{},"hub_version":"1.0.12","min_agent_version":"0.2.0"}`)
	w.a.cfg.LocalModelID, w.a.cfg.LocalModelPath = "smollm2-360m-q8_0", "local.gguf"
	h.send(loadOK)
	if e := h.next("cmd.err", 2*time.Second); e["code"] != "unsupported" {
		t.Fatalf("load in update_required mode: %v", e)
	}
	h.send(reqStream)
	if hd := h.next("res.head", 2*time.Second); hd["status"].(float64) != 503 {
		t.Fatalf("req in update_required mode: %v", hd)
	}
	h.send(updateCmd("c-01JCKZ3Q9X8YB7N6M5P4R3S2T2", "0.2.0"))
	h.until("update in update_required mode", 5*time.Second, func(m map[string]any) bool { return m["t"] == "cmd.done" })
	if w.a.HubVersion() != "1.0.12" {
		t.Fatalf("hub version %q", w.a.HubVersion())
	}
}

// After the launcher reverted a bad version, the old agent tells the hub once.
func TestRollbackNotice(t *testing.T) {
	said := false
	w := newWorldWith(t, func(c *Config) {
		c.RolledBack = func() string {
			if said {
				return ""
			}
			said = true
			return "0.1.2"
		}
	})
	h, _ := w.connect()
	st := h.until("rollback notice", 3*time.Second, func(m map[string]any) bool { return m["t"] == "status" && m["update"] != nil })
	if u := st["update"].(map[string]any); u["state"] != "failed" || u["version"] != "0.1.2" {
		t.Fatalf("notice %v", u)
	}
}

// Engine files that are not the signed ones are never run.
func TestEngineCheckBlocksLoad(t *testing.T) {
	w := newWorldWith(t, func(c *Config) {
		c.EngineCheck = func() error { return errors.New("engine/b12125/llama-server.exe does not match the signed release") }
	})
	h, _ := w.connect()
	w.a.cfg.LocalModelID, w.a.cfg.LocalModelPath = "smollm2-360m-q8_0", "local.gguf"
	h.send(loadOK)
	if e := h.next("cmd.err", 3*time.Second); e["code"] != "engine_failed" {
		t.Fatalf("got %v", e)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.started) != 0 {
		t.Fatal("the engine was started although its files failed the check")
	}
}
