package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf/gguftest"
)

func TestParseTiny(t *testing.T) {
	raw := gguftest.Build(gguftest.Default())
	h, err := Parse(bytes.NewReader(raw), MaxHeaderBytes, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Arch != "llama" || h.BlockCount != 2 || h.ContextLength != 8192 || h.EmbeddingLength != 64 ||
		h.HeadCount != 4 || h.HeadCountKV != 2 || h.NVocab != 100 || h.FileType != 15 || !h.HasChatTemplate || h.SplitCount != 1 {
		t.Fatalf("header %+v", h)
	}
	if len(h.Tensors) != 6 || h.DataOffset%32 != 0 || h.ParameterCount != 6*8*8 {
		t.Fatalf("tensors %d data offset %d params %d", len(h.Tensors), h.DataOffset, h.ParameterCount)
	}
	if err := h.CheckLayout(uint64(len(raw))); err != nil {
		t.Fatal(err)
	}
	s, ok := h.TensorSizes(uint64(len(raw)))
	if !ok || s.TokenEmbd != 64 || s.Output != 64 || s.PerLayerRepeating != 128 || s.Total != 6*64 {
		t.Fatalf("sizes %+v ok=%v", s, ok)
	}
	// The header alone is enough: a Range read never needs the weights.
	h2, err := Parse(bytes.NewReader(raw[:h.HeaderSize]), MaxHeaderBytes, time.Time{})
	if err != nil || h2.Arch != "llama" {
		t.Fatalf("header-only parse: %v", err)
	}
}

func TestParseSplit(t *testing.T) {
	f := gguftest.Default()
	f.SplitCount, f.Template = 3, false
	h, err := Parse(bytes.NewReader(gguftest.Build(f)), MaxHeaderBytes, time.Time{})
	if err != nil || h.SplitCount != 3 || h.HasChatTemplate {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestParseRejects(t *testing.T) {
	good := gguftest.Build(gguftest.Default())
	mut := func(fn func(b []byte) []byte) []byte { return fn(append([]byte(nil), good...)) }
	u64 := func(b []byte, off int, v uint64) []byte { binary.LittleEndian.PutUint64(b[off:], v); return b }
	cases := map[string][]byte{
		"bad magic":         mut(func(b []byte) []byte { b[0] = 'X'; return b }),
		"version 2":         mut(func(b []byte) []byte { b[4] = 2; return b }),
		"tensor count huge": mut(func(b []byte) []byte { return u64(b, 8, 1<<40) }),
		"kv count huge":     mut(func(b []byte) []byte { return u64(b, 16, 1<<40) }),
		"key length huge":   mut(func(b []byte) []byte { return u64(b, 24, 1<<60) }),
		"empty":             {},
		"magic only":        []byte("GGUF"),
	}
	for name, raw := range cases {
		if _, err := Parse(bytes.NewReader(raw), MaxHeaderBytes, time.Time{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A header cut short is "truncated", so inspect can say the budget was too small.
	if _, err := Parse(bytes.NewReader(good[:200]), MaxHeaderBytes, time.Time{}); !errors.Is(err, ErrTruncated) {
		t.Errorf("cut file: %v", err)
	}
	if _, err := Parse(bytes.NewReader(good), 100, time.Time{}); !errors.Is(err, ErrTruncated) {
		t.Errorf("tiny budget: %v", err)
	}
	// The time budget stops a slow parse.
	if _, err := Parse(bytes.NewReader(good), MaxHeaderBytes, time.Now().Add(-time.Second)); err == nil {
		t.Error("expired deadline ignored")
	}
	// A tensor that points past the end of the file fails the layout check.
	h, _ := Parse(bytes.NewReader(good), MaxHeaderBytes, time.Time{})
	if err := h.CheckLayout(h.DataOffset + 10); err == nil {
		t.Error("layout check passed a short file")
	}
}

// A huge string array must not be allocated: the parser skips it inside the byte budget.
func TestHugeArrayIsNotAllocated(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0))
	binary.Write(&b, binary.LittleEndian, uint64(1))
	binary.Write(&b, binary.LittleEndian, uint64(1))
	b.WriteString("k")
	binary.Write(&b, binary.LittleEndian, uint32(9))
	binary.Write(&b, binary.LittleEndian, uint32(4))
	binary.Write(&b, binary.LittleEndian, uint64(40_000_000)) // 160 MB of u32 claimed, none present
	if _, err := Parse(&b, MaxHeaderBytes, time.Time{}); !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v", err)
	}
}

// The real model used for the manual checks, when it is on this PC.
func TestParseRealModel(t *testing.T) {
	path := os.Getenv("CRAGENT_TEST_GGUF")
	if path == "" {
		path = `C:\Users\Admin\Desktop\AI\models\smollm2-360m-q8_0.gguf`
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip("no real model on this PC")
	}
	defer f.Close()
	st, _ := f.Stat()
	t0 := time.Now()
	h, err := Parse(f, MaxHeaderBytes, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.CheckLayout(uint64(st.Size())); err != nil {
		t.Fatal(err)
	}
	s, ok := h.TensorSizes(uint64(st.Size()))
	t.Logf("%s: arch=%s blocks=%d ctx=%d embd=%d heads=%d/%d vocab=%d file_type=%d params=%d template=%v header=%d bytes, tensors %d MB (ok=%v), parsed in %v",
		h.Name, h.Arch, h.BlockCount, h.ContextLength, h.EmbeddingLength, h.HeadCount, h.HeadCountKV, h.NVocab, h.FileType,
		h.ParameterCount, h.HasChatTemplate, h.HeaderSize, s.Total>>20, ok, time.Since(t0))
	if h.Arch == "" || h.BlockCount <= 0 || h.ContextLength <= 0 || h.NVocab <= 0 || !ok || s.Total <= 0 || s.Total > st.Size() {
		t.Fatalf("implausible header %+v", s)
	}
}
