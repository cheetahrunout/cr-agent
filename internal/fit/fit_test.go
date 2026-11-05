package fit

import (
	"os"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

func settings(layers protocol.GPULayers, ctx int, kv string) protocol.Settings {
	return protocol.Settings{Ctx: ctx, GPULayers: layers, Threads: 4, KVType: kv, FlashAttn: "on"}
}

// A model shaped like Qwen3-8B: §12.2 says 144 KiB of KV per token, so 4.5 GiB at 32k and f16.
func TestKVMatchesThePlansExample(t *testing.T) {
	h := &gguf.Header{BlockCount: 36, HeadCount: 32, HeadCountKV: 8, KeyLength: 128, EmbeddingLength: 4096, NVocab: 151936}
	e := For(h, map[string]int64{}, settings(protocol.AllLayers(), 32768, "f16"))
	if perToken := e.KV / 32768; perToken != 144<<10 {
		t.Fatalf("KV per token = %d B, want 144 KiB", perToken)
	}
	if got := float64(e.KV) / (1 << 30); got < 4.49 || got > 4.51 {
		t.Fatalf("KV at 32k = %.2f GiB", got)
	}
	q8 := For(h, map[string]int64{}, settings(protocol.AllLayers(), 32768, "q8_0"))
	if ratio := float64(q8.KV) / float64(e.KV); ratio < 0.53 || ratio > 0.54 {
		t.Fatalf("q8_0 / f16 = %.3f, want 1.0625/2", ratio)
	}
	if e.Compute != 256<<20+512*151936*4 {
		t.Fatalf("compute buffer %d", e.Compute)
	}
}

func TestWhichLayersGoToTheGPU(t *testing.T) {
	h := &gguf.Header{BlockCount: 4, HeadCount: 4, HeadCountKV: 2, EmbeddingLength: 64, NVocab: 100}
	sizes := map[string]int64{"token_embd.weight": 1000, "output.weight": 500, "output_norm.weight": 5}
	for i := 0; i < 4; i++ {
		b := "blk." + string(rune('0'+i))
		sizes[b+".attn_q.weight"] = 100
		sizes[b+".ffn_up_exps.weight"] = 300
	}
	cases := []struct {
		layers  protocol.GPULayers
		moe     int
		blocks  int
		weights int64
	}{
		{protocol.Layers(0), 0, 0, 0},                   // nothing on the GPU: no check at all
		{protocol.Layers(1), 0, 0, 505},                 // the output layer comes first
		{protocol.Layers(3), 0, 2, 505 + 2*400},         // then the last two blocks
		{protocol.Layers(5), 0, 4, 505 + 4*400},         // n_layer + 1 = everything
		{protocol.Layers(99), 0, 4, 505 + 4*400},        // more than there is
		{protocol.AllLayers(), 0, 4, 505 + 4*400},       //
		{protocol.AllLayers(), 3, 4, 505 + 4*100 + 300}, // experts of the first three blocks stay in RAM
	}
	for _, c := range cases {
		s := settings(c.layers, 4096, "f16")
		if c.moe > 0 {
			s.NCPUMoE = &c.moe
		}
		e := For(h, sizes, s)
		if e.GPUBlocks != c.blocks || e.Weights != c.weights {
			t.Errorf("layers %+v moe %d: %d blocks, %d weight bytes; want %d, %d", c.layers, c.moe, e.GPUBlocks, e.Weights, c.blocks, c.weights)
		}
		if c.blocks == 0 && !c.layers.All && c.layers.N == 0 && e.Total() != 0 {
			t.Errorf("gpu_layers 0 needs %d bytes of VRAM", e.Total())
		}
	}
	// Tied embeddings (no output.weight): the token embedding is needed on the GPU as the output matrix.
	delete(sizes, "output.weight")
	if e := For(h, sizes, settings(protocol.Layers(1), 4096, "f16")); e.Weights != 1005 {
		t.Errorf("tied output: %d", e.Weights)
	}
}

func TestCheck(t *testing.T) {
	e := Estimate{Weights: 10 << 30, KV: 1 << 30, Compute: 512 << 20}
	if need, free, ok := Check(e, 12<<30); !ok || need != (11<<30+1<<30)>>20 || free != 12<<10 {
		t.Fatalf("fits exactly: need %d free %d ok %v", need, free, ok)
	}
	if need, free, ok := Check(e, 12<<30-1); ok || need <= free-1 {
		t.Fatalf("one byte short: need %d free %d ok %v", need, free, ok)
	}
	if _, _, ok := Check(Estimate{}, 0); !ok {
		t.Fatal("a CPU-only load was refused for VRAM")
	}
}

// Calibration against loads measured on the RTX 5090 with llama.cpp b12125 (docs/m0-findings.md).
func TestAgainstMeasuredLoads(t *testing.T) {
	for _, c := range []struct {
		path       string
		layers     protocol.GPULayers
		ctx        int
		measuredMB int64 // rise of GPU memory in use during the load (includes the CUDA context)
	}{
		{`C:\Users\Admin\Documents\hfdownloader\models\Qwen3.6Q6_K.gguf`, protocol.Layers(20), 8192, 14392},
	} {
		f, err := os.Open(c.path)
		if err != nil {
			t.Skip("the measured model is not on this PC")
		}
		st, _ := f.Stat()
		h, err := gguf.Parse(f, gguf.MaxHeaderBytes, time.Now().Add(2*time.Second))
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		sizes, ok := h.TensorBytes(uint64(st.Size()))
		if !ok {
			t.Fatal("tensor sizes not complete")
		}
		e := For(h, sizes, settings(c.layers, c.ctx, "f16"))
		gotMB := (e.Total() + Reserve) >> 20
		ratio := float64(gotMB) / float64(c.measuredMB)
		t.Logf("%s: %d blocks on GPU; weights %d MB + KV %d MB + compute %d MB + reserve 512 MB = %d MB; measured %d MB (ratio %.2f)",
			h.Name, e.GPUBlocks, e.Weights>>20, e.KV>>20, e.Compute>>20, gotMB, c.measuredMB, ratio)
		if ratio < 0.85 || ratio > 1.15 { // §12.2: the constants are good to ±15 %
			t.Errorf("estimate is off by more than 15 %%")
		}
	}
}
