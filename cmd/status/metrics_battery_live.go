package main

import (
	"fmt"
	"slices"
)

// liveBattery is the battery and charger state from the IOKit Power Sources
// API, the source pmset -g batt reads, taken without running pmset so the
// fast collection can refresh the Power card.
type liveBattery struct {
	percent      float64
	status       string // pmset's words: charging, discharging, charged, AC, finishing
	timeLeft     string // "h:mm", or empty while macOS is still estimating
	adapterWatts float64
}

// readLiveBattery is swapped in tests; the platform implementations live in
// metrics_ports_darwin.go and metrics_ports_other.go.
var readLiveBattery = platformLiveBattery

// batteryStatusFromPowerSource names the state the way pmset -g batt does, so
// formatBatteryStatus and isPoweredByAC read it unchanged.
func batteryStatusFromPowerSource(onAC, charging, charged, finishing bool) string {
	switch {
	case !onAC:
		return "discharging"
	case finishing:
		return "finishing"
	case charging:
		return "charging"
	case charged:
		return "charged"
	default:
		return "AC"
	}
}

// formatMinutes renders a Power Sources time estimate as pmset does ("1:05").
// A negative value means macOS is still estimating.
func formatMinutes(minutes int64) string {
	if minutes < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%02d", minutes/60, minutes%60)
}

// applyLiveBattery puts a live reading into the snapshot. Health, cycles and
// capacity change slowly and stay as the last full collection read them.
func applyLiveBattery(snapshot *MetricsSnapshot, live liveBattery) {
	batteries := slices.Clone(snapshot.Batteries)
	if len(batteries) == 0 {
		batteries = []BatteryStatus{{}}
	}
	batteries[0].Percent = live.percent
	batteries[0].Status = live.status
	batteries[0].TimeLeft = live.timeLeft
	snapshot.Batteries = batteries
	snapshot.Thermal.AdapterPower = live.adapterWatts
}
