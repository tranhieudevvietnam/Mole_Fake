package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/shirou/gopsutil/v4/sensors"
)

// Hardware readers, swapped in tests. The platform implementations live in
// metrics_thermal_darwin.go and metrics_thermal_other.go.
var (
	readTemperatureSensors = platformTemperatureSensors
	readCPUCoreTemps       = platformCPUCoreTemps
	readFans               = platformFans
)

// m1CPUCoreTempKeys are the per-core CPU temperatures on M1, M1 Pro, M1 Max
// and M1 Ultra. Each core has three SMC keys (Tp00, Tp01, Tp02, then Tp04...);
// the middle one is the core reading other Mac thermal tools use, and on an
// M1 Pro all ten rose 2-6 °C under an all-core load while running about 10 °C
// above the die sensors (measured 2026-11-09).
var m1CPUCoreTempKeys = []string{"Tp01", "Tp05", "Tp09", "Tp0D", "Tp0H", "Tp0L", "Tp0P", "Tp0T", "Tp0X", "Tp0b"}

// cpuCoreTempKeys returns the SMC per-core CPU temperature keys for the chip
// machdep.cpu.brand_string names. M2 and later number their cores with other
// keys, so they keep the HID fallback until their keys are measured.
func cpuCoreTempKeys(brand string) []string {
	if strings.HasPrefix(brand, "Apple M1") {
		return m1CPUCoreTempKeys
	}
	return nil
}

// decodeSMCTemp decodes an SMC temperature: a little-endian float32 ("flt ")
// on Apple Silicon, or signed 7.8 fixed point ("sp78") on Intel.
func decodeSMCTemp(dataType string, data []byte) (float64, bool) {
	var t float64
	switch {
	case dataType == "flt " && len(data) == 4:
		t = float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
	case dataType == "sp78" && len(data) == 2:
		t = float64(int16(binary.BigEndian.Uint16(data))) / 256
	default:
		return 0, false
	}
	if !plausibleTemp(t) {
		return 0, false
	}
	return t, true
}

func plausibleTemp(t float64) bool {
	return !math.IsNaN(t) && t > 0 && t < maxPlausibleTempC
}

// averageTemp averages the plausible readings, rounded to 0.1 °C, or 0 when
// there are none.
func averageTemp(temps []float64) float64 {
	var sum float64
	var n int
	for _, t := range temps {
		if plausibleTemp(t) {
			sum += t
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Round(sum/float64(n)*10) / 10
}

// maxTemp returns the hottest plausible reading, rounded to 0.1 °C, or 0 when
// there are none.
func maxTemp(temps []float64) float64 {
	hottest := 0.0
	for _, t := range temps {
		if plausibleTemp(t) && t > hottest {
			hottest = t
		}
	}
	return math.Round(hottest*10) / 10
}

// cpuTempSensorGroups lists the HID sensors used for the CPU temperature when
// the chip has no known SMC core keys, in preference order. The first group
// with a usable reading wins.
//
//   - "pACC MTR Temp Sensor" / "eACC MTR Temp Sensor": performance and
//     efficiency core clusters, exposed as HID sensors on Apple Silicon.
//   - "TC0P" / "TC0D" / "TC0H": Intel SMC CPU proximity, diode and heatsink
//     keys, as gopsutil names them on Intel Macs.
//
// "PMU tdie" is deliberately absent. On an M1 Pro it reads exactly the SMC
// TPD* keys and barely moves with CPU load (53 to 57 °C while the core keys
// went from 66 to 93 °C, 2026-11-09): it is the power-management chip, not the
// CPU. Battery, NAND and PMU tdev/TP sensors are absent for the same reason. A
// Mac with none of these sensors shows no CPU temperature rather than a wrong
// one.
var cpuTempSensorGroups = [][]string{
	{"pACC MTR Temp Sensor", "eACC MTR Temp Sensor"},
	{"TC0P", "TC0D", "TC0H"},
}

const (
	maxPlausibleTempC  = 150.0
	maxPlausibleFanRPM = 20000.0
)

// cpuTempsFromSensors returns the readings of the first CPU sensor group that
// has a plausible reading, or nil when no CPU sensor is present.
func cpuTempsFromSensors(stats []sensors.TemperatureStat) []float64 {
	for _, prefixes := range cpuTempSensorGroups {
		var temps []float64
		for _, stat := range stats {
			if hasAnyPrefix(stat.SensorKey, prefixes) {
				temps = append(temps, stat.Temperature)
			}
		}
		if averageTemp(temps) > 0 {
			return temps
		}
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// decodeSMCFanRPM decodes an SMC F<n>Ac (actual) or F<n>Mx (maximum) fan speed. Apple Silicon
// reports a little-endian float32 ("flt "); Intel Macs report fixed point
// "fpe2", a big-endian uint16 with two fraction bits.
func decodeSMCFanRPM(dataType string, data []byte) (float64, bool) {
	var rpm float64
	switch {
	case dataType == "flt " && len(data) == 4:
		rpm = float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
	case dataType == "fpe2" && len(data) == 2:
		rpm = float64(binary.BigEndian.Uint16(data)) / 4
	default:
		return 0, false
	}
	if math.IsNaN(rpm) || rpm < 0 || rpm >= maxPlausibleFanRPM {
		return 0, false
	}
	return rpm, true
}

// fanName labels fan index of a Mac with count fans. Apple Silicon SMCs carry
// no F<n>ID name key, so a two-fan Mac follows the order Intel SMCs name
// outright: F0 "Left side", F1 "Right side".
func fanName(index, count int) string {
	switch {
	case count == 1:
		return "Fan"
	case count == 2 && index == 0:
		return "Left"
	case count == 2 && index == 1:
		return "Right"
	default:
		return fmt.Sprintf("Fan %d", index+1)
	}
}

// newFanReading names fan index and rounds its speeds. A maximum that is not
// above zero is left unset, so no bar is drawn against it.
func newFanReading(index, count int, rpm, maxRPM float64) FanReading {
	fan := FanReading{Name: fanName(index, count), RPM: int(math.Round(rpm))}
	if maxRPM > 0 {
		fan.MaxRPM = int(math.Round(maxRPM))
	}
	return fan
}

// fanSummary reduces per-fan readings to the fan count and their average RPM.
func fanSummary(fans []FanReading) (count int, avgRPM int) {
	if len(fans) == 0 {
		return 0, 0
	}
	sum := 0
	for _, f := range fans {
		sum += f.RPM
	}
	return len(fans), int(math.Round(float64(sum) / float64(len(fans))))
}
