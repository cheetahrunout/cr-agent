// Package agent is the agent's core: it answers the link's frames, runs the engine supervisor on
// load and unload, relays requests, hands downloads, inspects and deletes to the model manager, owns
// pause and sleep, and builds hello and status from its own state.
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
	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/relay"
	"github.com/cheetahrunout/cr-agent/internal/status"
	"github.com/cheetahrunout/cr-agent/internal/store"
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
	LowIL     bool
	MemLimit  uint64
	// LogicalCores bounds threads; n_layer and n_ctx_train come from each model's own header.
	LogicalCores int
	Log          *slog.Logger

	// LocalModelPath and LocalModelID name one GGUF file outside the model folder (a development
	// aid kept from M0). Models downloaded through the hub come from Models.
	LocalModelPath string
	LocalModelID   string

	Models *models.Manager // nil: no model folder (downloads, inspect and delete answer unsupported)
	Store  *store.Store    // nil: pause and config are not persisted

	Start    StartFunc                                           // default: supervisor.Start
	Hardware func(ctx context.Context) protocol.Hardware         // default: empty report
	GPUInUse func(ctx context.Context, pid uint32) (bool, error) // nil: skip the CUDA check
	HFToken  func() string                                       // absent | present | invalid; nil: not reported

	// OnChange is called when anything the tray shows may have changed. It must not block.
	OnChange func()
	// ResumedByHub is called when a pause set on this PC was lifted from the hub (ruling B4).
	ResumedByHub func()
	// TuneLink lets tests shorten the link's timings.
	TuneLink func(*link.Config)
	// CheckHub validates a hub URL read from the store; default link.ValidateHubURL (the build's host
	// allowlist). Tests that run a local hub replace it.
	CheckHub func(string) (string, error)
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
	loadedID    string
	settings    *protocol.Settings
	engineErr   string
	lastDisc    string
	loading     bool
	stopping    bool
	paused      bool
	sleeping    bool
	config      protocol.ConfigArgs
	lowILResult string
	cmdJobs     map[string]string             // download command id → model id
	inspects    map[string]context.CancelFunc // inspect command id → cancel

	linkMu     sync.Mutex
	linkCancel context.CancelFunc
	linkDone   chan struct{}
	linkHub    string
	linkKey    string
	phase      link.Phase
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
	if cfg.OnChange == nil {
		cfg.OnChange = func() {}
	}
	if cfg.CheckHub == nil {
		cfg.CheckHub = link.ValidateHubURL
	}
	a := &Agent{cfg: cfg, log: cfg.Log, state: protocol.EngineUnloaded, mode: "normal",
		cmdJobs: map[string]string{}, inspects: map[string]context.CancelFunc{}}
	a.relay = relay.New(a.target, cfg.Log)
	if cfg.Store != nil {
		c := cfg.Store.Config()
		a.paused = c.LocalPause
		if c.KeepAwake {
			t := true
			a.config.KeepAwake = &t
		}
		if c.IdleUnloadMin > 0 {
			n := c.IdleUnloadMin
			a.config.IdleUnloadMin = &n
		}
	}
	if a.paused && cfg.Models != nil {
		cfg.Models.SetPaused(true)
	}
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

func (a *Agent) capabilities() []string {
	if a.cfg.Models != nil {
		return []string{"config", "inspect"}
	}
	return []string{"config"}
}

func (a *Agent) localModels() ([]protocol.LocalModel, []protocol.Job) {
	if a.cfg.Models == nil {
		return []protocol.LocalModel{}, []protocol.Job{}
	}
	return a.cfg.Models.Models(), a.cfg.Models.Jobs()
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
	ms, jobs := a.localModels()
	a.mu.Lock()
	defer a.mu.Unlock()
	h := &protocol.Hello{
		AgentVersion:         a.cfg.Version,
		Proto:                protocol.Proto,
		Platform:             runtime.GOOS + "-" + runtime.GOARCH,
		Backend:              "cuda",
		Capabilities:         a.capabilities(),
		SupportedArchs:       []string{},
		LlamaTag:             a.cfg.LlamaTag,
		Hardware:             hwr,
		Engine:               a.state,
		Models:               ms,
		Jobs:                 jobs,
		Inflight:             append([]string{}, a.relay.Inflight()...),
		LastDisconnectReason: a.lastDisc,
		LocalPause:           a.paused,
	}
	if a.cfg.HFToken != nil {
		h.HFToken = a.cfg.HFToken()
	}
	if a.state == protocol.EngineLoaded || a.state == protocol.EngineLoading {
		h.ModelID = a.loadedID
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
	a.cfg.OnChange()
}

// OnDisconnect forgets the session. The link has already cancelled every request on it.
func (a *Agent) OnDisconnect(reason string) {
	a.mu.Lock()
	a.sess = nil
	if a.sleeping {
		reason = "sleep"
	}
	a.lastDisc = reason
	for id, cancel := range a.inspects { // command ids die with the socket
		cancel()
		delete(a.inspects, id)
	}
	a.mu.Unlock()
	a.cfg.OnChange()
}

// OnFrame dispatches hub frames; it never blocks the reader.
func (a *Agent) OnFrame(s *link.Session, f protocol.Frame) {
	a.mu.Lock()
	if a.sess == nil {
		a.sess = s // a hub that skips welcome still gets answers on this socket
	}
	mode, paused := a.mode, a.paused
	a.mu.Unlock()
	switch f := f.(type) {
	case *protocol.Req:
		switch {
		case mode == "update_required":
			a.relay.Refuse(s, f.ID, 503, "machine_offline", "this PC needs an agent update")
		case paused:
			a.relay.Refuse(s, f.ID, 503, "machine_paused", "the host paused this PC")
		default:
			a.relay.Handle(s, f)
			a.cfg.OnChange()
		}
	case *protocol.Cancel:
		if !a.cancelID(f.ID) {
			a.log.Info("cancel for an unknown id dropped", "id", f.ID)
		}
	case *protocol.ReqBody:
		a.log.Info("req.body dropped: chunked bodies are not supported yet", "id", f.ID)
	case *protocol.Cmd:
		go a.handleCmd(s, f)
	}
}

// cancelID aborts a request (r-) or the job of a command (c-).
func (a *Agent) cancelID(id string) bool {
	if a.relay.Cancel(id) {
		return true
	}
	a.mu.Lock()
	modelID, isJob := a.cmdJobs[id]
	stopInspect := a.inspects[id]
	a.mu.Unlock()
	switch {
	case stopInspect != nil:
		stopInspect()
		return true
	case isJob && a.cfg.Models != nil:
		go a.cfg.Models.Cancel(modelID)
		return true
	}
	return false
}

func (a *Agent) cmdErr(s *link.Session, id, code, field, msg string) {
	_ = s.Send(&protocol.CmdErr{ID: id, Code: code, Field: field, Message: msg})
}

func (a *Agent) modelErr(s *link.Session, id string, e *models.Error) {
	_ = s.Send(&protocol.CmdErr{ID: id, Code: e.Code, Field: e.Field, Message: e.Message, NeedMB: e.NeedMB, FreeMB: e.FreeMB})
}

func (a *Agent) handleCmd(s *link.Session, c *protocol.Cmd) {
	a.log.Info("cmd", "id", c.ID, "op", c.Op)
	a.mu.Lock()
	mode, paused, loadedID := a.mode, a.paused, a.loadedID
	a.mu.Unlock()
	if mode == "update_required" && c.Op != protocol.OpUpdate && c.Op != protocol.OpConfig {
		a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "update required")
		return
	}
	switch args := c.Args.(type) {
	case *protocol.LoadArgs:
		a.cmdLoad(s, c.ID, args, paused)

	case *protocol.DownloadArgs:
		if a.cfg.Models == nil {
			a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "no model folder")
			return
		}
		if args.ModelID == loadedID {
			a.cmdErr(s, c.ID, protocol.ErrBusy, "", "the model is loaded; unload it first")
			return
		}
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		a.mu.Lock()
		a.cmdJobs[c.ID] = args.ModelID
		a.mu.Unlock()
		a.cfg.Models.Download(args, func(o models.Outcome) {
			a.mu.Lock()
			delete(a.cmdJobs, c.ID)
			a.mu.Unlock()
			// The command id died with its socket if that is gone; hello and status carry the outcome then.
			if o.Err != nil {
				a.modelErr(s, c.ID, o.Err)
			} else {
				_ = s.Send(&protocol.CmdDone{ID: c.ID, Result: o.Result})
			}
		})

	case *protocol.InspectArgs:
		if a.cfg.Models == nil {
			a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "no model folder")
			return
		}
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		ctx, cancel := context.WithCancel(s.Context())
		a.mu.Lock()
		a.inspects[c.ID] = cancel
		a.mu.Unlock()
		facts, ierr := a.cfg.Models.Inspect(ctx, args)
		a.mu.Lock()
		delete(a.inspects, c.ID)
		a.mu.Unlock()
		cancelled := ctx.Err() != nil
		cancel()
		switch {
		case cancelled:
			a.cmdErr(s, c.ID, protocol.ErrCancelled, "", "cancelled")
		case ierr != nil:
			a.modelErr(s, c.ID, ierr)
		default:
			a.log.Info("inspect", "repo", args.Repo, "arch", facts.Header.Arch, "from_disk", facts.FromDisk, "read_bytes", facts.ReadBytes)
			_ = s.Send(&protocol.CmdDone{ID: c.ID, Result: facts.Result()})
		}

	case *protocol.ModelArgs: // delete
		if a.cfg.Models == nil {
			a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "no model folder")
			return
		}
		if derr := a.cfg.Models.Delete(args.ModelID, args.ModelID == loadedID); derr != nil {
			a.modelErr(s, c.ID, derr)
			return
		}
		_ = s.Send(&protocol.CmdDone{ID: c.ID})

	case *protocol.EmptyArgs:
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		switch c.Op {
		case protocol.OpUnload:
			a.Unload()
		case protocol.OpPause:
			a.SetPaused(true, false)
		case protocol.OpResume:
			a.SetPaused(false, false)
		}
		_ = s.Send(&protocol.CmdDone{ID: c.ID})

	case *protocol.ConfigArgs:
		// Values are bounded by the decoder and stored; keep-awake and idle unload act on them later.
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
		if a.cfg.Store != nil {
			if err := a.cfg.Store.Update(func(sc *store.Config) {
				if args.KeepAwake != nil {
					sc.KeepAwake = *args.KeepAwake
				}
				if args.IdleUnloadMin != nil {
					sc.IdleUnloadMin = *args.IdleUnloadMin
				}
				if args.GPUSelect != nil {
					sc.GPUSelect = *args.GPUSelect
				}
			}); err != nil {
				a.log.Warn("config not saved", "err", err.Error())
			}
		}
		_ = s.Send(&protocol.CmdAck{ID: c.ID})
		_ = s.Send(&protocol.CmdDone{ID: c.ID})

	case *protocol.IDArgs: // op cancel
		a.cancelID(args.ID)
		_ = s.Send(&protocol.CmdDone{ID: c.ID})

	default: // update: the updater is a later slice
		a.cmdErr(s, c.ID, protocol.ErrUnsupported, "", "not in this version")
	}
}

func (a *Agent) cmdLoad(s *link.Session, id string, args *protocol.LoadArgs, paused bool) {
	if paused {
		a.cmdErr(s, id, protocol.ErrBusy, "", "the host paused this PC")
		return
	}
	lim := supervisor.Limits{LogicalCores: a.cfg.LogicalCores}
	// Settings are judged against the machine's bounds first, so a bad value is named even for an unknown model.
	if fe := supervisor.CheckSettings(args.Settings, lim); fe != nil {
		a.log.Warn("load refused", "id", id, "field", fe.Field)
		a.cmdErr(s, id, protocol.ErrBadSetting, fe.Field, fe.Msg)
		return
	}
	path := ""
	switch {
	case a.cfg.LocalModelID != "" && args.ModelID == a.cfg.LocalModelID:
		path = a.cfg.LocalModelPath
	case a.cfg.Models != nil:
		l, lerr := a.cfg.Models.Resolve(args.ModelID)
		if lerr != nil {
			a.modelErr(s, id, lerr)
			return
		}
		path = l.Path
		lim.NLayer, lim.NCtxTrain = int(l.Header.BlockCount), int(l.Header.ContextLength)
		if fe := supervisor.CheckSettings(args.Settings, lim); fe != nil { // now against the model's own header
			a.log.Warn("load refused", "id", id, "field", fe.Field)
			a.cmdErr(s, id, protocol.ErrBadSetting, fe.Field, fe.Msg)
			return
		}
	default:
		a.cmdErr(s, id, protocol.ErrNotFound, "model_id", "model not on this PC")
		return
	}
	_ = s.Send(&protocol.CmdAck{ID: id})
	res, err := a.Load(s.Context(), args.ModelID, path, args.Settings)
	if err != nil {
		code := protocol.ErrEngineFailed
		if errors.Is(err, errBusy) {
			code = protocol.ErrBusy
		}
		a.cmdErr(s, id, code, "", "engine did not start")
		return
	}
	_ = s.Send(&protocol.CmdDone{ID: id, Result: res})
}

var errBusy = errors.New("a load is already running, or the PC is paused or asleep")

// Load (re)starts the engine on a model file with the given settings. ctx bounds the wait for /health.
func (a *Agent) Load(ctx context.Context, modelID, path string, st protocol.Settings) (map[string]any, error) {
	a.mu.Lock()
	if a.loading || a.paused || a.sleeping {
		a.mu.Unlock()
		return nil, errBusy
	}
	a.loading = true
	old := a.engine
	a.engine, a.state, a.settings, a.engineErr, a.loadedID = nil, protocol.EngineLoading, &st, "", modelID
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.loading = false
		a.mu.Unlock()
	}()
	a.sendStatus(false)
	if old != nil {
		a.stopEngine(old)
	}
	// The load continues even if the socket drops (long jobs are agent-owned, ruling B3).
	lctx := context.WithoutCancel(ctx)

	sc := supervisor.Config{EngineDir: a.cfg.EngineDir, ModelPath: path, Alias: modelID,
		Settings: st, LowIL: a.cfg.LowIL, MemLimit: a.cfg.MemLimit, Log: a.log}
	t0 := time.Now()
	eng, err := a.cfg.Start(lctx, sc)
	lowIL := sc.LowIL
	wantGPU := st.GPULayers.All || st.GPULayers.N > 0
	if sc.LowIL {
		switch {
		case err != nil:
			a.setLowIL("fallback: engine failed under low integrity")
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
	if err == nil && (a.paused || a.sleeping) { // paused or put to sleep while the engine was starting
		a.state, a.settings, a.loadedID = protocol.EngineUnloaded, nil, ""
		a.mu.Unlock()
		eng.Stop()
		a.sendStatus(false)
		return nil, errBusy
	}
	if err != nil {
		a.state, a.engineErr, a.settings, a.loadedID = protocol.EngineError, "engine_failed", nil, ""
		a.mu.Unlock()
		a.log.Error("load failed", "err", err.Error(), "ms", ms)
		a.sendStatus(false)
		return nil, err
	}
	a.engine, a.state = eng, protocol.EngineLoaded
	a.mu.Unlock()
	res := map[string]any{"start_ms": ms, "low_il": lowIL}
	if a.cfg.GPUInUse != nil && wantGPU {
		onGPU, err := a.cfg.GPUInUse(lctx, eng.PID())
		res["cuda_context"] = onGPU
		if err != nil || !onGPU {
			a.log.Warn("engine holds no CUDA context; it is running on the CPU", "err", fmt.Sprint(err))
		}
	}
	a.log.Info("model loaded", "model_id", modelID, "ms", ms, "low_il", lowIL, "cuda_context", res["cuda_context"])
	go a.watch(eng)
	a.sendStatus(false)
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
	a.engine, a.state, a.engineErr, a.settings, a.loadedID = nil, protocol.EngineError, "engine_exited", nil, ""
	a.mu.Unlock()
	a.log.Error("engine exited unexpectedly")
	a.sendStatus(false)
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
	a.engine, a.settings, a.loadedID = nil, nil, ""
	if e != nil {
		a.state = protocol.EngineUnloading
	} else if a.state != protocol.EngineLoading {
		a.state, a.engineErr = protocol.EngineUnloaded, ""
	}
	a.mu.Unlock()
	if e != nil {
		a.sendStatus(false)
		a.stopEngine(e)
		a.mu.Lock()
		if a.state == protocol.EngineUnloading {
			a.state = protocol.EngineUnloaded
		}
		a.mu.Unlock()
	}
	a.sendStatus(false)
}

// SetPaused is "Pause hosting": it stops every answer, unloads the model so the GPU is free, and
// pauses downloads. It is one switch whether flipped in the tray (local) or by the hub's pause and
// resume commands; a pause lifted from the hub is reported to the tray (ruling B4).
func (a *Agent) SetPaused(p, local bool) {
	a.mu.Lock()
	was := a.paused
	a.paused = p
	a.mu.Unlock()
	if was == p {
		return
	}
	a.log.Info("pause changed", "paused", p, "local", local)
	if a.cfg.Store != nil {
		if err := a.cfg.Store.Update(func(c *store.Config) { c.LocalPause = p }); err != nil {
			a.log.Warn("pause not saved", "err", err.Error())
		}
	}
	if a.cfg.Models != nil {
		a.cfg.Models.SetPaused(p)
	}
	if p {
		a.relay.CancelAll()
		a.Unload()
		return
	}
	a.sendStatus(false)
	if !local && a.cfg.ResumedByHub != nil {
		a.cfg.ResumedByHub()
	}
}

// Paused reports the pause switch.
func (a *Agent) Paused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

// ModelsChanged is the model manager's OnChange: a job or a model changed.
func (a *Agent) ModelsChanged(withModels bool) { a.sendStatus(withModels) }

func (a *Agent) sendStatus(withModels bool) {
	ms, jobs := a.localModels()
	a.mu.Lock()
	s := a.sess
	st := &protocol.Status{Engine: a.state, EngineError: a.engineErr, Jobs: jobs, SettingsInUse: a.settings, LocalPause: a.paused}
	if withModels {
		st.Models = ms
	}
	if a.state == protocol.EngineLoaded || a.state == protocol.EngineLoading {
		st.ModelID = a.loadedID
	}
	a.mu.Unlock()
	if s != nil {
		_ = s.Send(st)
	}
	a.cfg.OnChange()
}

// Snapshot is the input of the tray's status line.
func (a *Agent) Snapshot(paired bool) status.Input {
	_, jobs := a.localModels()
	a.linkMu.Lock()
	phase := a.phase
	a.linkMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	in := status.Input{Paired: paired, Link: phase, Mode: a.mode, Paused: a.paused, Engine: a.state,
		ModelID: a.loadedID, Inflight: len(a.relay.Inflight())}
	for _, j := range jobs {
		if j.Kind == "download" && j.State == "running" && j.Pct != nil {
			in.Downloading, in.DownloadPct = true, *j.Pct
		}
	}
	return in
}

// Connect starts (or restarts) the link with a hub URL and machine key. It returns at once.
func (a *Agent) Connect(hub, key string) {
	a.Disconnect()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.linkMu.Lock()
	a.linkCancel, a.linkDone, a.linkHub, a.linkKey, a.phase = cancel, done, hub, key, link.PhaseConnecting
	a.linkMu.Unlock()
	lc := link.Config{URL: hub, Key: key, Handler: a, Log: a.log, OnPhase: func(p link.Phase) {
		a.linkMu.Lock()
		if a.linkDone == done {
			a.phase = p
		}
		a.linkMu.Unlock()
		a.cfg.OnChange()
	}}
	if a.cfg.TuneLink != nil {
		a.cfg.TuneLink(&lc)
	}
	c := link.New(lc)
	go func() {
		defer close(done)
		if err := c.Run(ctx); errors.Is(err, link.ErrRevoked) {
			a.log.Error("this PC was removed from the hub; pair again to use it")
		}
	}()
	a.cfg.OnChange()
}

// Disconnect stops the link and waits for it to end.
func (a *Agent) Disconnect() {
	a.linkMu.Lock()
	cancel, done := a.linkCancel, a.linkDone
	a.linkCancel, a.linkDone = nil, nil
	a.linkMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Suspend is called when Windows is about to sleep (ruling B12): abort the answers in flight, say
// status{sleeping}, close the link with 1001, and stop the engine so nothing holds the GPU across
// the sleep. It returns within about two seconds, the time Windows grants.
func (a *Agent) Suspend() {
	a.mu.Lock()
	if a.sleeping {
		a.mu.Unlock()
		return
	}
	a.sleeping = true
	s := a.sess
	e := a.engine
	a.engine, a.state, a.settings, a.loadedID, a.engineErr = nil, protocol.EngineUnloaded, nil, "", ""
	paused := a.paused
	a.mu.Unlock()
	a.log.Info("suspending")
	a.relay.CancelAll()
	if s != nil {
		t := true
		_, jobs := a.localModels()
		_ = s.Send(&protocol.Status{Engine: protocol.EngineUnloaded, Jobs: jobs, LocalPause: paused, Sleeping: &t})
		s.Close(1001, "sleep")
	}
	a.linkMu.Lock()
	done := a.linkDone
	a.linkMu.Unlock()
	stopped := make(chan struct{})
	go func() {
		if e != nil {
			a.stopEngine(e)
		}
		close(stopped)
	}()
	if done != nil && s != nil { // give the close frame a moment to leave before the link is torn down
		select {
		case <-s.Context().Done():
		case <-time.After(700 * time.Millisecond):
		}
	}
	a.Disconnect()
	select {
	case <-stopped:
	case <-time.After(time.Second):
	}
	a.cfg.OnChange()
}

// Resume is called when Windows wakes: reconnect at once, without backoff, and report the real
// state in hello. The model is not reloaded: the hub decides what to load.
func (a *Agent) Resume() {
	a.mu.Lock()
	if !a.sleeping {
		a.mu.Unlock()
		return
	}
	a.sleeping = false
	a.mu.Unlock()
	a.log.Info("resumed from sleep")
	a.linkMu.Lock()
	hub, key := a.linkHub, a.linkKey
	a.linkMu.Unlock()
	if hub != "" && key != "" {
		a.Connect(hub, key)
	}
}

// Shutdown stops the link and the engine.
func (a *Agent) Shutdown() {
	a.Disconnect()
	a.Unload()
}
