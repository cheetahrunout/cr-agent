//go:build windows

package hw

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// nvml.dll is loaded from System32 only (ruling B17).
var (
	modNVML                  = windows.NewLazySystemDLL("nvml.dll")
	procNvmlInit             = modNVML.NewProc("nvmlInit_v2")
	procNvmlShutdown         = modNVML.NewProc("nvmlShutdown")
	procNvmlDeviceGetCount   = modNVML.NewProc("nvmlDeviceGetCount_v2")
	procNvmlDeviceGetHandle  = modNVML.NewProc("nvmlDeviceGetHandleByIndex_v2")
	procNvmlDeviceGetMemInfo = modNVML.NewProc("nvmlDeviceGetMemoryInfo")
	nvmlMu                   sync.Mutex
	errNVMLUnavailable       = errors.New("nvml.dll not available")
)

// nvmlMemory is nvmlMemory_t.
type nvmlMemory struct{ Total, Free, Used uint64 }

func nvmlFree(index int) (int64, error) {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	for _, p := range []*windows.LazyProc{procNvmlInit, procNvmlShutdown, procNvmlDeviceGetCount, procNvmlDeviceGetHandle, procNvmlDeviceGetMemInfo} {
		if p.Find() != nil {
			return 0, errNVMLUnavailable
		}
	}
	if r, _, _ := procNvmlInit.Call(); r != 0 {
		return 0, fmt.Errorf("nvmlInit: error %d", r)
	}
	defer procNvmlShutdown.Call()
	var count uint32
	if r, _, _ := procNvmlDeviceGetCount.Call(uintptr(unsafe.Pointer(&count))); r != 0 || count == 0 {
		return 0, fmt.Errorf("nvmlDeviceGetCount: error %d, %d devices", r, count)
	}
	var free int64
	for i := uint32(0); i < count; i++ {
		if index >= 0 && int(i) != index {
			continue
		}
		var dev uintptr
		if r, _, _ := procNvmlDeviceGetHandle.Call(uintptr(i), uintptr(unsafe.Pointer(&dev))); r != 0 {
			return 0, fmt.Errorf("nvmlDeviceGetHandleByIndex(%d): error %d", i, r)
		}
		var m nvmlMemory
		if r, _, _ := procNvmlDeviceGetMemInfo.Call(dev, uintptr(unsafe.Pointer(&m))); r != 0 {
			return 0, fmt.Errorf("nvmlDeviceGetMemoryInfo(%d): error %d", i, r)
		}
		free += int64(m.Free)
	}
	if index >= 0 && index >= int(count) {
		return 0, fmt.Errorf("no GPU %d", index)
	}
	return free, nil
}

// GPUFree is the free VRAM in bytes of one GPU, or of all of them together for index -1: from NVML,
// and from nvidia-smi when nvml.dll cannot be used.
func GPUFree(ctx context.Context, index int) (int64, error) {
	if free, err := nvmlFree(index); err == nil {
		return free, nil
	}
	gpus, err := QueryGPUs(ctx)
	if err != nil {
		return 0, err
	}
	var free int64
	found := false
	for i, g := range gpus {
		if index >= 0 && i != index {
			continue
		}
		found = true
		free += max(g.TotalMB-g.UsedMB, 0) << 20
	}
	if !found {
		return 0, fmt.Errorf("no GPU %d", index)
	}
	return free, nil
}
