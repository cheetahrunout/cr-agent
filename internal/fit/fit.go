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

	// KV cache: 2 × n_head_kv × head_dim × bytes per token, for each block on the GPU.
	heads := h.HeadCountKV
	if heads <= 0 {
		heads = h.HeadCount
	}
	headDim := h.KeyLength
	if headDim <= 0 && h.HeadCount > 0 {
		headDim = h.EmbeddingLength / h.HeadCount
	}
	tokens := int64(s.Ctx)
	if h.SlidingWindow > 0 && h.SlidingWindow < tokens {
		tokens = h.SlidingWindow
	}
	perBlock := float64(2*heads*headDim) * kvBytes(s.KVType) * float64(tokens)
	if heads <= 0 || headDim <= 0 {
		// Unknown layout: every layer with a full-width cache, plus 20 % (§12.2).
		perBlock = float64(2*h.EmbeddingLength) * kvBytes(s.KVType) * float64(tokens) * 1.2
	}
	e.KV = int64(perBlock * float64(e.GPUBlocks))

	// Compute buffer ≈ 256 MiB + 512 × n_vocab × 4 B.
	e.Compute = 256<<20 + 512*h.NVocab*4
	return e
}

// Check is the pre-load rule. It returns the megabytes needed and free when the load must be refused.
func Check(e Estimate, freeBytes int64) (needMB, freeMB int64, ok bool) {
	need := e.Total()
	if need == 0 {
		return 0, freeBytes >> 20, true // nothing goes to the GPU
	}
	return (need + Reserve) >> 20, freeBytes >> 20, need <= freeBytes-Reserve
}
