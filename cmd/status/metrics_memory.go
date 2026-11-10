package main

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

func collectMemory() (MemoryStatus, error) {
	return collectMemoryWithOptions(true)
}

func collectMemoryFast() (MemoryStatus, error) {
	return collectMemoryWithOptions(false)
}

func collectMemoryWithOptions(includeSlowAnnotations bool) (MemoryStatus, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return MemoryStatus{}, err
	}

	swap, _ := mem.SwapMemory()
	if swap == nil {
		swap = &mem.SwapMemoryStat{}
	}
	var pressure string
	var swapApps []SwapApp
	if includeSlowAnnotations {
		pressure = getMemoryPressure()
		// Naming who is in swap costs a one-second top run; swapAppsFor runs
		// it only under pressure or heavy swap, at most once a minute.
		if runtime.GOOS == "darwin" {
			swapApps = swapAppsFor(pressure, swap.Used, time.Now())
		}
	}

	// On macOS, vm.Cached is 0, so we calculate from file-backed pages.
	cached := vm.Cached
	if includeSlowAnnotations && runtime.GOOS == "darwin" && cached == 0 {
		cached = getFileBackedMemory()
	}

	return MemoryStatus{
		Used:        vm.Used,
		Total:       vm.Total,
		Available:   vm.Available,
		UsedPercent: vm.UsedPercent,
		SwapUsed:    swap.Used,
		SwapTotal:   swap.Total,
		Cached:      cached,
		Pressure:    pressure,
		SwapApps:    swapApps,
	}, nil
}

func getFileBackedMemory() uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := runCmd(ctx, "vm_stat")
	if err != nil {
		return 0
	}

	// Parse page size from first line: "Mach Virtual Memory Statistics: (page size of 16384 bytes)"
	var pageSize uint64 = 4096 // Default
	firstLine := true
	for line := range strings.Lines(out) {
		if firstLine {
			firstLine = false
			if strings.Contains(line, "page size of") {
				if _, after, found := strings.Cut(line, "page size of "); found {
					if before, _, found := strings.Cut(after, " bytes"); found {
						if size, err := strconv.ParseUint(strings.TrimSpace(before), 10, 64); err == nil {
							pageSize = size
						}
					}
				}
			}
		}

		// Parse "File-backed pages: 388975."
		if strings.Contains(line, "File-backed pages:") {
			if _, after, found := strings.Cut(line, ":"); found {
				numStr := strings.TrimSpace(after)
				numStr = strings.TrimSuffix(numStr, ".")
				if pages, err := strconv.ParseUint(numStr, 10, 64); err == nil {
					return pages * pageSize
				}
			}
		}
	}
	return 0
}

func getMemoryPressure() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := runCmd(ctx, "memory_pressure")
	if err != nil {
		return ""
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, "critical") {
		return "critical"
	}
	if strings.Contains(lower, "warn") {
		return "warn"
	}
	if strings.Contains(lower, "normal") {
		return "normal"
	}
	// memory_pressure on macOS 27 prints page statistics and a free percentage
	// but no level word, which left Pressure empty and every memory pressure
	// warning silent. Take the level from the kernel instead.
	if level, err := readVMPressureLevel(); err == nil {
		return pressureFromLevel(level)
	}
	return ""
}

// readVMPressureLevel reads kern.memorystatus_vm_pressure_level, the kernel
// level Activity Monitor's Memory Pressure graph is colored by. Swapped in
// tests; the platform implementations live in metrics_memory_darwin.go and
// metrics_memory_other.go.
var readVMPressureLevel = platformVMPressureLevel

// pressureFromLevel maps the kernel's pressure level to Mole's labels:
// 1 normal, 2 warn, 4 critical. Any other value is unknown.
func pressureFromLevel(level uint32) string {
	switch level {
	case 1:
		return "normal"
	case 2:
		return "warn"
	case 4:
		return "critical"
	default:
		return ""
	}
}
