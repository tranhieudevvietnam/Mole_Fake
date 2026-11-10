//go:build !darwin

package main

import "runtime"

// selfMemoryFootprint is the memory the Go runtime holds from the OS.
func selfMemoryFootprint() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Sys
}
