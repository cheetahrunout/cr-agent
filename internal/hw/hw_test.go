package hw

import (
	"context"
	"runtime"
	"testing"
)

func TestParseNvidiaSMI(t *testing.T) {
	g, err := ParseNvidiaSMI("NVIDIA GeForce RTX 5090, 32607, 3907, 610.88\nNVIDIA GeForce RTX 3060, 12288, 100, 610.88\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 2 || g[0].Name != "NVIDIA GeForce RTX 5090" || g[0].TotalMB != 32607 || g[0].UsedMB != 3907 || g[0].Driver != "610.88" {
		t.Fatalf("parsed %+v", g)
	}
	if _, err := ParseNvidiaSMI("x, [N/A], 1, 2\n"); err == nil {
		t.Fatal("bad row accepted")
	}
}

func TestReport(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	h, err := Report(context.Background(), ".")
	if err != nil {
		t.Logf("report warnings: %v", err) // no NVIDIA GPU on a CI runner is fine
	}
	if h.RAMMB <= 0 || h.CPU.Logical <= 0 || h.CPU.Physical <= 0 || h.OS == "" || h.DiskFreeMB <= 0 {
		t.Fatalf("incomplete report %+v", h)
	}
	t.Logf("%+v", h)
}
