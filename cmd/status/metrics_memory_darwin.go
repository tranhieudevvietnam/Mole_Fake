//go:build darwin

package main

import "golang.org/x/sys/unix"

func platformVMPressureLevel() (uint32, error) {
	return unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
}
