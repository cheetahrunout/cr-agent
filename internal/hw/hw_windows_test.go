//go:build windows

package hw

import (
	"context"
	"testing"
)

func TestGPUFree(t *testing.T) {
	gpus, err := QueryGPUs(context.Background())
	if err != nil || len(gpus) == 0 {
		t.Skip("no NVIDIA GPU on this PC")
	}
	free, err := GPUFree(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if nv, nerr := nvmlFree(0); nerr != nil {
		t.Logf("NVML not used (%v); the figure is from nvidia-smi", nerr)
	} else if diff := nv - free; diff > 1<<30 || diff < -(1<<30) {
		t.Fatalf("NVML %d MB and GPUFree %d MB disagree", nv>>20, free>>20)
	}
	total := gpus[0].TotalMB << 20
	if free <= 0 || free > total {
		t.Fatalf("free %d MB of %d MB", free>>20, total>>20)
	}
	all, err := GPUFree(context.Background(), -1)
	if err != nil || all < free {
		t.Fatalf("all GPUs: %d MB, %v", all>>20, err)
	}
	if _, err := GPUFree(context.Background(), 7); err == nil && len(gpus) < 8 {
		t.Fatal("a GPU that does not exist has free memory")
	}
	t.Logf("GPU 0: %d MB free of %d MB", free>>20, total>>20)
}
