package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

// A parked Apple Silicon core stops accumulating idle ticks, so its raw delta
// only covers the sliver of the window it was awake for. The old
// busy/(busy+idle) math read such a core as ~100% busy (#1237); flooring the
// denominator at the wall-clock window must report the real fraction.
func TestPerCoreUsageParkedCoreIsNotInflated(t *testing.T) {
	before := []cpu.TimesStat{{CPU: "cpu0", User: 10, System: 5, Idle: 100}}
	// Awake for only 0.02s of a 0.1s window, all of it busy; no idle ticks.
	after := []cpu.TimesStat{{CPU: "cpu0", User: 10.01, System: 5.01, Idle: 100}}

	percents, total, err := perCoreUsageFromTimes(before, after, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(percents[0], 20) {
		t.Fatalf("parked core should read 20%% of the window, got %.2f", percents[0])
	}
	if !almostEqual(total, 20) {
		t.Fatalf("total should read 20%%, got %.2f", total)
	}
}

// When the tick deltas already cover the window (Intel, awake cores), the
// wall-clock floor must not change the classic busy/(busy+idle) result.
func TestPerCoreUsageFullyCoveredWindowUnchanged(t *testing.T) {
	before := []cpu.TimesStat{{CPU: "cpu0", User: 1, System: 1, Idle: 10}}
	after := []cpu.TimesStat{{CPU: "cpu0", User: 1.03, System: 1.02, Idle: 10.05}}

	percents, total, err := perCoreUsageFromTimes(before, after, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(percents[0], 50) {
		t.Fatalf("expected 50%%, got %.2f", percents[0])
	}
	if !almostEqual(total, 50) {
		t.Fatalf("expected 50%% total, got %.2f", total)
	}
}

// The total must be busy-over-window across cores, not the mean of the raw
// per-core percentages: a mostly-parked core contributes its true window, so
// it cannot drag the machine total toward 100%.
func TestPerCoreUsageTotalIsWindowWeighted(t *testing.T) {
	before := []cpu.TimesStat{
		{CPU: "cpu0", User: 10, Idle: 100},
		{CPU: "cpu1", User: 20, Idle: 200},
	}
	after := []cpu.TimesStat{
		// Parked: 0.01s busy, no idle ticks. Raw math would say 100%.
		{CPU: "cpu0", User: 10.01, Idle: 100},
		// Awake: 0.09s busy + 0.01s idle over the full window.
		{CPU: "cpu1", User: 20.09, Idle: 200.01},
	}

	percents, total, err := perCoreUsageFromTimes(before, after, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(percents[0], 10) {
		t.Fatalf("parked core expected 10%%, got %.2f", percents[0])
	}
	if !almostEqual(percents[1], 90) {
		t.Fatalf("busy core expected 90%%, got %.2f", percents[1])
	}
	// (0.01 + 0.09) busy over (0.1 + 0.1) window = 50%.
	if !almostEqual(total, 50) {
		t.Fatalf("window-weighted total expected 50%%, got %.2f", total)
	}
}

func TestPerCoreUsageClampsAndErrors(t *testing.T) {
	// Busy jitter slightly above the window clamps to 100.
	before := []cpu.TimesStat{{CPU: "cpu0", User: 1, Idle: 1}}
	after := []cpu.TimesStat{{CPU: "cpu0", User: 1.12, Idle: 1}}
	percents, total, err := perCoreUsageFromTimes(before, after, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if percents[0] != 100 || total != 100 {
		t.Fatalf("expected clamp to 100, got %.2f / %.2f", percents[0], total)
	}

	if _, _, err := perCoreUsageFromTimes(before, nil, 0.1); err == nil {
		t.Fatal("mismatched snapshots must error")
	}
	if _, _, err := perCoreUsageFromTimes(before, after, 0); err == nil {
		t.Fatal("non-positive window must error")
	}
	// A counter reset (negative busy delta) degrades to 0, not garbage.
	reset := []cpu.TimesStat{{CPU: "cpu0", User: 0.5, Idle: 1.1}}
	percents, _, err = perCoreUsageFromTimes(before, reset, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if percents[0] != 0 {
		t.Fatalf("negative busy delta should clamp to 0, got %.2f", percents[0])
	}
}

func TestParseCoreTopologyUsesLevelOrderNotLevelNames(t *testing.T) {
	// M5 Pro: perflevel0 is "Super" (6), perflevel1 is "Performance" (12), and
	// there is no "Efficiency" level at all. Name matching reported 12P + 0E
	// here, which also hid the "P+E" load line because it needs both non-zero.
	if p, e := parseCoreTopology("6\n12\n"); p != 6 || e != 12 {
		t.Fatalf("M5 Pro topology: expected 6P + 12E, got %dP + %dE", p, e)
	}
	// M4 Pro, where the names did work: 10 performance + 4 efficiency.
	if p, e := parseCoreTopology("10\n4\n"); p != 10 || e != 4 {
		t.Fatalf("M4 Pro topology: expected 10P + 4E, got %dP + %dE", p, e)
	}

	// A single-level machine, a short read, or garbage reports nothing rather
	// than a half-filled topology.
	for _, out := range []string{"", "10\n", "10\nnope\n", "0\n0\n", "-4\n2\n"} {
		if p, e := parseCoreTopology(out); p != 0 || e != 0 {
			t.Fatalf("unusable output %q: expected 0P + 0E, got %dP + %dE", out, p, e)
		}
	}
}

// stubFastCPUTimes replays per-core time snapshots and resets the fast sampler.
func stubFastCPUTimes(t *testing.T, snapshots ...[]cpu.TimesStat) {
	t.Helper()
	origTimes := cpuTimesFunc
	fastCPUMu.Lock()
	origPrev, origAt := fastCPUTimes, fastCPUAt
	fastCPUTimes, fastCPUAt = nil, time.Time{}
	fastCPUMu.Unlock()
	t.Cleanup(func() {
		cpuTimesFunc = origTimes
		fastCPUMu.Lock()
		fastCPUTimes, fastCPUAt = origPrev, origAt
		fastCPUMu.Unlock()
	})
	next := 0
	cpuTimesFunc = func(bool) ([]cpu.TimesStat, error) {
		if next >= len(snapshots) {
			t.Fatalf("cpu.Times called %d times, only %d snapshots", next+1, len(snapshots))
		}
		snap := snapshots[next]
		next++
		return snap, nil
	}
}

// The fast path used cpu.Percent(0, true), which divides by raw ticks: between
// full refreshes a parked E-core read 90-100% (#1237). It must apply the same
// wall-clock floor as the full path over the time since the previous tick.
func TestFastCPUPercentsFloorsParkedCoreAtTickWindow(t *testing.T) {
	stubFastCPUTimes(t,
		[]cpu.TimesStat{{CPU: "cpu0", User: 10, Idle: 100}, {CPU: "cpu1", User: 10, Idle: 100}},
		// One second later: cpu0 was awake 0.05s, all busy, with no idle
		// ticks (parked); cpu1 was awake the whole second, half busy.
		[]cpu.TimesStat{{CPU: "cpu0", User: 10.05, Idle: 100}, {CPU: "cpu1", User: 10.5, Idle: 100.5}},
	)
	start := time.Now()

	if _, _, ok := fastCPUPercents(start); ok {
		t.Fatal("the first tick only records a baseline")
	}
	percents, total, ok := fastCPUPercents(start.Add(time.Second))
	if !ok {
		t.Fatal("the second tick should produce a reading")
	}
	if !almostEqual(percents[0], 5) || !almostEqual(percents[1], 50) {
		t.Fatalf("expected the parked core at 5%% and the awake core at 50%%, got %v", percents)
	}
	if !almostEqual(total, 27.5) {
		t.Fatalf("expected a window-weighted total of 27.5%%, got %.2f", total)
	}
}

func TestFastCPUPercentsKeepsBaselineForShortWindowAndResetsOnClockBack(t *testing.T) {
	base := []cpu.TimesStat{{CPU: "cpu0", User: 10, Idle: 100}}
	stubFastCPUTimes(t,
		base,
		[]cpu.TimesStat{{CPU: "cpu0", User: 10.05, Idle: 100.05}},
		[]cpu.TimesStat{{CPU: "cpu0", User: 10.25, Idle: 100.75}},
		[]cpu.TimesStat{{CPU: "cpu0", User: 11, Idle: 101}},
	)
	start := time.Now()

	fastCPUPercents(start)
	if _, _, ok := fastCPUPercents(start.Add(100 * time.Millisecond)); ok {
		t.Fatal("a 100ms window is too short to report")
	}
	// The baseline is still the first snapshot, so this window is a full second.
	percents, _, ok := fastCPUPercents(start.Add(time.Second))
	if !ok || !almostEqual(percents[0], 25) {
		t.Fatalf("expected 25%% over the second since the first tick, got %v (ok=%v)", percents, ok)
	}
	if _, _, ok := fastCPUPercents(start); ok {
		t.Fatal("a clock that went back must reset the baseline, not report")
	}
}

// cpusTreeFixture is `ioreg -p IODeviceTree -r -n cpus -l -w0` on an M1 Pro,
// trimmed to three CPUs. cpu1 lists cluster-type before logical-cpu-id.
const cpusTreeFixture = `+-o cpus  <class IOPlatformDevice, id 0x10000028c, registered, matched, active, busy 0 (62 ms), retain 16>
  | {
  |   "#size-cells" = <00000000>
  | }
  |
  +-o cpu0@0  <class IOPlatformDevice, id 0x100000280, registered, matched, active, busy 0 (4 ms), retain 8>
  |   {
  |     "logical-cluster-id" = 0
  |     "logical-cpu-id" = 0
  |     "cluster-type" = <"E">
  |   }
  |
  +-o cpu1@1  <class IOPlatformDevice, id 0x100000281, registered, matched, active, busy 0 (61 ms), retain 8>
  |   {
  |     "cluster-type" = <"E">
  |     "logical-cpu-id" = 1
  |   }
  |
  +-o cpu2@100  <class IOPlatformDevice, id 0x100000282, registered, matched, active, busy 0 (71 ms), retain 8>
      {
        "cluster-type" = <"P">
        "logical-cpu-id" = 2
      }
`

func TestParseCoreClusterTypes(t *testing.T) {
	got := parseCoreClusterTypes(cpusTreeFixture)
	if len(got) != 3 || got[0] != "E" || got[1] != "E" || got[2] != "P" {
		t.Fatalf("expected E E P, got %v", got)
	}
}

func TestParseCoreClusterTypesRejectsGapsAndEmptyTrees(t *testing.T) {
	gap := strings.Replace(cpusTreeFixture, `"logical-cpu-id" = 1`, `"logical-cpu-id" = 5`, 1)
	if got := parseCoreClusterTypes(gap); got != nil {
		t.Fatalf("a missing logical CPU must not yield a partial map, got %v", got)
	}
	if got := parseCoreClusterTypes("+-o cpus  <class IOPlatformDevice>\n"); got != nil {
		t.Fatalf("a tree without cluster types yields nil, got %v", got)
	}
}
