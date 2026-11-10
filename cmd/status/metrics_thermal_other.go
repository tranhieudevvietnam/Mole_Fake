//go:build !darwin

package main

import (
	"errors"

	"github.com/shirou/gopsutil/v4/sensors"
)

var errThermalUnsupported = errors.New("thermal sensors are only read on macOS")

func platformTemperatureSensors() ([]sensors.TemperatureStat, error) {
	return nil, errThermalUnsupported
}

func platformCPUCoreTemps() ([]float64, error) {
	return nil, errThermalUnsupported
}

func platformFans() ([]FanReading, error) {
	return nil, errThermalUnsupported
}
