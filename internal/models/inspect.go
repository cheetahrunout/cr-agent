package models

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/gguf"
	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Inspect limits (ruling B6).
const (
	inspectPerMinute = 10
	inspectFirstRead = 4 << 20             // most headers fit; the rest is fetched only when needed
	inspectMaxRead   = gguf.MaxHeaderBytes // 32 MB
	inspectParseTime = 2 * time.Second
	inspectNetTime   = 90 * time.Second
)

// Facts is what a header read yields. Result() is the protocol's inspectResult; the other fields are
// kept for the agent's own use (the schema has no place for them yet).
type Facts struct {
	Header    *gguf.Header
	Sizes     gguf.Sizes
	FileSize  int64
	FromDisk  bool
	ReadBytes int64
}

// Result shapes the facts as schema.json's inspectResult: only fields the header really has.
func (f *Facts) Result() map[string]any {
	h := f.Header
	kv := map[string]any{}
	put := func(k string, v int64) {
		if v > 0 {
			kv[k] = v
		}
	}
	put("block_count", h.BlockCount)
	put("context_length", h.ContextLength)
	put("embedding_length", h.EmbeddingLength)
	put("head_count", h.HeadCount)
	put("head_count_kv", h.HeadCountKV)
	put("key_length", h.KeyLength)
	put("value_length", h.ValueLength)
	put("sliding_window", h.SlidingWindow)
	put("n_vocab", h.NVocab)
	put("expert_count", h.ExpertCount)
	put("expert_used_count", h.ExpertUsedCount)
	put("ssm_state", h.SSMState)
	if h.FileType >= 0 {
		kv["file_type"] = h.FileType
	}
	return map[string]any{
		"arch": h.Arch,
		"kv":   kv,
		"tensors_bytes": map[string]any{
			"token_embd":          f.Sizes.TokenEmbd,
			"output":              f.Sizes.Output,
			"per_layer_repeating": f.Sizes.PerLayerRepeating,
			"total":               f.Sizes.Total,
		},
		"split_count": h.SplitCount,
	}
}

// Inspect reads a GGUF header without loading the model and without writing anything: from the
// local file when this exact file is already on disk, otherwise by Range from Hugging Face.
// One at a time, at most ten a minute.
func (m *Manager) Inspect(ctx context.Context, a *protocol.InspectArgs) (*Facts, *Error) {
	if !protocol.ValidRepo(a.Repo) || !reCommit.MatchString(a.Commit) || !protocol.ValidGGUFFile(a.File) {
		return nil, errf(protocol.ErrBadArgs, "invalid inspect")
	}
	m.mu.Lock()
	now := time.Now()
	kept := m.recent[:0]
	for _, t := range m.recent {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	m.recent = kept
	if len(m.recent) >= inspectPerMinute {
		m.mu.Unlock()
		return nil, errf(protocol.ErrBusy, "too many inspects in a minute")
	}
	m.recent = append(m.recent, now)
	if reservedRepo(a.Repo) { // never Hugging Face, and only a file the host added
		var e *Entry
		for _, x := range m.index {
			if x.imported() && x.State == StateReady && x.Repo == a.Repo && x.Commit == a.Commit {
				for _, f := range x.Files {
					if f.Path == a.File {
						e = x
					}
				}
			}
		}
		root := m.dir
		m.mu.Unlock()
		m.inspect.Lock()
		defer m.inspect.Unlock()
		if ctx.Err() != nil {
			return nil, errf(protocol.ErrCancelled, "cancelled")
		}
		return m.inspectImported(root, e, a.File)
	}
	local := ""
	for _, e := range m.index {
		if e.State == StateReady && e.Repo == a.Repo && e.Commit == a.Commit {
			for _, f := range e.Files {
				if f.Path == a.File {
					local = e.path(m.dir, f.Path)
				}
			}
		}
	}
	m.mu.Unlock()

	m.inspect.Lock()
	defer m.inspect.Unlock()
	if ctx.Err() != nil {
		return nil, errf(protocol.ErrCancelled, "cancelled")
	}
	if local != "" {
		if f, err := inspectLocal(local); err == nil {
			return f, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, inspectNetTime)
	defer cancel()

	var buf bytes.Buffer
	var total int64 = -1
	read := func(offset, length int64) *Error {
		body, err := m.o.HF.Open(ctx, a.Repo, a.Commit, a.File, offset, length, "")
		if err != nil {
			var se *hf.StatusError
			switch {
			case errors.As(err, &se) && se.Code == 404:
				return errf(protocol.ErrNotFound, "the file is not on Hugging Face at this commit")
			case errors.As(err, &se) && (se.Code == 401 || se.Code == 403):
				return errf(protocol.ErrDenied, "Hugging Face refused access: the repo is gated or private; accept its licence and set a token in the agent")
			}
			return errf(protocol.ErrDownloadFailed, "the header could not be read from Hugging Face")
		}
		defer body.Close()
		if body.Offset != offset {
			return errf(protocol.ErrDownloadFailed, "Hugging Face did not honour the byte range")
		}
		total = body.Total
		if _, err := io.Copy(&buf, io.LimitReader(body, length)); err != nil {
			return errf(protocol.ErrDownloadFailed, "the header read was interrupted")
		}
		return nil
	}
	if e := read(0, inspectFirstRead); e != nil {
		return nil, e
	}
	h, err := gguf.Parse(bytes.NewReader(buf.Bytes()), inspectMaxRead, time.Now().Add(inspectParseTime))
	if errors.Is(err, gguf.ErrTruncated) && int64(buf.Len()) == inspectFirstRead && total > inspectFirstRead {
		if e := read(inspectFirstRead, inspectMaxRead-inspectFirstRead); e != nil {
			return nil, e
		}
		h, err = gguf.Parse(bytes.NewReader(buf.Bytes()), inspectMaxRead, time.Now().Add(inspectParseTime))
	}
	if err != nil {
		if errors.Is(err, gguf.ErrTruncated) {
			return nil, errf(protocol.ErrUnsupported, "the GGUF header is larger than 32 MB")
		}
		return nil, errf(protocol.ErrUnsupported, "not a GGUF file this agent can read")
	}
	f := &Facts{Header: h, FileSize: total, ReadBytes: int64(buf.Len())}
	if total > 0 {
		f.Sizes, _ = h.TensorSizes(uint64(total))
	} else {
		f.Sizes, _ = h.TensorSizes(0)
	}
	return f, nil
}

func inspectLocal(path string) (*Facts, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	h, err := gguf.Parse(fh, inspectMaxRead, time.Now().Add(inspectParseTime))
	if err != nil {
		return nil, err
	}
	f := &Facts{Header: h, FileSize: st.Size(), FromDisk: true, ReadBytes: int64(h.HeaderSize)}
	f.Sizes, _ = h.TensorSizes(uint64(st.Size()))
	return f, nil
}
