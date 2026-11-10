//go:build !darwin

package main

import "errors"

func platformGPUStats() (gpuStats, error) {
	return gpuStats{}, errors.New("GPU performance statistics are only read on macOS")
}
