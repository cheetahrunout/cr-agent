// Command cragent is the M0 console build of the CR agent (no tray yet). It logs JSON lines to stderr.
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

	"github.com/cheetahrunout/cr-agent/internal/agent"
	"github.com/cheetahrunout/cr-agent/internal/hw"
	"github.com/cheetahrunout/cr-agent/internal/link"
	"github.com/cheetahrunout/cr-agent/internal/protocol"
	"github.com/cheetahrunout/cr-agent/internal/supervisor"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = "0.0.1"

// llamaTag is the pinned llama.cpp build (engine.lock in later milestones).
const llamaTag = "b12125"

func main() {
	os.Exit(run())
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run() int {
	hub := flag.String("hub", env("CRAGENT_HUB", "wss://"+link.ProductionHost+"/agent"), "hub WebSocket URL")
	key := flag.String("key", "", "machine key crm_<id>_<secret> (or env CRAGENT_KEY)")
	engineDir := flag.String("engine", env("CRAGENT_ENGINE", ""), "folder with llama-server.exe")
	model := flag.String("model", env("CRAGENT_MODEL", ""), "M0 only: path to a local .gguf")
	modelID := flag.String("model-id", "", "model id for the local file (default: derived from the file name)")
	load := flag.Bool("load", false, "load the model at start")
	lowIL := flag.Bool("low-il", false, "start the engine with a low-integrity token (falls back if CUDA fails)")
	noMemCap := flag.Bool("no-mem-cap", false, "do not cap the engine's job memory at RAM - 4 GiB")
	debug := flag.Bool("v", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if *key == "" {
		*key = os.Getenv("CRAGENT_KEY")
	}
	hubURL, err := link.ValidateHubURL(*hub)
	if err != nil {
		log.Error("hub URL refused", "err", err.Error())
		return 2
	}
	if !validKey(*key) {
		log.Error("machine key missing or malformed (want crm_<id>_<secret>)")
		return 2
	}
	if *engineDir == "" || *model == "" {
		log.Error("--engine and --model are required in M0")
		return 2
	}
	modelPath, err := filepath.Abs(*model)
	if err != nil || !strings.EqualFold(filepath.Ext(modelPath), ".gguf") {
		log.Error("--model must be a .gguf file")
		return 2
	}
	if st, err := os.Stat(modelPath); err != nil || st.IsDir() {
		log.Error("--model not found")
		return 2
	}
	id := *modelID
	if id == "" {
		id = deriveID(filepath.Base(modelPath))
	}
	if !protocol.ValidModelID(id) {
		log.Error("model id invalid", "model_id", id)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sys, err := hw.QuerySystem(filepath.Dir(modelPath))
	if err != nil {
		log.Warn("system probe incomplete", "err", err.Error())
	}
	var memCap uint64
	if !*noMemCap && sys.RAMTotalMB > 8192 {
		memCap = uint64(sys.RAMTotalMB-4096) << 20 // ruling B15: RAM - 4 GiB
	}
	a := agent.New(agent.Config{
		Version: version, LlamaTag: llamaTag, EngineDir: *engineDir, ModelPath: modelPath, ModelID: id,
		LowIL: *lowIL, MemLimit: memCap, Log: log,
		Limits: supervisor.Limits{LogicalCores: sys.Logical},
		Hardware: func(ctx context.Context) protocol.Hardware {
			h, err := hw.Report(ctx, filepath.Dir(modelPath))
			if err != nil {
				log.Warn("hardware report incomplete", "err", err.Error())
			}
			return h
		},
		GPUInUse: func(ctx context.Context, pid uint32) (bool, error) {
			pids, err := hw.ComputePIDs(ctx)
			return pids[pid], err
		},
	})
	log.Info("cragent starting", "version", version, "llama_tag", llamaTag, "model_id", id, "low_il", *lowIL, "mem_cap_mb", memCap>>20)

	if *load {
		go func() {
			if _, err := a.Load(ctx, supervisor.DefaultSettings(sys.Physical)); err != nil {
				log.Error("load at start failed", "err", err.Error())
			}
		}()
	}

	c := link.New(link.Config{URL: hubURL, Key: *key, Handler: a, Log: log})
	err = c.Run(ctx)
	a.Unload()
	switch {
	case errors.Is(err, link.ErrRevoked):
		fmt.Fprintln(os.Stderr, "This PC was removed from the hub.")
		return 3
	case errors.Is(err, context.Canceled):
		log.Info("stopped")
		return 0
	default:
		log.Error("link ended", "err", fmt.Sprint(err))
		return 1
	}
}

func validKey(k string) bool {
	parts := strings.Split(k, "_")
	return len(parts) == 3 && parts[0] == "crm" && parts[1] != "" && parts[2] != ""
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
