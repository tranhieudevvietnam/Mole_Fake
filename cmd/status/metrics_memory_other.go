//go:build !darwin

package main

import "errors"

func platformVMPressureLevel() (uint32, error) {
	return 0, errors.New("kern.memorystatus_vm_pressure_level is macOS only")
}
