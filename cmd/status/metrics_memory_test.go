package main

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// macOS27MemoryPressureOutput is memory_pressure's output on macOS 27: page
// statistics and a free percentage, with no level word anywhere.
const macOS27MemoryPressureOutput = `The system has 34359738368 (2097152 pages with a page size of 16384).

Stats:
Pages free: 6837
Pages purgeable: 11456

Swap I/O:
Swapins: 4350771
Swapouts: 5919471

Compressor Stats:
Pages used by compressor: 825226

System-wide memory free percentage: 47%
`

func stubMemoryPressureProbes(t *testing.T, out string, cmdErr error, level uint32, levelErr error) *bool {
	t.Helper()
	origRunCmd, origLevel := runCmd, readVMPressureLevel
	t.Cleanup(func() { runCmd, readVMPressureLevel = origRunCmd, origLevel })

	runCmd = func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "memory_pressure" {
			return out, cmdErr
		}
		return "", errors.New("command not mocked")
	}
	levelRead := false
	readVMPressureLevel = func() (uint32, error) {
		levelRead = true
		return level, levelErr
	}
	return &levelRead
}

func TestPressureFromLevel(t *testing.T) {
	for level, want := range map[uint32]string{0: "", 1: "normal", 2: "warn", 3: "", 4: "critical", 8: ""} {
		if got := pressureFromLevel(level); got != want {
			t.Errorf("pressureFromLevel(%d) = %q, want %q", level, got, want)
		}
	}
}

func TestGetMemoryPressureReadsKernelLevelWhenCommandHasNoLevelWord(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memory pressure is a macOS probe")
	}
	for level, want := range map[uint32]string{1: "normal", 2: "warn", 4: "critical"} {
		stubMemoryPressureProbes(t, macOS27MemoryPressureOutput, nil, level, nil)
		if got := getMemoryPressure(); got != want {
			t.Errorf("kernel level %d: got %q, want %q", level, got, want)
		}
	}
}

func TestGetMemoryPressureKeepsLevelWordFromCommand(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memory pressure is a macOS probe")
	}
	levelRead := stubMemoryPressureProbes(t, "System memory pressure: warn", nil, 4, nil)

	if got := getMemoryPressure(); got != "warn" {
		t.Fatalf("expected the command's own level word, got %q", got)
	}
	if *levelRead {
		t.Fatal("the kernel level is only the fallback for output without a level word")
	}
}

func TestGetMemoryPressureStaysEmptyWhenProbesFail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("memory pressure is a macOS probe")
	}
	levelRead := stubMemoryPressureProbes(t, "", errors.New("timed out"), 4, nil)
	if got := getMemoryPressure(); got != "" {
		t.Fatalf("a failed memory_pressure run must stay unknown, got %q", got)
	}
	if *levelRead {
		t.Fatal("a failed memory_pressure run marks the full probe failed; do not substitute the kernel level")
	}

	stubMemoryPressureProbes(t, macOS27MemoryPressureOutput, nil, 0, errors.New("sysctl unavailable"))
	if got := getMemoryPressure(); got != "" {
		t.Fatalf("no level word and no kernel level must stay unknown, got %q", got)
	}
}
