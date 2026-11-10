package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// SelfUsage is what mo status itself costs the Mac: CPU as a percent of one
// core over the last selfUsageWindow, counting the commands it ran (top,
// ioreg, system_profiler), and its own physical memory footprint.
type SelfUsage struct {
	CPUPercent float64 `json:"cpu_percent"`
	Memory     uint64  `json:"memory"` // bytes
}

// selfUsageWindow averages the CPU figure: the full collection's commands
// land in bursts every 30 s, so a per-tick figure would swing between a few
// percent and a whole core.
const selfUsageWindow = 30 * time.Second

// selfUsageMinSpan is the shortest span reported. Over the first second the
// start-up collection (system_profiler, ioreg, top together) reads as nearly
// two cores, a figure about launching, not about running.
const selfUsageMinSpan = 5 * time.Second

// Readers swapped in tests. selfMemoryFootprint lives in metrics_self_darwin.go
// and metrics_self_other.go.
var (
	readSelfCPUTime    = selfCPUTime
	readSelfMemoryUsed = selfMemoryFootprint
)

type selfCPUSample struct {
	at  time.Time
	cpu time.Duration
}

// selfCPUTime is the user and system time of this process plus the children
// it has waited for, which covers every command run through runCmd.
func selfCPUTime() (time.Duration, bool) {
	var total time.Duration
	for _, who := range []int{unix.RUSAGE_SELF, unix.RUSAGE_CHILDREN} {
		var ru unix.Rusage
		if err := unix.Getrusage(who, &ru); err != nil {
			return 0, false
		}
		total += time.Duration(ru.Utime.Nano()) + time.Duration(ru.Stime.Nano())
	}
	return total, true
}

// selfUsageAt records a CPU sample at now and returns the usage over the
// last selfUsageWindow. It reports false until the samples span
// selfUsageMinSpan, and starts over when the clock goes back.
func (c *Collector) selfUsageAt(now time.Time) (SelfUsage, bool) {
	cpu, ok := readSelfCPUTime()
	if !ok {
		return SelfUsage{}, false
	}
	c.selfMu.Lock()
	defer c.selfMu.Unlock()
	if n := len(c.selfSamples); n > 0 && now.Before(c.selfSamples[n-1].at) {
		c.selfSamples = nil
	}
	c.selfSamples = append(c.selfSamples, selfCPUSample{at: now, cpu: cpu})
	// Keep the newest sample at least a window old as the baseline.
	for len(c.selfSamples) > 2 && now.Sub(c.selfSamples[1].at) >= selfUsageWindow {
		c.selfSamples = c.selfSamples[1:]
	}
	base := c.selfSamples[0]
	elapsed := now.Sub(base.at)
	if elapsed < selfUsageMinSpan {
		return SelfUsage{}, false
	}
	percent := float64(max(cpu-base.cpu, 0)) / float64(elapsed) * 100
	return SelfUsage{CPUPercent: percent, Memory: readSelfMemoryUsed()}, true
}

// selfUsageSnapshot is selfUsageAt as the optional snapshot field.
func (c *Collector) selfUsageSnapshot(now time.Time) *SelfUsage {
	usage, ok := c.selfUsageAt(now)
	if !ok {
		return nil
	}
	return &usage
}
