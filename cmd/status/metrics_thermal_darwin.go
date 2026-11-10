//go:build darwin

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/shirou/gopsutil/v4/sensors"
	"golang.org/x/sys/unix"
)

// platformTemperatureSensors reads the HID temperature sensors on Apple Silicon
// and the SMC temperature keys on Intel, both through gopsutil without cgo or
// root.
func platformTemperatureSensors() ([]sensors.TemperatureStat, error) {
	return sensors.TemperaturesWithContext(context.Background())
}

// SMC access for fan speeds and CPU core temperatures. gopsutil keeps its SMC helpers internal, so this
// mirrors its approach: IOKit through purego, readable without root on both
// Apple Silicon and Intel.

const (
	smcSelectorHandleYPCEvent = 2
	smcCmdReadKey             = 5
	smcCmdGetKeyInfo          = 9
	smcMaxFans                = 10
)

type smcVersion struct {
	major, minor, build, reserved byte
	release                       uint16
}

type smcPLimitData struct {
	version, length                 uint16
	cpuPLimit, gpuPLimit, memPLimit uint32
}

type smcKeyInfoData struct {
	dataSize, dataType uint32
	dataAttributes     uint8
}

// smcParam matches the 80-byte SMCKeyData_t the AppleSMC user client expects.
type smcParam struct {
	key                   uint32
	vers                  smcVersion
	pLimitData            smcPLimitData
	keyInfo               smcKeyInfoData
	result, status, data8 uint8
	data32                uint32
	bytes                 [32]byte
}

type smcLibrary struct {
	serviceMatching         func(name string) uintptr
	getMatchingService      func(mainPort uint32, matching uintptr) uint32
	serviceOpen             func(service, owningTask, connType uint32, connect *uint32) int32
	serviceClose            func(connect uint32) int32
	objectRelease           func(object uint32) int32
	connectCallStructMethod func(connection, selector uint32, in unsafe.Pointer, inCnt uintptr, out unsafe.Pointer, outCnt *uintptr) int32
	machTaskSelf            func() uint32
}

// The IOKit handle stays open for the process lifetime: closing a purego
// library while the runtime may still call into it crashes (gopsutil #1832).
var (
	smcLibOnce sync.Once
	smcLib     *smcLibrary
	smcLibErr  error
)

func loadSMCLibrary() {
	handle, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		smcLibErr = err
		return
	}
	lib := &smcLibrary{}
	// Dlsym before RegisterFunc so a missing symbol is an error, not a panic.
	bindings := []struct {
		fptr any
		name string
	}{
		{&lib.serviceMatching, "IOServiceMatching"},
		{&lib.getMatchingService, "IOServiceGetMatchingService"},
		{&lib.serviceOpen, "IOServiceOpen"},
		{&lib.serviceClose, "IOServiceClose"},
		{&lib.objectRelease, "IOObjectRelease"},
		{&lib.connectCallStructMethod, "IOConnectCallStructMethod"},
		{&lib.machTaskSelf, "mach_task_self"},
	}
	for _, b := range bindings {
		addr, err := purego.Dlsym(handle, b.name)
		if err != nil || addr == 0 {
			smcLibErr = fmt.Errorf("IOKit symbol %s unavailable: %w", b.name, err)
			return
		}
		purego.RegisterFunc(b.fptr, addr)
	}
	smcLib = lib
}

// openSMC opens a user client on AppleSMC. The caller closes conn with
// lib.serviceClose.
func openSMC() (*smcLibrary, uint32, error) {
	smcLibOnce.Do(loadSMCLibrary)
	if smcLibErr != nil {
		return nil, 0, smcLibErr
	}
	lib := smcLib

	service := lib.getMatchingService(0, lib.serviceMatching("AppleSMC"))
	if service == 0 {
		return nil, 0, errors.New("AppleSMC service not found")
	}
	var conn uint32
	kr := lib.serviceOpen(service, lib.machTaskSelf(), 0, &conn)
	lib.objectRelease(service)
	if kr != 0 {
		return nil, 0, fmt.Errorf("AppleSMC open failed: %#x", kr)
	}
	return lib, conn, nil
}

// platformCPUCoreTemps reads this chip's per-core CPU temperature keys from
// the SMC. A chip without a measured key set returns no readings, and the
// caller falls back to the HID sensors.
func platformCPUCoreTemps() ([]float64, error) {
	brand, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return nil, err
	}
	keys := cpuCoreTempKeys(brand)
	if len(keys) == 0 {
		return nil, nil
	}
	lib, conn, err := openSMC()
	if err != nil {
		return nil, err
	}
	defer lib.serviceClose(conn)

	temps := make([]float64, 0, len(keys))
	for _, key := range keys {
		typ, data, err := smcReadKey(lib, conn, key)
		if err != nil {
			continue
		}
		if t, ok := decodeSMCTemp(typ, data); ok {
			temps = append(temps, t)
		}
	}
	return temps, nil
}

func platformFans() ([]FanReading, error) {
	lib, conn, err := openSMC()
	if err != nil {
		return nil, err
	}
	defer lib.serviceClose(conn)

	typ, data, err := smcReadKey(lib, conn, "FNum")
	if err != nil {
		return nil, err
	}
	if typ != "ui8 " || len(data) != 1 {
		return nil, fmt.Errorf("unexpected FNum type %q", typ)
	}
	count := min(int(data[0]), smcMaxFans)

	fans := make([]FanReading, 0, count)
	for i := range count {
		typ, data, err := smcReadKey(lib, conn, fmt.Sprintf("F%dAc", i))
		if err != nil {
			continue
		}
		rpm, ok := decodeSMCFanRPM(typ, data)
		if !ok {
			continue
		}
		// The maximum only scales the bar, so a missing F<n>Mx keeps the fan.
		var maxRPM float64
		if typ, data, err := smcReadKey(lib, conn, fmt.Sprintf("F%dMx", i)); err == nil {
			maxRPM, _ = decodeSMCFanRPM(typ, data)
		}
		fans = append(fans, newFanReading(i, count, rpm, maxRPM))
	}
	return fans, nil
}

func smcReadKey(lib *smcLibrary, conn uint32, key string) (string, []byte, error) {
	keyCode := binary.BigEndian.Uint32([]byte(key))

	info, err := smcCall(lib, conn, &smcParam{key: keyCode, data8: smcCmdGetKeyInfo})
	if err != nil {
		return "", nil, fmt.Errorf("SMC key info %s: %w", key, err)
	}
	size := info.keyInfo.dataSize
	if size == 0 || size > uint32(len(info.bytes)) {
		return "", nil, fmt.Errorf("SMC key %s has size %d", key, size)
	}

	req := &smcParam{key: keyCode, data8: smcCmdReadKey}
	req.keyInfo.dataSize = size
	out, err := smcCall(lib, conn, req)
	if err != nil {
		return "", nil, fmt.Errorf("SMC read %s: %w", key, err)
	}

	typ := make([]byte, 4)
	binary.BigEndian.PutUint32(typ, info.keyInfo.dataType)
	data := make([]byte, size)
	copy(data, out.bytes[:size])
	return string(typ), data, nil
}

func smcCall(lib *smcLibrary, conn uint32, in *smcParam) (*smcParam, error) {
	out := new(smcParam)
	outSize := unsafe.Sizeof(*out)
	kr := lib.connectCallStructMethod(conn, smcSelectorHandleYPCEvent,
		unsafe.Pointer(in), unsafe.Sizeof(*in), unsafe.Pointer(out), &outSize)
	if kr != 0 {
		return nil, fmt.Errorf("kern_return %#x", kr)
	}
	if out.result != 0 {
		return nil, fmt.Errorf("SMC result %d", out.result)
	}
	return out, nil
}
