package main

import (
	"context"
	"errors"
	"math"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	gopsutilnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

// m1ProPortNodes is what the IORegistry reports on an M1 Pro MacBook Pro with
// a 67W charger on MagSafe, a display on USB-C 3 and the other ports empty.
func m1ProPortNodes() []portNode {
	return []portNode{
		{name: "USB-C 1", usbC: true, number: 1},
		{name: "USB-C 2", usbC: true, number: 2},
		{name: "USB-C 3", usbC: true, number: 3, connected: true},
		{name: "MagSafe", number: 1, connected: true, contractVoltage: 20, contractPower: 65.2},
	}
}

// batteryPowerFixture carries the AppleSmartBattery lines the port card reads.
// AdapterDetails also has "Current" and "AdapterVoltage", and PowerOutDetails
// puts "VConnCurrent" before "Current": neither may leak into the readings.
const batteryPowerFixture = `  | |   "AdapterDetails" = {"Watts"=65,"Current"=3260,"AdapterVoltage"=20000,"Name"="67W USB-C Power Adapter"}
  | |   "PowerTelemetryData" = {"SystemCurrentIn"=1313,"SystemVoltageIn"=20188,"SystemPowerIn"=26513,"AdapterEfficiencyLoss"=622}
  | |   "PowerOutDetails" = ({"PowerState"=0,"VConnCurrent"=3,"PortIndex"=3,"Watts"=1014,"ConfiguredVoltage"=5000,"Current"=193,"ConfiguredCurrent"=3000,"PDPowermW"=15000,"AdapterVoltage"=5243})
`

// smcLiveFixture is the SMC's per-second view a moment later: more input power
// and the display drawing more current on USB-C 3 (D3), other ports idle.
func smcLiveFixture() livePortPower {
	return livePortPower{
		inVoltage: 20.0, inCurrent: 2.3, inPower: 46.6,
		out: map[int]portOutput{1: {}, 2: {}, 3: {voltage: 5.2, current: 0.7}, 4: {}},
	}
}

// stubPortReaders swaps the port readers and the cached AppleSmartBattery data.
func stubPortReaders(t *testing.T, nodes func() ([]portNode, error), live func() (livePortPower, error), cached batteryPortPower) {
	t.Helper()
	origNodes, origLive, origBattery := readPortNodes, readLivePortPower, readLiveBattery
	batteryPortPowerMu.Lock()
	origCached := lastBatteryPortPower
	lastBatteryPortPower = cached
	batteryPortPowerMu.Unlock()
	t.Cleanup(func() {
		readPortNodes, readLivePortPower, readLiveBattery = origNodes, origLive, origBattery
		batteryPortPowerMu.Lock()
		lastBatteryPortPower = origCached
		batteryPortPowerMu.Unlock()
	})
	readPortNodes, readLivePortPower = nodes, live
	// No live battery unless a test sets one.
	readLiveBattery = func() (liveBattery, error) { return liveBattery{}, errors.New("no battery stub") }
}

func TestNewPortNode(t *testing.T) {
	cases := []struct {
		node   string
		want   string
		usbC   bool
		number int
		ok     bool
	}{
		{"Port-USB-C@3", "USB-C 3", true, 3, true},
		{"Port-MagSafe 3@1", "MagSafe", false, 1, true},
		{"Port-MagSafe 3@2", "MagSafe 2", false, 2, true},
		{"Port-HDMI@1", "", false, 0, false},
		{"Port-SD Card@1", "", false, 0, false},
		{"Port-USB-C@x", "", false, 0, false},
	}
	for _, tc := range cases {
		got, ok := newPortNode(tc.node)
		if ok != tc.ok || got.name != tc.want || got.usbC != tc.usbC || got.number != tc.number {
			t.Errorf("newPortNode(%q) = %+v, %v; want %q usbC=%v number=%d ok=%v", tc.node, got, ok, tc.want, tc.usbC, tc.number, tc.ok)
		}
	}
}

func TestParseBatteryPortPower(t *testing.T) {
	power := parseBatteryPortPower(batteryPowerFixture)
	if math.Abs(power.inVoltage-20.188) > 0.001 || math.Abs(power.inCurrent-1.313) > 0.001 || math.Abs(power.inPower-26.513) > 0.001 {
		t.Fatalf("unexpected input telemetry %+v", power)
	}
	out, ok := power.out[3]
	if !ok || math.Abs(out.voltage-5.243) > 0.001 || math.Abs(out.current-0.193) > 0.001 || out.configuredVoltage != 5 || out.maxPower != 15 {
		t.Fatalf("unexpected port 3 output %+v (found=%v)", out, ok)
	}
}

func TestMergePortPower(t *testing.T) {
	ports := mergePortPower(m1ProPortNodes(), parseBatteryPortPower(batteryPowerFixture))
	if len(ports) != 4 {
		t.Fatalf("expected 4 ports, got %+v", ports)
	}
	if ports[0] != (PortPower{Name: "USB-C 1"}) {
		t.Fatalf("an empty port carries nothing: %+v", ports[0])
	}
	usbC3 := ports[2]
	if usbC3.Direction != "out" || math.Abs(usbC3.Power-5.243*0.193) > 0.001 || usbC3.ContractVoltage != 5 || usbC3.MaxPower != 15 {
		t.Fatalf("USB-C 3 should power its device at about 1W of 15W: %+v", usbC3)
	}
	magSafe := ports[3]
	if magSafe.Direction != "in" || math.Abs(magSafe.Power-26.513) > 0.001 || math.Abs(magSafe.Voltage-20.188) > 0.001 || magSafe.ContractVoltage != 20 {
		t.Fatalf("MagSafe should take the input telemetry: %+v", magSafe)
	}
}

func TestMergePortPowerLeavesInputUnassignedWithTwoInputs(t *testing.T) {
	nodes := []portNode{
		{name: "USB-C 1", usbC: true, number: 1, connected: true, contractVoltage: 20, contractPower: 100},
		{name: "MagSafe", number: 1, connected: true, contractVoltage: 20, contractPower: 65.2},
	}
	for _, port := range mergePortPower(nodes, parseBatteryPortPower(batteryPowerFixture)) {
		if port.Direction != "in" || port.Power != 0 || port.ContractVoltage != 20 {
			t.Fatalf("system-wide readings cannot be split between two chargers: %+v", port)
		}
	}
}

func TestMergePortPowerTreatsUnpoweredPortAsConnectedOnly(t *testing.T) {
	nodes := []portNode{{name: "USB-C 2", usbC: true, number: 2, connected: true}}
	power := batteryPortPower{out: map[int]portOutput{2: {}}}
	if got := mergePortPower(nodes, power)[0]; got.Direction != "" || !got.Connected {
		t.Fatalf("a connected port at 0 V powers nothing, got %+v", got)
	}
}

func TestBatteryPortPowerWithLiveKeepsStaticFieldsAndCache(t *testing.T) {
	cached := parseBatteryPortPower(batteryPowerFixture)
	merged := cached.withLive(smcLiveFixture())

	if merged.inPower != 46.6 || merged.inVoltage != 20.0 || merged.inCurrent != 2.3 {
		t.Fatalf("expected the SMC input readings, got %+v", merged)
	}
	out := merged.out[3]
	if out.voltage != 5.2 || out.current != 0.7 || out.maxPower != 15 || out.configuredVoltage != 5 {
		t.Fatalf("expected live volts and amps with the cached maximum, got %+v", out)
	}
	if cached.out[3].current != 0.193 {
		t.Fatalf("withLive must not write into the cached map, got %+v", cached.out[3])
	}
}

func TestCollectLivePortsUsesSMCReadings(t *testing.T) {
	stubPortReaders(t,
		func() ([]portNode, error) { return m1ProPortNodes(), nil },
		func() (livePortPower, error) { return smcLiveFixture(), nil },
		parseBatteryPortPower(batteryPowerFixture),
	)

	ports, ok := collectLivePorts()

	if !ok || len(ports) != 4 {
		t.Fatalf("expected the four ports, got ok=%v %+v", ok, ports)
	}
	if magSafe := ports[3]; magSafe.Power != 46.6 || magSafe.Current != 2.3 {
		t.Fatalf("MagSafe should show the live 46.6W input, got %+v", magSafe)
	}
	if usbC3 := ports[2]; math.Abs(usbC3.Power-5.2*0.7) > 0.001 || usbC3.MaxPower != 15 {
		t.Fatalf("USB-C 3 should show the live output with its cached 15W maximum, got %+v", usbC3)
	}
}

func TestCollectLivePortsFollowsUnplugBeforeBatteryRefresh(t *testing.T) {
	// The charger came out: the IORegistry already says so, while the cached
	// AppleSmartBattery parse still shows 26.5W coming in.
	stubPortReaders(t,
		func() ([]portNode, error) {
			nodes := m1ProPortNodes()
			nodes[3] = portNode{name: "MagSafe", number: 1}
			return nodes, nil
		},
		func() (livePortPower, error) {
			return livePortPower{out: map[int]portOutput{3: {voltage: 5.2, current: 0.7}}}, nil
		},
		parseBatteryPortPower(batteryPowerFixture),
	)

	ports, _ := collectLivePorts()

	if magSafe := ports[3]; magSafe.Connected || magSafe.Direction != "" || magSafe.Power != 0 {
		t.Fatalf("an unplugged MagSafe must show free at once, got %+v", magSafe)
	}
}

func TestCollectLivePortsKeepsBatteryReadingsWhenSMCFails(t *testing.T) {
	stubPortReaders(t,
		func() ([]portNode, error) { return m1ProPortNodes(), nil },
		func() (livePortPower, error) { return livePortPower{}, errors.New("AppleSMC open failed") },
		parseBatteryPortPower(batteryPowerFixture),
	)

	ports, ok := collectLivePorts()

	if !ok || math.Abs(ports[3].Power-26.513) > 0.001 {
		t.Fatalf("without the SMC the minute-old readings still show, got ok=%v %+v", ok, ports)
	}
}

func TestCollectLivePortsReportsFailureWhenPortsUnreadable(t *testing.T) {
	stubPortReaders(t,
		func() ([]portNode, error) { return nil, errors.New("IOKit unavailable") },
		func() (livePortPower, error) { return smcLiveFixture(), nil },
		batteryPortPower{},
	)

	if ports, ok := collectLivePorts(); ok || ports != nil {
		t.Fatalf("an unreadable port list must report failure, got ok=%v %+v", ok, ports)
	}
}

func TestCollectThermalReadsPortsAndCachesBatteryData(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	stubThermalReaders(t,
		func() ([]FanReading, error) { return nil, errors.New("no SMC") },
		func() ([]sensors.TemperatureStat, error) { return nil, errors.New("no HID sensors") },
		func(_ context.Context, name string, args ...string) (string, error) {
			if name == "ioreg" && strings.Join(args, " ") == "-rn AppleSmartBattery" {
				return batteryPowerFixture, nil
			}
			return "", errors.New("command not mocked")
		},
	)
	stubPortReaders(t,
		func() ([]portNode, error) { return m1ProPortNodes(), nil },
		func() (livePortPower, error) { return livePortPower{}, errors.New("no SMC") },
		batteryPortPower{},
	)

	thermal := collectThermal()

	if len(thermal.Ports) != 4 || math.Abs(thermal.Ports[3].Power-26.513) > 0.001 {
		t.Fatalf("expected MagSafe charging at 26.5W, got %+v", thermal.Ports)
	}
	batteryPortPowerMu.Lock()
	cachedMax := lastBatteryPortPower.out[3].maxPower
	batteryPortPowerMu.Unlock()
	if cachedMax != 15 {
		t.Fatalf("the full collection must cache the AppleSmartBattery parse, got max %v", cachedMax)
	}
}

func TestCollectFastReplacesCachedPowerWithLiveRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("port power is darwin-only")
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
	stubPortReaders(t,
		func() ([]portNode, error) { return m1ProPortNodes(), nil },
		func() (livePortPower, error) { return smcLiveFixture(), nil },
		parseBatteryPortPower(batteryPowerFixture),
	)
	readLiveBattery = func() (liveBattery, error) {
		return liveBattery{percent: 89, status: "AC", adapterWatts: 65}, nil
	}

	collector := NewCollector(ProcessWatchOptions{})
	collector.hasEnrichment = true
	collector.enrichment = snapshotEnrichment{
		thermal: ThermalStatus{
			BatteryTemp: 31,
			Ports:       []PortPower{{Name: "MagSafe"}},
		},
		batteries: []BatteryStatus{{Percent: 70, Status: "discharging", CycleCount: 271, Capacity: 84}},
	}

	snapshot, err := collector.CollectFast()
	if err != nil {
		t.Fatalf("CollectFast() error = %v", err)
	}
	if len(snapshot.Thermal.Ports) != 4 || snapshot.Thermal.Ports[3].Power != 46.6 {
		t.Fatalf("the live port read must replace the cached list, got %+v", snapshot.Thermal.Ports)
	}
	if snapshot.Thermal.BatteryTemp != 31 || snapshot.Thermal.AdapterPower != 65 {
		t.Fatalf("expected the cached battery temperature and the live adapter, got %+v", snapshot.Thermal)
	}
	if b := snapshot.Batteries; len(b) != 1 || b[0].Status != "AC" || b[0].Percent != 89 || b[0].CycleCount != 271 {
		t.Fatalf("the live battery state must replace the cached one and keep its cycles, got %+v", b)
	}
	if externalCalls.Load() != 0 {
		t.Fatalf("the live port read must not run external commands, got %d", externalCalls.Load())
	}
}

func TestLivePowerForTickReadsAtMostEveryInterval(t *testing.T) {
	reads := 0
	stubPortReaders(t,
		func() ([]portNode, error) { reads++; return m1ProPortNodes(), nil },
		func() (livePortPower, error) { return smcLiveFixture(), nil },
		batteryPortPower{},
	)
	collector := NewCollector(ProcessWatchOptions{})
	start := time.Now()

	for _, offset := range []time.Duration{0, time.Second, powerRefreshInterval - time.Millisecond} {
		if ports := collector.livePowerForTick(start.Add(offset)).ports; len(ports) != 4 {
			t.Fatalf("at +%v expected the cached four ports, got %+v", offset, ports)
		}
	}
	if reads != 1 {
		t.Fatalf("expected one read inside the first interval, got %d", reads)
	}

	collector.livePowerForTick(start.Add(powerRefreshInterval))
	if reads != 2 {
		t.Fatalf("expected a fresh read once the interval passed, got %d reads", reads)
	}

	collector.livePowerForTick(start.Add(time.Second))
	if reads != 3 {
		t.Fatalf("a clock that went back must not freeze the card, got %d reads", reads)
	}
}

func TestLivePowerForTickKeepsLastReadingWhenReadFails(t *testing.T) {
	fail := false
	stubPortReaders(t,
		func() ([]portNode, error) {
			if fail {
				return nil, errors.New("IOKit unavailable")
			}
			return m1ProPortNodes(), nil
		},
		func() (livePortPower, error) { return smcLiveFixture(), nil },
		batteryPortPower{},
	)
	collector := NewCollector(ProcessWatchOptions{})
	start := time.Now()

	readLiveBattery = func() (liveBattery, error) {
		if fail {
			return liveBattery{}, errors.New("no power sources")
		}
		return liveBattery{percent: 89, status: "AC", adapterWatts: 65}, nil
	}

	if got := collector.livePowerForTick(start); len(got.ports) != 4 || !got.hasBattery {
		t.Fatalf("the first read should succeed, got %+v", got)
	}
	fail = true
	got := collector.livePowerForTick(start.Add(powerRefreshInterval))
	if len(got.ports) != 4 || !got.hasBattery || got.battery.percent != 89 {
		t.Fatalf("a failed read keeps the last good reading, got %+v", got)
	}

	empty := NewCollector(ProcessWatchOptions{})
	if got := empty.livePowerForTick(start); got.ports != nil || got.hasBattery {
		t.Fatalf("without any good read the full collection's values must stay, got %+v", got)
	}
}

func TestBatteryStatusFromPowerSourceMatchesPMSet(t *testing.T) {
	cases := []struct {
		onAC, charging, charged, finishing bool
		want                               string
	}{
		{false, false, false, false, "discharging"},
		{true, true, false, false, "charging"},
		{true, true, false, true, "finishing"},
		{true, false, true, false, "charged"},
		{true, false, false, false, "AC"},
	}
	for _, tc := range cases {
		if got := batteryStatusFromPowerSource(tc.onAC, tc.charging, tc.charged, tc.finishing); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc, got, tc.want)
		}
	}
	// The words must keep the Power card's existing reading of pmset's output.
	if !isPoweredByAC("ac") || !isPoweredByAC("charging") || isPoweredByAC("discharging") || formatBatteryStatus("AC") != "AC" {
		t.Fatal("live statuses must read the way pmset's do")
	}
}

func TestFormatMinutes(t *testing.T) {
	for minutes, want := range map[int64]string{-1: "", 0: "0:00", 5: "0:05", 65: "1:05", 600: "10:00"} {
		if got := formatMinutes(minutes); got != want {
			t.Errorf("formatMinutes(%d) = %q, want %q", minutes, got, want)
		}
	}
}

func TestApplyLiveBatteryKeepsHealthAndCopiesSlice(t *testing.T) {
	cached := []BatteryStatus{{Percent: 70, Status: "discharging", TimeLeft: "3:10", Health: "Good", CycleCount: 271, Capacity: 84}}
	snapshot := MetricsSnapshot{Batteries: cached, Thermal: ThermalStatus{AdapterPower: 0, BatteryTemp: 31}}

	applyLiveBattery(&snapshot, liveBattery{percent: 71, status: "charging", timeLeft: "1:05", adapterWatts: 65})

	got := snapshot.Batteries[0]
	want := BatteryStatus{Percent: 71, Status: "charging", TimeLeft: "1:05", Health: "Good", CycleCount: 271, Capacity: 84}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if snapshot.Thermal.AdapterPower != 65 || snapshot.Thermal.BatteryTemp != 31 {
		t.Fatalf("expected the live 65W adapter and the cached battery temperature, got %+v", snapshot.Thermal)
	}
	if cached[0].Status != "discharging" {
		t.Fatalf("the cached battery slice must not be written, got %+v", cached[0])
	}

	unplugged := MetricsSnapshot{Thermal: ThermalStatus{AdapterPower: 65}}
	applyLiveBattery(&unplugged, liveBattery{percent: 88, status: "discharging"})
	if len(unplugged.Batteries) != 1 || unplugged.Batteries[0].Percent != 88 || unplugged.Thermal.AdapterPower != 0 {
		t.Fatalf("an unplugged reading clears the adapter at once, got %+v", unplugged)
	}
}
