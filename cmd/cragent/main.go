// Command cragent is the CR agent: one tray icon on the host's PC, one outbound WebSocket to the hub.
// It logs JSON lines to <home>\logs\agent.log (and to stderr when started from a console).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cragentroot "github.com/cheetahrunout/cr-agent"
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

// devEnv reads a variable that only a debug build listens to (finding L7).
func devEnv(name string) string {
	if debugBuild {
		return os.Getenv(name)
	}
	return ""
}

// debugOnlyFlags names the flags set that only a debug build accepts. In a release build the engine
// comes from the install layout, the key from the pairing, the model from the hub's model list, and
// the sandbox is not a flag (finding L7).
func debugOnlyFlags(o options) []string {
	var set []string
	add := func(on bool, name string) {
		if on {
			set = append(set, name)
		}
	}
	add(o.approve != "ask", "--approve")
	add(o.hfBase != "", "--hf-base")
	add(o.releaseBase != "", "--release-base")
	add(o.engine != "", "--engine")
	add(o.key != "", "--key")
	add(o.model != "", "--model")
	add(o.modelID != "", "--model-id")
	add(o.load, "--load")
	add(o.noMemCap, "--no-mem-cap")
	return set
}

type options struct {
	home, hub, key, engine, model, modelID, modelsDir, pair, hfBase, approve, releaseBase string
	load, noMemCap, noTray, unpair, verbose                                               bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.home, "home", env("CRAGENT_HOME", ""), "folder for config, state and logs (default %LOCALAPPDATA%\\cr-agent)")
	flag.StringVar(&o.hub, "hub", env("CRAGENT_HUB", "wss://"+link.ProductionHost+"/agent"), "hub WebSocket URL a bare pairing code is used with")
	flag.StringVar(&o.pair, "pair", "", "pair with the hub using this code or pairing string, then run")
	flag.BoolVar(&o.unpair, "unpair", false, "forget this PC's machine key and exit")
	flag.StringVar(&o.engine, "engine", devEnv("CRAGENT_ENGINE"), "debug builds only: folder with llama-server.exe")
	flag.StringVar(&o.modelsDir, "models-dir", "", "set the model folder (kept in the config)")
	flag.BoolVar(&o.noMemCap, "no-mem-cap", false, "debug builds only: do not cap the engine's job memory at RAM - 4 GiB")
	flag.BoolVar(&o.noTray, "no-tray", false, "run as a console program without the tray icon")
	flag.BoolVar(&o.verbose, "v", false, "debug logging")
	// Development aids.
	flag.StringVar(&o.key, "key", "", "debug builds only: connect with this machine key instead of the stored pairing (or env CRAGENT_KEY)")
	flag.StringVar(&o.model, "model", devEnv("CRAGENT_MODEL"), "debug builds only: a local .gguf outside the model folder")
	flag.StringVar(&o.modelID, "model-id", "", "debug builds only: model id for --model (default: from the file name)")
	flag.BoolVar(&o.load, "load", false, "debug builds only: load --model at start")
	flag.StringVar(&o.hfBase, "hf-base", "", "debug builds only: a local stand-in for huggingface.co")
	flag.StringVar(&o.releaseBase, "release-base", "", "debug builds only: a local stand-in for the GitHub release downloads")
	flag.StringVar(&o.approve, "approve", "ask", "debug builds only: ask | allow | deny for downloads outside the publisher allowlist")
	flag.Parse()
	if o.key == "" {
		o.key = devEnv("CRAGENT_KEY")
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
	adding     atomic.Bool // the Add model files dialog or its import is running
	q          *approve.Queue
	defaultHub string

	mu       sync.Mutex
	hfToken  string
	hfState  hf.TokenState
	devKeyed bool
	refresh  chan struct{}

	available latestBox // the version the daily check offers, "" for none
	root      string    // the install folder, when this agent runs from one
	managed   bool
	up        *updater.Updater
	trayUp    atomic.Bool
	quit      chan struct{}
	code      int
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
		// A build without a console has no other way to say this. Nothing retries it: the file stays damaged.
		if !o.noTray {
			ui.Error(openFailure(home, err))
		}
		return 2
	}
	logFile := newLogWriter(st.LogPath())
	defer logFile.Close()
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if set := debugOnlyFlags(o); !debugBuild && len(set) > 0 {
		log.Error("these flags exist only in debug builds", "flags", strings.Join(set, " "))
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
		ptr, perr := layout.ReadPointer(root)
		text, code := startFailure(err, managed, managed && perr == nil && ptr.PendingSince != nil)
		if text != "" && !o.noTray {
			ui.Error(text)
		}
		return code
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
			_ = slug
			fmt.Fprintln(os.Stderr, "Paired.")
		}
	}

	if !o.noTray {
		ready := make(chan struct{})
		go func() {
			<-ready
			ap.trayUp.Store(true)
			ap.requestRefresh()
			ap.noticeModelFiles()
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

// latestBox holds a string for the tray, set by the daily check.
type latestBox struct{ v atomic.Pointer[string] }

func (b *latestBox) Store(s string) { b.v.Store(&s) }
func (b *latestBox) Load() string {
	if p := b.v.Load(); p != nil {
		return *p
	}
	return ""
}

// startFailure decides what a failed start means to the host. The exit code tells the launcher what
// to do: 1 is retried a few times, 2 is not. Only release state that cannot be read needs a reinstall:
// anything else may pass (a model drive that is not connected yet), so the launcher retries, and it
// tells the host itself if it gives up. A version on trial just fails: the launcher reverts it.
func startFailure(err error, managed, onTrial bool) (text string, code int) {
	switch {
	case onTrial:
		return "", 1
	case errors.Is(err, updater.ErrStateUnreadable):
		return "CR Agent cannot read its release state on this PC, so it cannot tell which versions are safe to run.\n\nUninstall CR Agent and install it again. Your models stay where they are.", 2
	case managed:
		return "", 1 // the launcher retries, and tells the host if it gives up
	default: // no launcher to retry or to tell the host
		return "CR Agent cannot start: " + err.Error() + ".", 1
	}
}

// openFailure is what the host reads when the agent's folder or settings cannot be opened.
func openFailure(home string, err error) string {
	return "CR Agent cannot read its settings in " + home + ":\n" + err.Error() +
		".\n\nIf config.json is damaged, move it out of that folder and start CR Agent again. You may then have to pair this PC again."
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

// pollLatest is the daily check of the signed latest release (§13, ruling B9): the hub has no part in
// it, so a hacked hub cannot hide an update.
func (ap *app) pollLatest() {
	time.Sleep(time.Minute)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		v, err := ap.up.CheckLatest(ctx)
		cancel()
		if err != nil {
			ap.log.Info("the daily check of the latest release found nothing usable", "err", err.Error())
		} else {
			ap.log.Info("daily check of the latest release", "latest", v, "offered", ap.up.UpdateAvailable())
		}
		ap.available.Store(ap.up.UpdateAvailable())
		ap.requestRefresh()
		time.Sleep(latestDelay(rand.Int64N(int64(time.Hour))))
	}
}

// latestDelay is a day and some jitter, so PCs do not all ask at one moment.
func latestDelay(jitter int64) time.Duration { return 24*time.Hour + time.Duration(jitter) }

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
	ap.hfToken = storedToken(st, log)
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
		Version: version, LlamaTag: llamaTag, SupportedArchs: enginelock.Pinned().Archs(), EngineDir: o.engine, ModelGrant: ap.modelGrant, ModelGranted: ap.recordGrant, ModelFolderCheck: ap.modelFolderCheck, MemLimit: memCap, LogicalCores: sys.Logical,
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
		LatestVerified: func() string {
			if ap.up != nil {
				return ap.up.LatestVerified()
			}
			return ""
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
		cfg.EngineDir = layout.EngineDir(ap.root, llamaTag)
	}
	// Whatever a model left in the container's profile between loads goes now, before any engine runs.
	supervisor.ScopeContainer(st.Dir())
	if err := supervisor.ResetSandbox(); err != nil {
		log.Warn("the engine's sandbox could not be reset", "err", err.Error())
	}
	ap.a = agent.New(cfg)
	ap.aRef.Store(ap.a)
	if ap.up != nil {
		ap.available.Store(ap.up.UpdateAvailable())
		go ap.bootstrap()
		go ap.healthGate()
		go ap.pollLatest()
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
		"isolation", ap.a.Level().String(), "mem_cap_mb", memCap>>20, "paired", st.Paired(), "debug_build", debugBuild)

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
	s := tray.State{Status: text, Icon: icon, Paired: paired, Paused: in.Paused, ModelsDir: ap.mm.Dir(), Version: version,
		UpdateAvailable: ap.available.Load(), SandboxBlocked: ap.a.SandboxBlocked(), LowIntegrity: ap.a.Level() == supervisor.LevelLowIntegrity}
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
		_ = slug
		ui.Info("This PC is now paired with the hub.")
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
	case tray.ActChangelog:
		// A plain file beside the log, so it opens in the host's own text viewer and goes with the logs on uninstall.
		path := filepath.Join(filepath.Dir(ap.st.LogPath()), "changelog.txt")
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(cragentroot.Changelog, "\n", "\r\n")), 0o600); err != nil {
			ui.Error("The list of changes could not be written: " + err.Error())
			return
		}
		_ = ui.Open(path)
	case tray.ActAbout:
		c := ap.st.Config()
		text := "CR Agent " + version + "\nllama.cpp " + llamaTag + "\n" + isolationText(ap.a.Level())
		for _, k := range trust.Keys() {
			text += "\nRelease key id: " + k.ID.String()
		}
		if c.KeyID != "" {
			text += "\nThis PC's machine key id: " + c.KeyID
		}
		if ap.up != nil && ap.up.Unverified() {
			text += "\nThis version's signed manifest could not be checked yet (offline, or the release is not published). Trying again."
		}
		ui.Info(text)
	case tray.ActInstallUpdate:
		if !layout.ValidVersion(arg) || !ui.Confirm("Install CR Agent "+arg+"?\n\nIt was checked against the release key. The agent restarts, and any answer running now stops.", "Install") {
			return
		}
		if err := ap.a.InstallUpdate(arg); err != nil {
			ui.Error("The update did not install: " + err.Error() + ".")
		}
	case tray.ActSandbox:
		ap.toggleSandbox()
	case tray.ActAddModels:
		ap.addModels()
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

func (ap *app) engineSandboxed() bool { return ap.a.Level() == supervisor.LevelAppContainer }

// isolationText says the engine's true level, for the About window.
func isolationText(l supervisor.Level) string {
	if l == supervisor.LevelAppContainer {
		return "Engine isolation: AppContainer"
	}
	return "Engine isolation: low integrity, your choice on this PC. The engine can read your files."
}

// lowIntegrityQuestion says plainly what the opt-in gives up.
func lowIntegrityQuestion() string {
	return "The engine's sandbox does not work on this PC.\n\n" +
		"At low integrity the engine still cannot change your files. But a malicious model that attacks the engine could read your files, " +
		"including the keys CR Agent keeps on this PC, and could connect to the internet.\n\n" +
		"Allow this only if you trust the models you load.\n\nRun the engine at low integrity on this PC?"
}

// modelGrant is the model folder the agent may let the sandbox read at a load: <home>\models, or the one
// the host allowed by a click in this agent, if it may still be used. It changes the permissions of no other folder.
func (ap *app) modelGrant() string {
	dir := ap.mm.Dir()
	if ap.modelFolderCheck() != nil {
		return ""
	}
	if supervisor.IsOwnModelFolder(dir, ap.st.Dir()) || strings.EqualFold(ap.st.Config().EngineReadFolder, dir) {
		return dir
	}
	return ""
}

func (ap *app) modelFolderCheck() error { return supervisor.CheckModelFolder(ap.mm.Dir(), ap.st.Dir()) }

// storedToken is the Hugging Face token the host stored through the tray. There is no other way in: the
// sandbox can read HKCU\Environment, so a token in an environment variable would be open to a model.
func storedToken(st *store.Store, log *slog.Logger) string {
	b, err := st.ReadSecret(store.SecretHFToken)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Warn("the stored Hugging Face token cannot be read", "err", err.Error())
		}
		return ""
	}
	return string(b)
}

func (ap *app) recordGrant(path string) {
	if err := recordGrant(ap.st, path); err != nil {
		ap.log.Warn("a read entry could not be recorded", "err", err.Error())
	}
}

// recordGrant remembers a path the agent itself added the sandbox's read entry to.
func recordGrant(st *store.Store, path string) error {
	return st.Update(func(c *store.Config) {
		for _, p := range c.EngineGrants {
			if strings.EqualFold(p, path) {
				return
			}
		}
		c.EngineGrants = append(c.EngineGrants, path)
	})
}

// revokeGrants takes away the read entries the agent added in a folder it no longer uses. An entry it
// did not add is not in the record, so a host's own is never touched.
func revokeGrants(st *store.Store, dir string, log *slog.Logger) {
	clean := strings.ToLower(filepath.Clean(dir))
	var keep []string
	for _, p := range st.Config().EngineGrants {
		cp := strings.ToLower(filepath.Clean(p))
		if cp != clean && !strings.HasPrefix(cp, clean+string(filepath.Separator)) {
			keep = append(keep, p)
			continue
		}
		if err := supervisor.RevokeModelGrant(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("a read entry could not be taken away", "err", err.Error())
		}
	}
	if err := st.Update(func(c *store.Config) { c.EngineGrants = keep }); err != nil {
		log.Warn("the read entries could not be recorded", "err", err.Error())
	}
}

func modelFolderQuestion(dir string) string {
	return "The engine runs in a sandbox that cannot read this folder unless you allow it:\n" + dir +
		"\n\nAllowing adds one read-only permission to the folder, for sandboxed programs on this PC, the engine among them. Your other programs gain nothing." +
		"\n\nAllow only a folder that holds nothing but models: a model that attacks the engine could read what is in it."
}

// noticeModelFiles says, once at start and passively, that the model folder holds files the host could
// add. It offers no button and starts nothing: the tray's Add model files… is the only way in.
func (ap *app) noticeModelFiles() {
	if ap.mm == nil {
		return
	}
	if ap.modelFolderCheck() != nil {
		ap.tr.Notify("Your model folder cannot be used", "It holds more than models. Make a subfolder just for models, and choose it under Model folder in the tray menu.")
		return
	}
	// An earlier version had no sandbox to ask for: a folder the host chose then needs this one click.
	if ap.engineSandboxed() && ap.modelGrant() == "" && !supervisor.CanEngineRead(ap.mm.Dir()) {
		ap.tr.Notify("The engine cannot read your model folder", "Choose Model folder in the tray menu and pick it again to allow this.")
		return
	}
	s := ap.mm.Scan()
	if n := len(s.Found) + len(s.Refused); n > 0 { // a refused file also needs the host: the dialog says why
		ap.tr.Notify("Model files found", modelFilesNotice(n))
	}
}

func modelFilesNotice(n int) string {
	what := "1 model file is"
	if n > 1 {
		what = fmt.Sprintf("%d model files are", n)
	}
	return what + " in the model folder but not added. To use them, choose Add model files in the tray menu."
}

// addModels is the host's own import of GGUF files from the model folder (ruling B64). Nothing from
// the hub leads here. onAction runs on its own goroutine, so the hash blocks neither tray nor link.
func (ap *app) addModels() {
	if !ap.adding.CompareAndSwap(false, true) {
		return
	}
	defer ap.adding.Store(false)
	s := ap.mm.Scan()
	if len(s.Found) == 0 {
		ui.Info(ui.NothingToAddText(s))
		return
	}
	if len(s.Refused) > 0 {
		ui.Info(ui.RefusedText(s))
	}
	keys, ok := ui.PickModelFiles(s)
	if !ok || !ui.Confirm(ui.ConfirmAddText(s, keys), "Add") {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := ui.NewProgress(cancel)
	importAndTell(ctx, ap.mm, s.Picks(keys), p.Set, func() {
		p.Close()
		ap.requestRefresh()
	}, ui.Info)
}

// importAndTell always tells the host what happened: a cancel keeps the files finished before it, so
// even a cancelled import has something to say.
func importAndTell(ctx context.Context, mm *models.Manager, picks []models.Found, progress models.ImportProgress, finished func(), tell func(string)) {
	results := mm.Import(ctx, picks, progress)
	finished()
	tell(ui.ImportResultText(results))
}

// toggleSandbox is the host's own choice, on this PC, about running the engine outside its
// low-integrity sandbox when CUDA does not work inside it. Nothing from the hub leads here.
func (ap *app) toggleSandbox() {
	low := ap.a.Level() == supervisor.LevelAppContainer
	if low && !ui.Confirm(lowIntegrityQuestion(), "Use low integrity") {
		return
	}
	if err := ap.st.Update(func(c *store.Config) { c.EngineLowIntegrity = low }); err != nil {
		ui.Error("The choice could not be saved on this PC.")
		return
	}
	ap.a.AllowLowIntegrity(low)
	ap.log.Warn("the host changed the engine's isolation", "isolation", ap.a.Level().String())
	text := "This takes effect the next time a model loads."
	if !low && ap.modelGrant() == "" && !supervisor.CanEngineRead(ap.mm.Dir()) {
		text += "\n\nThe sandbox cannot read your model folder yet: choose Model folder in the tray menu and pick it again to allow this."
	}
	ui.Info(text)
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
	if err := supervisor.CheckModelFolder(dir, ap.st.Dir()); err != nil {
		ui.Error(err.Error())
		return
	}
	// The engine's sandbox can read a folder only if the folder says so, and this one is the host's.
	own := supervisor.IsOwnModelFolder(dir, ap.st.Dir())
	readable := own || supervisor.CanEngineRead(dir)
	added := false
	if !readable && ap.engineSandboxed() {
		if !ui.Confirm(modelFolderQuestion(dir), "Allow") {
			return
		}
		var err error
		if added, err = supervisor.GrantModelFolder(dir); err != nil {
			ui.Error("The engine cannot be allowed to read that folder: " + err.Error() + ".")
			return
		}
		readable = true
	}
	old := ap.mm.Dir()
	if err := ap.mm.SetDir(dir); err != nil {
		if added {
			_ = supervisor.RevokeModelGrant(dir)
		}
		ui.Error("That folder cannot be used: " + err.Error() + ".")
		return
	}
	if err := ap.st.Update(func(c *store.Config) {
		c.ModelsDir = ap.mm.Dir()
		c.EngineReadFolder = ""
		if !own && readable { // never without the host's click: a later load may renew this folder's entry
			c.EngineReadFolder = ap.mm.Dir()
		}
	}); err != nil {
		ap.log.Warn("model folder not saved", "err", err.Error())
	}
	if added {
		ap.recordGrant(dir)
	}
	if !strings.EqualFold(filepath.Clean(old), filepath.Clean(ap.mm.Dir())) {
		revokeGrants(ap.st, old, ap.log)
	}
	ap.log.Info("model folder changed", "models_dir", ap.mm.Dir())
	ap.requestRefresh()
	ui.Info("The model folder is now " + ap.mm.Dir() + ".\nFiles in the old folder were left where they are.")
}

// allowAll is the debug build's --approve=allow.
type allowAll struct{}

func (allowAll) Ask(context.Context, models.Prompt) bool { return true }

// maxLogSize is when the log moves to agent.log.1, while the agent runs too: a talkative hub must
// not be able to fill the disk (finding L4).
const maxLogSize = 10 << 20

// logWriter writes each line to the log file and, best effort, to stderr.
type logWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// newLogWriter opens the log for appending. The agent runs on without a log if that fails.
func newLogWriter(path string) *logWriter {
	w := &logWriter{path: path}
	w.open()
	return w
}

func (w *logWriter) open() {
	if st, err := os.Stat(w.path); err == nil && st.Size() > maxLogSize {
		os.Remove(w.path + ".1")
		os.Rename(w.path, w.path+".1")
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		w.f, w.size = nil, 0
		return
	}
	w.f, w.size = f, 0
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		if w.size+int64(len(p)) > maxLogSize {
			w.f.Close()
			if st, err := os.Stat(w.path); err == nil && st.Size() > 0 {
				os.Remove(w.path + ".1")
				os.Rename(w.path, w.path+".1")
			}
			w.open()
		}
		if w.f != nil {
			n, _ := w.f.Write(p)
			w.size += int64(n)
		}
	}
	os.Stderr.Write(p) // fails silently in a build without a console
	return len(p), nil
}

func (w *logWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
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
