package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// topCompressedFixture is `top -l 1 -o cmprs -stats pid,cmprs,command` output
// from an M1 Pro under memory pressure, trimmed to its header and rows.
const topCompressedFixture = `Processes: 812 total, 4 running, 808 sleeping, 4219 threads
2026/11/09 16:20:11
Load Avg: 4.12, 4.40, 4.61

PID    CMPRS  COMMAND
46963  3434M  dartaotruntime
32443  3283M  dart
33050  1590M  Code Helper (Plu
39348  1544M  Figma Helper (Re
53398  790M   Runner
39087  743M+  Figma Helper (Re
32871  460M   java
57719  332M   Figma Helper (Re
2720   12K    Google Chrome He
`

const psCommFixture = `46963 /Users/me/fvm/versions/3.44.8/bin/cache/dart-sdk/bin/dartaotruntime
32443 /Users/me/fvm/versions/3.44.8/bin/cache/dart-sdk/bin/dart
33050 /Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper (Plugin).app/Contents/MacOS/Code Helper (Plugin)
39348 /Applications/Figma.app/Contents/Frameworks/Figma Helper (Renderer).app/Contents/MacOS/Figma Helper (Renderer)
53398 /Users/me/Library/Developer/CoreSimulator/Devices/DFDA/data/Containers/Bundle/Application/D3ED/Runner.app/Runner
39087 /Applications/Figma.app/Contents/Frameworks/Figma Helper (Renderer).app/Contents/MacOS/Figma Helper (Renderer)
32871 /Users/me/Library/Java/JavaVirtualMachines/ms-17.0.15/Contents/Home/bin/java
57719 /Applications/Figma.app/Contents/Frameworks/Figma Helper (Renderer).app/Contents/MacOS/Figma Helper (Renderer)
`

func TestParseTopCompressed(t *testing.T) {
	samples := parseTopCompressed(topCompressedFixture)
	if len(samples) != 9 {
		t.Fatalf("expected 9 rows after the PID header, got %d: %+v", len(samples), samples)
	}
	first := samples[0]
	if first.pid != 46963 || first.compressed != 3434<<20 || first.command != "dartaotruntime" {
		t.Fatalf("unexpected first row %+v", first)
	}
	if samples[2].command != "Code Helper (Plu" {
		t.Fatalf("a command with spaces must stay whole, got %q", samples[2].command)
	}
	if samples[5].compressed != 743<<20 {
		t.Fatalf("top's change marker must be ignored, got %d", samples[5].compressed)
	}
}

func TestParseTopSize(t *testing.T) {
	cases := map[string]uint64{"0B": 0, "12K": 12 << 10, "790M": 790 << 20, "743M+": 743 << 20, "1.5G": 3 << 29, "2048": 2048}
	for in, want := range cases {
		if got, ok := parseTopSize(in); !ok || got != want {
			t.Errorf("parseTopSize(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "M", "abcM", "-"} {
		if _, ok := parseTopSize(bad); ok {
			t.Errorf("parseTopSize(%q) should fail", bad)
		}
	}
}

func TestSwapAppLabel(t *testing.T) {
	paths := parsePSComm(psCommFixture)
	cases := map[int]string{
		46963: "dart",
		32443: "dart",
		33050: "Visual Studio Code",
		39348: "Figma",
		53398: "Runner",
		32871: "java",
	}
	for pid, want := range cases {
		if got := swapAppLabel(paths[pid], "ignored"); got != want {
			t.Errorf("pid %d: label %q, want %q", pid, got, want)
		}
	}
	if got := swapAppLabel("", "Google Chrome He"); got != "Google Chrome He" {
		t.Fatalf("an exited process keeps top's name, got %q", got)
	}
}

func TestGroupSwapAppsSumsHelpersAndRanks(t *testing.T) {
	apps := groupSwapApps(parseTopCompressed(topCompressedFixture), parsePSComm(psCommFixture))

	want := []SwapApp{
		{Name: "dart", Compressed: (3434 + 3283) << 20},
		{Name: "Figma", Compressed: (1544 + 743 + 332) << 20},
		{Name: "Visual Studio Code", Compressed: 1590 << 20},
		{Name: "Runner", Compressed: 790 << 20},
		{Name: "java", Compressed: 460 << 20},
	}
	if len(apps) != len(want) {
		t.Fatalf("expected %d apps (Chrome's 12K is noise), got %+v", len(want), apps)
	}
	for i := range want {
		if apps[i] != want[i] {
			t.Errorf("app %d = %+v, want %+v", i, apps[i], want[i])
		}
	}
}

func TestGroupSwapAppsKeepsTheLargestFive(t *testing.T) {
	var samples []compressedSample
	for i, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		samples = append(samples, compressedSample{pid: i + 1, compressed: uint64(i+1) << 28, command: name})
	}
	apps := groupSwapApps(samples, nil)
	if len(apps) != swapAppsMax || apps[0].Name != "g" || apps[swapAppsMax-1].Name != "c" {
		t.Fatalf("expected g..c, got %+v", apps)
	}
}

func TestCollectSwapAppsRunsTopThenPS(t *testing.T) {
	origRunCmd := runCmd
	t.Cleanup(func() { runCmd = origRunCmd })
	var calls []string
	runCmd = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "top":
			return topCompressedFixture, nil
		case "ps":
			return psCommFixture, nil
		}
		return "", errors.New("command not mocked")
	}

	apps := collectSwapApps()

	if len(apps) != 5 || apps[0].Name != "dart" {
		t.Fatalf("expected dart first of five apps, got %+v", apps)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "top -l 1 -o cmprs") || !strings.HasPrefix(calls[1], "ps -o pid=,comm= -p 46963,32443,") {
		t.Fatalf("expected one top and one ps call, got %q", calls)
	}
}

func TestShouldCollectSwapApps(t *testing.T) {
	cases := []struct {
		pressure string
		swapUsed uint64
		want     bool
	}{
		{"normal", 0, false},
		{"normal", 512 << 20, false},
		{"normal", swapAppsMinSwapUsed, true},
		{"", 7 << 30, true},
		{"warn", 0, true},
		{"critical", 0, true},
	}
	for _, tc := range cases {
		if got := shouldCollectSwapApps(tc.pressure, tc.swapUsed); got != tc.want {
			t.Errorf("shouldCollectSwapApps(%q, %d) = %v, want %v", tc.pressure, tc.swapUsed, got, tc.want)
		}
	}
}

// stubSwapApps empties the swap app cache and counts top runs.
func stubSwapApps(t *testing.T) *int {
	t.Helper()
	origRunCmd := runCmd
	swapAppsMu.Lock()
	origAt, origCached := swapAppsAt, cachedSwapApps
	swapAppsAt, cachedSwapApps = time.Time{}, nil
	swapAppsMu.Unlock()
	t.Cleanup(func() {
		runCmd = origRunCmd
		swapAppsMu.Lock()
		swapAppsAt, cachedSwapApps = origAt, origCached
		swapAppsMu.Unlock()
	})
	topRuns := 0
	runCmd = func(_ context.Context, name string, _ ...string) (string, error) {
		switch name {
		case "top":
			topRuns++
			return topCompressedFixture, nil
		case "ps":
			return psCommFixture, nil
		}
		return "", errors.New("command not mocked")
	}
	return &topRuns
}

func TestSwapAppsForCachesForAMinute(t *testing.T) {
	topRuns := stubSwapApps(t)
	start := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)

	if apps := swapAppsFor("normal", 7<<30, start); len(apps) != 5 || *topRuns != 1 {
		t.Fatalf("heavy swap at normal pressure should read the apps, got %+v after %d runs", apps, *topRuns)
	}
	if apps := swapAppsFor("normal", 7<<30, start.Add(30*time.Second)); len(apps) != 5 || *topRuns != 1 {
		t.Fatalf("a read inside the minute must come from the cache, got %d runs", *topRuns)
	}
	swapAppsFor("warn", 0, start.Add(swapAppsCacheTTL))
	if *topRuns != 2 {
		t.Fatalf("expected a fresh read after a minute, got %d runs", *topRuns)
	}
	swapAppsFor("warn", 0, start)
	if *topRuns != 3 {
		t.Fatalf("a clock that went back must read again, got %d runs", *topRuns)
	}
}

func TestSwapAppsForSkipsTopWithoutPressureOrSwap(t *testing.T) {
	topRuns := stubSwapApps(t)
	if apps := swapAppsFor("normal", 200<<20, time.Now()); apps != nil || *topRuns != 0 {
		t.Fatalf("light swap at normal pressure must not run top, got %+v after %d runs", apps, *topRuns)
	}
}

func TestFastMemoryNeverReadsSwapApps(t *testing.T) {
	origRunCmd := runCmd
	t.Cleanup(func() { runCmd = origRunCmd })
	runCmd = func(_ context.Context, name string, _ ...string) (string, error) {
		t.Fatalf("the fast memory read must not run %s", name)
		return "", nil
	}
	if memory, err := collectMemoryFast(); err != nil || memory.SwapApps != nil {
		t.Fatalf("fast memory = %+v, %v", memory, err)
	}
}
