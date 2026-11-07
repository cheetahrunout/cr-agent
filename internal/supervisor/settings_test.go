package supervisor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

func TestCheckSettings(t *testing.T) {
	lim := Limits{LogicalCores: 16, NLayer: 40, NCtxTrain: 32768}
	ok := DefaultSettings(8)
	if fe := CheckSettings(ok, lim); fe != nil {
		t.Fatalf("defaults refused: %v", fe)
	}
	five, moe := 5, 41
	cases := map[string]func(s *protocol.Settings){
		"ctx":        func(s *protocol.Settings) { s.Ctx = 65536 }, // > n_ctx_train
		"threads":    func(s *protocol.Settings) { s.Threads = 17 },
		"gpu_layers": func(s *protocol.Settings) { s.GPULayers = protocol.Layers(42) },
		"n_cpu_moe":  func(s *protocol.Settings) { s.NCPUMoE = &moe },
		"parallel":   func(s *protocol.Settings) { s.Parallel = &five },
		"kv_type":    func(s *protocol.Settings) { s.KVType = "q4_0" },
		"flash_attn": func(s *protocol.Settings) { s.FlashAttn = "off" },
	}
	for field, mut := range cases {
		s := DefaultSettings(8)
		mut(&s)
		fe := CheckSettings(s, lim)
		if fe == nil || fe.Field != field {
			t.Errorf("%s: got %v", field, fe)
		}
	}
	s := DefaultSettings(8)
	s.GPULayers = protocol.Layers(41)
	if fe := CheckSettings(s, lim); fe != nil {
		t.Errorf("n_layer+1 refused: %v", fe)
	}
	// Up to 4 people share one engine (ruling B74).
	for n := 1; n <= 4; n++ {
		s := DefaultSettings(8)
		s.Parallel = &n
		if fe := CheckSettings(s, lim); fe != nil {
			t.Errorf("parallel %d refused: %v", n, fe)
		}
	}
	zero := 0
	s.Parallel = &zero
	if fe := CheckSettings(s, lim); fe == nil || fe.Field != "parallel" {
		t.Errorf("parallel 0: %v", fe)
	}
}

func TestBuildArgs(t *testing.T) {
	s := DefaultSettings(8)
	s.KVType, s.FlashAttn = "q8_0", "auto"
	args := BuildArgs(Launch{ModelPath: `C:\m\x.gguf`, Alias: "x", Port: 5555, APIKey: "k", Settings: s})
	line := strings.Join(args, " ")
	for _, want := range []string{"--cors-origins http://127.0.0.1:5555", "--no-cors-credentials", "--alias x", "--host 127.0.0.1", "--port 5555", "--api-key k", "--jinja", "--no-webui", "--no-slots",
		"--fit off", "-c 4096", "-ngl 999", "-t 8", "-ctk q8_0", "-ctv q8_0", "-fa auto", "-np 1", "-no-kvu", "--reasoning-format deepseek"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	for _, banned := range []string{"-v", "--verbose", "--log-verbose", "-lv", "--verbosity", "--metrics", "--slots", "--rpc", "--lora",
		"--slot-save-path", "--chat-template-file", "--log-prompts-dir", "--log-file", "0.0.0.0"} {
		if slices.Contains(args, banned) {
			t.Errorf("banned flag %q present", banned)
		}
	}
	for _, kv := range []string{"-kvu", "--kv-unified"} {
		if slices.Contains(args, kv) {
			t.Errorf("a shared cache (%s) lets one long prompt end everyone's answer", kv)
		}
	}
	three := 3
	s.Parallel = &three
	if line := strings.Join(BuildArgs(Launch{ModelPath: "m.gguf", Alias: "x", Port: 1, APIKey: "k", Settings: s}), " "); !strings.Contains(line, "-np 3 -no-kvu") || strings.Contains(line, "-np 1") {
		t.Errorf("three people: %s", line)
	}
	s.Parallel = nil
	if line := strings.Join(BuildArgs(Launch{ModelPath: "m.gguf", Alias: "x", Port: 1, APIKey: "k", Settings: s}), " "); !strings.Contains(line, "-np 1 -no-kvu") {
		t.Errorf("no parallel given: %s", line)
	}
	s.GPULayers = protocol.Layers(12)
	s.ChatTemplate = "chatml"
	line = strings.Join(BuildArgs(Launch{ModelPath: "m.gguf", Alias: "x", Port: 1, APIKey: "k", Settings: s}), " ")
	if !strings.Contains(line, "-ngl 12") || !strings.Contains(line, "--chat-template chatml") {
		t.Errorf("args %s", line)
	}
}

// Finding L7: without an engine folder nothing is looked for in the current directory.
func TestStartNeedsAnEngineFolder(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil || !strings.Contains(err.Error(), "no engine folder") {
		t.Fatalf("got %v", err)
	}
}

// In the sandbox the engine listens on a Unix socket and on nothing else.
func TestSocketArgs(t *testing.T) {
	args := BuildArgs(Launch{ModelPath: "m.gguf", Alias: "m", Socket: `C:\\x\\e-1.sock`, APIKey: "k", Port: 5555})
	line := strings.Join(args, " ")
	if !strings.Contains(line, `--host C:\\x\\e-1.sock`) || strings.Contains(line, "--port") || strings.Contains(line, "127.0.0.1") {
		t.Fatalf("args: %s", line)
	}
	if !strings.Contains(line, "--cors-origins "+socketOrigin) {
		t.Fatalf("origin: %s", line)
	}
}

// DialEngine knows the sandboxed engines, and leaves every other address alone.
func TestDialEngineTable(t *testing.T) {
	base := registerSocket("some-path.sock")
	host := strings.TrimPrefix(base, "http://") + ":80"
	socketsMu.Lock()
	got := sockets[host]
	socketsMu.Unlock()
	if got != "some-path.sock" {
		t.Fatalf("%q", got)
	}
	forgetSocket("some-path.sock")
	socketsMu.Lock()
	n := len(sockets)
	socketsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d left", n)
	}
	if forgetSocket(""); len(sockets) != 0 {
		t.Fatal("an empty path changed the table")
	}
}
