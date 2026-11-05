package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cheetahrunout/cr-agent/internal/enginelock"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// TestPinnedEngine is the engine smoke test of the release pipeline (PLAN §13): the engine in
// CRAGENT_ENGINE_DIR is the build engine.lock pins, and it knows every flag and every chat template
// name the agent can put on its command line. It needs no GPU.
func TestPinnedEngine(t *testing.T) {
	dir := os.Getenv("CRAGENT_ENGINE_DIR")
	if dir == "" {
		t.Skip("CRAGENT_ENGINE_DIR not set")
	}
	exe := filepath.Join(dir, "llama-server.exe")
	version, err := exec.Command(exe, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, version)
	}
	build := strings.TrimPrefix(enginelock.Pinned().Tag, "b")
	if !strings.Contains(string(version), "build "+build) {
		t.Fatalf("the engine is not %s:\n%s", enginelock.Pinned().Tag, version)
	}
	help, err := exec.Command(exe, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	moe, par := 3, 1
	s := protocol.Settings{Ctx: 8192, GPULayers: protocol.Layers(10), Threads: 4, KVType: "q8_0", FlashAttn: "auto", NCPUMoE: &moe, ChatTemplate: "chatml", Parallel: &par}
	for _, arg := range BuildArgs(Launch{ModelPath: "m.gguf", Alias: "m", Port: 1, APIKey: "k", Settings: s}) {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if !regexp.MustCompile(`(?m)(^|[\s,])` + regexp.QuoteMeta(arg) + `([\s,]|$)`).Match(help) {
			t.Errorf("the pinned engine does not know the flag %s", arg)
		}
	}
	for name := range protocol.BuiltinTemplates {
		if !regexp.MustCompile(`[\s,]` + regexp.QuoteMeta(name) + `[\s,]`).Match(help) {
			t.Errorf("the pinned engine has no built-in chat template %q", name)
		}
	}
	for _, v := range []string{"f16", "q8_0"} {
		if !strings.Contains(string(help), v) {
			t.Errorf("KV cache type %s is not in --help", v)
		}
	}
}
