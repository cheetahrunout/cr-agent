//go:build debug

// Command fakehub is a scripted local hub for the M0 manual check (debug builds only):
// it accepts one agent on ws://localhost:<port>/agent, waits for a loaded engine, streams one chat,
// cancels it after -cancel-after, measures how fast res.end{cancelled} arrives, then proves the slot
// is free with a one-token non-stream request, unloads, and exits.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	key := flag.String("key", "crm_x_y", "expected bearer key")
	cancelAfter := flag.Duration("cancel-after", time.Second, "cancel the stream this long after the first token")
	flag.Parse()

	done := make(chan int, 1)
	http.HandleFunc("/agent", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+*key {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		defer c.CloseNow()
		done <- script(r.Context(), c, *cancelAfter)
	})
	go func() { log.Fatal(http.ListenAndServe(*addr, nil)) }()
	log.Printf("fakehub listening on ws://%s/agent", *addr)
	os.Exit(<-done)
}

type frame map[string]any

func send(ctx context.Context, c *websocket.Conn, s string) {
	if err := c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		log.Fatalf("write: %v", err)
	}
}

func read(ctx context.Context, c *websocket.Conn, d time.Duration) (frame, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var f frame
	return f, json.Unmarshal(b, &f)
}

func script(ctx context.Context, c *websocket.Conn, cancelAfter time.Duration) int {
	hello, err := read(ctx, c, 10*time.Second)
	if err != nil || hello["t"] != "hello" {
		log.Printf("FAIL: first frame %v %v", hello, err)
		return 1
	}
	hwj, _ := json.Marshal(hello["hardware"])
	log.Printf("hello: agent %v proto %v engine %v model %v hardware %s", hello["agent_version"], hello["proto"], hello["engine"], hello["model_id"], hwj)
	send(ctx, c, `{"v":1,"t":"welcome","proto":"1.0","mode":"normal","ping_interval_s":25,"limits":{"max_frame":262144,"max_body":262144},"hub_version":"fake"}`)
	ts := time.Now().UnixMilli()
	send(ctx, c, fmt.Sprintf(`{"v":1,"t":"ping","ts":%d}`, ts))

	loaded := hello["engine"] == "loaded"
	for !loaded {
		f, err := read(ctx, c, 6*time.Minute)
		if err != nil {
			log.Printf("FAIL: waiting for loaded: %v", err)
			return 1
		}
		switch f["t"] {
		case "pong":
			log.Printf("pong ts ok=%v", int64(f["ts"].(float64)) == ts)
		case "status":
			log.Printf("status engine=%v", f["engine"])
			loaded = f["engine"] == "loaded"
			if f["engine"] == "error" {
				log.Printf("FAIL: engine error %v", f["engine_error"])
				return 1
			}
		}
	}

	id := "r-01JCKZ3Q9X8YB7N6M5P4R3S2T3"
	t0 := time.Now()
	send(ctx, c, `{"v":1,"t":"req","id":"`+id+`","path":"/v1/chat/completions","stream":true,"body":{"model":"x","messages":[{"role":"user","content":"Write a very long story about a lighthouse keeper. Make it at least 3000 words."}],"stream":true,"stream_options":{"include_usage":true},"max_tokens":4000}}`)
	var first, cancelAt time.Time
	tokens, frames, afterCancel := 0, 0, 0
	var text strings.Builder
	for {
		f, err := read(ctx, c, 30*time.Second)
		if err != nil {
			log.Printf("FAIL: stream: %v", err)
			return 1
		}
		switch f["t"] {
		case "res.head":
			log.Printf("res.head status=%v ctype=%v after %v", f["status"], f["ctype"], time.Since(t0))
		case "res.data":
			frames++
			for _, p := range f["d"].([]any) {
				tokens++
				if !cancelAt.IsZero() {
					afterCancel++
				}
				var chunk struct {
					Choices []struct {
						Delta struct{ Content string } `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(p.(string)), &chunk) == nil && len(chunk.Choices) > 0 {
					text.WriteString(chunk.Choices[0].Delta.Content)
				}
			}
			if first.IsZero() {
				first = time.Now()
				log.Printf("first token after %v", first.Sub(t0))
			}
			if cancelAt.IsZero() && time.Since(first) >= cancelAfter {
				cancelAt = time.Now()
				send(ctx, c, `{"v":1,"t":"cancel","id":"`+id+`"}`)
				log.Printf("cancel sent after %d payloads in %d frames", tokens, frames)
			}
		case "res.end":
			log.Printf("res.end reason=%v error=%v", f["reason"], f["error"])
			if cancelAt.IsZero() {
				log.Printf("FAIL: stream ended before cancel")
				return 1
			}
			lat := time.Since(cancelAt)
			log.Printf("cancel -> res.end in %v; %d payloads arrived after cancel; ~%.0f payloads/s", lat, afterCancel, float64(tokens)/cancelAt.Sub(first).Seconds())
			log.Printf("text sample: %q", truncate(text.String(), 160))
			if f["reason"] != "cancelled" || lat > 2*time.Second {
				log.Printf("FAIL: cancel did not stop the stream within 2 s")
				return 1
			}
			goto probe
		}
	}
probe:
	// -np 1: if the slot were still generating, this would queue behind it.
	t1 := time.Now()
	send(ctx, c, `{"v":1,"t":"req","id":"r-01JCKZ3Q9X8YB7N6M5P4R3S2T4","path":"/v1/chat/completions","stream":false,"body":{"messages":[{"role":"user","content":"Say OK."}],"max_tokens":1,"temperature":0}}`)
	for {
		f, err := read(ctx, c, 30*time.Second)
		if err != nil {
			log.Printf("FAIL: probe: %v", err)
			return 1
		}
		if f["t"] == "res.end" {
			log.Printf("one-token probe answered in %v (reason %v): the slot was free", time.Since(t1), f["reason"])
			break
		}
	}
	send(ctx, c, `{"v":1,"t":"cmd","id":"c-01JCKZ3Q9X8YB7N6M5P4R3S2T5","op":"unload","args":{}}`)
	for {
		f, err := read(ctx, c, 30*time.Second)
		if err != nil {
			log.Printf("FAIL: unload: %v", err)
			return 1
		}
		if f["t"] == "cmd.done" {
			log.Printf("unload done")
			break
		}
	}
	c.Close(websocket.StatusNormalClosure, "")
	log.Printf("PASS")
	return 0
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
