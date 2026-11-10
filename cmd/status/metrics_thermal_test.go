package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/sensors"
)

// m1ProMacOS27Sensors is the HID sensor set an M1 Pro reports on macOS 27:
// no pACC/eACC cluster sensors, only PMU, NAND and battery sensors, none of
// which is a CPU temperature.
var m1ProMacOS27Sensors = []sensors.TemperatureStat{
	{SensorKey: "NAND CH0 temp", Temperature: 40.0},
	{SensorKey: "PMU TP1g", Temperature: 53.5},
	{SensorKey: "PMU tcal", Temperature: 51.9},
	{SensorKey: "PMU tdev4", Temperature: 39.1},
	{SensorKey: "PMU tdie0", Temperature: 53.4},
	{SensorKey: "PMU tdie1", Temperature: 53.7},
	{SensorKey: "PMU tdie10", Temperature: 53.1},
	{SensorKey: "gas gauge battery", Temperature: 37.4},
}

// clusterSensors are HID CPU cluster sensors, as some Apple Silicon releases
// expose them.
var clusterSensors = []sensors.TemperatureStat{
	{SensorKey: "pACC MTR Temp Sensor0", Temperature: 70.0},
	{SensorKey: "eACC MTR Temp Sensor1", Temperature: 60.0},
}

func TestCPUTempsFromSensorsIgnoresPMUDieSensors(t *testing.T) {
	if got := cpuTempsFromSensors(m1ProMacOS27Sensors); got != nil {
		t.Fatalf("PMU tdie tracks the power-management chip, not the CPU; got %v", got)
	}
}

func TestCPUTempsFromSensorsUsesClusterSensors(t *testing.T) {
	got := cpuTempsFromSensors(append(slices.Clone(clusterSensors), m1ProMacOS27Sensors...))
	if averageTemp(got) != 65.0 || maxTemp(got) != 70.0 {
		t.Fatalf("expected the pACC/eACC readings (avg 65, max 70), got %v", got)
	}
}

func TestCPUTempsFromSensorsReadsIntelSMCKeys(t *testing.T) {
	stats := []sensors.TemperatureStat{
		{SensorKey: "TA0P", Temperature: 30.0},
		{SensorKey: "TB0T", Temperature: 33.0},
		{SensorKey: "TC0P", Temperature: 61.0},
		{SensorKey: "TG0D", Temperature: 0},
	}

	if got := cpuTempsFromSensors(stats); averageTemp(got) != 61.0 {
		t.Fatalf("expected the Intel CPU proximity 61, got %v", got)
	}
}

func TestCPUTempsFromSensorsIgnoresNonCPUSensors(t *testing.T) {
	stats := []sensors.TemperatureStat{
		{SensorKey: "gas gauge battery", Temperature: 37.4},
		{SensorKey: "NAND CH0 temp", Temperature: 40.0},
		{SensorKey: "PMU tdev1", Temperature: 45.9},
	}

	if got := cpuTempsFromSensors(stats); got != nil {
		t.Fatalf("battery, NAND and tdev sensors are not CPU temperatures, got %v", got)
	}
}

func TestCPUTempsFromSensorsSkipsImplausibleReadings(t *testing.T) {
	stats := []sensors.TemperatureStat{
		{SensorKey: "pACC MTR Temp Sensor0", Temperature: 0},
		{SensorKey: "pACC MTR Temp Sensor1", Temperature: -12},
		{SensorKey: "pACC MTR Temp Sensor2", Temperature: 200},
		{SensorKey: "pACC MTR Temp Sensor3", Temperature: math.NaN()},
		{SensorKey: "pACC MTR Temp Sensor4", Temperature: 50.0},
	}

	got := cpuTempsFromSensors(stats)
	if averageTemp(got) != 50.0 || maxTemp(got) != 50.0 {
		t.Fatalf("expected only the plausible 50, got %v", got)
	}
}

func TestDecodeSMCFanRPM(t *testing.T) {
	mustHex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	cases := []struct {
		name   string
		typ    string
		data   []byte
		want   float64
		wantOK bool
	}{
		// Raw F0Ac bytes read from an M1 Pro.
		{"apple silicon float", "flt ", mustHex("ce0b1545"), 2384.7, true},
		{"intel fpe2", "fpe2", []byte{0x25, 0x80}, 2400, true},
		{"stopped fan", "flt ", mustHex("00000000"), 0, true},
		{"negative float", "flt ", mustHex("000096c4"), 0, false},
		{"wrong size", "flt ", []byte{0x01, 0x02}, 0, false},
		{"unknown type", "ui16", []byte{0x09, 0x60}, 0, false},
	}
	for _, tc := range cases {
		got, ok := decodeSMCFanRPM(tc.typ, tc.data)
		if ok != tc.wantOK || math.Abs(got-tc.want) > 0.1 {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// m1ProFans is what the SMC of a two-fan M1 Pro reports: F0Ac/F0Mx and
// F1Ac/F1Mx, named in SMC order.
var m1ProFans = []FanReading{
	{Name: "Left", RPM: 2385, MaxRPM: 5779},
	{Name: "Right", RPM: 2561, MaxRPM: 6241},
}

func TestFanName(t *testing.T) {
	cases := []struct {
		index, count int
		want         string
	}{
		{0, 1, "Fan"},
		{0, 2, "Left"},
		{1, 2, "Right"},
		{0, 3, "Fan 1"},
		{2, 3, "Fan 3"},
	}
	for _, tc := range cases {
		if got := fanName(tc.index, tc.count); got != tc.want {
			t.Errorf("fanName(%d, %d) = %q, want %q", tc.index, tc.count, got, tc.want)
		}
	}
}

func TestNewFanReadingRoundsAndDropsUnknownMax(t *testing.T) {
	if got := newFanReading(1, 2, 2561.1, 6241.0); got != (FanReading{Name: "Right", RPM: 2561, MaxRPM: 6241}) {
		t.Fatalf("unexpected reading %+v", got)
	}
	if got := newFanReading(0, 1, 1800.6, 0); got != (FanReading{Name: "Fan", RPM: 1801}) {
		t.Fatalf("a missing maximum must stay unset, got %+v", got)
	}
}

func TestFanSummary(t *testing.T) {
	if count, rpm := fanSummary(m1ProFans); count != 2 || rpm != 2473 {
		t.Fatalf("expected 2 fans at 2473 RPM, got %d at %d", count, rpm)
	}
	if count, rpm := fanSummary(nil); count != 0 || rpm != 0 {
		t.Fatalf("expected no fans, got %d at %d", count, rpm)
	}
}

// stubThermalReaders swaps the hardware readers and the command runner, and
// empties the system_profiler cache so a test sees only its own fixtures.
func stubThermalReaders(t *testing.T, fans func() ([]FanReading, error), temps func() ([]sensors.TemperatureStat, error), cmd func(context.Context, string, ...string) (string, error)) {
	t.Helper()
	origFans, origTemps, origCores, origRunCmd := readFans, readTemperatureSensors, readCPUCoreTemps, runCmd
	powerCacheMu.Lock()
	origCached, origAt := cachedPower, lastPowerAt
	cachedPower, lastPowerAt = "", time.Time{}
	powerCacheMu.Unlock()
	t.Cleanup(func() {
		readFans, readTemperatureSensors, readCPUCoreTemps, runCmd = origFans, origTemps, origCores, origRunCmd
		powerCacheMu.Lock()
		cachedPower, lastPowerAt = origCached, origAt
		powerCacheMu.Unlock()
	})
	readFans, readTemperatureSensors, runCmd = fans, temps, cmd
	// No SMC core keys unless a test sets them, so the HID path is what runs.
	readCPUCoreTemps = func() ([]float64, error) { return nil, nil }
}

func TestCPUCoreTempKeys(t *testing.T) {
	for _, brand := range []string{"Apple M1", "Apple M1 Pro", "Apple M1 Max", "Apple M1 Ultra"} {
		if keys := cpuCoreTempKeys(brand); len(keys) != 10 || keys[0] != "Tp01" || keys[9] != "Tp0b" {
			t.Errorf("%s: expected the ten M1 core keys, got %v", brand, keys)
		}
	}
	for _, brand := range []string{"Apple M2 Pro", "Apple M3 Max", "Apple M4", "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz", ""} {
		if keys := cpuCoreTempKeys(brand); keys != nil {
			t.Errorf("%q: core keys are unmeasured and must use the HID fallback, got %v", brand, keys)
		}
	}
}

func TestDecodeSMCTemp(t *testing.T) {
	flt := func(v float32) []byte {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, math.Float32bits(v))
		return b
	}
	cases := []struct {
		name   string
		typ    string
		data   []byte
		want   float64
		wantOK bool
	}{
		{"apple silicon core", "flt ", flt(66.3), 66.3, true},
		{"intel sp78", "sp78", []byte{0x42, 0x80}, 66.5, true},
		{"zero is no reading", "flt ", flt(0), 0, false},
		{"negative sp78", "sp78", []byte{0xff, 0x00}, 0, false},
		{"implausible", "flt ", flt(200), 0, false},
		{"wrong size", "flt ", []byte{0x01}, 0, false},
		{"unknown type", "ui8 ", []byte{0x42}, 0, false},
	}
	for _, tc := range cases {
		got, ok := decodeSMCTemp(tc.typ, tc.data)
		if ok != tc.wantOK || math.Abs(got-tc.want) > 0.01 {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestAverageTempSkipsImplausibleReadings(t *testing.T) {
	if got := averageTemp([]float64{66.3, 68.0, 0, 200, math.NaN(), 65.5}); math.Abs(got-66.6) > 0.001 {
		t.Fatalf("expected 66.6, got %v", got)
	}
	if got := averageTemp(nil); got != 0 {
		t.Fatalf("no readings must stay 0, got %v", got)
	}
	if got := maxTemp([]float64{66.34, 68.04, 200, math.NaN(), 65.5}); got != 68.0 {
		t.Fatalf("expected the hottest plausible 68.0, got %v", got)
	}
	if got := maxTemp(nil); got != 0 {
		t.Fatalf("no readings must stay 0, got %v", got)
	}
}

func TestCollectThermalPrefersSMCCoreTempsForCPU(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	stubThermalReaders(t,
		func() ([]FanReading, error) { return nil, errors.New("no SMC") },
		func() ([]sensors.TemperatureStat, error) { return m1ProMacOS27Sensors, nil },
		func(context.Context, string, ...string) (string, error) { return "", errors.New("command not mocked") },
	)
	readCPUCoreTemps = func() ([]float64, error) { return []float64{66.3, 68.0, 65.5}, nil }

	thermal := collectThermal()

	if thermal.CPUTemp != 66.6 || thermal.CPUMaxTemp != 68.0 {
		t.Fatalf("expected the core average 66.6 and hottest core 68.0, got %v and %v", thermal.CPUTemp, thermal.CPUMaxTemp)
	}
}

func TestCollectThermalFallsBackToHIDWhenCoreKeysFail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	stubThermalReaders(t,
		func() ([]FanReading, error) { return nil, errors.New("no SMC") },
		func() ([]sensors.TemperatureStat, error) {
			return append(slices.Clone(clusterSensors), m1ProMacOS27Sensors...), nil
		},
		func(context.Context, string, ...string) (string, error) { return "", errors.New("command not mocked") },
	)
	readCPUCoreTemps = func() ([]float64, error) { return nil, errors.New("AppleSMC open failed") }

	thermal := collectThermal()

	if thermal.CPUTemp != 65.0 || thermal.CPUMaxTemp != 70.0 {
		t.Fatalf("without core keys the HID cluster sensors give avg 65 and max 70, got %v and %v", thermal.CPUTemp, thermal.CPUMaxTemp)
	}
}

func TestCollectThermalLeavesCPUTempUnsetWithOnlyPMUSensors(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	stubThermalReaders(t,
		func() ([]FanReading, error) { return nil, errors.New("no SMC") },
		func() ([]sensors.TemperatureStat, error) { return m1ProMacOS27Sensors, nil },
		func(context.Context, string, ...string) (string, error) { return "", errors.New("command not mocked") },
	)

	thermal := collectThermal()

	if thermal.CPUTemp != 0 || thermal.CPUMaxTemp != 0 {
		t.Fatalf("a PMU reading must not stand in for the CPU, got %v and %v", thermal.CPUTemp, thermal.CPUMaxTemp)
	}
}

func TestCollectThermalReadsSMCFansAndCPUSensors(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	profilerRead := false
	stubThermalReaders(t,
		func() ([]FanReading, error) { return m1ProFans, nil },
		func() ([]sensors.TemperatureStat, error) { return m1ProMacOS27Sensors, nil },
		func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "system_profiler" {
				profilerRead = true
			}
			return "", errors.New("command not mocked")
		},
	)

	readCPUCoreTemps = func() ([]float64, error) { return []float64{66.3, 68.0, 65.5}, nil }

	thermal := collectThermal()

	if thermal.FanCount != 2 || thermal.FanSpeed != 2473 {
		t.Fatalf("expected 2 fans at 2473 RPM, got %d at %d", thermal.FanCount, thermal.FanSpeed)
	}
	if len(thermal.Fans) != 2 || thermal.Fans[0] != m1ProFans[0] || thermal.Fans[1] != m1ProFans[1] {
		t.Fatalf("expected each fan kept in SMC order, got %+v", thermal.Fans)
	}
	if thermal.CPUTemp != 66.6 || thermal.CPUMaxTemp != 68.0 {
		t.Fatalf("expected CPU avg 66.6 and max 68.0, got %v and %v", thermal.CPUTemp, thermal.CPUMaxTemp)
	}
	if profilerRead {
		t.Fatal("system_profiler is the fan fallback and must not run when the SMC answers")
	}
}

func TestCollectThermalFallsBackToSystemProfilerFanWhenSMCFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	stubThermalReaders(t,
		func() ([]FanReading, error) { return nil, errors.New("AppleSMC open failed") },
		func() ([]sensors.TemperatureStat, error) { return nil, errors.New("no HID sensors") },
		func(_ context.Context, name string, args ...string) (string, error) {
			if name == "system_profiler" && strings.Join(args, " ") == "SPPowerDataType" {
				return "      Fan Speed: 1800 rpm\n", nil
			}
			return "", errors.New("command not mocked")
		},
	)

	thermal := collectThermal()

	if thermal.FanSpeed != 1800 {
		t.Fatalf("expected the system_profiler fan speed 1800, got %d", thermal.FanSpeed)
	}
	if thermal.CPUTemp != 0 {
		t.Fatalf("CPU temp must stay unset without CPU sensors, got %v", thermal.CPUTemp)
	}
}

func TestCollectThermalKeepsZeroFansFromSMC(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("thermal collection is darwin-only")
	}
	profilerRead := false
	stubThermalReaders(t,
		func() ([]FanReading, error) { return []FanReading{}, nil },
		func() ([]sensors.TemperatureStat, error) { return nil, nil },
		func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "system_profiler" {
				profilerRead = true
				return "      Fan Speed: 1800 rpm\n", nil
			}
			return "", errors.New("command not mocked")
		},
	)

	thermal := collectThermal()

	if thermal.FanCount != 0 || thermal.FanSpeed != 0 || len(thermal.Fans) != 0 {
		t.Fatalf("a fanless Mac must report no fans, got %d at %d (%+v)", thermal.FanCount, thermal.FanSpeed, thermal.Fans)
	}
	if profilerRead {
		t.Fatal("an SMC answer of zero fans is final and must not fall back to system_profiler")
	}
}
