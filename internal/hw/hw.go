// Package hw builds the hardware report of hello and hw: GPUs from nvidia-smi (by its System32 path,
// ruling B17), RAM, CPU, disk and OS from Windows APIs.
package hw

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// GPU is one row of nvidia-smi.
type GPU struct {
	Name    string
	TotalMB int64
	UsedMB  int64
	Driver  string
}

// ParseNvidiaSMI parses `--query-gpu=name,memory.total,memory.used,driver_version --format=csv,noheader,nounits`.
func ParseNvidiaSMI(out string) ([]GPU, error) {
	r := csv.NewReader(strings.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 4
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var gpus []GPU
	for _, rec := range recs {
		total, err1 := strconv.ParseInt(strings.TrimSpace(rec[1]), 10, 64)
		used, err2 := strconv.ParseInt(strings.TrimSpace(rec[2]), 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("nvidia-smi: bad memory values %q", rec)
		}
		gpus = append(gpus, GPU{Name: strings.TrimSpace(rec[0]), TotalMB: total, UsedMB: used, Driver: strings.TrimSpace(rec[3])})
	}
	return gpus, nil
}

// QueryGPUs runs nvidia-smi.
func QueryGPUs(ctx context.Context) ([]GPU, error) {
	path, err := nvidiaSMIPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--query-gpu=name,memory.total,memory.used,driver_version", "--format=csv,noheader,nounits")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return ParseNvidiaSMI(string(out))
}

// ComputePIDs lists the PIDs nvidia-smi reports as holding a CUDA context. M0 uses it to see whether
// the engine really runs on the GPU (this llama.cpp build prints no CUDA lines at default verbosity).
func ComputePIDs(ctx context.Context) (map[uint32]bool, error) {
	path, err := nvidiaSMIPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--query-compute-apps=pid", "--format=csv,noheader,nounits")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	pids := map[uint32]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if n, err := strconv.ParseUint(strings.TrimSpace(l), 10, 32); err == nil {
			pids[uint32(n)] = true
		}
	}
	return pids, nil
}

// Report builds the hardware object. diskPath is the folder whose volume is reported (the model folder).
func Report(ctx context.Context, diskPath string) (protocol.Hardware, error) {
	h := protocol.Hardware{Pools: []protocol.Pool{}}
	gpus, gerr := QueryGPUs(ctx)
	for i, g := range gpus {
		free := g.TotalMB - g.UsedMB
		if free < 0 {
			free = 0
		}
		h.Pools = append(h.Pools, protocol.Pool{ID: fmt.Sprintf("gpu%d", i), Kind: "dedicated", Name: g.Name,
			TotalMB: g.TotalMB, FreeMB: free, Backend: "cuda"})
		if i == 0 {
			h.Driver = g.Driver
		}
	}
	sys, err := querySystem(diskPath)
	if err != nil {
		return h, err
	}
	h.CPU = protocol.CPU{Name: sys.CPUName, Physical: sys.Physical, Logical: sys.Logical}
	h.RAMMB = sys.RAMTotalMB
	h.DiskFreeMB = sys.DiskFreeMB
	h.DiskTotalMB = sys.DiskTotalMB
	h.OS = sys.OS
	h.Pools = append(h.Pools, protocol.Pool{ID: "host", Kind: "host", TotalMB: sys.RAMTotalMB, FreeMB: sys.RAMFreeMB})
	return h, gerr
}

// System is what the OS reports.
type System struct {
	CPUName                 string
	Physical, Logical       int
	RAMTotalMB, RAMFreeMB   int64
	DiskFreeMB, DiskTotalMB int64
	OS                      string
}

// QuerySystem exposes the OS probe (used for settings bounds and defaults).
func QuerySystem(diskPath string) (System, error) { return querySystem(diskPath) }
