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
	two, moe := 2, 41
	cases := map[string]func(s *protocol.Settings){
		"ctx":        func(s *protocol.Settings) { s.Ctx = 65536 }, // > n_ctx_train
		"threads":    func(s *protocol.Settings) { s.Threads = 17 },
		"gpu_layers": func(s *protocol.Settings) { s.GPULayers = protocol.Layers(42) },
		"n_cpu_moe":  func(s *protocol.Settings) { s.NCPUMoE = &moe },
		"parallel":   func(s *protocol.Settings) { s.Parallel = &two },
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
}

func TestBuildArgs(t *testing.T) {
	s := DefaultSettings(8)
	s.KVType, s.FlashAttn = "q8_0", "auto"
	args := BuildArgs(Launch{ModelPath: `C:\m\x.gguf`, Alias: "x", Port: 5555, APIKey: "k", Settings: s})
	line := strings.Join(args, " ")
	for _, want := range []string{"--cors-origins http://127.0.0.1:5555", "--no-cors-credentials", "--alias x", "--host 127.0.0.1", "--port 5555", "--api-key k", "--jinja", "--no-webui", "--no-slots",
		"--fit off", "-c 4096", "-ngl 999", "-t 8", "-ctk q8_0", "-ctv q8_0", "-fa auto", "-np 1", "--reasoning-format deepseek"} {
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
