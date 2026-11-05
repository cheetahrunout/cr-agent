// Command cragent is the CR agent: one tray icon on the host's PC, one outbound WebSocket to the hub.
// It logs JSON lines to <home>\logs\agent.log (and to stderr when started from a console).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/agent"
	"github.com/cheetahrunout/cr-agent/internal/approve"
	"github.com/cheetahrunout/cr-agent/internal/enginelock"
	"github.com/cheetahrunout/cr-agent/internal/hf"
	"github.com/cheetahrunout/cr-agent/internal/hw"
	"github.com/cheetahrunout/cr-agent/internal/keepawake"
	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/models"
	"github.com/cheetahrunout/cr-agent/internal/pairing"
	"github.com/cheetahrunout/cr-agent/internal/power"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/status"
	"github.com/cheetahrunout/cr-agent/internal/store"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
	"github.com/cheetahrunout/cr-agent/internal/tray"
	"github.com/cheetahrunout/cr-agent/internal/trust"
	"github.com/cheetahrunout/cr-agent/internal/ui"
	"github.com/cheetahrunout/cr-agent/internal/updater"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = "0.1.0"

// sabotage is empty in every build but the rollback test's (PLAN ruling B27), which is linked with
// -X main.sabotage=unhealthy-<its own version>: that one binary never passes the health gate, so the
// launcher has something to revert. It is a link-time string naming one version: nothing at run time
// can set it, and the source on main has no broken path of its own.
var sabotage string

// neverHealthy reports whether this binary is the rollback test's.
func neverHealthy() bool { return sabotage == "unhealthy-"+version }

// llamaTag is the pinned llama.cpp build, from engine.lock.
var llamaTag = enginelock.Pinned().Tag

func main() { os.Exit(run()) }

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type options struct {
	home, hub, key, engine, model, modelID, modelsDir, pair, hfBase, approve, releaseBase string
	load, lowIL, noMemCap, noTray, unpair, verbose                                        bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.home, "home", env("CRAGENT_HOME", ""), "folder for config, state and logs (default %LOCALAPPDATA%\\cr-agent)")
	flag.StringVar(&o.hub, "hub", env("CRAGENT_HUB", "wss://"+link.ProductionHost+"/agent"), "hub WebSocket URL a bare pairing code is used with")
	flag.StringVar(&o.pair, "pair", "", "pair with the hub using this code or pairing string, then run")
	flag.BoolVar(&o.unpair, "unpair", false, "forget this PC's machine key and exit")
	flag.StringVar(&o.engine, "engine", env("CRAGENT_ENGINE", ""), "folder with llama-server.exe")
	flag.StringVar(&o.modelsDir, "models-dir", "", "set the model folder (kept in the config)")
	flag.BoolVar(&o.lowIL, "low-il", true, "start the engine with a low-integrity token (falls back if CUDA fails)")
	flag.BoolVar(&o.noMemCap, "no-mem-cap", false, "do not cap the engine's job memory at RAM - 4 GiB")
	flag.BoolVar(&o.noTray, "no-tray", false, "run as a console program without the tray icon")
	flag.BoolVar(&o.verbose, "v", false, "debug logging")
	// Development aids.
	flag.StringVar(&o.key, "key", "", "development: connect with this machine key instead of the stored pairing (or env CRAGENT_KEY)")
	flag.StringVar(&o.model, "model", env("CRAGENT_MODEL", ""), "development: a local .gguf outside the model folder")
	flag.StringVar(&o.modelID, "model-id", "", "development: model id for --model (default: from the file name)")
	flag.BoolVar(&o.load, "load", false, "development: load --model at start")
	flag.StringVar(&o.hfBase, "hf-base", "", "debug builds only: a local stand-in for huggingface.co")
	flag.StringVar(&o.releaseBase, "release-base", "", "debug builds only: a local stand-in for the GitHub release downloads")
	flag.StringVar(&o.approve, "approve", "ask", "debug builds only: ask | allow | deny for downloads outside the publisher allowlist")
	flag.Parse()
	if o.key == "" {
		o.key = os.Getenv("CRAGENT_KEY")
	}
	return o
}

// app is the running agent with its tray.
type app struct {
	o          options
	log        *slog.Logger
	st         *store.Store
	hf         *hf.Client
	mm         *models.Manager
	a          *agent.Agent
	aRef       atomic.Pointer[agent.Agent]
	tr         *tray.Tray
	q          *approve.Queue
	defaultHub string

	mu       sync.Mutex
	hfToken  string
	hfState  hf.TokenState
	devKeyed bool
	refresh  chan struct{}

	root    string // the install folder, when this agent runs from one
	managed bool
	up      *updater.Updater
	trayUp  atomic.Bool
	quit    chan struct{}
	code    int
}

func run() int {
	o := parseFlags()
	// An installed agent lives in <root>\versions\<version>\ beside the launcher's current.json.
	root, layoutVersion, managed := "", "", false
	if exe, err := os.Executable(); err == nil {
		root, layoutVersion, managed = layout.Find(exe)
	}
	home := o.home
	if home == "" && managed {
		home = root
	}
	if home == "" {
		var err error
		if home, err = store.DefaultDir(); err != nil {
			fmt.Fprintln(os.Stderr, "cannot find a home folder:", err)
			return 2
		}
	}
	st, err := store.Open(home)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open the agent's folder:", err)
		return 2
	}
	logFile := openLog(st.LogPath())
	if logFile != nil {
		defer logFile.Close()
	}
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(logWriter{logFile}, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if !debugBuild && (o.approve != "ask" || o.hfBase != "" || o.releaseBase != "") {
		log.Error("--approve, --hf-base and --release-base exist only in debug builds")
		return 2
	}
	if managed && layoutVersion != version {
		log.Error("this agent is not the version its folder is named for", "folder", layoutVersion, "agent", version)
		return 2
	}
	defaultHub, err := link.ValidateHubURL(o.hub)
	if err != nil {
		log.Error("hub URL refused", "err", err.Error())
		return 2
	}
	if o.unpair {
		if err := st.Unpair(); err != nil {
			log.Error("unpair failed", "err", err.Error())
			return 1
		}
		log.Info("unpaired: the machine key was removed from this PC")
		return 0
	}
	if o.key != "" && !pairing.ValidKey(o.key) {
		log.Error("machine key malformed (want crm_<8 base32>_<52 base32>)")
		return 2
	}
	// A release build runs only with a production signing key compiled in (ruling B20): without one
	// it could never verify an update, and with the test key anyone could sign one.
	if err := trust.Check(); err != nil {
		log.Error("this build cannot run", "err", err.Error())
		fmt.Fprintln(os.Stderr, "cragent:", err)
		return 2
	}
	release, running := singleInstance(st.Dir())
	if running {
		log.Error("another CR agent is already running with this folder")
		return 2
	}
	defer release()

	ap := &app{o: o, log: log, st: st, defaultHub: defaultHub, refresh: make(chan struct{}, 1), hfState: hf.TokenAbsent,
		root: root, managed: managed, quit: make(chan struct{}, 1)}
	if err := ap.start(); err != nil {
		log.Error("start failed", "err", err.Error())
		return 1
	}
	defer ap.stop()

	if o.pair != "" {
		if slug, err := ap.pair(o.pair, func() { log.Info("this PC was paired before: choose Replace key or a new name in the hub") }); err != nil {
			log.Error("pairing failed", "why", pairingMessage(err))
			fmt.Fprintln(os.Stderr, pairingMessage(err))
			// A PC that is already paired stays connected with the key it has (a code on the command
			// line is used once; a later start with the same line must not leave the PC offline).
			if ap.a.ConnectStored() {
				log.Info("using the pairing this PC already has")
			}
		} else {
			fmt.Fprintln(os.Stderr, "Paired as "+slug+".")
		}
	}

	if !o.noTray {
		ready := make(chan struct{})
		go func() {
			<-ready
			ap.trayUp.Store(true)
			ap.requestRefresh()
		}()
		go ap.signalQuit()
		err := ap.tr.Run(ready)
		if err == nil {
			return ap.exitCode()
		}
		log.Warn("no tray available; running as a console program", "err", err.Error())
	}
	ap.trayUp.Store(true) // console mode: there is no tray to wait for
	ctx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSig()
	select {
	case <-ctx.Done():
	case <-ap.quit:
	}
	return ap.exitCode()
}

// exit ends the agent with an exit code: 75 asks the launcher to start the version its pointer names.
func (ap *app) exit(code int) {
	ap.mu.Lock()
	ap.code = code
	ap.mu.Unlock()
	select {
	case ap.quit <- struct{}{}:
	default:
	}
	ap.tr.Close()
}

func (ap *app) exitCode() int {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	ap.log.Info("stopped", "exit_code", ap.code)
	return ap.code
}

// healthGate decides when this agent is healthy (ruling B22): it verified its files (Startup passed),
// its tray is up, and it reached welcome or made three connection attempts. It then writes
// healthy.<version>, which the launcher waits for, and commits the release.
func (ap *app) healthGate() {
	if neverHealthy() {
		ap.log.Warn("this build is the rollback test: it never passes the health gate", "version", version)
		return
	}
	for {
		time.Sleep(250 * time.Millisecond)
		paired := ap.paired()
		in := ap.a.Snapshot(paired)
		if ap.trayUp.Load() && (!paired || in.Link == link.PhaseConnected || ap.a.LinkAttempts() > 3) {
			if err := ap.up.MarkHealthy(); err != nil {
				ap.log.Error("health mark not written", "err", err.Error())
			} else {
				ap.log.Info("healthy", "version", version, "connected", in.Link == link.PhaseConnected)
			}
			return
		}
	}
}

// bootstrap fetches and checks the manifest of a freshly installed version (the installer cannot
// carry the manifest that lists the installer itself). A PC that is offline or a release not
// published yet is no reason to stop the agent: it runs unverified, says so in About, and keeps trying.
func (ap *app) bootstrap() {
	for try := 0; ; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := ap.up.Bootstrap(ctx)
		cancel()
		if err == nil {
			ap.requestRefresh()
			return
		}
		ap.log.Warn("this version's signed manifest could not be checked yet; the agent runs on and tries again",
			"err", err.Error(), "next_retry_min", int(bootstrapDelay(try)/time.Minute))
		time.Sleep(bootstrapDelay(try))
	}
}

// bootstrapDelay is one minute, then five, then an hour for as long as it takes.
func bootstrapDelay(try int) time.Duration {
	switch try {
	case 0:
		return time.Minute
	case 1:
		return 5 * time.Minute
	}
	return time.Hour
}

func (ap *app) signalQuit() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	ap.tr.Close()
}

// start builds everything and connects when this PC is paired.
func (ap *app) start() error {
	o, st, log := ap.o, ap.st, ap.log
	if o.modelsDir != "" {
		dir, err := models.CheckFolder(o.modelsDir)
		if err != nil {
			return fmt.Errorf("model folder: %w", err)
		}
		if err := st.Update(func(c *store.Config) { c.ModelsDir = dir }); err != nil {
			return err
		}
	}
	if b, err := st.ReadSecret(store.SecretHFToken); err == nil {
		ap.hfToken = string(b)
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Warn("the stored Hugging Face token cannot be read", "err", err.Error())
	}
	if ap.hfToken == "" {
		ap.hfToken = os.Getenv("HF_TOKEN") // the documented escape hatch (§12.1)
	}
	var err error
	if ap.hf, err = hf.New(hf.Options{Base: o.hfBase, UserAgent: "cr-agent/" + version, Token: func() string {
		ap.mu.Lock()
		defer ap.mu.Unlock()
		return ap.hfToken
	}}); err != nil {
		return err
	}

	sys, err := hw.QuerySystem(st.Dir())
	if err != nil {
		log.Warn("system probe incomplete", "err", err.Error())
	}
	var memCap uint64
	if !o.noMemCap && sys.RAMTotalMB > 8192 {
		memCap = uint64(sys.RAMTotalMB-4096) << 20 // ruling B15: RAM - 4 GiB
	}

	ap.tr = tray.New(ap.onAction)
	var approver models.Approver = models.DenyAll{}
	switch {
	case debugBuild && o.approve == "allow":
		approver = allowAll{}
	case debugBuild && o.approve == "deny":
	case !o.noTray:
		ap.q = approve.New(ap.tr.Notify, ap.requestRefresh)
		approver = ap.q
	}
	ap.mm, err = models.New(models.Options{Store: st, Dir: st.ModelsDir(), HF: ap.hf, Log: log, Approver: approver,
		Started: func(p models.Prompt) {
			ap.tr.Notify("Downloading a model", ui.ApprovalLabel(p)+"\nRequested in the hub.")
		},
		OnChange: func(withModels bool) {
			if a := ap.aRef.Load(); a != nil {
				a.ModelsChanged(withModels)
			}
		}})
	if err != nil {
		return fmt.Errorf("model folder: %w", err)
	}

	cfg := agent.Config{
		Version: version, LlamaTag: llamaTag, SupportedArchs: enginelock.Pinned().Archs(), EngineDir: o.engine, LowIL: o.lowIL, MemLimit: memCap, LogicalCores: sys.Logical,
		Log: log, Models: ap.mm, Store: st,
		Hardware: func(ctx context.Context) protocol.Hardware {
			h, err := hw.Report(ctx, ap.mm.Dir())
			if err != nil {
				log.Warn("hardware report incomplete", "err", err.Error())
			}
			return h
		},
		GPUInUse: func(ctx context.Context, pid uint32) (bool, error) {
			pids, err := hw.ComputePIDs(ctx)
			return pids[pid], err
		},
		HFToken: func() string {
			ap.mu.Lock()
			defer ap.mu.Unlock()
			return string(ap.hfState)
		},
		GPUFree: func(ctx context.Context) (int64, error) {
			index := -1 // all GPUs
			if sel := st.Config().GPUSelect; len(sel) == 1 && sel[0] >= '0' && sel[0] <= '7' {
				index = int(sel[0] - '0')
			}
			return hw.GPUFree(ctx, index)
		},
		KeepAwake:    keepawake.New().Set,
		OnChange:     ap.requestRefresh,
		ResumedByHub: func() { ap.tr.Notify("Hosting resumed", "Sharing on this PC was resumed from the hub.") },
	}
	if o.model != "" {
		path, err := filepath.Abs(o.model)
		if err != nil || !strings.EqualFold(filepath.Ext(path), ".gguf") {
			return errors.New("--model must be a .gguf file")
		}
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			return errors.New("--model not found")
		}
		cfg.LocalModelPath, cfg.LocalModelID = path, o.modelID
		if cfg.LocalModelID == "" {
			cfg.LocalModelID = deriveID(filepath.Base(path))
		}
		if !protocol.ValidModelID(cfg.LocalModelID) {
			return errors.New("model id invalid")
		}
	}
	if ap.managed {
		up, err := updater.New(updater.Config{Root: ap.root, Version: version, Store: st, Keys: trust.Keys(), Base: o.releaseBase, Log: log,
			HubVersion: func() string {
				if a := ap.aRef.Load(); a != nil {
					return a.HubVersion()
				}
				return ""
			},
			StopEngine: func() {
				if a := ap.aRef.Load(); a != nil {
					a.Unload()
				}
			}})
		if err != nil {
			return err
		}
		// The signature of the installed release and the agent's own files, at every start (ruling B21).
		if err := up.Startup(); err != nil {
			return fmt.Errorf("install check: %w", err)
		}
		ap.up = up
		cfg.Updater, cfg.RolledBack, cfg.EngineCheck = up, up.RolledBack, up.VerifyEngine
		cfg.Restart = func() { ap.exit(layout.ExitRestart) }
		if cfg.EngineDir == "" {
			cfg.EngineDir = layout.EngineDir(ap.root, llamaTag)
		}
	}
	ap.a = agent.New(cfg)
	ap.aRef.Store(ap.a)
	if ap.up != nil {
		go ap.bootstrap()
		go ap.healthGate()
	}
	go ap.refreshLoop()
	go ap.checkToken()

	if stopWatch, err := power.Watch(func(e power.Event) {
		if e == power.Suspend {
			ap.a.Suspend()
		} else {
			ap.a.Resume()
		}
	}); err != nil {
		log.Warn("sleep and wake are not watched", "err", err.Error())
	} else {
		_ = stopWatch // lives as long as the process
	}

	log.Info("cragent starting", "version", version, "llama_tag", llamaTag, "home", st.Dir(), "models_dir", ap.mm.Dir(),
		"low_il", o.lowIL, "mem_cap_mb", memCap>>20, "paired", st.Paired(), "debug_build", debugBuild)

	switch {
	case o.key != "":
		ap.mu.Lock()
		ap.devKeyed = true
		ap.mu.Unlock()
		ap.a.Connect(ap.defaultHub, o.key)
	case o.pair == "":
		if !ap.a.ConnectStored() {
			log.Info("not paired: choose Pair with the hub in the tray menu")
		}
	}
	if o.load && cfg.LocalModelPath != "" {
		go func() {
			if _, err := ap.a.Load(context.Background(), cfg.LocalModelID, cfg.LocalModelPath, supervisor.DefaultSettings(sys.Physical)); err != nil {
				log.Error("load at start failed", "err", err.Error())
			}
		}()
	}
	return nil
}

func (ap *app) stop() {
	ap.a.Shutdown()
	ap.mm.Close()
}

func (ap *app) paired() bool {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.devKeyed || ap.st.Paired()
}

func (ap *app) requestRefresh() {
	select {
	case ap.refresh <- struct{}{}:
	default:
	}
}

// refreshLoop redraws the tray at most ten times a second.
func (ap *app) refreshLoop() {
	for range ap.refresh {
		ap.tr.Update(ap.trayState())
		time.Sleep(100 * time.Millisecond)
	}
}

func (ap *app) trayState() tray.State {
	paired := ap.paired()
	in := ap.a.Snapshot(paired)
	text, icon := status.Line(in)
	s := tray.State{Status: text, Icon: icon, Paired: paired, Paused: in.Paused, ModelsDir: ap.mm.Dir(), Version: version}
	if paired {
		s.Slug = ap.st.Config().Slug
	}
	if ap.q != nil {
		s.Pending = ap.q.Pending()
	}
	ap.mu.Lock()
	s.HFToken = string(ap.hfState)
	ap.mu.Unlock()
	return s
}

// checkToken learns whether the stored token is accepted; the hub hears only present, absent or invalid.
func (ap *app) checkToken() {
	ap.mu.Lock()
	tok := ap.hfToken
	ap.mu.Unlock()
	state := hf.TokenAbsent
	if tok != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		got, err := ap.hf.CheckToken(ctx, tok)
		if err != nil {
			got = hf.TokenPresent // not checkable now (offline): it is there; a download will tell
		}
		state = got
	}
	ap.mu.Lock()
	ap.hfState = state
	ap.mu.Unlock()
	ap.requestRefresh()
}

// pair runs the exchange for what the host typed and returns the machine's name.
func (ap *app) pair(input string, waiting func()) (string, error) {
	hub, code, err := pairing.ParseInput(input, ap.defaultHub)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	res, err := ap.a.PairWith(ctx, hub, code, nil, waiting)
	if err != nil {
		return "", err
	}
	ap.mu.Lock()
	ap.devKeyed = false
	ap.mu.Unlock()
	ap.requestRefresh()
	return res.Slug, nil
}

func pairingMessage(err error) string {
	var pe *pairing.Error
	if errors.As(err, &pe) {
		return pe.Message()
	}
	return "Pairing failed. Try again."
}

// onAction handles a choice from the tray. It runs on its own goroutine.
func (ap *app) onAction(a tray.Action, arg string) {
	switch a {
	case tray.ActPauseToggle:
		ap.a.SetPaused(!ap.a.Paused(), true)
	case tray.ActPair:
		input, ok := ui.AskPairingCode()
		if !ok || input == "" {
			return
		}
		slug, err := ap.pair(input, func() {
			ap.tr.Notify("Finish pairing in the hub", "This PC was paired before. In the hub, choose Replace key or a new name.")
		})
		if err != nil {
			ap.log.Warn("pairing failed", "why", pairingMessage(err))
			ui.Error(pairingMessage(err))
			return
		}
		ui.Info("This PC is now paired with the hub as " + slug + ".")
	case tray.ActHFToken:
		ap.setToken()
	case tray.ActModelFolder:
		ap.setFolder()
	case tray.ActOpenHub:
		hub := ap.st.Config().Hub
		if hub == "" {
			hub = ap.defaultHub
		}
		if u := link.OpenURL(hub); u != "" {
			_ = ui.Open(u)
		}
	case tray.ActOpenLog:
		_ = ui.Open(ap.st.LogPath())
	case tray.ActAbout:
		c := ap.st.Config()
		text := "CR Agent " + version + "\nllama.cpp " + llamaTag
		if c.Slug != "" {
			text += "\nThis PC: " + c.Slug + " (key id " + c.KeyID + ")"
		}
		if ap.up != nil && ap.up.Unverified() {
			text += "\nThis version's signed manifest could not be checked yet (offline, or the release is not published). Trying again."
		}
		ui.Info(text)
	case tray.ActQuit:
		ap.tr.Close()
	case tray.ActPending:
		if ap.q != nil {
			ap.q.Open(arg)
		}
	case tray.ActNotification:
		if ap.q != nil {
			ap.q.Open("")
		}
	}
}

func (ap *app) setToken() {
	tok, ok := ui.AskHFToken()
	if !ok {
		return
	}
	if tok == "" {
		if err := ap.st.DeleteSecret(store.SecretHFToken); err != nil {
			ui.Error("The token could not be removed.")
			return
		}
		ap.mu.Lock()
		ap.hfToken, ap.hfState = "", hf.TokenAbsent
		ap.mu.Unlock()
		ap.requestRefresh()
		ui.Info("The Hugging Face token was removed from this PC.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	state, err := ap.hf.CheckToken(ctx, tok)
	switch {
	case err != nil:
		ui.Error("Hugging Face could not be reached to check the token. Nothing was saved.")
		return
	case state == hf.TokenInvalid:
		ui.Error("Hugging Face did not accept that token. Nothing was saved.")
		return
	}
	if err := ap.st.WriteSecret(store.SecretHFToken, []byte(tok)); err != nil {
		ui.Error("The token could not be saved on this PC.")
		return
	}
	ap.mu.Lock()
	ap.hfToken, ap.hfState = tok, hf.TokenPresent
	ap.mu.Unlock()
	ap.log.Info("hugging face token saved") // never the token
	ap.requestRefresh()
	ui.Info("The token was saved on this PC. It is sent only to huggingface.co, never to the hub.")
}

func (ap *app) setFolder() {
	dir, ok := ui.PickFolder(ap.mm.Dir())
	if !ok {
		return
	}
	if in := ap.a.Snapshot(true); in.Engine != protocol.EngineUnloaded && in.Engine != protocol.EngineError {
		ui.Error("A model is loaded. Unload it in the hub (or pause hosting) before changing the model folder.")
		return
	}
	if err := ap.mm.SetDir(dir); err != nil {
		ui.Error("That folder cannot be used: " + err.Error() + ".")
		return
	}
	if err := ap.st.Update(func(c *store.Config) { c.ModelsDir = ap.mm.Dir() }); err != nil {
		ap.log.Warn("model folder not saved", "err", err.Error())
	}
	ap.log.Info("model folder changed", "models_dir", ap.mm.Dir())
	ap.requestRefresh()
	ui.Info("The model folder is now " + ap.mm.Dir() + ".\nFiles in the old folder were left where they are.")
}

// allowAll is the debug build's --approve=allow.
type allowAll struct{}

func (allowAll) Ask(context.Context, models.Prompt) bool { return true }

// logWriter writes each line to the log file and, best effort, to stderr.
type logWriter struct{ f *os.File }

func (w logWriter) Write(p []byte) (int, error) {
	if w.f != nil {
		w.f.Write(p)
	}
	os.Stderr.Write(p) // fails silently in a build without a console
	return len(p), nil
}

// openLog opens the log for appending; a log over 10 MB is kept as agent.log.1 first.
func openLog(path string) *os.File {
	if st, err := os.Stat(path); err == nil && st.Size() > 10<<20 {
		os.Remove(path + ".1")
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// deriveID lowercases the file name without .gguf and keeps [a-z0-9._-].
func deriveID(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.TrimLeft(b.String(), "._-")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
