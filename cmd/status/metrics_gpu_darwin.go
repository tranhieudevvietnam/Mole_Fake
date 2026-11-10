//go:build darwin

package main

import "errors"

// platformGPUStats reads the GPU driver's PerformanceStatistics from the
// IORegistry (AGXAccelerator on Apple Silicon). Unlike powermetrics it needs
// no root and starts no command, and the driver refreshes it every second,
// so the fast collection can read it each tick.
func platformGPUStats() (gpuStats, error) {
	ioRegLibOnce.Do(loadIORegistryLibrary)
	if ioRegLibErr != nil {
		return gpuStats{}, ioRegLibErr
	}
	lib := ioRegLib

	var accelerators uint32
	if kr := lib.getMatchingServices(0, lib.serviceMatching("IOAccelerator"), &accelerators); kr != 0 {
		return gpuStats{}, errors.New("no IOAccelerator in the IORegistry")
	}
	defer lib.objectRelease(accelerators)
	for entry := lib.iteratorNext(accelerators); entry != 0; entry = lib.iteratorNext(accelerators) {
		stats := lib.property(entry, "PerformanceStatistics")
		lib.objectRelease(entry)
		if stats == 0 {
			continue
		}
		if lib.cfGetTypeID(stats) != lib.cfDictionaryTypeID() {
			lib.cfRelease(stats)
			continue
		}
		usage, ok := lib.dictNumber(stats, "Device Utilization %")
		inUse, _ := lib.dictNumber(stats, "In use system memory")
		lib.cfRelease(stats)
		if ok {
			return gpuStats{usage: float64(min(max(usage, 0), 100)), memoryUsed: uint64(max(inUse, 0))}, nil
		}
	}
	return gpuStats{}, errors.New("no GPU performance statistics")
}
