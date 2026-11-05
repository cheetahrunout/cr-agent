// Package agent is the M0 core: it answers the link's frames, runs the engine supervisor on load and
// unload, relays requests, and builds hello and status from its own state.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/relay"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// Engine is a running engine (supervisor.Engine, or a fake in tests).
type Engine interface {
	Target() (base, key, alias string)
	PID() uint32
	Done() <-chan struct{}
	Stop()
}

// StartFunc starts an engine and returns once it is healthy.
type StartFunc func(ctx context.Context, c supervisor.Config) (Engine, error)

// Config configures the agent.
type Config struct {
	Version   string
	LlamaTag  string
	EngineDir string
	ModelPath string // M0: one local GGUF
	ModelID   string // its id; load must name it
	LowIL     bool
	MemLimit  uint64
	Limits    supervisor.Limits
	Log       *slog.Logger

	Start    StartFunc                                           // default: supervisor.Start
	Hardware func(ctx context.Context) protocol.Hardware         // default: empty report
	GPUInUse func(ctx context.Context, pid uint32) (bool, error) // nil: skip the CUDA check
}

// Agent implements link.Handler.
type Agent struct {
	cfg   Config
	log   *slog.Logger
	relay *relay.Relay

	mu          sync.Mutex
	sess        *link.Session
	mode        string
	state       string
	engine      Engine
	settings    *protocol.Settings
	engineErr   string
	lastDisc    string
	loading     bool
	stopping    bool
	config      protocol.ConfigArgs
	lowILResult string
}

// New builds an agent.
func New(cfg Config) *Agent {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Start == nil {
		cfg.Start = func(ctx context.Context, c supervisor.Config) (Engine, error) {
			e, err := supervisor.Start(ctx, c)
			if err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	a := &Agent{cfg: cfg, log: cfg.Log, state: protocol.EngineUnloaded, mode: "normal"}
	a.relay = relay.New(a.target, cfg.Log)
	return a
}

// Relay exposes the relay (tests).
func (a *Agent) Relay() *relay.Relay { return a.relay }

func (a *Agent) target() (string, string, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine == nil || a.state != protocol.EngineLoaded {
		return "", "", "", false
	}
	b, k, al := a.engine.Target()
	return b, k, al, true
}

// Hello is the full state snapshot (§10.1).
func (a *Agent) Hello() *protocol.Hello {
	var hwr protocol.Hardware
	if a.cfg.Hardware != nil {
		hwr = a.cfg.Hardware(context.Background())
	}
	if hwr.Pools == nil {
		hwr.Pools = []protocol.Pool{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	h := &protocol.Hello{
		AgentVersion:         a.cfg.Version,
		Proto:                protocol.Proto,
		Platform:             runtime.GOOS + "-" + runtime.GOARCH,
		Backend:              "cuda",
		Capabilities:         []string{"config"},
		SupportedArchs:       []string{},
		LlamaTag:             a.cfg.LlamaTag,
		Hardware:             hwr,
		Engine:               a.state,
		Models:               []protocol.LocalModel{}, // M0: the local file is not an HF download, so it has no repo/commit to list
		Jobs:                 []protocol.Job{},
		Inflight:             append([]string{}, a.relay.Inflight()...),
		LastDisconnectReason: a.lastDisc,
		LocalPause:           false,
	}
	if a.state == protocol.EngineLoaded || a.state == protocol.EngineLoading {
		h.ModelID = a.cfg.ModelID
		h.SettingsInUse = a.settings
	}
	return h
}

// OnWelcome records the session and mode.
func (a *Agent) OnWelcome(s *link.Session, w *protocol.Welcome) {
	a.mu.Lock()
	a.sess = s
	a.mode = w.Mode
	a.mu.Unlock()
}

// OnDisconnect forgets the session. The link has already cancelled every request on it.
func (a *Agent) OnDisconnect(reason string) {
	a.mu.Lock()
	a.sess = nil
	a.lastDisc = reason
	a.mu.Unlock()
}

// OnFrame dispatches hub frames; it never blocks the reader.
func (a *Agent) OnFrame(s *link.Session, f protocol.Frame) {
	a.mu.Lock()
	if a.sess == nil {
		a.sess = s // a hub that skips welcome still gets answers on this socket
	}
	mode := a.mode
	a.mu.Unlock()
	switch f := f.(type) {
	case *protocol.Req:
		if mode == "update_required" {
			_ = s.Send(&protocol.ResHead{ID: f.ID, Status: 503, CType: "application/json"})
			_ = s.Send(&protocol.ResEnd{ID: f.ID, Reason: protocol.ReasonError, Error: "update_required"})
			return
		}
		a.relay.Handle(s, f)
	case *protocol.Cancel:
		if !a.relay.Cancel(f.ID) {
			a.log.Info("cancel for an unknown id dropped", "id", f.ID)
		}
	case *protocol.ReqBody:
		a.log.Info("req.body dropped: chunked bodies are not supported in M0", "id", f.ID)
	case *protocol.Cmd:
		go a.handleCmd(s, f)
	}
}

func (a *Agent) cmdErr(s *link.Session, id, code, field, msg string) {
	_ = s.Send(&protocol.CmdErr{ID: id, Code: code, Field: field, Message: msg})
}

func (a *Agent) handleCmd(s *link.Session, c *protocol.Cmd) {
	a.log.Info("cmd", "id", c.ID, "op", c.Op)
	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	if mode == "update_required" && c.Op != protocol.OpUpdate && c.Op != protocol.OpConfig {
		a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "update required")
		return
	}
	switch args := c.Args.(type) {
	case *protocol.LoadArgs:
		if fe := supervisor.CheckSettings(args.Settings, a.cfg.Limits); fe != nil {
			a.log.Warn("load refused", "id", c.ID, "field", fe.Field)
			a.cmdErr(s, c.ID, protocol.ErrBadSetting, fe.Field, fe.Msg)
			return
		}
		if args.ModelID != a.cfg.ModelID {
			a.cmdErr(s, c.ID, protocol.ErrNotFound, "model_id", "model not on this PC")
			return
		}
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		res, err := a.Load(s.Context(), args.Settings)
		if err != nil {
			code := protocol.ErrEngineFailed
			if errors.Is(err, errBusy) {
				code = protocol.ErrBusy
			}
			a.cmdErr(s, c.ID, code, "", "engine did not start")
			return
		}
		_ = s.Send(&protocol.CmdDone{ID: c.ID, Result: res})
	case *protocol.EmptyArgs:
		switch c.Op {
		case protocol.OpUnload:
			_ = s.Send(&protocol.CmdAck{ID: c.ID})
			a.Unload()
			_ = s.Send(&protocol.CmdDone{ID: c.ID})
		default: // pause, resume: not in M0
			a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "not in this version")
		}
	case *protocol.ConfigArgs:
		// M0 stub: values are bounded by the decoder and stored; nothing acts on them yet.
		a.mu.Lock()
		if args.KeepAwake != nil {
			a.config.KeepAwake = args.KeepAwake
		}
		if args.IdleUnloadMin != nil {
			a.config.IdleUnloadMin = args.IdleUnloadMin
		}
		if args.GPUSelect != nil {
			a.config.GPUSelect = args.GPUSelect
		}
		a.mu.Unlock()
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		_ = s.Send(&protocol.CmdDone{ID: c.ID})
	case *protocol.IDArgs: // op cancel
		a.relay.Cancel(args.ID)
		_ = s.Send(&protocol.CmdDone{ID: c.ID})
	default: // download, inspect, delete, update: later milestones
		a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "not in this version")
	}
}

var errBusy = errors.New("a load is already running")

// Load (re)starts the engine with the given settings. ctx bounds the wait for /health.
func (a *Agent) Load(ctx context.Context, st protocol.Settings) (map[string]any, error) {
	a.mu.Lock()
	if a.loading {
		a.mu.Unlock()
		return nil, errBusy
	}
	a.loading = true
	old := a.engine
	a.engine, a.state, a.settings, a.engineErr = nil, protocol.EngineLoading, &st, ""
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.loading = false
		a.mu.Unlock()
	}()
	a.sendStatus()
	if old != nil {
		a.stopEngine(old)
	}
	// The load continues even if the socket drops (long jobs are agent-owned, ruling B3).
	lctx := context.WithoutCancel(ctx)

	sc := supervisor.Config{EngineDir: a.cfg.EngineDir, ModelPath: a.cfg.ModelPath, Alias: a.cfg.ModelID,
		Settings: st, LowIL: a.cfg.LowIL, MemLimit: a.cfg.MemLimit, Log: a.log}
	t0 := time.Now()
	eng, err := a.cfg.Start(lctx, sc)
	lowIL := sc.LowIL
	if sc.LowIL {
		wantGPU := st.GPULayers.All || st.GPULayers.N > 0
		switch {
		case err != nil:
			a.setLowIL("fallback: engine failed under low integrity: " + err.Error())
		case wantGPU && a.cfg.GPUInUse != nil:
			ok, cerr := a.cfg.GPUInUse(lctx, eng.PID())
			if cerr != nil || !ok {
				a.setLowIL("fallback: engine started under low integrity but holds no CUDA context")
				eng.Stop()
				eng, err = nil, errors.New("no CUDA under low IL")
			} else {
				a.setLowIL("ok: CUDA context present under low integrity")
			}
		default:
			a.setLowIL("ok: engine healthy under low integrity (CUDA not checked)")
		}
		if err != nil {
			a.log.Warn("low-IL start failed; falling back to the normal token", "err", err.Error())
			sc.LowIL, lowIL = false, false
			eng, err = a.cfg.Start(lctx, sc)
		}
	}
	ms := time.Since(t0).Milliseconds()
	a.mu.Lock()
	if err != nil {
		a.state, a.engineErr, a.settings = protocol.EngineError, "engine_failed", nil
		a.mu.Unlock()
		a.log.Error("load failed", "err", err.Error(), "ms", ms)
		a.sendStatus()
		return nil, err
	}
	a.engine, a.state = eng, protocol.EngineLoaded
	a.mu.Unlock()
	res := map[string]any{"start_ms": ms, "low_il": lowIL}
	if a.cfg.GPUInUse != nil && (st.GPULayers.All || st.GPULayers.N > 0) {
		onGPU, err := a.cfg.GPUInUse(lctx, eng.PID())
		res["cuda_context"] = onGPU
		if err != nil || !onGPU {
			a.log.Warn("engine holds no CUDA context; it is running on the CPU", "err", fmt.Sprint(err))
		}
	}
	a.log.Info("model loaded", "model_id", a.cfg.ModelID, "ms", ms, "low_il", lowIL, "cuda_context", res["cuda_context"])
	go a.watch(eng)
	a.sendStatus()
	return res, nil
}

func (a *Agent) setLowIL(s string) {
	a.mu.Lock()
	a.lowILResult = s
	a.mu.Unlock()
	a.log.Info("low-IL result", "result", s)
}

// LowILResult reports the outcome of the last low-integrity attempt.
func (a *Agent) LowILResult() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lowILResult
}

// watch notices an engine that exits without being asked to.
func (a *Agent) watch(e Engine) {
	<-e.Done()
	a.mu.Lock()
	if a.engine != e || a.stopping {
		a.mu.Unlock()
		return
	}
	a.engine, a.state, a.engineErr, a.settings = nil, protocol.EngineError, "engine_exited", nil
	a.mu.Unlock()
	a.log.Error("engine exited unexpectedly")
	a.sendStatus()
}

func (a *Agent) stopEngine(e Engine) {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	e.Stop()
	a.mu.Lock()
	a.stopping = false
	a.mu.Unlock()
}

// Unload stops the engine.
func (a *Agent) Unload() {
	a.mu.Lock()
	e := a.engine
	a.engine, a.state, a.settings = nil, protocol.EngineUnloading, nil
	a.mu.Unlock()
	if e != nil {
		a.sendStatus()
		a.stopEngine(e)
	}
	a.mu.Lock()
	a.state = protocol.EngineUnloaded
	a.mu.Unlock()
	a.sendStatus()
}

func (a *Agent) sendStatus() {
	a.mu.Lock()
	s := a.sess
	st := &protocol.Status{Engine: a.state, EngineError: a.engineErr, Jobs: []protocol.Job{}, SettingsInUse: a.settings}
	if a.state == protocol.EngineLoaded || a.state == protocol.EngineLoading {
		st.ModelID = a.cfg.ModelID
	}
	a.mu.Unlock()
	if s != nil {
		_ = s.Send(st)
	}
}
