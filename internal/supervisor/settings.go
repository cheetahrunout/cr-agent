// Package supervisor starts, watches and stops llama-server. The agent builds every argument itself
// from bounded settings (ruling B5, §9.2, §12.3); nothing in argv comes from the hub as text except
// the model id used as the alias, which the protocol has already restricted to [a-z0-9._-].
package supervisor

import (
	"fmt"
	"strconv"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Limits are the machine-specific bounds the agent enforces on top of the schema (ruling B5).
type Limits struct {
	LogicalCores int // threads ≤ this
	NLayer       int // gpu_layers ≤ NLayer+1, n_cpu_moe ≤ NLayer; 0 = unknown (schema cap 1024)
	NCtxTrain    int // ctx ≤ min(NCtxTrain, 262144); 0 = unknown
}

// CheckSettings refuses (never clamps) an out-of-bounds value. The returned error names the field so
// the caller can answer cmd.err{bad_setting, field}.
func CheckSettings(s protocol.Settings, lim Limits) *protocol.FieldError {
	if fe := s.Validate(); fe != nil {
		return fe
	}
	maxCtx := 262144
	if lim.NCtxTrain > 0 && lim.NCtxTrain < maxCtx {
		maxCtx = lim.NCtxTrain
	}
	if s.Ctx > maxCtx {
		return &protocol.FieldError{Field: "ctx", Msg: fmt.Sprintf("must be 4096..%d", maxCtx)}
	}
	if lim.LogicalCores > 0 && s.Threads > lim.LogicalCores {
		return &protocol.FieldError{Field: "threads", Msg: fmt.Sprintf("must be 1..%d", lim.LogicalCores)}
	}
	if lim.NLayer > 0 {
		if !s.GPULayers.All && s.GPULayers.N > lim.NLayer+1 {
			return &protocol.FieldError{Field: "gpu_layers", Msg: fmt.Sprintf("must be \"all\" or 0..%d", lim.NLayer+1)}
		}
		if s.NCPUMoE != nil && *s.NCPUMoE > lim.NLayer {
			return &protocol.FieldError{Field: "n_cpu_moe", Msg: fmt.Sprintf("must be 0..%d", lim.NLayer)}
		}
	}
	// -np is fixed at 1 in v1 (ruling I2); the 1..4 bound is kept for later but not reachable.
	if s.Parallel != nil && *s.Parallel != 1 {
		return &protocol.FieldError{Field: "parallel", Msg: "must be 1 in this version"}
	}
	return nil
}

// DefaultSettings are the M0 defaults when the agent loads at start (--load) without the hub.
func DefaultSettings(physicalCores int) protocol.Settings {
	t := physicalCores
	if t < 1 {
		t = 4
	}
	if t > 16 {
		t = 16
	}
	one := 1
	return protocol.Settings{Ctx: 4096, GPULayers: protocol.AllLayers(), Threads: t, KVType: "f16", FlashAttn: "on", Parallel: &one}
}

// Launch is everything argv is built from.
type Launch struct {
	ModelPath string // a file the agent itself resolved; never a hub string
	Alias     string // the model id (ruling B7)
	Port      int
	APIKey    string
	Settings  protocol.Settings
}

// BuildArgs returns llama-server's arguments (without argv[0]). Fixed by the agent (§12.3): --jinja,
// 127.0.0.1, random port, per-launch --api-key, --no-webui, --no-slots, --reasoning-format deepseek,
// --fit off, no --metrics, no verbose flags, no file arguments except -m.
func BuildArgs(l Launch) []string {
	s := l.Settings
	ngl := "999"
	if !s.GPULayers.All {
		ngl = strconv.Itoa(s.GPULayers.N)
	}
	args := []string{
		"-m", l.ModelPath,
		"--alias", l.Alias,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(l.Port),
		"--api-key", l.APIKey,
		"--jinja",
		"--no-webui",
		"--no-slots",
		"--cors-origins", "http://127.0.0.1:" + strconv.Itoa(l.Port), // only the engine's own origin: no web page may call it
		"--no-cors-credentials",
		"--reasoning-format", "deepseek",
		"--fit", "off",
		"-c", strconv.Itoa(s.Ctx),
		"-ngl", ngl,
		"-t", strconv.Itoa(s.Threads),
		"-ctk", s.KVType,
		"-ctv", s.KVType,
		"-fa", s.FlashAttn,
		"-np", "1",
	}
	if s.NCPUMoE != nil && *s.NCPUMoE > 0 {
		args = append(args, "--n-cpu-moe", strconv.Itoa(*s.NCPUMoE))
	}
	if s.ChatTemplate != "" && protocol.BuiltinTemplates[s.ChatTemplate] {
		args = append(args, "--chat-template", s.ChatTemplate)
	}
	return args
}
