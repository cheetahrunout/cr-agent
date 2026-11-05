package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Config describes one engine launch.
type Config struct {
	EngineDir string // the folder holding llama-server.exe and its DLLs; also the working directory
	ModelPath string
	Alias     string
	Settings  protocol.Settings
	LowIL     bool
	MemLimit  uint64        // job memory cap, 0 = none
	Deadline  time.Duration // load deadline (ruling B28: 5 min)
	Log       *slog.Logger
}

// Engine is a running llama-server.
type Engine struct {
	p       *proc
	base    string
	key     string
	alias   string
	port    int
	log     *slog.Logger
	Started time.Duration // spawn to /health 200

	suppressed atomic.Int64
	mu         sync.Mutex
	tail       []string // last allowed lines, for error reports
	pumpDone   chan struct{}
}

// ErrEngineExited is returned when llama-server exits before it is healthy.
var ErrEngineExited = errors.New("engine exited before it was healthy")

// Start launches llama-server and returns once /health answers 200 and the port is owned by the child.
func Start(ctx context.Context, c Config) (*Engine, error) {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	dir, err := filepath.Abs(c.EngineDir)
	if err != nil {
		return nil, err
	}
	exe := filepath.Join(dir, "llama-server.exe")
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		return nil, fmt.Errorf("llama-server.exe not found in %s", dir)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	key := randHex(32)
	l := Launch{ModelPath: c.ModelPath, Alias: c.Alias, Port: port, APIKey: key, Settings: c.Settings}
	deadline := c.Deadline
	if deadline <= 0 {
		deadline = 5 * time.Minute
	}

	t0 := time.Now()
	p, err := spawn(spawnOptions{Exe: exe, Args: BuildArgs(l), Dir: dir, LowIL: c.LowIL, MemLimit: c.MemLimit})
	if err != nil {
		return nil, err
	}
	e := &Engine{p: p, base: fmt.Sprintf("http://127.0.0.1:%d", port), key: key, alias: c.Alias, port: port, log: log, pumpDone: make(chan struct{})}
	go e.pump(p.out)
	log.Info("engine spawned", "pid", p.PID(), "port", port, "low_il", c.LowIL, "integrity", p.Integrity(),
		"mem_cap_mb", c.MemLimit>>20, "ctx", l.Settings.Ctx, "threads", l.Settings.Threads)

	if err := e.waitHealthy(ctx, deadline); err != nil {
		e.Stop()
		return nil, err
	}
	if owner, err := portOwner(port); err != nil || owner != p.PID() {
		e.Stop()
		return nil, fmt.Errorf("port %d is not owned by the engine (owner %d, err %v)", port, owner, err)
	}
	e.Started = time.Since(t0)
	log.Info("engine healthy", "pid", p.PID(), "start_ms", e.Started.Milliseconds())
	return e, nil
}

func (e *Engine) waitHealthy(ctx context.Context, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		if resp, err := client.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-e.p.Done():
			<-e.pumpDone
			return fmt.Errorf("%w (exit code %d): %s", ErrEngineExited, e.p.ExitCode(), e.lastLines())
		case <-ctx.Done():
			return fmt.Errorf("engine not healthy: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// Target is what the relay needs: the local base URL, the per-launch key and the alias.
func (e *Engine) Target() (base, key, alias string) { return e.base, e.key, e.alias }

// PID is the engine's process id.
func (e *Engine) PID() uint32 { return e.p.PID() }

// PeakMemory is the most memory the engine's job ever had committed, in bytes.
func (e *Engine) PeakMemory() uint64 { return e.p.PeakMemory() }

// Done is closed when the engine process exits.
func (e *Engine) Done() <-chan struct{} { return e.p.Done() }

// ExitCode is valid after Done.
func (e *Engine) ExitCode() uint32 { return e.p.ExitCode() }

// Stop terminates the engine (the Job Object takes the whole tree) and waits for it to exit.
// llama-server keeps no state worth saving (no slots, no prompt cache on disk), so terminating
// the job is the clean stop.
func (e *Engine) Stop() {
	peak := e.p.PeakMemory()
	e.p.Kill()
	select {
	case <-e.pumpDone:
	case <-time.After(2 * time.Second):
	}
	e.p.out.Close()
	e.log.Info("engine stopped", "pid", e.p.PID(), "suppressed_lines", e.suppressed.Load(), "peak_job_mb", peak>>20)
}

// Engine output is filtered before it reaches the agent log (ruling B16): only lines matching this
// allow-list are written, truncated; everything else is counted. The list is per pinned tag (b12125).
var allowLines = []*regexp.Regexp{
	regexp.MustCompile(`listening on http://127\.0\.0\.1:\d+$`),
	regexp.MustCompile(`server_run_i: model loaded$`),
	regexp.MustCompile(`(?i)\bcuda\b|ggml_cuda`),
	regexp.MustCompile(`(?i)out of memory|failed to (load|allocate|initiali[sz]e)|error loading model|unable to load`),
	regexp.MustCompile(`offloaded \d+/\d+ layers to GPU`),
}

func allowed(line string) bool {
	for _, re := range allowLines {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func (e *Engine) pump(r io.Reader) {
	defer close(e.pumpDone)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !allowed(line) {
			e.suppressed.Add(1)
			continue
		}
		if len(line) > 300 {
			line = line[:300]
		}
		e.mu.Lock()
		e.tail = append(e.tail, line)
		if len(e.tail) > 8 {
			e.tail = e.tail[1:]
		}
		e.mu.Unlock()
		e.log.Info("engine output", "line", line)
	}
}

func (e *Engine) lastLines() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.tail) == 0 {
		return "no allowed output"
	}
	return strings.Join(e.tail, " | ")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
