// Package gguftest builds small, valid GGUF files for tests.
package gguftest

import (
	"bytes"
	"encoding/binary"
)

// File describes a tiny model.
type File struct {
	Arch       string
	Blocks     int
	Ctx        int
	Vocab      int
	Template   bool
	SplitCount int
	DataBytes  int  // bytes of fake weights per tensor
	NoHeads    bool // no attention head counts in the header: the fit check's unknown layout
}

// Default is a 2-block "llama" model.
func Default() File {
	return File{Arch: "llama", Blocks: 2, Ctx: 8192, Vocab: 100, Template: true, DataBytes: 64}
}

type w struct{ bytes.Buffer }

func (b *w) u32(v uint32) { binary.Write(b, binary.LittleEndian, v) }
func (b *w) u64(v uint64) { binary.Write(b, binary.LittleEndian, v) }
func (b *w) str(s string) { b.u64(uint64(len(s))); b.WriteString(s) }
func (b *w) kvU32(k string, v uint32) {
	b.str(k)
	b.u32(4)
	b.u32(v)
}
func (b *w) kvStr(k, v string) {
	b.str(k)
	b.u32(8)
	b.str(v)
}

// Build returns the file's bytes.
func Build(f File) []byte {
	names := []string{"token_embd.weight"}
	for i := 0; i < f.Blocks; i++ {
		names = append(names, "blk."+string(rune('0'+i))+".attn_q.weight", "blk."+string(rune('0'+i))+".ffn_up.weight")
	}
	names = append(names, "output.weight")

	var b w
	b.WriteString("GGUF")
	b.u32(3)
	b.u64(uint64(len(names)))
	kvs := 9
	if f.NoHeads {
		kvs -= 2
	}
	if f.Template {
		kvs++
	}
	if f.SplitCount > 1 {
		kvs += 2
	}
	b.u64(uint64(kvs))
	b.kvStr("general.architecture", f.Arch)
	b.kvStr("general.name", "tiny test model")
	b.kvU32("general.file_type", 15) // Q4_K_M
	b.kvU32(f.Arch+".block_count", uint32(f.Blocks))
	b.kvU32(f.Arch+".context_length", uint32(f.Ctx))
	b.kvU32(f.Arch+".embedding_length", 64)
	if !f.NoHeads {
		b.kvU32(f.Arch+".attention.head_count", 4)
		b.kvU32(f.Arch+".attention.head_count_kv", 2)
	}
	b.str("tokenizer.ggml.tokens")
	b.u32(9) // array
	b.u32(8) // of strings
	b.u64(uint64(f.Vocab))
	for i := 0; i < f.Vocab; i++ {
		b.str("tok")
	}
	if f.Template {
		b.kvStr("tokenizer.chat_template", "{{ messages }}")
	}
	if f.SplitCount > 1 {
		b.str("split.count")
		b.u32(2) // u16
		binary.Write(&b, binary.LittleEndian, uint16(f.SplitCount))
		b.str("split.no")
		b.u32(2)
		binary.Write(&b, binary.LittleEndian, uint16(0))
	}
	data := (f.DataBytes + 31) / 32 * 32
	for i, n := range names {
		b.str(n)
		b.u32(2)
		b.u64(8)
		b.u64(uint64(f.DataBytes / 8))
		b.u32(0) // F32
		b.u64(uint64(i * data))
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	for i := range names {
		chunk := bytes.Repeat([]byte{byte(i + 1)}, data)
		b.Write(chunk)
	}
	return b.Bytes()
}
