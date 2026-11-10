package main

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// swapAppsTopCount processes are sampled before grouping, so an app split
	// over many helpers (Figma, VS Code, Chrome) still adds up.
	swapAppsTopCount = 25
	swapAppsMax      = 5
	// Groups smaller than this are noise next to gigabytes of swap.
	swapAppsMinBytes = 100 << 20
	swapAppsTimeout  = 3 * time.Second
	// Swap this full is worth naming even while the kernel calls pressure
	// normal: swapped data stays out long after the pressure that pushed it.
	swapAppsMinSwapUsed = 1 << 30
	// top spends about a second of CPU walking every process, and who holds
	// the swap changes slowly, so one read serves two full collections.
	swapAppsCacheTTL = time.Minute
)

var (
	swapAppsMu     sync.Mutex
	swapAppsAt     time.Time
	cachedSwapApps []SwapApp
)

// shouldCollectSwapApps reports whether the swap apps are worth a top run:
// under memory pressure, or with at least swapAppsMinSwapUsed in swap.
func shouldCollectSwapApps(pressure string, swapUsed uint64) bool {
	return pressure == "warn" || pressure == "critical" || swapUsed >= swapAppsMinSwapUsed
}

// swapAppsFor returns the swap apps for the full collection at now, reading
// them again only after swapAppsCacheTTL (or a clock that went back).
func swapAppsFor(pressure string, swapUsed uint64, now time.Time) []SwapApp {
	if !shouldCollectSwapApps(pressure, swapUsed) {
		return nil
	}
	swapAppsMu.Lock()
	defer swapAppsMu.Unlock()
	if swapAppsAt.IsZero() || now.Sub(swapAppsAt) >= swapAppsCacheTTL || now.Before(swapAppsAt) {
		cachedSwapApps = collectSwapApps()
		swapAppsAt = now
	}
	return slices.Clone(cachedSwapApps)
}

// swapAppAliases merges executables that are one tool to the reader.
var swapAppAliases = map[string]string{
	"dartaotruntime": "dart",
}

// collectSwapApps names the apps macOS compressed the most memory from.
// macOS swaps compressor segments, not pages of a named process, and reports
// only each process's compressed total (Activity Monitor's "Compressed
// Memory"), so this is the closest per-app view of who is in swap. top is the
// one source of that number for every process without root and takes about a
// second, so this runs in the full collection only, gated and cached by
// swapAppsFor.
func collectSwapApps() []SwapApp {
	ctx, cancel := context.WithTimeout(context.Background(), swapAppsTimeout)
	defer cancel()
	out, err := runCmd(ctx, "top", "-l", "1", "-o", "cmprs", "-n", strconv.Itoa(swapAppsTopCount), "-stats", "pid,cmprs,command")
	if err != nil {
		return nil
	}
	samples := parseTopCompressed(out)
	if len(samples) == 0 {
		return nil
	}

	pids := make([]string, 0, len(samples))
	for _, s := range samples {
		pids = append(pids, strconv.Itoa(s.pid))
	}
	paths := map[int]string{}
	// ps comm is the full executable path on macOS, which names the .app a
	// helper belongs to. Without it each sample keeps top's short name.
	if psOut, err := runCmd(ctx, "ps", "-o", "pid=,comm=", "-p", strings.Join(pids, ",")); err == nil {
		paths = parsePSComm(psOut)
	}
	return groupSwapApps(samples, paths)
}

type compressedSample struct {
	pid        int
	compressed uint64
	command    string // top's name, cut to 16 characters
}

// parseTopCompressed reads `top -l 1 -stats pid,cmprs,command` rows, which
// follow the "PID" header line.
func parseTopCompressed(out string) []compressedSample {
	var samples []compressedSample
	inRows := false
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !inRows {
			inRows = fields[0] == "PID"
			continue
		}
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		size, ok := parseTopSize(fields[1])
		if !ok {
			continue
		}
		samples = append(samples, compressedSample{pid: pid, compressed: size, command: strings.Join(fields[2:], " ")})
	}
	return samples
}

// parseTopSize reads a top memory figure such as "3434M", "12K" or "1.5G",
// with the optional +/- change marker top may append.
func parseTopSize(field string) (uint64, bool) {
	field = strings.TrimRight(field, "+-")
	if field == "" {
		return 0, false
	}
	multiplier := 1.0
	switch field[len(field)-1] {
	case 'B':
		field = field[:len(field)-1]
	case 'K':
		multiplier, field = 1<<10, field[:len(field)-1]
	case 'M':
		multiplier, field = 1<<20, field[:len(field)-1]
	case 'G':
		multiplier, field = 1<<30, field[:len(field)-1]
	case 'T':
		multiplier, field = 1<<40, field[:len(field)-1]
	}
	value, err := strconv.ParseFloat(field, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return uint64(value * multiplier), true
}

// parsePSComm reads `ps -o pid=,comm=` lines into executable paths by pid.
func parsePSComm(out string) map[int]string {
	paths := map[int]string{}
	for line := range strings.Lines(out) {
		pidText, path, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			continue
		}
		paths[pid] = strings.TrimSpace(path)
	}
	return paths
}

// swapAppLabel names the app a process belongs to: the outermost .app bundle
// in its executable path (a Figma or VS Code helper counts as Figma or Visual
// Studio Code), else the executable's name.
func swapAppLabel(path, topCommand string) string {
	if path == "" {
		return topCommand
	}
	if i := strings.Index(path, ".app/"); i >= 0 {
		return filepath.Base(path[:i])
	}
	name := filepath.Base(path)
	if alias, ok := swapAppAliases[name]; ok {
		return alias
	}
	return name
}

// groupSwapApps sums the samples by app and keeps the largest.
func groupSwapApps(samples []compressedSample, paths map[int]string) []SwapApp {
	totals := map[string]uint64{}
	for _, s := range samples {
		totals[swapAppLabel(paths[s.pid], s.command)] += s.compressed
	}
	apps := make([]SwapApp, 0, len(totals))
	for name, bytes := range totals {
		if bytes >= swapAppsMinBytes {
			apps = append(apps, SwapApp{Name: name, Compressed: bytes})
		}
	}
	slices.SortFunc(apps, func(a, b SwapApp) int {
		switch {
		case a.Compressed > b.Compressed:
			return -1
		case a.Compressed < b.Compressed:
			return 1
		default:
			return strings.Compare(a.Name, b.Name)
		}
	})
	if len(apps) > swapAppsMax {
		apps = apps[:swapAppsMax]
	}
	return apps
}
