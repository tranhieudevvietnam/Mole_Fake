package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	gopsutilnet "github.com/shirou/gopsutil/v4/net"
)

func stubGPUStats(t *testing.T, fn func() (gpuStats, error)) {
	t.Helper()
	orig := readGPUStats
	t.Cleanup(func() { readGPUStats = orig })
	readGPUStats = fn
}

func appleGPU() []GPUStatus {
	return []GPUStatus{{Name: "Apple M1 Pro", Usage: -1, CoreCount: 16, Note: "sppci_vendor_Apple"}}
}

func TestCollectGPUReadsDriverStatisticsWithoutPowermetrics(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple GPU statistics are darwin-only")
	}
	stubGPUStats(t, func() (gpuStats, error) { return gpuStats{usage: 60, memoryUsed: 1345 << 20}, nil })
	origRunCmd := runCmd
	t.Cleanup(func() { runCmd = origRunCmd })
	runCmd = func(_ context.Context, name string, _ ...string) (string, error) {
		t.Fatalf("the driver statistics answered; %s must not run", name)
		return "", nil
	}
	collector := NewCollector(ProcessWatchOptions{})
	now := time.Now()
	collector.cachedGPU, collector.lastGPUAt = appleGPU(), now

	gpus, err := collector.collectGPU(now)
	if err != nil || len(gpus) != 1 || gpus[0].Usage != 60 || gpus[0].MemoryUsed != 1345 {
		t.Fatalf("expected 60%% and 1345 MiB from the driver, got %+v (%v)", gpus, err)
	}
	if collector.cachedGPU[0].Usage != -1 {
		t.Fatal("the cached static GPU info must not take the live reading")
	}
}

func TestCollectGPUFallsBackToPowermetrics(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple GPU statistics are darwin-only")
	}
	stubGPUStats(t, func() (gpuStats, error) { return gpuStats{}, errors.New("no IOAccelerator") })
	origRunCmd := runCmd
	t.Cleanup(func() { runCmd = origRunCmd })
	runCmd = func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "powermetrics" {
			return "GPU HW active residency:  12.50%\n", nil
		}
		return "", errors.New("command not mocked")
	}
	collector := NewCollector(ProcessWatchOptions{})
	now := time.Now()
	collector.cachedGPU, collector.lastGPUAt = appleGPU(), now

	if gpus, _ := collector.collectGPU(now); len(gpus) != 1 || gpus[0].Usage != 12.5 {
		t.Fatalf("expected the powermetrics 12.5%% fallback, got %+v", gpus)
	}
}

func TestCollectFastAppliesLiveGPUWithoutCommands(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple GPU statistics are darwin-only")
	}
	origRunCmd, origCommandExists := runCmd, commandExists
	origPartitions, origUsage, origIOCounters := diskPartitionsFunc, diskUsageFunc, ioCountersFunc
	t.Cleanup(func() {
		runCmd, commandExists = origRunCmd, origCommandExists
		diskPartitionsFunc, diskUsageFunc, ioCountersFunc = origPartitions, origUsage, origIOCounters
	})
	var externalCalls atomic.Int32
	runCmd = func(context.Context, string, ...string) (string, error) {
		externalCalls.Add(1)
		return "", errors.New("unexpected command")
	}
	commandExists = func(string) bool { externalCalls.Add(1); return false }
	diskPartitionsFunc = func(bool) ([]disk.PartitionStat, error) {
		return []disk.PartitionStat{{Device: "/dev/disk3s1s1", Mountpoint: "/", Fstype: "apfs"}}, nil
	}
	diskUsageFunc = func(path string) (*disk.UsageStat, error) {
		return &disk.UsageStat{Path: path, Fstype: "apfs", Total: 2 << 30, Used: 1 << 30, UsedPercent: 50}, nil
	}
	ioCountersFunc = func(bool) ([]gopsutilnet.IOCountersStat, error) { return nil, nil }
	stubGPUStats(t, func() (gpuStats, error) { return gpuStats{usage: 42, memoryUsed: 2 << 30}, nil })

	collector := NewCollector(ProcessWatchOptions{})
	collector.hasEnrichment = true
	collector.enrichment = snapshotEnrichment{gpu: appleGPU()}

	snapshot, err := collector.CollectFast()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GPU) != 1 || snapshot.GPU[0].Usage != 42 || snapshot.GPU[0].MemoryUsed != 2048 || snapshot.GPU[0].Name != "Apple M1 Pro" {
		t.Fatalf("expected the live 42%% over the cached GPU, got %+v", snapshot.GPU)
	}
	if collector.enrichment.gpu[0].Usage != -1 {
		t.Fatal("the cached GPU must not take the live reading")
	}
	if externalCalls.Load() != 0 {
		t.Fatalf("the live GPU read must not run commands, got %d", externalCalls.Load())
	}
}

func TestGPUUsageRow(t *testing.T) {
	row, ok := gpuUsageRow([]GPUStatus{{Name: "Apple M1 Pro", Usage: 60, MemoryUsed: 1345, CoreCount: 16}})
	if plain := stripANSI(row); !ok || !strings.HasPrefix(plain, "GPU    ") || !strings.HasSuffix(plain, " 60.0%  1.3G") {
		t.Fatalf("unexpected GPU row %q (ok=%v)", plain, ok)
	}
	if _, ok := gpuUsageRow([]GPUStatus{{Name: "Apple M1 Pro", Usage: -1, CoreCount: 16}}); ok {
		t.Fatal("an unmeasured GPU gets no row")
	}
	if _, ok := gpuUsageRow([]GPUStatus{{Name: "No GPU metrics available", Note: "Install nvidia-smi"}}); ok {
		t.Fatal("a placeholder entry gets no row")
	}
	if _, ok := gpuUsageRow(nil); ok {
		t.Fatal("no GPU, no row")
	}
}

func TestCPUCardPlacesGPURowAboveLoad(t *testing.T) {
	cpu := CPUStatus{
		Usage:      40,
		PerCore:    []float64{100, 98, 60, 40},
		CoreTypes:  []string{"E", "E", "P", "P"},
		PCoreCount: 2, ECoreCount: 2, LogicalCPU: 4,
	}
	card := renderCPUCardWithGPU(cpu, ThermalStatus{}, 2, []GPUStatus{{Name: "Apple M1 Pro", Usage: 60, CoreCount: 16}})
	if len(card.lines) != 5 {
		t.Fatalf("expected Total, E-core, P-core, GPU and Load, got %q", card.lines)
	}
	if !strings.HasPrefix(stripANSI(card.lines[3]), "GPU ") || !strings.HasPrefix(stripANSI(card.lines[4]), "Load ") {
		t.Fatalf("the GPU row belongs right above the load line, got %q", card.lines)
	}
}
