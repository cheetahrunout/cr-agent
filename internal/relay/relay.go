// Package relay carries req frames to the local llama-server and streams its answer back as
// res.head / res.data / res.end (§10.1, §10.2). cancel aborts the local HTTP request, which makes
// llama-server stop generating. Request bodies are never logged.
package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// Limits of ruling B7 and §10.1.
const (
	MaxBody      = 256 * 1024
	MaxMessages  = 200
	maxBatch     = 128 * 1024       // bytes of payload per res.data frame (frames ≤ 256 KiB)
	maxPlainBody = 16 * 1024 * 1024 // non-stream answer cap
)

// allowedFields is the agent's own copy of the §14.1 body field list (ruling B7).
var allowedFields = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true, "temperature": true,
	"top_p": true, "stop": true, "seed": true, "stream": true, "stream_options": true, "tools": true,
	"tool_choice": true, "response_format": true, "n": true,
}

// Sender is the link session a request arrived on. Its context ends when the socket is lost.
type Sender interface {
	Send(protocol.Frame) error
	Context() context.Context
}

// Target returns the loaded engine's base URL, key and alias; ok is false when nothing is loaded.
type Target func() (base, key, alias string, ok bool)

// Relay runs requests against the engine.
type Relay struct {
	Target     Target
	Client     *http.Client
	Log        *slog.Logger
	FlushEvery time.Duration // res.data batching window, default 20 ms

	mu       sync.Mutex
	inflight map[string]flight
	wg       sync.WaitGroup
}

type flight struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
}

var errCancelled = errors.New("cancelled by the hub")

// New returns a relay with a client that never uses a proxy and never times out a stream.
func New(target Target, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	return &Relay{
		Target: target,
		Client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second, DialContext: supervisor.DialEngine}},
		Log:    log,
	}
}

// Handle starts the request in its own goroutine. Its context derives from the session, so a lost
// socket cancels it (§10.2).
func (r *Relay) Handle(s Sender, req *protocol.Req) {
	ctx, cancel := context.WithCancelCause(s.Context())
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = map[string]flight{}
	}
	if _, dup := r.inflight[req.ID]; dup {
		r.mu.Unlock()
		cancel(nil)
		r.Log.Warn("duplicate request id dropped", "id", req.ID)
		return
	}
	r.inflight[req.ID] = flight{ctx, cancel}
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.inflight, req.ID)
			r.mu.Unlock()
			cancel(nil)
		}()
		r.run(ctx, s, req)
	}()
}

// Cancel aborts a request in flight; it reports whether the id was known.
func (r *Relay) Cancel(id string) bool {
	r.mu.Lock()
	f, ok := r.inflight[id]
	r.mu.Unlock()
	if ok {
		f.cancel(errCancelled)
	}
	return ok
}

// CancelAll aborts every request in flight (pause, sleep).
func (r *Relay) CancelAll() {
	r.mu.Lock()
	fs := make([]flight, 0, len(r.inflight))
	for _, f := range r.inflight {
		fs = append(fs, f)
	}
	r.mu.Unlock()
	for _, f := range fs {
		f.cancel(errCancelled)
	}
}

// Refuse answers a request without the engine: an error body, then res.end{error}.
func (r *Relay) Refuse(s Sender, id string, status int, code, msg string) {
	r.localError(s, id, status, code, msg)
}

// Inflight lists the ids still live (a request whose socket is gone is already dead).
func (r *Relay) Inflight() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.inflight))
	for id, f := range r.inflight {
		if f.ctx.Err() == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// Wait blocks until every request goroutine has finished.
func (r *Relay) Wait() { r.wg.Wait() }

func (r *Relay) run(ctx context.Context, s Sender, req *protocol.Req) {
	t0 := time.Now()
	base, key, alias, ok := r.Target()
	if !ok {
		r.localError(s, req.ID, http.StatusServiceUnavailable, "model_loading", "no model is loaded")
		return
	}
	method, body := http.MethodGet, []byte(nil)
	if req.Path == protocol.PathChat {
		var status int
		var code, msg string
		body, status, code, msg = rewriteBody(req.Body, alias, req.Stream)
		if status != 0 {
			r.localError(s, req.ID, status, code, msg)
			return
		}
		method = http.MethodPost
	}
	hr, err := http.NewRequestWithContext(ctx, method, base+req.Path, bytes.NewReader(body))
	if err != nil {
		r.localError(s, req.ID, http.StatusInternalServerError, "internal_error", "bad local request")
		return
	}
	hr.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if req.Stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	resp, err := r.Client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			r.end(s, req.ID, protocol.ReasonCancelled, "")
		} else {
			r.localError(s, req.ID, http.StatusBadGateway, "engine_unreachable", "the local engine did not answer")
		}
		r.Log.Info("req finished", "id", req.ID, "path", req.Path, "result", "no_response", "ms", time.Since(t0).Milliseconds())
		return
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	if s.Send(&protocol.ResHead{ID: req.ID, Status: resp.StatusCode, CType: ctype}) != nil {
		return
	}
	var n int
	if strings.HasPrefix(ctype, "text/event-stream") {
		n, err = r.relaySSE(ctx, s, req.ID, resp.Body)
	} else {
		n, err = r.relayPlain(s, req.ID, resp.Body)
	}
	reason, msg := protocol.ReasonDone, ""
	switch {
	case ctx.Err() != nil:
		reason = protocol.ReasonCancelled
	case err != nil:
		reason, msg = protocol.ReasonError, "engine_stream_error"
	}
	r.end(s, req.ID, reason, msg)
	// Metadata only: id, path, status, payload count and timing. Never the body.
	r.Log.Info("req finished", "id", req.ID, "path", req.Path, "status", resp.StatusCode, "reason", reason,
		"payloads", n, "ms", time.Since(t0).Milliseconds(), "cause", causeName(ctx))
}

func causeName(ctx context.Context) string {
	switch c := context.Cause(ctx); {
	case c == nil:
		return ""
	case errors.Is(c, errCancelled):
		return "hub_cancel"
	default:
		return "socket_lost"
	}
}

// relaySSE forwards each SSE event's data payload unparsed, gathered into ~20 ms res.data batches.
func (r *Relay) relaySSE(ctx context.Context, s Sender, id string, body io.Reader) (int, error) {
	events := make(chan string, 256)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var data []string
		for sc.Scan() {
			line := strings.TrimSuffix(sc.Text(), "\r")
			if line == "" {
				if len(data) > 0 {
					select {
					case events <- strings.Join(data, "\n"):
					case <-ctx.Done():
						readErr <- ctx.Err()
						return
					}
					data = data[:0]
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(v, " "))
			}
		}
		if len(data) > 0 {
			select {
			case events <- strings.Join(data, "\n"):
			case <-ctx.Done():
			}
		}
		readErr <- sc.Err()
	}()

	every := r.FlushEvery
	if every <= 0 {
		every = 20 * time.Millisecond
	}
	var batch []string
	size, total := 0, 0
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.Send(&protocol.ResData{ID: id, D: batch})
		total += len(batch)
		batch, size = nil, 0
		return err
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if err := flush(); err != nil {
					return total, err
				}
				return total, <-readErr
			}
			if len(batch) == 0 {
				timer.Reset(every)
			}
			batch = append(batch, ev)
			size += len(ev)
			if size >= maxBatch {
				timer.Stop()
				if err := flush(); err != nil {
					return total, err
				}
			}
		case <-timer.C:
			if err := flush(); err != nil {
				return total, err
			}
		case <-ctx.Done():
			return total, ctx.Err()
		}
	}
}

// relayPlain sends a non-stream body as one or more res.data pieces split on UTF-8 boundaries.
func (r *Relay) relayPlain(s Sender, id string, body io.Reader) (int, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxPlainBody+1))
	if err != nil {
		return 0, err
	}
	if len(b) > maxPlainBody {
		return 0, errors.New("answer too large")
	}
	n := 0
	for len(b) > 0 || n == 0 {
		cut := len(b)
		if cut > maxBatch {
			cut = maxBatch
			for cut > maxBatch-4 && !utf8.RuneStart(b[cut]) {
				cut--
			}
		}
		if err := s.Send(&protocol.ResData{ID: id, D: []string{string(b[:cut])}}); err != nil {
			return n, err
		}
		b = b[cut:]
		n++
	}
	return n, nil
}

func (r *Relay) end(s Sender, id, reason, msg string) {
	_ = s.Send(&protocol.ResEnd{ID: id, Reason: reason, Error: msg})
}

// localError answers without the engine: an OpenAI-shaped error body, then res.end{error}.
func (r *Relay) localError(s Sender, id string, status int, code, msg string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": "invalid_request_error"}})
	if s.Send(&protocol.ResHead{ID: id, Status: status, CType: "application/json"}) != nil {
		return
	}
	if s.Send(&protocol.ResData{ID: id, D: []string{string(body)}}) != nil {
		return
	}
	r.end(s, id, protocol.ReasonError, code)
	r.Log.Info("req refused locally", "id", id, "status", status, "code", code)
}

// rewriteBody re-whitelists the chat body (ruling B7): unknown fields are a local 400, the body is
// capped at 256 KB and 200 messages, n must be 1, model becomes the alias, stream follows req.stream.
func rewriteBody(raw protocol.RawObject, alias string, stream bool) (out []byte, status int, code, msg string) {
	if len(raw) > MaxBody {
		return nil, http.StatusRequestEntityTooLarge, "request_too_large", "request body over 256 KB"
	}
	if len(raw) == 0 {
		return nil, http.StatusBadRequest, "invalid_request", "body required"
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, http.StatusBadRequest, "invalid_request", "body is not a JSON object"
	}
	for k := range m {
		if !allowedFields[k] {
			return nil, http.StatusBadRequest, "invalid_request", "unsupported field: " + k
		}
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(m["messages"], &msgs); err != nil || len(msgs) == 0 {
		return nil, http.StatusBadRequest, "invalid_request", "messages must be a non-empty array"
	}
	if len(msgs) > MaxMessages {
		return nil, http.StatusBadRequest, "invalid_request", "more than 200 messages"
	}
	if n, ok := m["n"]; ok {
		var v int
		if json.Unmarshal(n, &v) != nil || v != 1 {
			return nil, http.StatusBadRequest, "invalid_request", "n must be 1"
		}
	}
	m["model"], _ = json.Marshal(alias)
	m["stream"], _ = json.Marshal(stream)
	if !stream {
		delete(m, "stream_options")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, http.StatusBadRequest, "invalid_request", "body could not be re-encoded"
	}
	return out, 0, "", ""
}
