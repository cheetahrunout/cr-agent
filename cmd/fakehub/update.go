//go:build debug

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/updater/relsrv"
)

// updateRun is the dry run of the release path (PLAN ruling B27), spread over the connections of
// one agent as the launcher restarts it:
//
//	0.1.0 connects → update{0.1.1} → restart → 0.1.1 connects (healthy, committed)
//	→ update{0.1.2}, a build that never becomes healthy → the launcher reverts to 0.1.1 within 2 min
//	→ 0.1.1 says the update failed → update{0.1.0}, signed again, is refused by committed_seq
//	→ update{0.1.3}, whose manifest has one flipped byte, is refused before any archive is fetched
type updateRun struct {
	rel *relsrv.Server

	mu     sync.Mutex
	step   int
	sentAt time.Time // when the last update command was sent
	doneAt time.Time // when its cmd.done arrived
}

// update sends update{version} and reports how it ended.
func (h *conn) update(version string) (frame, []string, bool) {
	id := h.cmd("update", fmt.Sprintf(`{"version":%q}`, version))
	var states []string
	f, ok := h.until("update "+version, 5*time.Minute, func(f frame) {
		if f["t"] != "status" {
			return
		}
		if u, isUpdate := f["update"].(map[string]any); isUpdate {
			s := fmt.Sprintf("%v", u["state"])
			if u["pct"] != nil && s == "downloading" {
				s = fmt.Sprintf("%s %v%%", s, u["pct"])
			}
			if len(states) == 0 || states[len(states)-1] != s {
				states = append(states, s)
			}
		}
	}, doneOf(id))
	return f, states, ok
}

// archives counts the archive requests the release server saw for a version.
func (u *updateRun) archives(version string) int {
	n := 0
	for _, r := range u.rel.Requests() {
		if strings.Contains(r, "/v"+version+"/") && strings.HasSuffix(r, ".zip") {
			n++
		}
	}
	return n
}

// session handles one connection. finished is true when the whole run is over.
func (u *updateRun) session(ctx context.Context, c *websocket.Conn) (finished bool, code int) {
	h := &conn{ctx: ctx, c: c}
	hello, err := h.read(10 * time.Second)
	if err != nil || hello["t"] != "hello" {
		log.Printf("FAIL: first frame %v %v", hello, err)
		return true, 1
	}
	version := fmt.Sprintf("%v", hello["agent_version"])
	u.mu.Lock()
	step, sentAt, doneAt := u.step, u.sentAt, u.doneAt
	u.mu.Unlock()
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{},"hub_version":"1.0.12"}`)
	fail := func(format string, a ...any) (bool, int) {
		log.Printf("FAIL: "+format, a...)
		return true, 1
	}
	waitClose := func() { // the agent restarts: its socket goes away
		for {
			if _, err := h.read(5 * time.Minute); err != nil {
				return
			}
		}
	}

	switch {
	case step == 0 && version == "0.1.0":
		log.Printf("step 1: agent 0.1.0 connected (installed without a manifest; it fetches and checks its own)")
		time.Sleep(2 * time.Second)
		t0 := time.Now()
		f, states, ok := h.update("0.1.1")
		if !ok || f["t"] != "cmd.done" {
			return fail("update to 0.1.1: %v", f)
		}
		log.Printf("step 1: update{0.1.1} installed in %v; status frames: %s; archive requests for 0.1.1: %d",
			time.Since(t0).Round(time.Millisecond), strings.Join(states, " → "), u.archives("0.1.1"))
		u.mu.Lock()
		u.step, u.sentAt, u.doneAt = 1, t0, time.Now()
		u.mu.Unlock()
		waitClose()

	case step == 1 && version == "0.1.1":
		log.Printf("step 2: agent 0.1.1 connected %v after cmd.done (launcher restart, signature and file check, reconnect)", time.Since(doneAt).Round(time.Millisecond))
		time.Sleep(3 * time.Second) // let it pass the health gate and commit release_seq 2
		t0 := time.Now()
		f, _, ok := h.update("0.1.2")
		if !ok || f["t"] != "cmd.done" {
			return fail("update to 0.1.2: %v", f)
		}
		log.Printf("step 3: update{0.1.2} installed in %v; 0.1.2 is the build that never becomes healthy", time.Since(t0).Round(time.Millisecond))
		u.mu.Lock()
		u.step, u.sentAt, u.doneAt = 2, t0, time.Now()
		u.mu.Unlock()
		waitClose()

	case step == 2 && version == "0.1.2":
		log.Printf("step 3: agent 0.1.2 connected %v after cmd.done; now the launcher's health gate (%v) runs out", time.Since(doneAt).Round(time.Millisecond), layout.HealthGate)
		waitClose()

	case step == 2 && version == "0.1.1":
		back := time.Since(doneAt)
		log.Printf("step 4: agent 0.1.1 is back %v after 0.1.2 was installed (%v after the update command)", back.Round(time.Millisecond), time.Since(sentAt).Round(time.Millisecond))
		if back > layout.HealthGate+15*time.Second {
			return fail("the launcher took %v to revert", back)
		}
		f, ok := h.until("the rollback notice", 15*time.Second, nil, func(f frame) bool {
			up, isUpdate := f["update"].(map[string]any)
			return f["t"] == "status" && isUpdate && up["state"] == "failed" && up["version"] == "0.1.2"
		})
		if !ok {
			return fail("0.1.1 did not report the failed update")
		}
		log.Printf("step 4: 0.1.1 reports update %v", f["update"])

		u.rel.Reset()
		f, _, ok = h.update("0.1.0")
		if !ok || f["t"] != "cmd.err" || f["code"] != "update_failed" {
			return fail("the re-signed 0.1.0 was not refused: %v", f)
		}
		if n := u.archives("0.1.0"); n != 0 {
			return fail("%d archives were fetched for the refused 0.1.0", n)
		}
		log.Printf("step 5: update{0.1.0} (signed again, release_seq 1) refused: %v; requests: %s", f["message"], strings.Join(u.rel.Requests(), ", "))

		u.rel.Reset()
		f, _, ok = h.update("0.1.3")
		if !ok || f["t"] != "cmd.err" || f["code"] != "update_failed" {
			return fail("the manifest with a flipped byte was not refused: %v", f)
		}
		if n := u.archives("0.1.3"); n != 0 {
			return fail("%d archives were fetched before the manifest of 0.1.3 was refused", n)
		}
		log.Printf("step 6: update{0.1.3} (one flipped byte in manifest.json) refused: %v; requests: %s", f["message"], strings.Join(u.rel.Requests(), ", "))
		c.Close(websocket.StatusNormalClosure, "")
		log.Printf("PASS")
		return true, 0

	default:
		return fail("agent %s connected at step %d", version, step)
	}
	return false, 0
}
