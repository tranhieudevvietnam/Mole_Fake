package main

import (
	"bufio"
	"context"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
)

const (
	cpuSampleInterval = 100 * time.Millisecond
)

func collectCPU() (CPUStatus, error) {
	return collectCPUWithOptions(true)
}

func collectCPUFast() (CPUStatus, error) {
	return collectCPUWithOptions(false)
}

func collectCPUWithOptions(includeSlowFallbacks bool) (CPUStatus, error) {
	counts, countsErr := cpu.Counts(false)
	if countsErr != nil || counts == 0 {
		counts = runtime.NumCPU()
	}

	logical, logicalErr := cpu.Counts(true)
	if logicalErr != nil || logical == 0 {
		logical = runtime.NumCPU()
	}
	if logical <= 0 {
		logical = 1
	}

	var percents []float64
	var err error
	var totalPercent float64
	sampled := false
	if includeSlowFallbacks {
		// Explicit two-snapshot sampling on the full refresh path. On Apple
		// Silicon, host_processor_info stops accumulating idle ticks for a
		// parked core, so busy/(busy+idle) over the raw deltas reports a
		// mostly-sleeping E-core as 90-100% (#1237). sampleCPUPercents floors
		// each core's denominator at the wall-clock window, which counts the
		// missing parked time as idle; on Intel the ticks already cover the
		// window and the result is unchanged.
		warmUpCPU()
		percents, totalPercent, err = sampleCPUPercents(cpuSampleInterval)
		sampled = err == nil && len(percents) > 0
	}
	if !sampled && !includeSlowFallbacks {
		percents, totalPercent, sampled = fastCPUPercents(time.Now())
	}
	if !sampled {
		percents, err = cpu.Percent(0, true)
	}
	perCoreEstimated := false
	if !sampled && (err != nil || len(percents) == 0) {
		if !includeSlowFallbacks {
			// Fast path: skip the expensive secondary sampling and just
			// estimate zeroed per-core usage. The next full refresh corrects it.
			percents = make([]float64, logical)
			perCoreEstimated = true
		} else {
			fallbackUsage, fallbackPerCore, fallbackErr := fallbackCPUUtilization(logical)
			if fallbackErr != nil {
				if err != nil {
					return CPUStatus{}, err
				}
				return CPUStatus{}, fallbackErr
			}
			totalPercent = fallbackUsage
			percents = fallbackPerCore
			perCoreEstimated = true
		}
	} else if !sampled {
		for _, v := range percents {
			totalPercent += v
		}
		totalPercent /= float64(len(percents))
	}

	loadStats, loadErr := load.Avg()
	var loadAvg load.AvgStat
	if loadStats != nil {
		loadAvg = *loadStats
	}
	if includeSlowFallbacks && (loadErr != nil || isZeroLoad(loadAvg)) {
		if fallback, err := fallbackLoadAvgFromUptime(); err == nil {
			loadAvg = fallback
		}
	}

	// P/E core counts and each core's cluster for Apple Silicon.
	var pCores, eCores int
	var coreTypes []string
	if includeSlowFallbacks {
		pCores, eCores = getCoreTopology()
		coreTypes = getCoreClusterTypes()
	}

	return CPUStatus{
		Usage:            totalPercent,
		PerCore:          percents,
		PerCoreEstimated: perCoreEstimated,
		Load1:            loadAvg.Load1,
		Load5:            loadAvg.Load5,
		Load15:           loadAvg.Load15,
		CoreCount:        counts,
		LogicalCPU:       logical,
		PCoreCount:       pCores,
		ECoreCount:       eCores,
		CoreTypes:        coreTypes,
	}, nil
}

func isZeroLoad(avg load.AvgStat) bool {
	return avg.Load1 == 0 && avg.Load5 == 0 && avg.Load15 == 0
}

var (
	// Cache for core topology.
	lastTopologyAt   time.Time
	cachedP, cachedE int
	topologyTTL      = 10 * time.Minute
)

var (
	// Cache for each logical CPU's cluster type.
	lastClusterTypesAt time.Time
	cachedClusterTypes []string
)

// getCoreClusterTypes returns each logical CPU's cluster ("E" or "P") from the
// IODeviceTree, indexed like the per-core usage. Ordering is not assumed: an
// Ultra interleaves its two dies' clusters, so "the first E-count CPUs are E"
// would mislabel half of them. Returns nil when the tree has no cluster types.
func getCoreClusterTypes() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	now := time.Now()
	if cachedClusterTypes != nil && now.Sub(lastClusterTypesAt) < topologyTTL && !now.Before(lastClusterTypesAt) {
		return slices.Clone(cachedClusterTypes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := runCmd(ctx, "ioreg", "-p", "IODeviceTree", "-r", "-n", "cpus", "-l", "-w0")
	if err != nil {
		return nil
	}
	types := parseCoreClusterTypes(out)
	if types == nil {
		return nil
	}
	cachedClusterTypes, lastClusterTypesAt = types, now
	return slices.Clone(types)
}

// parseCoreClusterTypes reads the "+-o cpuN" nodes of the device tree's cpus
// subtree, where "logical-cpu-id" = N and "cluster-type" = <"E"> come in
// either order. Every logical CPU from 0 up must be typed, or the result is
// nil rather than a partial map.
func parseCoreClusterTypes(out string) []string {
	byID := map[int]string{}
	id, kind := -1, ""
	commit := func() {
		if id >= 0 && (kind == "E" || kind == "P") {
			byID[id] = kind
		}
		id, kind = -1, ""
	}
	for line := range strings.Lines(out) {
		trimmed := strings.TrimLeft(line, " |")
		if strings.HasPrefix(trimmed, "+-o ") {
			commit()
			continue
		}
		if v, ok := ioRegValueForKey(trimmed, "logical-cpu-id"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				id = n
			}
		}
		if _, rest, found := strings.Cut(trimmed, `"cluster-type" = <"`); found {
			kind, _, _ = strings.Cut(rest, `"`)
		}
	}
	commit()
	if len(byID) == 0 {
		return nil
	}
	types := make([]string, len(byID))
	for i := range types {
		t, ok := byID[i]
		if !ok {
			return nil
		}
		types[i] = t
	}
	return types
}

// getCoreTopology returns P/E core counts on Apple Silicon.
func getCoreTopology() (pCores, eCores int) {
	if runtime.GOOS != "darwin" {
		return 0, 0
	}

	now := time.Now()
	if cachedP > 0 || cachedE > 0 {
		if now.Sub(lastTopologyAt) < topologyTTL {
			return cachedP, cachedE
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	out, err := runCmd(ctx, "/usr/sbin/sysctl", "-n",
		"hw.perflevel0.logicalcpu",
		"hw.perflevel1.logicalcpu")
	if err != nil {
		return 0, 0
	}

	pCores, eCores = parseCoreTopology(out)
	if pCores == 0 && eCores == 0 {
		return 0, 0
	}

	cachedP, cachedE = pCores, eCores
	lastTopologyAt = now
	return pCores, eCores
}

// parseCoreTopology reads `sysctl -n hw.perflevel0.logicalcpu
// hw.perflevel1.logicalcpu` output as top tier then the tier below it.
//
// The level ORDER decides which bucket a count lands in, never the level name.
// M5 names its two levels "Super" and "Performance" and ships no "Efficiency"
// level at all, so matching the word "performance" put 12 efficiency cores in
// the P bucket and reported e_core_count 0 on an 18-core M5 Pro, which also
// dropped the "12P+4E" load line entirely. Apple documents perflevel0 as the
// highest-performance level, and that survives the next rename.
func parseCoreTopology(out string) (pCores, eCores int) {
	var lines []string
	for line := range strings.Lines(strings.TrimSpace(out)) {
		lines = append(lines, line)
	}
	if len(lines) < 2 {
		return 0, 0
	}

	pCores, errP := strconv.Atoi(strings.TrimSpace(lines[0]))
	eCores, errE := strconv.Atoi(strings.TrimSpace(lines[1]))
	if errP != nil || errE != nil || pCores <= 0 || eCores <= 0 {
		return 0, 0
	}
	return pCores, eCores
}

func fallbackLoadAvgFromUptime() (load.AvgStat, error) {
	if !commandExists("uptime") {
		return load.AvgStat{}, errors.New("uptime command unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	out, err := runCmd(ctx, "uptime")
	if err != nil {
		return load.AvgStat{}, err
	}

	markers := []string{"load averages:", "load average:"}
	idx := -1
	for _, marker := range markers {
		if pos := strings.LastIndex(out, marker); pos != -1 {
			idx = pos + len(marker)
			break
		}
	}
	if idx == -1 {
		return load.AvgStat{}, errors.New("load averages not found in uptime output")
	}

	segment := strings.TrimSpace(out[idx:])
	fields := strings.Fields(segment)
	var values []float64
	for _, field := range fields {
		field = strings.Trim(field, ",;")
		if field == "" {
			continue
		}
		val, err := strconv.ParseFloat(field, 64)
		if err != nil {
			continue
		}
		values = append(values, val)
		if len(values) == 3 {
			break
		}
	}
	if len(values) < 3 {
		return load.AvgStat{}, errors.New("could not parse load averages from uptime output")
	}

	return load.AvgStat{
		Load1:  values[0],
		Load5:  values[1],
		Load15: values[2],
	}, nil
}

func fallbackCPUUtilization(logical int) (float64, []float64, error) {
	if logical <= 0 {
		logical = runtime.NumCPU()
	}
	if logical <= 0 {
		logical = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	out, err := runCmd(ctx, "ps", "-Aceo", "pcpu")
	if err != nil {
		return 0, nil, err
	}

	scanner := bufio.NewScanner(strings.NewReader(out))
	total := 0.0
	lineIndex := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineIndex++
		if lineIndex == 1 && (strings.Contains(strings.ToLower(line), "cpu") || strings.Contains(line, "%")) {
			continue
		}

		val, parseErr := strconv.ParseFloat(line, 64)
		if parseErr != nil {
			continue
		}
		total += val
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return 0, nil, scanErr
	}

	maxTotal := float64(logical * 100)
	if total < 0 {
		total = 0
	} else if total > maxTotal {
		total = maxTotal
	}

	avg := total / float64(logical)
	perCore := make([]float64, logical)
	for i := range perCore {
		perCore[i] = avg
	}
	return avg, perCore, nil
}

func warmUpCPU() {
	cpu.Percent(0, true) //nolint:errcheck
}

// The fast path keeps the previous tick's per-core times so each tick applies
// the full path's wall-clock floor (#1237) over the time since the last tick,
// without sleeping. cpu.Percent(0, true) measures the same interval but
// divides by the raw ticks, so between full refreshes it showed a parked
// E-core at 90-100%.
var (
	fastCPUMu    sync.Mutex
	fastCPUTimes []cpu.TimesStat
	fastCPUAt    time.Time
	cpuTimesFunc = cpu.Times
)

// minFastCPUWindow is the shortest tick-to-tick window worth a reading; a
// shorter one keeps the older baseline so the next tick measures more.
const minFastCPUWindow = 200 * time.Millisecond

// fastCPUPercents returns per-core and total usage since the previous call,
// or false on the first call, after a core-count change, a clock that went
// back, or a window under minFastCPUWindow.
func fastCPUPercents(now time.Time) ([]float64, float64, bool) {
	times, err := cpuTimesFunc(true)
	if err != nil || len(times) == 0 {
		return nil, 0, false
	}
	fastCPUMu.Lock()
	defer fastCPUMu.Unlock()
	prev, elapsed := fastCPUTimes, now.Sub(fastCPUAt)
	if len(prev) != len(times) || elapsed < 0 {
		fastCPUTimes, fastCPUAt = times, now
		return nil, 0, false
	}
	if elapsed < minFastCPUWindow {
		return nil, 0, false
	}
	fastCPUTimes, fastCPUAt = times, now
	percents, total, err := perCoreUsageFromTimes(prev, times, elapsed.Seconds())
	if err != nil {
		return nil, 0, false
	}
	return percents, total, true
}

// sampleCPUPercents measures per-core and total CPU usage over one wall-clock
// window using two cpu.Times snapshots.
func sampleCPUPercents(interval time.Duration) ([]float64, float64, error) {
	before, err := cpu.Times(true)
	if err != nil {
		return nil, 0, err
	}
	start := time.Now()
	time.Sleep(interval)
	after, err := cpu.Times(true)
	if err != nil {
		return nil, 0, err
	}
	return perCoreUsageFromTimes(before, after, time.Since(start).Seconds())
}

// perCoreUsageFromTimes converts two per-core cpu.Times snapshots into per-core
// percentages and a tick-weighted total.
//
// Each core's denominator is floored at the elapsed wall-clock time: a parked
// Apple Silicon core stops accumulating idle ticks, so its raw delta covers
// only the sliver of the window it was awake for and busy/(busy+idle) reads
// ~100% while the core mostly slept (#1237). Treating the missing ticks as
// idle time yields the fraction of the window the core actually worked. The
// total is busy-over-window across cores, not the mean of per-core
// percentages, so a core that barely ran cannot drag the total upward.
func perCoreUsageFromTimes(before, after []cpu.TimesStat, elapsed float64) ([]float64, float64, error) {
	if len(before) == 0 || len(before) != len(after) {
		return nil, 0, errors.New("mismatched cpu times snapshots")
	}
	if elapsed <= 0 {
		return nil, 0, errors.New("non-positive sampling window")
	}

	percents := make([]float64, len(before))
	var busySum, windowSum float64
	for i := range before {
		busy := cpuBusyTime(after[i]) - cpuBusyTime(before[i])
		if busy < 0 {
			busy = 0
		}
		total := busy + (after[i].Idle - before[i].Idle)
		window := total
		if window < elapsed {
			window = elapsed
		}
		usage := busy / window * 100
		if usage < 0 {
			usage = 0
		} else if usage > 100 {
			usage = 100
		}
		percents[i] = usage
		busySum += busy
		windowSum += window
	}
	if windowSum <= 0 {
		return nil, 0, errors.New("empty cpu sampling window")
	}
	total := busySum / windowSum * 100
	if total < 0 {
		total = 0
	} else if total > 100 {
		total = 100
	}
	return percents, total, nil
}

func cpuBusyTime(t cpu.TimesStat) float64 {
	return t.User + t.System + t.Nice + t.Irq + t.Softirq + t.Steal
}
