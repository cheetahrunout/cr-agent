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

	"github.com/cheetahrunout/cr-agent/internal/enginelock"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Config describes one engine launch.
type Config struct {
	EngineDir string // the folder holding llama-server.exe and its DLLs; also the working directory
	ModelPath string
	Alias     string
	Settings  protocol.Settings
	Level     Level // how the engine is isolated; the zero value is the strongest
	// ModelGrant is a model folder the agent may let the AppContainer read: its own, or one the host allowed.
	// Empty for any other: then the sandbox can read the model only if the folder already allows it.
	ModelGrant string
	GPUSelect  string        // "" or "all": every GPU; "0".."7": only that one (CUDA_VISIBLE_DEVICES)
	MemLimit   uint64        // job memory cap, 0 = none
	Deadline   time.Duration // load deadline (ruling B28: 5 min)
	Log        *slog.Logger
}

// Engine is a running llama-server.
type Engine struct {
	p       *proc
	base    string
	key     string
	alias   string
	port    int
	socket  string
	log     *slog.Logger
	Started time.Duration // spawn to /health 200

	suppressed atomic.Int64
	mu         sync.Mutex
	tail       []string // last allowed lines, for error reports
	pumpDone   chan struct{}
}

// Level is how the engine is isolated. There are these two and nothing weaker: no path in the agent
// starts the engine at normal integrity. Both run at low integrity, so a process at either level cannot
// change the host's files.
type Level int

const (
	// LevelAppContainer is the default: an AppContainer with no capabilities, reached over a Unix socket.
	LevelAppContainer Level = iota
	// LevelLowIntegrity is the host's own opt-in on a PC where the AppContainer does not work: a low-integrity
	// copy of the host's token, reached on loopback. The engine can then read the host's files, open
	// DPAPI blobs and connect out.
	LevelLowIntegrity
)

func (l Level) String() string {
	switch l {
	case LevelAppContainer:
		return "appcontainer"
	case LevelLowIntegrity:
		return "low_integrity"
	}
	return "unknown"
}

// ErrSandboxUnavailable means the isolation asked for could not be set up on this PC, before any of the
// child ran, so no model or hub can have caused it. It is an engine error like any other, never a reason to
// start with weaker isolation (ruling B63). For the AppContainer it lets the agent offer the host's opt-in;
// for low integrity there is nothing further to offer.
var ErrSandboxUnavailable = errors.New("the engine's sandbox cannot be used on this PC")

// ErrEngineExited is returned when llama-server exits before it is healthy.
var ErrEngineExited = errors.New("engine exited before it was healthy")

// Start launches llama-server and returns once /health answers 200 and the port is owned by the child.
func Start(ctx context.Context, c Config) (*Engine, error) {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	if c.EngineDir == "" {
		return nil, errors.New("no engine folder: this copy of the agent was not started from its install folder")
	}
	dir, err := filepath.Abs(c.EngineDir)
	if err != nil {
		return nil, err
	}
	exe := filepath.Join(dir, "llama-server.exe")
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		return nil, fmt.Errorf("llama-server.exe not found in %s", dir)
	}
	key := randHex(32)
	l := Launch{ModelPath: c.ModelPath, Alias: c.Alias, APIKey: key, Settings: c.Settings}
	var port int
	base := ""
	if c.Level == LevelAppContainer {
		if err := prepareSandbox(dir, c.ModelGrant, c.ModelPath); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		if l.Socket, err = newSocketPath(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		base = registerSocket(l.Socket)
	} else {
		if port, err = freePort(); err != nil {
			return nil, err
		}
		l.Port = port
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	deadline := c.Deadline
	if deadline <= 0 {
		deadline = 5 * time.Minute
	}

	t0 := time.Now()
	p, err := spawn(spawnOptions{Exe: exe, Args: BuildArgs(l), Dir: dir, Level: c.Level, MemLimit: c.MemLimit, GPUSelect: c.GPUSelect})
	if err != nil {
		forgetSocket(l.Socket)
		return nil, err
	}
	e := &Engine{p: p, base: base, key: key, alias: c.Alias, port: port, socket: l.Socket, log: log, pumpDone: make(chan struct{})}
	go e.pump(p.out)
	log.Info("engine spawned", "pid", p.PID(), "port", port, "isolation", c.Level.String(), "integrity", p.Integrity(),
		"mem_cap_mb", c.MemLimit>>20, "ctx", l.Settings.Ctx, "threads", l.Settings.Threads)

	if err := e.waitHealthy(ctx, deadline); err != nil {
		e.Stop()
		return nil, err
	}
	if l.Socket != "" {
		if owner, err := socketOwner(l.Socket); err != nil || owner != p.PID() {
			e.Stop()
			if err != nil {
				return nil, fmt.Errorf("the socket's owner cannot be checked: %w", err)
			}
			return nil, fmt.Errorf("the socket is not owned by the engine (owner %d)", owner)
		}
	} else if owner, err := portOwner(port); err != nil || owner != p.PID() {
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
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: DialEngine}}
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

// A sandboxed engine has a Unix socket, not a port. Its base URL names a host that exists only in this
// table, so the relay and every other HTTP client keep working with a plain base URL.
var (
	socketsMu sync.Mutex
	sockets   = map[string]string{}
)

func registerSocket(path string) string {
	host := "cr-engine-" + randHex(8)
	socketsMu.Lock()
	sockets[host+":80"] = path
	socketsMu.Unlock()
	return "http://" + host
}

func forgetSocket(path string) {
	if path == "" {
		return
	}
	socketsMu.Lock()
	for k, v := range sockets {
		if v == path {
			delete(sockets, k)
		}
	}
	socketsMu.Unlock()
	os.Remove(path)
}

// DialEngine is the dialer of every HTTP client that talks to an engine.
func DialEngine(ctx context.Context, network, addr string) (net.Conn, error) {
	socketsMu.Lock()
	path := sockets[addr]
	socketsMu.Unlock()
	var d net.Dialer
	if path != "" {
		return d.DialContext(ctx, "unix", path)
	}
	return d.DialContext(ctx, network, addr)
}

var reCUDADevice = regexp.MustCompile(`(?m)^\s*CUDA\d+:`)

// ProbeCUDA asks the engine, in the AppContainer, with no model and no GPU selection, whether it sees a
// CUDA device: llama-server --list-devices. Nothing the hub sends has a part in it. A list that does not
// come in time shows no device, the same as a list without one.
func ProbeCUDA(engineDir string) (bool, error) {
	if engineDir == "" {
		return false, errors.New("no engine folder")
	}
	dir, err := filepath.Abs(engineDir)
	if err != nil {
		return false, err
	}
	if err := prepareSandbox(dir, "", ""); err != nil {
		return false, err
	}
	p, err := spawn(spawnOptions{Exe: filepath.Join(dir, "llama-server.exe"), Args: []string{"--list-devices"}, Dir: dir, Level: LevelAppContainer})
	if err != nil {
		return false, err
	}
	defer p.out.Close()
	defer p.Kill()
	out := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(p.out, 1<<20))
		out <- b
	}()
	select {
	case b := <-out:
		return reCUDADevice.Match(b), nil
	case <-time.After(30 * time.Second):
		return false, nil
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
	forgetSocket(e.socket)
	e.log.Info("engine stopped", "pid", e.p.PID(), "suppressed_lines", e.suppressed.Load(), "peak_job_mb", peak>>20)
}

// Engine output is filtered before it reaches the agent log (ruling B16): only lines matching the
// allow-list of the pinned tag in engine.lock are written, truncated; everything else is counted.
func allowed(line string) bool { return enginelock.Pinned().Allowed(line) }

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
