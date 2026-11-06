package relay

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRewriteBody(t *testing.T) {
	out, status, _, _ := rewriteBody([]byte(`{"model":"anna-pc/qwen","messages":[{"role":"user","content":"x"}],"temperature":0.2,"stream_options":{"include_usage":true}}`), "qwen3-14b", true)
	if status != 0 {
		t.Fatalf("status %d", status)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["model"] != "qwen3-14b" || m["stream"] != true || m["temperature"] != 0.2 {
		t.Fatalf("rewritten %s", out)
	}
	out, _, _, _ = rewriteBody([]byte(`{"messages":[{"role":"user","content":"x"}],"stream":true,"stream_options":{}}`), "a", false)
	if strings.Contains(string(out), "stream_options") || !strings.Contains(string(out), `"stream":false`) {
		t.Fatalf("non-stream body %s", out)
	}

	many := `{"messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 200) + `{"role":"user","content":"x"}]}`
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", MaxBody) + `"}]}`
	for name, c := range map[string]struct {
		body   string
		status int
	}{
		"unknown field": {`{"messages":[{"role":"user","content":"x"}],"lora":"C:/x"}`, 400},
		"slot field":    {`{"messages":[{"role":"user","content":"x"}],"id_slot":0}`, 400},
		"no messages":   {`{"model":"x"}`, 400},
		"201 messages":  {many, 400},
		"n=2":           {`{"messages":[{"role":"user","content":"x"}],"n":2}`, 400},
		"too big":       {big, 413},
		"empty":         {``, 400},
	} {
		if _, status, _, _ := rewriteBody([]byte(c.body), "a", true); status != c.status {
			t.Errorf("%s: status %d, want %d", name, status, c.status)
		}
	}
	if _, status, _, _ := rewriteBody([]byte(`{"messages":[{"role":"user","content":"x"}],"n":1}`), "a", true); status != 0 {
		t.Errorf("n=1 refused")
	}
}

// Each request takes a new connection: over the sandbox's Unix socket a kept-alive one can stall for ever.
func TestEachRequestTakesANewConnection(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	r := New(func() (string, string, string, bool) { return srv.URL, "", "", true }, nil)
	for i := 0; i < 3; i++ {
		resp, err := r.Client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := conns.Load(); n != 3 {
		t.Fatalf("%d connections for 3 requests", n)
	}
}
