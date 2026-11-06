// Package fit estimates how much GPU memory a load will take, for the agent's pre-load check
// (ruling B6): estimate(gpu_layers, ctx) must be at most the free VRAM minus 0.5 GiB, otherwise the
// load is refused with cmd.err{insufficient_vram, need, free}. Nothing is adjusted automatically.
//
// The formula is §12.2's, from the model's own GGUF header: the weights of the layers that go to the
// GPU (by tensor sizes, not file size), the KV cache for those layers, and a compute buffer.
package fit

import (
	"strconv"
	"strings"

	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Reserve is what must stay free beside the estimate (the CUDA context and driver overhead).
const Reserve = 512 << 20

// Estimate is the breakdown of a load's GPU memory, in bytes.
type Estimate struct {
	Weights   int64
	KV        int64
	Compute   int64
	GPUBlocks int  // repeating blocks placed on the GPU
	Output    bool // the output layer is on the GPU
	// Uncertain means the KV cache rests on the unknown-layout fallback: the header lacks what it takes.
	Uncertain bool
}

// Total is the sum.
func (e Estimate) Total() int64 { return e.Weights + e.KV + e.Compute }

// kvBytes is the size of one KV cache element.
func kvBytes(kvType string) float64 {
	if kvType == "q8_0" {
		return 1.0625
	}
	return 2 // f16
}

// blockOf returns the block index of a tensor named blk.<i>.<rest>.
func blockOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "blk.")
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(num)
	return i, err == nil && i >= 0
}

// For computes the estimate. sizes are the tensor byte sizes by name (gguf.Header.TensorBytes).
//
// llama.cpp puts the output layer and the last gpu_layers-1 blocks on the GPU: block i is offloaded
// when i >= n_layer + 1 - gpu_layers. With n_cpu_moe, the expert weights of the first n_cpu_moe
// blocks stay in RAM.
func For(h *gguf.Header, sizes map[string]int64, s protocol.Settings) Estimate {
	var e Estimate
	nLayer := int(h.BlockCount)
	n := nLayer + 1
	if !s.GPULayers.All {
		n = min(s.GPULayers.N, nLayer+1)
	}
	if n <= 0 || nLayer <= 0 {
		return e
	}
	start := nLayer + 1 - n
	e.GPUBlocks, e.Output = nLayer-start, true
	cpuMoE := 0
	if s.NCPUMoE != nil {
		cpuMoE = *s.NCPUMoE
	}
	tied := true // no output.weight: the token embedding doubles as the output matrix
	for name, size := range sizes {
		if i, ok := blockOf(name); ok {
			if i >= start && !(i < cpuMoE && strings.Contains(name, "_exps")) {
				e.Weights += size
			}
			continue
		}
		if strings.HasPrefix(name, "output") { // output.weight, output_norm.*
			e.Weights += size
			if name == "output.weight" {
				tied = false
			}
		}
	}
	if tied {
		e.Weights += sizes["token_embd.weight"]
	}

	e.KV, e.Uncertain = kvCache(h, s, start)

	// Compute buffer ≈ 256 MiB + 512 × n_vocab × 4 B.
	e.Compute = 256<<20 + 512*h.NVocab*4
	return e
}

// kvCache is the KV cache of the blocks from start up: n_head_kv(il) × (key_length + value_length) × bytes
// per value × tokens, per block, as llama_kv_cache sizes it. uncertain is true when it rests on the
// unknown-layout fallback, every layer full-width plus 20 % (§12.2).
func kvCache(h *gguf.Header, s protocol.Settings, start int) (bytes int64, uncertain bool) {
	switch {
	case noKVCache[h.Arch]:
		return 0, false
	}
	nLayer := int(h.BlockCount)
	headDim := h.KeyLength
	if headDim <= 0 && h.HeadCount > 0 {
		headDim = h.EmbeddingLength / h.HeadCount // llama.cpp's default for a missing key_length
	}
	valueDim := h.ValueLength
	if valueDim <= 0 {
		valueDim = headDim
	}
	defaultHeads := h.HeadCountKV
	if defaultHeads <= 0 {
		defaultHeads = h.HeadCount // llama.cpp's default for a missing head_count_kv
	}
	if len(h.HeadCountKVByBlock) == 0 && defaultHeads <= 0 || headDim <= 0 || valueDim <= 0 {
		perBlock := float64(2*h.EmbeddingLength) * kvBytes(s.KVType) * float64(s.Ctx) * 1.2
		return int64(perBlock * float64(nLayer-start)), true
	}
	var total float64
	for i := start; i < nLayer; i++ {
		if isRecurrent(h, i) {
			continue
		}
		heads := defaultHeads
		if i < len(h.HeadCountKVByBlock) {
			heads = h.HeadCountKVByBlock[i]
		}
		tokens := int64(s.Ctx)
		if h.SlidingWindow > 0 && isSWA(h, i) {
			// llama_kv_cache_iswa: n_swa + n_ubatch cells, padded, for the windowed layers.
			tokens = min(tokens, (h.SlidingWindow+engineUbatch+255)/256*256)
		}
		total += float64(heads*(headDim+valueDim)) * kvBytes(s.KVType) * float64(tokens)
	}
	return int64(total), false
}

// engineUbatch is llama.cpp's default n_ubatch, which the agent does not change.
const engineUbatch = 512

// noKVCache is every architecture whose memory does not grow with the context, from llama.cpp b12125:
// llama_model::create_memory returns no memory for the first group, and llm_arch_is_recurrent names the second.
var noKVCache = map[string]bool{
	"bert": true, "jina-bert-v2": true, "jina-bert-v3": true, "nomic-bert": true, "nomic-bert-moe": true, "neo-bert": true,
	"eurobert": true, "wavtokenizer-dec": true, "modern-bert": true, "gemma-embedding": true, "gemma-embedding2": true,
	"dream": true, "llada": true, "llada-moe": true, "rnd1": true, "clef": true,
	"mamba": true, "mamba2": true, "rwkv6": true, "rwkv6qwen2": true, "rwkv7": true, "arwkv7": true,
}

// fullAttentionDefaults is the default full_attention_interval of the hybrid models that have one, from
// llama.cpp b12125's src/models/: every Nth block has attention and a KV cache, the others linear attention.
var fullAttentionDefaults = map[string]int64{"qwen35": 4, "qwen35moe": 4, "qwen3next": 4, "qwen4exp": 4, "minimax-01": 8}

// isRecurrent reports a block with linear attention, which holds no KV cache: the file's own list, else its
// interval, else the architecture's. A hybrid model that marks such blocks by head_count_kv 0 needs nothing here.
func isRecurrent(h *gguf.Header, i int) bool {
	if len(h.RecurrentLayers) > 0 {
		return i < len(h.RecurrentLayers) && h.RecurrentLayers[i] != 0
	}
	every := h.FullAttentionInterval
	if every <= 0 {
		every = fullAttentionDefaults[h.Arch]
	}
	return every > 0 && (i+1)%int(every) != 0
}

// swaLayout is the default of load_swa_pattern(ml, every, denseFirst) in llama.cpp b12125's src/models/.
type swaLayout struct {
	every      int
	denseFirst bool
}

var swaDefaults = map[string]swaLayout{
	"afmoe": {4, false}, "cohere2": {4, false}, "cohere2moe": {4, true}, "exaone-moe": {4, false}, "exaone4": {4, false},
	"gemma2": {2, false}, "gemma3": {6, false}, "gemma3n": {5, false}, "laguna": {4, true}, "limite": {4, false},
	"llama4": {4, false}, "mellum": {4, false}, "muse-glimmer": {4, false}, "olmo2": {4, false}, "gpt-oss": {2, false},
	"plamo3": {8, false}, "smallthinker": {4, true},
}

// isSWA reports whether a block sees only the sliding window, by the rule of set_swa_pattern. When neither
// the file nor the table says, the block counts as full: better a little over than far under.
func isSWA(h *gguf.Header, i int) bool {
	if len(h.SWAPattern) > 0 {
		return i < len(h.SWAPattern) && h.SWAPattern[i] != 0
	}
	layout, known := swaDefaults[h.Arch]
	if h.SWAEvery > 0 {
		layout.every, known = int(h.SWAEvery), true
	}
	if !known || layout.every <= 0 {
		return false
	}
	if layout.denseFirst {
		return i%layout.every != 0
	}
	return i%layout.every < layout.every-1
}

// Check is the pre-load rule. It returns the megabytes needed and free when the load must be refused.
func Check(e Estimate, freeBytes int64) (needMB, freeMB int64, ok bool) {
	need := e.Total()
	if need == 0 {
		return 0, freeBytes >> 20, true // nothing goes to the GPU
	}
	return (need + Reserve) >> 20, freeBytes >> 20, need <= freeBytes-Reserve
}
