package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

// stubSelfUsage replays CPU times for this process and fixes its memory.
func stubSelfUsage(t *testing.T, cpu *time.Duration, memory uint64) {
	t.Helper()
	origCPU, origMem := readSelfCPUTime, readSelfMemoryUsed
	t.Cleanup(func() { readSelfCPUTime, readSelfMemoryUsed = origCPU, origMem })
	readSelfCPUTime = func() (time.Duration, bool) { return *cpu, true }
	readSelfMemoryUsed = func() uint64 { return memory }
}

func TestSelfUsageAveragesOverTheWindow(t *testing.T) {
	var cpu time.Duration
	stubSelfUsage(t, &cpu, 25<<20)
	collector := NewCollector(ProcessWatchOptions{})
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

	if _, ok := collector.selfUsageAt(start); ok {
		t.Fatal("one sample is not a rate")
	}
	cpu = 2 * time.Second
	if _, ok := collector.selfUsageAt(start.Add(time.Second)); ok {
		t.Fatal("a one-second span is start-up noise, not a reading")
	}
	cpu = 2*time.Second + 250*time.Millisecond
	if got, ok := collector.selfUsageAt(start.Add(selfUsageMinSpan)); !ok || math.Abs(got.CPUPercent-45) > 1e-9 || got.Memory != 25<<20 {
		t.Fatalf("expected 45%% of a core over five seconds and 25 MiB, got %+v (ok=%v)", got, ok)
	}
	cpu = 500 * time.Millisecond
	collector.selfUsageAt(start.Add(10 * time.Second))
	// Forty seconds in, the baseline is the +10s sample: 2.5 s over 30 s.
	cpu = 3 * time.Second
	got, ok := collector.selfUsageAt(start.Add(40 * time.Second))
	if !ok || math.Abs(got.CPUPercent-2500.0/30000*100) > 1e-9 {
		t.Fatalf("expected %.2f%% over the last window, got %+v", 2500.0/30000*100, got)
	}
}

func TestSelfUsageStartsOverWhenTheClockGoesBack(t *testing.T) {
	var cpu time.Duration
	stubSelfUsage(t, &cpu, 1)
	collector := NewCollector(ProcessWatchOptions{})
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	collector.selfUsageAt(start)
	cpu = time.Second
	collector.selfUsageAt(start.Add(10 * time.Second))
	if _, ok := collector.selfUsageAt(start.Add(8 * time.Second)); ok {
		t.Fatal("a clock that went back must start a new window")
	}
}

func TestSelfCPUTimeCountsThisProcess(t *testing.T) {
	before, ok := selfCPUTime()
	if !ok {
		t.Fatal("getrusage should be readable")
	}
	deadline := time.Now().Add(30 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ {
		_ = x * x
	}
	if after, _ := selfCPUTime(); after <= before {
		t.Fatalf("busy work must add CPU time: %v then %v", before, after)
	}
	if selfMemoryFootprint() == 0 {
		t.Fatal("the memory footprint should be readable")
	}
}

func TestProcessCardShowsMoleOwnUsage(t *testing.T) {
	stale := false
	snapshot := MetricsSnapshot{ProcessStale: &stale, TopProcesses: []ProcessInfo{{Name: "dart", CPU: 40}}}
	processLines := func(s MetricsSnapshot) []string {
		for _, card := range buildCards(s, 46, 0, true) {
			if card.title == "Processes" {
				var out []string
				for _, l := range card.lines {
					out = append(out, stripANSI(l))
				}
				return out
			}
		}
		t.Fatal("no Processes card")
		return nil
	}
	if lines := processLines(snapshot); strings.Contains(strings.Join(lines, "\n"), "Mole") {
		t.Fatalf("no reading yet, no Mole row: %q", lines)
	}
	snapshot.Self = &SelfUsage{CPUPercent: 5.1, Memory: 25 << 20}
	lines := processLines(snapshot)
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "Mole   ") || !strings.Contains(last, "  5.1% ") || !strings.HasSuffix(last, "25.0M") {
		t.Fatalf("expected the Mole row last, laid out like a process row, got %q", last)
	}
}
