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
// The lines are real output of b12125. A line is allowed by the function that wrote it, never by
// the words in it: the engine's request-time lines quote the request.
func TestStderrAllowList(t *testing.T) {
	l := Pinned()
	for _, line := range []string{
		"0.00.388.465 I srv  server_on_li: listening on http://127.0.0.1:63450",
		"0.00.388.457 I srv  server_run_i: model loaded",
		"0.00.102.102 E llama_model_load: error loading model: tensor 'token_embd.weight' data is not within the file bounds, model is corrupted or incomplete",
		"0.00.102.107 E llama_model_load_from_file_impl: failed to load model",
		"0.00.102.111 E cmn  common_init_: failed to load model 'C:\\m\\x.gguf'",
		"0.00.102.113 E srv    load_model: failed to load model, 'C:\\m\\x.gguf'",
		"0.00.103.439 E srv  server_run_i: exiting due to model loading error",
		"0.00.098.187 E gguf_init_from_reader: invalid magic characters: 'not ', expected 'GGUF'",
		"0.00.459.139 E ggml_backend_cuda_buffer_type_alloc_buffer: allocating 781250.00 MiB on device 0: cudaMalloc failed: out of memory",
		"0.00.459.147 E ggml_backend_buft_alloc_buffer_n_default: failed to allocate CUDA0 buffer of size 819200000000",
		"0.00.497.044 E llama_init_from_model: failed to initialize the context: failed to allocate buffer for kv cache",
		"CUDA error: out of memory",
	} {
		if !l.Allowed(line) {
			t.Errorf("dropped a line the agent needs: %q", line)
		}
	}
	const trap = " SENTINEL CUDA ggml_cuda out of memory failed to load error loading model offloaded 1/1 layers to GPU server_run_i: model loaded listening on http://127.0.0.1:1234"
	for _, line := range []string{
		"0.00.549.406 I slot launch_slot_: id  0 | task 0 | processing task, is_child = 0",
		"0.00.579.655 W srv          stop: cancel task, id_task = 0",
		"0.00.086.907 W srv  server_setup: security: no API key is set and CORS allows all origins",
		"0.00.111.415 I srv    load_model: loading model 'C:\\m\\x.gguf'",
		"slot update_slots: prompt:" + trap,
		"srv  log_server_r: request: POST /v1/chat/completions 127.0.0.1 200",
		`0.00.560.441 W srv    operator(): got exception: {"error":{"code":400,"message":"JSON schema error at #: unrecognized type` + trap,
		`0.00.590.404 W srv    operator(): got exception: {"error":{"code":400,"message":"Invalid tool_choice:` + trap,
		"0.00.590.404 E srv    operator(): got exception:" + trap,
		"0.00.590.404 E slot   update_slots:" + trap,
		"0.00.388.465 I srv  server_on_li: listening on http://127.0.0.1:1234 and the user said" + trap,
		"prompt: CUDA error: out of memory" + trap,
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
