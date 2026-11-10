//go:build darwin

package main

import (
	"os"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// rusageInfoV0 is struct rusage_info_v0 from <sys/resource.h>.
type rusageInfoV0 struct {
	uuid             [16]byte
	userTime         uint64
	systemTime       uint64
	pkgIdleWkups     uint64
	interruptWkups   uint64
	pageins          uint64
	wiredSize        uint64
	residentSize     uint64
	physFootprint    uint64
	procStartAbstime uint64
	procExitAbstime  uint64
}

const rusageInfoV0Flavor = 0

var (
	procRusageOnce sync.Once
	procPidRusage  func(pid int32, flavor int32, buffer unsafe.Pointer) int32
)

func loadProcPidRusage() {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return
	}
	if addr, err := purego.Dlsym(lib, "proc_pid_rusage"); err == nil && addr != 0 {
		purego.RegisterFunc(&procPidRusage, addr)
	}
}

// selfMemoryFootprint is this process's physical footprint, the Memory figure
// Activity Monitor shows, from proc_pid_rusage. Without it, the memory the Go
// runtime holds from the OS stands in.
func selfMemoryFootprint() uint64 {
	procRusageOnce.Do(loadProcPidRusage)
	if procPidRusage != nil {
		var info rusageInfoV0
		if procPidRusage(int32(os.Getpid()), rusageInfoV0Flavor, unsafe.Pointer(&info)) == 0 && info.physFootprint > 0 {
			return info.physFootprint
		}
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Sys
}
