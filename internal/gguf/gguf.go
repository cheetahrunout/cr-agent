// Package gguf reads the header of a GGUF file: the key/values and the tensor table, never the weights.
// It serves cmd inspect (a Range read from Hugging Face) and the pre-load check of a local file.
// It is a sanity filter with hard caps (ruling B6), not a second llama.cpp parser: lengths are checked
// before anything is allocated, large arrays are skipped, and the caller bounds bytes and time.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Caps (ruling B6: length caps before allocation).
const (
	MaxHeaderBytes = 32 << 20 // what inspect may read
	maxKVs         = 4096
	maxTensors     = 200_000
	maxKeyLen      = 1024
	maxKeptString  = 64 << 10 // longer strings are skipped, only their length is kept
	maxArrayLen    = 50_000_000
	maxDims        = 4
)

// ErrTruncated means the byte budget ended before the header did.
var ErrTruncated = errors.New("gguf: header larger than the read budget")

// Tensor is one row of the tensor table.
type Tensor struct {
	Name   string
	Dims   []uint64
	Type   uint32
	Offset uint64 // relative to the start of the data section
}

// Header is what the agent keeps of a GGUF header.
type Header struct {
	Version     uint32
	TensorCount uint64
	KVCount     uint64

	Arch            string
	Name            string
	FileType        int64 // general.file_type: the quantisation enum
	BlockCount      int64
	ContextLength   int64
	EmbeddingLength int64
	HeadCount       int64
	HeadCountKV     int64
	KeyLength       int64
	ValueLength     int64
	SlidingWindow   int64
	NVocab          int64
	ExpertCount     int64
	ExpertUsedCount int64
	SSMState        int64
	SplitCount      int64 // 1 when the file is not part of a split set
	SplitNo         int64
	ParameterCount  int64 // general.parameter_count, else the sum of tensor elements
	HasChatTemplate bool
	Alignment       uint64

	Tensors    []Tensor
	DataOffset uint64 // absolute offset of the data section
	HeaderSize uint64 // bytes read up to the end of the tensor table
}

type reader struct {
	r        *bufio.Reader
	n        uint64
	limit    uint64
	deadline time.Time
}

func (r *reader) full(b []byte) error {
	if r.n+uint64(len(b)) > r.limit {
		return ErrTruncated
	}
	if _, err := io.ReadFull(r.r, b); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrTruncated
		}
		return err
	}
	r.n += uint64(len(b))
	return nil
}

func (r *reader) skip(n uint64) error {
	if n > r.limit || r.n+n > r.limit {
		return ErrTruncated
	}
	got, err := io.CopyN(io.Discard, r.r, int64(n))
	r.n += uint64(got)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return ErrTruncated
		}
		return err
	}
	return nil
}

func (r *reader) u32() (uint32, error) {
	var b [4]byte
	err := r.full(b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}

func (r *reader) u64() (uint64, error) {
	var b [8]byte
	err := r.full(b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}

// str reads a string of at most max bytes; a longer one is skipped and reported by length only.
func (r *reader) str(max uint64) (s string, length uint64, err error) {
	length, err = r.u64()
	if err != nil {
		return "", 0, err
	}
	if length > r.limit {
		return "", 0, errors.New("gguf: string longer than the file")
	}
	if length > max {
		return "", length, r.skip(length)
	}
	b := make([]byte, length)
	if err := r.full(b); err != nil {
		return "", 0, err
	}
	return string(b), length, nil
}

// GGUF value types.
const (
	tU8 = iota
	tI8
	tU16
	tI16
	tU32
	tI32
	tF32
	tBool
	tString
	tArray
	tU64
	tI64
	tF64
)

func scalarSize(t uint32) uint64 {
	switch t {
	case tU8, tI8, tBool:
		return 1
	case tU16, tI16:
		return 2
	case tU32, tI32, tF32:
		return 4
	case tU64, tI64, tF64:
		return 8
	}
	return 0
}

// value is a decoded KV: an integer, a kept string, or an array summary.
type value struct {
	kind   uint32
	i      int64
	s      string
	strLen uint64
	arrLen uint64
	arrMax int64 // the largest element of a small integer array (per-layer head counts)
}

func (r *reader) scalar(t uint32) (int64, error) {
	var b [8]byte
	n := scalarSize(t)
	if n == 0 {
		return 0, fmt.Errorf("gguf: unknown value type %d", t)
	}
	if err := r.full(b[:n]); err != nil {
		return 0, err
	}
	switch t {
	case tU8, tBool:
		return int64(b[0]), nil
	case tI8:
		return int64(int8(b[0])), nil
	case tU16:
		return int64(binary.LittleEndian.Uint16(b[:])), nil
	case tI16:
		return int64(int16(binary.LittleEndian.Uint16(b[:]))), nil
	case tU32:
		return int64(binary.LittleEndian.Uint32(b[:])), nil
	case tI32:
		return int64(int32(binary.LittleEndian.Uint32(b[:]))), nil
	case tU64:
		u := binary.LittleEndian.Uint64(b[:])
		if u > math.MaxInt64 {
			return math.MaxInt64, nil
		}
		return int64(u), nil
	case tI64:
		return int64(binary.LittleEndian.Uint64(b[:])), nil
	case tF32:
		return int64(math.Float32frombits(binary.LittleEndian.Uint32(b[:]))), nil
	default: // tF64
		return int64(math.Float64frombits(binary.LittleEndian.Uint64(b[:]))), nil
	}
}

func (r *reader) value(t uint32, depth int) (value, error) {
	v := value{kind: t}
	switch t {
	case tString:
		s, n, err := r.str(maxKeptString)
		v.s, v.strLen = s, n
		return v, err
	case tArray:
		if depth > 0 {
			return v, errors.New("gguf: nested arrays are not accepted")
		}
		et, err := r.u32()
		if err != nil {
			return v, err
		}
		n, err := r.u64()
		if err != nil {
			return v, err
		}
		if n > maxArrayLen {
			return v, errors.New("gguf: array too long")
		}
		v.arrLen = n
		switch {
		case et == tString:
			for i := uint64(0); i < n; i++ {
				if _, _, err := r.str(0); err != nil { // skipped, never kept
					return v, err
				}
				if i&0xfff == 0 && r.late() {
					return v, errTime
				}
			}
		case scalarSize(et) == 0:
			return v, fmt.Errorf("gguf: unknown array element type %d", et)
		case n <= 4096 && et != tF32 && et != tF64:
			for i := uint64(0); i < n; i++ {
				x, err := r.scalar(et)
				if err != nil {
					return v, err
				}
				if x > v.arrMax {
					v.arrMax = x
				}
			}
		default:
			return v, r.skip(n * scalarSize(et))
		}
		return v, nil
	default:
		x, err := r.scalar(t)
		v.i = x
		return v, err
	}
}

var errTime = errors.New("gguf: parse budget exceeded")

func (r *reader) late() bool { return !r.deadline.IsZero() && time.Now().After(r.deadline) }

// Parse reads a header from src, reading at most limit bytes and stopping at deadline (zero = none).
func Parse(src io.Reader, limit uint64, deadline time.Time) (*Header, error) {
	r := &reader{r: bufio.NewReaderSize(src, 256<<10), limit: limit, deadline: deadline}
	var magic [4]byte
	if err := r.full(magic[:]); err != nil {
		return nil, err
	}
	if string(magic[:]) != "GGUF" {
		return nil, errors.New("gguf: not a GGUF file (bad magic)")
	}
	h := &Header{SplitCount: 1, Alignment: 32}
	var err error
	if h.Version, err = r.u32(); err != nil {
		return nil, err
	}
	if h.Version != 3 {
		return nil, fmt.Errorf("gguf: version %d is not supported (want 3)", h.Version)
	}
	if h.TensorCount, err = r.u64(); err != nil {
		return nil, err
	}
	if h.KVCount, err = r.u64(); err != nil {
		return nil, err
	}
	if h.TensorCount > maxTensors || h.KVCount > maxKVs {
		return nil, errors.New("gguf: tensor or key count over the cap")
	}

	kv := make(map[string]value, h.KVCount)
	for i := uint64(0); i < h.KVCount; i++ {
		key, klen, err := r.str(maxKeyLen)
		if err != nil {
			return nil, err
		}
		if klen == 0 || klen > maxKeyLen {
			return nil, errors.New("gguf: bad key length")
		}
		t, err := r.u32()
		if err != nil {
			return nil, err
		}
		v, err := r.value(t, 0)
		if err != nil {
			return nil, err
		}
		kv[key] = v
		if r.late() {
			return nil, errTime
		}
	}

	h.Tensors = make([]Tensor, 0, min(h.TensorCount, 4096))
	var elements int64
	for i := uint64(0); i < h.TensorCount; i++ {
		name, nlen, err := r.str(maxKeyLen)
		if err != nil {
			return nil, err
		}
		if nlen == 0 || nlen > maxKeyLen {
			return nil, errors.New("gguf: bad tensor name length")
		}
		nd, err := r.u32()
		if err != nil {
			return nil, err
		}
		if nd > maxDims {
			return nil, errors.New("gguf: tensor with too many dimensions")
		}
		t := Tensor{Name: name, Dims: make([]uint64, nd)}
		n := uint64(1)
		for d := range t.Dims {
			if t.Dims[d], err = r.u64(); err != nil {
				return nil, err
			}
			if t.Dims[d] == 0 || n > math.MaxInt64/t.Dims[d] {
				return nil, errors.New("gguf: tensor dimensions overflow")
			}
			n *= t.Dims[d]
		}
		if t.Type, err = r.u32(); err != nil {
			return nil, err
		}
		if t.Offset, err = r.u64(); err != nil {
			return nil, err
		}
		if elements > math.MaxInt64-int64(n) {
			return nil, errors.New("gguf: element count overflow")
		}
		elements += int64(n)
		h.Tensors = append(h.Tensors, t)
		if i&0x3ff == 0 && r.late() {
			return nil, errTime
		}
	}
	h.HeaderSize = r.n

	str := func(k string) string { return kv[k].s }
	num := func(k string) int64 {
		v, ok := kv[k]
		if !ok {
			return 0
		}
		if v.kind == tArray {
			return v.arrMax
		}
		return v.i
	}
	h.Arch = str("general.architecture")
	h.Name = str("general.name")
	h.FileType = num("general.file_type")
	if a := num("general.alignment"); a > 0 {
		if a > 1<<20 || a&(a-1) != 0 {
			return nil, errors.New("gguf: bad alignment")
		}
		h.Alignment = uint64(a)
	}
	a := h.Arch + "."
	h.BlockCount = num(a + "block_count")
	h.ContextLength = num(a + "context_length")
	h.EmbeddingLength = num(a + "embedding_length")
	h.HeadCount = num(a + "attention.head_count")
	h.HeadCountKV = num(a + "attention.head_count_kv")
	h.KeyLength = num(a + "attention.key_length")
	h.ValueLength = num(a + "attention.value_length")
	h.SlidingWindow = num(a + "attention.sliding_window")
	h.ExpertCount = num(a + "expert_count")
	h.ExpertUsedCount = num(a + "expert_used_count")
	h.SSMState = num(a + "ssm.state_size")
	if v, ok := kv["tokenizer.ggml.tokens"]; ok && v.kind == tArray {
		h.NVocab = int64(v.arrLen)
	} else {
		h.NVocab = num(a + "vocab_size")
	}
	if n := num("split.count"); n > 1 {
		h.SplitCount = n
		h.SplitNo = num("split.no")
	}
	if v, ok := kv["tokenizer.chat_template"]; ok && v.kind == tString && v.strLen > 0 {
		h.HasChatTemplate = true
	}
	if h.ParameterCount = num("general.parameter_count"); h.ParameterCount <= 0 {
		h.ParameterCount = elements
	}
	h.DataOffset = (h.HeaderSize + h.Alignment - 1) / h.Alignment * h.Alignment
	return h, nil
}

// Sizes are the tensor byte totals the fit check needs (§12.2).
type Sizes struct {
	TokenEmbd         int64
	Output            int64
	PerLayerRepeating int64 // bytes of blk.N.* tensors divided by block_count
	Total             int64
}

// TensorSizes derives each tensor's size from the gap to the next offset, so it needs no table of
// quantisation types; the last tensor ends at the end of the file. fileSize 0 means unknown: the last
// tensor then counts as its raw element size at 1 byte per element at most (a floor, flagged by ok=false).
func (h *Header) TensorSizes(fileSize uint64) (s Sizes, ok bool) {
	if len(h.Tensors) == 0 {
		return s, true
	}
	idx := make([]int, len(h.Tensors))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return h.Tensors[idx[a]].Offset < h.Tensors[idx[b]].Offset })
	ok = true
	var blocks int64
	for k, i := range idx {
		t := h.Tensors[i]
		var size uint64
		if k+1 < len(idx) {
			size = h.Tensors[idx[k+1]].Offset - t.Offset
		} else if end := h.DataOffset + t.Offset; fileSize > end {
			size = fileSize - end
		} else {
			ok = false
		}
		if size > math.MaxInt64/2 {
			return Sizes{}, false
		}
		n := int64(size)
		s.Total += n
		switch {
		case t.Name == "token_embd.weight":
			s.TokenEmbd = n
		case t.Name == "output.weight":
			s.Output = n
		case strings.HasPrefix(t.Name, "blk."):
			blocks += n
		}
	}
	if h.BlockCount > 0 {
		s.PerLayerRepeating = blocks / h.BlockCount
	}
	return s, ok
}

// CheckLayout is the pre-load sanity check on a whole local file: every tensor starts on the
// alignment and lies inside the file.
func (h *Header) CheckLayout(fileSize uint64) error {
	if h.DataOffset > fileSize {
		return errors.New("gguf: header runs past the end of the file")
	}
	data := fileSize - h.DataOffset
	for _, t := range h.Tensors {
		if t.Offset%h.Alignment != 0 {
			return fmt.Errorf("gguf: tensor %q is not aligned", t.Name)
		}
		if t.Offset >= data && !(t.Offset == data && data == 0) {
			return fmt.Errorf("gguf: tensor %q starts past the end of the file", t.Name)
		}
	}
	return nil
}
