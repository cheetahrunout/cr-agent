package enginelock

import (
	"strings"
	"testing"
)

func TestPinned(t *testing.T) {
	l := Pinned()
	if l.Tag != "b12125" || len(l.Archives) != 2 {
		t.Fatalf("pin %+v", l)
	}
	for _, a := range l.Archives {
		if !strings.Contains(a.Name, "win-cuda-13.4-x64") {
			t.Errorf("archive %q is not the Windows CUDA 13.4 build", a.Name)
		}
	}
	// Architectures seen on this project so far, and one that must not be there.
	for _, a := range []string{"llama", "qwen2", "qwen3", "qwen3moe", "qwen35moe", "gemma3", "gpt-oss"} {
		if !l.Supports(a) {
			t.Errorf("architecture %q missing from the generated list", a)
		}
	}
	if l.Supports("clip") || l.Supports("") || l.Supports("(unknown)") || len(l.Archs()) < 50 {
		t.Errorf("architecture list wrong: %d entries", len(l.Archs()))
	}
}

// The allow-list lets through what the agent needs and nothing that could carry a prompt (ruling B16).
func TestStderrAllowList(t *testing.T) {
	l := Pinned()
	for _, line := range []string{
		"0.00.388.465 I srv  server_on_li: listening on http://127.0.0.1:63450",
		"0.00.388.457 I srv  server_run_i: model loaded",
		"ggml_cuda_init: found 1 CUDA devices",
		"load_tensors: offloaded 33/33 layers to GPU",
		"E llama_model_load: error loading model: out of memory",
	} {
		if !l.Allowed(line) {
			t.Errorf("dropped a line the agent needs: %q", line)
		}
	}
	for _, line := range []string{
		"0.00.549.406 I slot launch_slot_: id  0 | task 0 | processing task, is_child = 0",
		"0.00.579.655 W srv          stop: cancel task, id_task = 0",
		"slot update_slots: prompt: SENTINEL-PROMPT-1234 tell me a secret",
		"srv  log_server_r: request: POST /v1/chat/completions 127.0.0.1 200",
		"listening on http://127.0.0.1:1234 and the user said hello",
		"",
	} {
		if l.Allowed(line) {
			t.Errorf("a line that must be suppressed got through: %q", line)
		}
	}
}

func TestParseRejects(t *testing.T) {
	archs := "# Architectures of llama.cpp b12125\nllama\n"
	good := `{"llama_tag":"b12125","upstream":"https://github.com/ggml-org/llama.cpp/releases/download/b12125/","archives":[{"name":"a.zip","sha256":"` + strings.Repeat("a", 64) + `"}],"stderr_allow":["x"]}`
	if _, err := Parse([]byte(good), archs); err != nil {
		t.Fatal(err)
	}
	for name, lock := range map[string]string{
		"bad tag":         strings.Replace(good, `"b12125"`, `"latest"`, 1),
		"foreign source":  strings.Replace(good, "ggml-org/llama.cpp", "evil/llama.cpp", 1),
		"another tag url": strings.Replace(good, "download/b12125/", "download/b1/", 1),
		"no archives":     strings.Replace(good, `[{"name":"a.zip","sha256":"`+strings.Repeat("a", 64)+`"}]`, `[]`, 1),
		"short hash":      strings.Replace(good, strings.Repeat("a", 64), "abc", 1),
		"path in name":    strings.Replace(good, `"a.zip"`, `"../a.zip"`, 1),
		"bad pattern":     strings.Replace(good, `["x"]`, `["("]`, 1),
		"not json":        "{",
	} {
		if _, err := Parse([]byte(lock), archs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(good), "# Architectures of llama.cpp b99999\nllama\n"); err == nil {
		t.Error("an architecture list of another tag was accepted")
	}
	if _, err := Parse([]byte(good), "# b12125\n"); err == nil {
		t.Error("an empty architecture list was accepted")
	}
}
