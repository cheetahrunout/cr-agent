//go:build debug

// Command fakehub is a scripted local hub for the manual checks (debug builds only). It plays the hub
// and Hugging Face for one agent and walks the whole M1 path against the real engine:
//
//	pair (POST /agent/pair, as the hub answers it) → socket with the paired key → hello → inspect →
//	download (from a local stand-in for huggingface.co) with progress → load → stream → cancel →
//	pause (model unloaded, requests refused) → resume → load → unload → delete → close
//
// Start it, then start the debug agent with:
//
//	cragent-debug --no-tray --home <dir> --hub ws://localhost:9000/agent --hf-base http://127.0.0.1:9001 \
//	  --engine <dir with llama-server.exe> --pair <the code fakehub prints>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/hf/hftest"
	"github.com/cheetahrunout/cr-agent/internal/updater/relsrv"
)

const (
	pairCode  = "K7M29QWDX3"
	machineID = "01JCKZ3Q9X8YB7N6M5P4R3S2T1"
	commit    = "7c1e2a9d40b3f1e8a9c0d2b4e6f8a1c3e5d7f9b2"
)

var reKey = regexp.MustCompile(`^crm_[A-Za-z2-7]{8}_[A-Za-z2-7]{52}$`)

type hubState struct {
	mu     sync.Mutex
	key    string // the machine key bound at pairing
	paired bool
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "hub listen address")
	hfAddr := flag.String("hf-addr", "127.0.0.1:9001", "listen address of the stand-in for huggingface.co")
	gguf := flag.String("gguf", "", "a real .gguf to serve for the download (required)")
	repo := flag.String("repo", "unsloth/SmolLM2-360M-GGUF", "the repo id the stand-in publishes the file under")
	modelID := flag.String("model-id", "smollm2-360m-q8_0", "model id the hub uses")
	cancelAfter := flag.Duration("cancel-after", time.Second, "cancel the stream this long after the first token")
	slow := flag.Duration("chunk-delay", 0, "delay per 64 KiB chunk of the download (to watch progress)")
	scenario := flag.String("scenario", "full", "full: the whole path with a download; load: only load the agent's local --model with the settings below; update: the release dry run")
	releases := flag.String("releases", "", "scenario update: the folder with v<version>/ release folders")
	releaseAddr := flag.String("release-addr", "127.0.0.1:9002", "scenario update: listen address of the stand-in for the GitHub release downloads")
	gpuLayers := flag.Int("gpu-layers", -1, "scenario load: layers on the GPU (-1 = all)")
	ctxSize := flag.Int("ctx", 8192, "scenario load: context size")
	threads := flag.Int("threads", 8, "scenario load: threads")
	flag.Parse()
	var data []byte
	file := ""
	if *scenario == "full" {
		if *gguf == "" {
			log.Fatal("-gguf is required")
		}
		var err error
		if data, err = os.ReadFile(*gguf); err != nil {
			log.Fatal(err)
		}
		file = (*gguf)[strings.LastIndexAny(*gguf, `/\`)+1:]
	}
	hfs, err := hftest.NewOn(*hfAddr)
	if err != nil {
		log.Fatal(err)
	}
	hfs.ChunkDelay = *slow
	if file != "" {
		hfs.AddRepo(*repo, commit, false, map[string][]byte{file: data})
		log.Printf("stand-in for huggingface.co on %s: %s at %s, %s (%d MB)", hfs.URL, *repo, commit[:12], file, len(data)>>20)
	}

	var upd *updateRun
	if *scenario == "update" {
		if *releases == "" {
			log.Fatal("-releases is required for the update scenario")
		}
		rel, err := relsrv.NewOn(*releaseAddr, *releases)
		if err != nil {
			log.Fatal(err)
		}
		upd = &updateRun{rel: rel}
		log.Printf("stand-in for the GitHub release downloads on %s/ serving %s", rel.URL, *releases)
	}
	st := &hubState{}
	done := make(chan int, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/pair", st.pair)
	mux.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		ok := st.paired && r.Header.Get("Authorization") == "Bearer "+st.key
		st.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if !ok { // as the hub: a key that is not accepted is upgraded and closed with 4000
			c.Close(4000, "key not accepted")
			return
		}
		c.SetReadLimit(1 << 20)
		if upd != nil {
			if finished, code := upd.session(r.Context(), c); finished {
				done <- code
			}
			return
		}
		a := scriptArgs{repo: *repo, file: file, modelID: *modelID, size: int64(len(data)), cancelAfter: *cancelAfter, hf: hfs,
			gpuLayers: *gpuLayers, ctx: *ctxSize, threads: *threads}
		if *scenario == "load" {
			done <- loadOnly(r.Context(), c, a)
			return
		}
		done <- script(r.Context(), c, a)
	})
	go func() { log.Fatal(http.ListenAndServe(*addr, mux)) }()
	log.Printf("fakehub on ws://%s/agent; pairing code %s-%s-%s", *addr, pairCode[:4], pairCode[4:8], pairCode[8:])
	os.Exit(<-done)
}

// pair answers POST /agent/pair the way the hub does for a fresh code and a new machine.
func (st *hubState) pair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code, KeyID, KeySecret, Prior string
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
	var m map[string]string
	json.Unmarshal(raw, &m)
	req.Code, req.KeyID, req.KeySecret, req.Prior = m["code"], m["key_id"], m["key_secret"], m["prior_machine_id"]
	key := "crm_" + req.KeyID + "_" + req.KeySecret
	st.mu.Lock()
	defer st.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case req.Code != pairCode || (st.paired && key != st.key):
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"pairing_code_invalid","type":"hub","message":"This pairing code is not valid or has expired."}}`)
		log.Printf("pair: refused (code ok=%v)", req.Code == pairCode)
	case !reKey.MatchString(key):
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"invalid_request","type":"hub","message":"The request is not valid."}}`)
		log.Printf("pair: malformed key material")
	default:
		st.key, st.paired = key, true
		io.WriteString(w, `{"machine_id":"`+machineID+`","slug":"test-pc"}`)
		log.Printf("pair: ok, key id %s (8 chars), secret of %d chars, prior machine %q", req.KeyID, len(req.KeySecret), req.Prior)
	}
}

type frame map[string]any

type scriptArgs struct {
	repo, file, modelID string
	size                int64
	cancelAfter         time.Duration
	hf                  *hftest.Server
	gpuLayers, ctx      int
	threads             int
}

// loadOnly loads the model the agent was started with (--model), with a chosen GPU offload, answers
// one short request and unloads: the check that the job memory cap does not stop a partly offloaded load.
func loadOnly(ctx context.Context, c *websocket.Conn, a scriptArgs) int {
	h := &conn{ctx: ctx, c: c}
	hello, err := h.read(10 * time.Second)
	if err != nil || hello["t"] != "hello" {
		log.Printf("FAIL: first frame %v %v", hello, err)
		return 1
	}
	hwj, _ := json.Marshal(hello["hardware"])
	log.Printf("hello: agent %v engine %v; hardware %s", hello["agent_version"], hello["engine"], hwj)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{},"hub_version":"fake"}`)
	layers := `"all"`
	if a.gpuLayers >= 0 {
		layers = fmt.Sprint(a.gpuLayers)
	}
	t0 := time.Now()
	id := h.cmd("load", fmt.Sprintf(`{"model_id":%q,"settings":{"ctx":%d,"gpu_layers":%s,"threads":%d,"kv_type":"f16","flash_attn":"on","parallel":1}}`,
		a.modelID, a.ctx, layers, a.threads))
	f, ok := h.until("load", 10*time.Minute, func(f frame) {
		if f["t"] == "status" {
			log.Printf("status: engine=%v error=%v", f["engine"], f["engine_error"])
		}
	}, doneOf(id))
	if !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: load with gpu_layers=%s", layers)
		return 1
	}
	log.Printf("load with gpu_layers=%s ctx=%d done in %v: %v", layers, a.ctx, time.Since(t0).Round(time.Millisecond), f["result"])
	rid := h.id("r")
	t0 = time.Now()
	h.send(`{"v":1,"t":"req","id":"` + rid + `","path":"/v1/chat/completions","stream":true,"body":{"messages":[{"role":"user","content":"Count from 1 to 30, one number per line."}],"stream":true,"max_tokens":120,"temperature":0}}`)
	var first time.Time
	n := 0
	f, ok = h.until("answer", 5*time.Minute, func(f frame) {
		if f["t"] == "res.data" && f["id"] == rid {
			if first.IsZero() {
				first = time.Now()
			}
			n += len(f["d"].([]any))
		}
	}, func(f frame) bool { return f["t"] == "res.end" && f["id"] == rid })
	if !ok || f["reason"] != "done" || n < 5 {
		log.Printf("FAIL: answer: %v after %d payloads", f, n)
		return 1
	}
	log.Printf("answer: first token after %v, %d payloads, about %.0f payloads/s", first.Sub(t0).Round(time.Millisecond), n, float64(n)/time.Since(first).Seconds())
	id = h.cmd("unload", `{}`)
	if _, ok = h.until("unload", time.Minute, nil, doneOf(id)); !ok {
		return 1
	}
	c.Close(websocket.StatusNormalClosure, "")
	log.Printf("PASS")
	return 0
}

type conn struct {
	ctx context.Context
	c   *websocket.Conn
	n   int
}

func (h *conn) id(prefix string) string {
	h.n++
	return fmt.Sprintf("%s-01JCKZ3Q9X8YB7N6M5P4R3S2%02d", prefix, h.n)
}

func (h *conn) send(s string) {
	if err := h.c.Write(h.ctx, websocket.MessageText, []byte(s)); err != nil {
		log.Fatalf("write: %v", err)
	}
}

func (h *conn) read(d time.Duration) (frame, error) {
	ctx, cancel := context.WithTimeout(h.ctx, d)
	defer cancel()
	_, b, err := h.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var f frame
	return f, json.Unmarshal(b, &f)
}

// until reads frames until pred accepts one; each frame also goes to see (when not nil).
func (h *conn) until(what string, d time.Duration, see func(frame), pred func(frame) bool) (frame, bool) {
	deadline := time.Now().Add(d)
	for {
		f, err := h.read(time.Until(deadline))
		if err != nil {
			log.Printf("FAIL: waiting for %s: %v", what, err)
			return nil, false
		}
		if see != nil {
			see(f)
		}
		if f["t"] == "cmd.err" {
			log.Printf("cmd.err: code=%v field=%v message=%v", f["code"], f["field"], f["message"])
		}
		if pred(f) {
			return f, true
		}
	}
}

func (h *conn) cmd(op, args string) string {
	id := h.id("c")
	h.send(`{"v":1,"t":"cmd","id":"` + id + `","op":"` + op + `","args":` + args + `}`)
	return id
}

func doneOf(id string) func(frame) bool {
	return func(f frame) bool { return (f["t"] == "cmd.done" || f["t"] == "cmd.err") && f["id"] == id }
}

func modelState(f frame, id string) string {
	ms, _ := f["models"].([]any)
	for _, x := range ms {
		if m := x.(map[string]any); m["model_id"] == id {
			return m["state"].(string)
		}
	}
	return ""
}

func script(ctx context.Context, c *websocket.Conn, a scriptArgs) int {
	h := &conn{ctx: ctx, c: c}
	hello, err := h.read(10 * time.Second)
	if err != nil || hello["t"] != "hello" {
		log.Printf("FAIL: first frame %v %v", hello, err)
		return 1
	}
	hwj, _ := json.Marshal(hello["hardware"])
	log.Printf("hello: agent %v proto %v engine %v capabilities %v hf_token %v models %v", hello["agent_version"], hello["proto"],
		hello["engine"], hello["capabilities"], hello["hf_token"], hello["models"])
	log.Printf("hardware: %s", hwj)
	h.send(`{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{"max_frame":262144,"max_body":262144},"hub_version":"fake"}`)
	h.send(fmt.Sprintf(`{"v":1,"t":"ping","ts":%d}`, time.Now().UnixMilli()))
	if _, ok := h.until("pong", 5*time.Second, nil, func(f frame) bool { return f["t"] == "pong" }); !ok {
		return 1
	}

	// inspect
	t0 := time.Now()
	id := h.cmd("inspect", fmt.Sprintf(`{"repo":%q,"commit":%q,"file":%q}`, a.repo, commit, a.file))
	f, ok := h.until("inspect", 60*time.Second, nil, doneOf(id))
	if !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: inspect")
		return 1
	}
	rj, _ := json.Marshal(f["result"])
	log.Printf("inspect in %v: %s", time.Since(t0).Round(time.Millisecond), rj)
	blobsAfterInspect := a.hf.BlobRequests.Load()

	// download
	t0 = time.Now()
	id = h.cmd("download", fmt.Sprintf(`{"model_id":%q,"repo":%q,"commit":%q,"files":[%q],"kind":"gguf"}`, a.modelID, a.repo, commit, a.file))
	statuses, lastPct, ready := 0, -1.0, false
	var firstStatus, lastStatus time.Time
	f, ok = h.until("download", 10*time.Minute, func(f frame) {
		if f["t"] != "status" {
			return
		}
		if modelState(f, a.modelID) == "ready" {
			ready = true
		}
		for _, j := range f["jobs"].([]any) {
			jm := j.(map[string]any)
			if jm["kind"] == "download" && jm["pct"] != nil {
				statuses++
				if firstStatus.IsZero() {
					firstStatus = time.Now()
				}
				lastStatus = time.Now()
				if p := jm["pct"].(float64); p < lastPct {
					log.Printf("FAIL: progress went backwards (%v after %v)", p, lastPct)
				} else {
					lastPct = p
				}
			}
		}
	}, doneOf(id))
	if !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: download")
		return 1
	}
	el := time.Since(t0)
	cadence := time.Duration(0)
	if statuses > 1 {
		cadence = lastStatus.Sub(firstStatus) / time.Duration(statuses-1)
	}
	log.Printf("download done in %v (%.0f MB/s): %d progress status frames, about one every %v, last pct %.1f; result %v",
		el.Round(time.Millisecond), float64(a.size)/(1<<20)/el.Seconds(), statuses, cadence.Round(10*time.Millisecond), lastPct, f["result"])
	if !ready {
		if _, ok := h.until("model ready", 10*time.Second, nil, func(f frame) bool { return f["t"] == "status" && modelState(f, a.modelID) == "ready" }); !ok {
			return 1
		}
	}
	log.Printf("status lists %s as ready; blob requests to the stand-in: %d for inspect, %d for the download; token seen off-origin: %d",
		a.modelID, blobsAfterInspect, a.hf.BlobRequests.Load()-blobsAfterInspect, a.hf.TokenAtCDN.Load())

	// load
	load := fmt.Sprintf(`{"model_id":%q,"settings":{"ctx":4096,"gpu_layers":"all","threads":8,"kv_type":"f16","flash_attn":"on","parallel":1}}`, a.modelID)
	t0 = time.Now()
	id = h.cmd("load", load)
	f, ok = h.until("load", 6*time.Minute, nil, doneOf(id))
	if !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: load")
		return 1
	}
	log.Printf("load done in %v: %v", time.Since(t0).Round(time.Millisecond), f["result"])

	// stream and cancel
	rid := h.id("r")
	t0 = time.Now()
	h.send(`{"v":1,"t":"req","id":"` + rid + `","path":"/v1/chat/completions","stream":true,"body":{"model":"x","messages":[{"role":"user","content":"Write a very long story about a lighthouse keeper. Make it at least 3000 words."}],"stream":true,"stream_options":{"include_usage":true},"max_tokens":4000}}`)
	var first, cancelAt time.Time
	tokens, afterCancel := 0, 0
	f, ok = h.until("stream end", 60*time.Second, func(f frame) {
		if f["t"] != "res.data" || f["id"] != rid {
			return
		}
		n := len(f["d"].([]any))
		tokens += n
		if !cancelAt.IsZero() {
			afterCancel += n
		}
		if first.IsZero() {
			first = time.Now()
			log.Printf("first token after %v", first.Sub(t0).Round(time.Millisecond))
		}
		if cancelAt.IsZero() && time.Since(first) >= a.cancelAfter {
			cancelAt = time.Now()
			h.send(`{"v":1,"t":"cancel","id":"` + rid + `"}`)
		}
	}, func(f frame) bool { return f["t"] == "res.end" && f["id"] == rid })
	if !ok || cancelAt.IsZero() || f["reason"] != "cancelled" || time.Since(cancelAt) > 2*time.Second {
		log.Printf("FAIL: stream/cancel (end %v)", f)
		return 1
	}
	log.Printf("cancel -> res.end{cancelled} in %v; %d payloads after the cancel; about %.0f payloads/s",
		time.Since(cancelAt).Round(10*time.Microsecond), afterCancel, float64(tokens)/cancelAt.Sub(first).Seconds())

	// pause: unloaded, requests refused; resume
	id = h.cmd("pause", `{}`)
	if _, ok = h.until("paused", 30*time.Second, nil, func(f frame) bool {
		return f["t"] == "status" && f["local_pause"] == true && f["engine"] == "unloaded"
	}); !ok {
		return 1
	}
	rid = h.id("r")
	h.send(`{"v":1,"t":"req","id":"` + rid + `","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"hi"}]}}`)
	f, ok = h.until("refusal while paused", 10*time.Second, nil, func(f frame) bool { return f["t"] == "res.head" && f["id"] == rid })
	if !ok || f["status"].(float64) != 503 {
		log.Printf("FAIL: a request was not refused while paused: %v", f)
		return 1
	}
	log.Printf("paused: status says local_pause, engine unloaded; a request got HTTP %v", f["status"])
	id = h.cmd("resume", `{}`)
	if _, ok = h.until("resumed", 30*time.Second, nil, func(f frame) bool { return f["t"] == "status" && f["local_pause"] == false }); !ok {
		return 1
	}

	// load again, one short answer, unload, delete
	id = h.cmd("load", load)
	if f, ok = h.until("second load", 6*time.Minute, nil, doneOf(id)); !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: second load")
		return 1
	}
	rid = h.id("r")
	t0 = time.Now()
	h.send(`{"v":1,"t":"req","id":"` + rid + `","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"Say OK."}],"max_tokens":4,"temperature":0}}`)
	if f, ok = h.until("short answer", 60*time.Second, nil, func(f frame) bool { return f["t"] == "res.end" && f["id"] == rid }); !ok || f["reason"] != "done" {
		log.Printf("FAIL: short answer: %v", f)
		return 1
	}
	log.Printf("after resume: loaded again and answered in %v", time.Since(t0).Round(time.Millisecond))
	id = h.cmd("delete", fmt.Sprintf(`{"model_id":%q}`, a.modelID))
	if f, ok = h.until("delete while loaded", 10*time.Second, nil, doneOf(id)); !ok || f["code"] != "busy" {
		log.Printf("FAIL: delete while loaded was not refused: %v", f)
		return 1
	}
	id = h.cmd("unload", `{}`)
	if _, ok = h.until("unload", 30*time.Second, nil, doneOf(id)); !ok {
		return 1
	}
	id = h.cmd("delete", fmt.Sprintf(`{"model_id":%q}`, a.modelID))
	if f, ok = h.until("delete", 30*time.Second, nil, doneOf(id)); !ok || f["t"] != "cmd.done" {
		log.Printf("FAIL: delete")
		return 1
	}
	log.Printf("unloaded and deleted")
	c.Close(websocket.StatusNormalClosure, "")
	log.Printf("PASS")
	return 0
}
