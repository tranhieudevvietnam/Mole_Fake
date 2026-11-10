//go:build darwin

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The port list is read straight from the IORegistry through IOKit, not by
// running ioreg: the fast collection runs every second and must not start
// external commands (TestCollectFastAvoidsExternalCommands).

const (
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt64Type   = 4
	// IORegistry PortType values, as ParentPortType on a power source.
	ioPortTypeUSBC    = 2
	ioPortTypeMagSafe = 17
)

// portNodeNames are the IORegistry names of the ports that carry power. The
// "@1" that ioreg prints after a name is the entry's location, not part of
// the name, so the port number comes from the PortNumber property.
var portNodeNames = []string{"Port-USB-C", "Port-MagSafe 3"}

type ioRegistryLibrary struct {
	serviceNameMatching func(name string) uintptr
	serviceMatching     func(name string) uintptr
	getMatchingService  func(mainPort uint32, matching uintptr) uint32
	getMatchingServices func(mainPort uint32, matching uintptr, iterator *uint32) int32
	iteratorNext        func(iterator uint32) uint32
	objectRelease       func(object uint32) int32
	createCFProperty    func(entry uint32, key uintptr, allocator uintptr, options uint32) uintptr

	cfStringCreate     func(allocator uintptr, s string, encoding uint32) uintptr
	cfRelease          func(ref uintptr)
	cfGetTypeID        func(ref uintptr) uintptr
	cfBooleanTypeID    func() uintptr
	cfBooleanGetValue  func(ref uintptr) bool
	cfNumberTypeID     func() uintptr
	cfNumberGetValue   func(number uintptr, numberType int, value unsafe.Pointer) bool
	cfDictionaryTypeID func() uintptr
	cfDictionaryGet    func(dict uintptr, key uintptr) uintptr
	cfArrayGetCount    func(array uintptr) int
	cfArrayGetValue    func(array uintptr, index int) uintptr
	cfEqual            func(a, b uintptr) bool

	// IOKit Power Sources, the API pmset reads.
	psCopyInfo           func() uintptr
	psCopyList           func(blob uintptr) uintptr
	psGetDescription     func(blob, source uintptr) uintptr
	psCopyAdapterDetails func() uintptr
}

// Both libraries stay open for the process lifetime, like the SMC handle.
var (
	ioRegLibOnce sync.Once
	ioRegLib     *ioRegistryLibrary
	ioRegLibErr  error
)

func loadIORegistryLibrary() {
	iokit, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		ioRegLibErr = err
		return
	}
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		ioRegLibErr = err
		return
	}
	lib := &ioRegistryLibrary{}
	bindings := []struct {
		handle uintptr
		fptr   any
		name   string
	}{
		{iokit, &lib.serviceNameMatching, "IOServiceNameMatching"},
		{iokit, &lib.serviceMatching, "IOServiceMatching"},
		{iokit, &lib.getMatchingService, "IOServiceGetMatchingService"},
		{iokit, &lib.getMatchingServices, "IOServiceGetMatchingServices"},
		{iokit, &lib.iteratorNext, "IOIteratorNext"},
		{iokit, &lib.objectRelease, "IOObjectRelease"},
		{iokit, &lib.createCFProperty, "IORegistryEntryCreateCFProperty"},
		{cf, &lib.cfStringCreate, "CFStringCreateWithCString"},
		{cf, &lib.cfRelease, "CFRelease"},
		{cf, &lib.cfGetTypeID, "CFGetTypeID"},
		{cf, &lib.cfBooleanTypeID, "CFBooleanGetTypeID"},
		{cf, &lib.cfBooleanGetValue, "CFBooleanGetValue"},
		{cf, &lib.cfNumberTypeID, "CFNumberGetTypeID"},
		{cf, &lib.cfNumberGetValue, "CFNumberGetValue"},
		{cf, &lib.cfDictionaryTypeID, "CFDictionaryGetTypeID"},
		{cf, &lib.cfDictionaryGet, "CFDictionaryGetValue"},
		{cf, &lib.cfArrayGetCount, "CFArrayGetCount"},
		{cf, &lib.cfArrayGetValue, "CFArrayGetValueAtIndex"},
		{cf, &lib.cfEqual, "CFEqual"},
		{iokit, &lib.psCopyInfo, "IOPSCopyPowerSourcesInfo"},
		{iokit, &lib.psCopyList, "IOPSCopyPowerSourcesList"},
		{iokit, &lib.psGetDescription, "IOPSGetPowerSourceDescription"},
		{iokit, &lib.psCopyAdapterDetails, "IOPSCopyExternalPowerAdapterDetails"},
	}
	for _, b := range bindings {
		addr, err := purego.Dlsym(b.handle, b.name)
		if err != nil || addr == 0 {
			ioRegLibErr = fmt.Errorf("symbol %s unavailable: %w", b.name, err)
			return
		}
		purego.RegisterFunc(b.fptr, addr)
	}
	ioRegLib = lib
}

// cfKey returns a CFString the caller releases.
func (lib *ioRegistryLibrary) cfKey(s string) uintptr {
	return lib.cfStringCreate(0, s, cfStringEncodingUTF8)
}

// property copies entry's top-level property key, or 0. The caller releases a
// non-zero result.
func (lib *ioRegistryLibrary) property(entry uint32, key string) uintptr {
	k := lib.cfKey(key)
	if k == 0 {
		return 0
	}
	defer lib.cfRelease(k)
	return lib.createCFProperty(entry, k, 0, 0)
}

func (lib *ioRegistryLibrary) boolProperty(entry uint32, key string) bool {
	ref := lib.property(entry, key)
	if ref == 0 {
		return false
	}
	defer lib.cfRelease(ref)
	return lib.cfGetTypeID(ref) == lib.cfBooleanTypeID() && lib.cfBooleanGetValue(ref)
}

// number reads a CFNumber that is not owned by the caller.
func (lib *ioRegistryLibrary) number(ref uintptr) (int64, bool) {
	if ref == 0 || lib.cfGetTypeID(ref) != lib.cfNumberTypeID() {
		return 0, false
	}
	var v int64
	if !lib.cfNumberGetValue(ref, cfNumberSInt64Type, unsafe.Pointer(&v)) {
		return 0, false
	}
	return v, true
}

func (lib *ioRegistryLibrary) numberProperty(entry uint32, key string) (int64, bool) {
	ref := lib.property(entry, key)
	if ref == 0 {
		return 0, false
	}
	defer lib.cfRelease(ref)
	return lib.number(ref)
}

func (lib *ioRegistryLibrary) dictNumber(dict uintptr, key string) (int64, bool) {
	k := lib.cfKey(key)
	if k == 0 {
		return 0, false
	}
	defer lib.cfRelease(k)
	return lib.number(lib.cfDictionaryGet(dict, k))
}

// dictBool reads a CFBoolean value of dict; ok is false when it is absent.
func (lib *ioRegistryLibrary) dictBool(dict uintptr, key string) (value, ok bool) {
	k := lib.cfKey(key)
	if k == 0 {
		return false, false
	}
	defer lib.cfRelease(k)
	ref := lib.cfDictionaryGet(dict, k)
	if ref == 0 || lib.cfGetTypeID(ref) != lib.cfBooleanTypeID() {
		return false, false
	}
	return lib.cfBooleanGetValue(ref), true
}

// dictStringIs reports whether dict's value for key is the string want.
func (lib *ioRegistryLibrary) dictStringIs(dict uintptr, key, want string) bool {
	k := lib.cfKey(key)
	if k == 0 {
		return false
	}
	defer lib.cfRelease(k)
	ref := lib.cfDictionaryGet(dict, k)
	if ref == 0 {
		return false
	}
	w := lib.cfKey(want)
	if w == 0 {
		return false
	}
	defer lib.cfRelease(w)
	return lib.cfEqual(ref, w)
}

// platformLiveBattery reads the internal battery and the charger from the
// IOKit Power Sources API, as pmset -g batt does, without running pmset.
func platformLiveBattery() (liveBattery, error) {
	ioRegLibOnce.Do(loadIORegistryLibrary)
	if ioRegLibErr != nil {
		return liveBattery{}, ioRegLibErr
	}
	lib := ioRegLib

	blob := lib.psCopyInfo()
	if blob == 0 {
		return liveBattery{}, errors.New("no power source information")
	}
	defer lib.cfRelease(blob)
	list := lib.psCopyList(blob)
	if list == 0 {
		return liveBattery{}, errors.New("no power source list")
	}
	defer lib.cfRelease(list)

	for i := range lib.cfArrayGetCount(list) {
		desc := lib.psGetDescription(blob, lib.cfArrayGetValue(list, i))
		if desc == 0 || !lib.dictStringIs(desc, "Type", "InternalBattery") {
			continue
		}
		if present, ok := lib.dictBool(desc, "Is Present"); ok && !present {
			continue
		}
		current, ok := lib.dictNumber(desc, "Current Capacity")
		if !ok {
			continue
		}
		percent := float64(current)
		if maxCap, ok := lib.dictNumber(desc, "Max Capacity"); ok && maxCap > 0 {
			percent = float64(current) * 100 / float64(maxCap)
		}

		onAC := lib.dictStringIs(desc, "Power Source State", "AC Power")
		charging, _ := lib.dictBool(desc, "Is Charging")
		charged, _ := lib.dictBool(desc, "Is Charged")
		finishing, _ := lib.dictBool(desc, "Is Finishing Charge")

		minutes := int64(-1)
		switch {
		case !onAC:
			if m, ok := lib.dictNumber(desc, "Time to Empty"); ok {
				minutes = m
			}
		case charging:
			if m, ok := lib.dictNumber(desc, "Time to Full Charge"); ok {
				minutes = m
			}
		}

		battery := liveBattery{
			percent:  math.Round(percent),
			status:   batteryStatusFromPowerSource(onAC, charging, charged, finishing),
			timeLeft: formatMinutes(minutes),
		}
		if details := lib.psCopyAdapterDetails(); details != 0 {
			if watts, ok := lib.dictNumber(details, "Watts"); ok && watts > 0 {
				battery.adapterWatts = float64(watts)
			}
			lib.cfRelease(details)
		}
		return battery, nil
	}
	return liveBattery{}, errors.New("no internal battery")
}

// platformPortNodes lists the built-in USB-C and MagSafe ports, whether each
// has something connected, and the power source option the port won while it
// powers the Mac.
func platformPortNodes() ([]portNode, error) {
	ioRegLibOnce.Do(loadIORegistryLibrary)
	if ioRegLibErr != nil {
		return nil, ioRegLibErr
	}
	lib := ioRegLib

	var nodes []portNode
	for _, base := range portNodeNames {
		var ports uint32
		if kr := lib.getMatchingServices(0, lib.serviceNameMatching(base), &ports); kr != 0 {
			continue
		}
		for entry := lib.iteratorNext(ports); entry != 0; entry = lib.iteratorNext(ports) {
			if number, ok := lib.numberProperty(entry, "PortNumber"); ok {
				if node, ok := newPortNode(fmt.Sprintf("%s@%d", base, number)); ok {
					node.connected = lib.boolProperty(entry, "ConnectionActive")
					nodes = append(nodes, node)
				}
			}
			lib.objectRelease(entry)
		}
		lib.objectRelease(ports)
	}
	if len(nodes) == 0 {
		return nil, errors.New("no USB-C or MagSafe port in the IORegistry")
	}
	// Registry order is not stable: USB-C ports by number, then MagSafe.
	slices.SortStableFunc(nodes, func(a, b portNode) int {
		if a.usbC != b.usbC {
			if a.usbC {
				return -1
			}
			return 1
		}
		return a.number - b.number
	})

	var iterator uint32
	if kr := lib.getMatchingServices(0, lib.serviceMatching("IOPortFeaturePowerSource"), &iterator); kr != 0 {
		// Connection state is still right; only the contracts are missing.
		return nodes, nil
	}
	defer lib.objectRelease(iterator)
	for entry := lib.iteratorNext(iterator); entry != 0; entry = lib.iteratorNext(iterator) {
		applyWinningOption(lib, entry, nodes)
		lib.objectRelease(entry)
	}
	return nodes, nil
}

// applyWinningOption copies a power source's WinningPowerSourceOption onto
// the port it belongs to. Sources that did not win carry no such property.
func applyWinningOption(lib *ioRegistryLibrary, source uint32, nodes []portNode) {
	option := lib.property(source, "WinningPowerSourceOption")
	if option == 0 {
		return
	}
	defer lib.cfRelease(option)
	if lib.cfGetTypeID(option) != lib.cfDictionaryTypeID() {
		return
	}
	portType, okType := lib.numberProperty(source, "ParentPortType")
	portNumber, okNumber := lib.numberProperty(source, "ParentPortNumber")
	if !okType || !okNumber {
		return
	}
	mv, okV := lib.dictNumber(option, "Voltage (mV)")
	mw, okW := lib.dictNumber(option, "Max Power (mW)")
	if !okV || !okW {
		return
	}
	for i := range nodes {
		node := &nodes[i]
		if int64(node.number) != portNumber {
			continue
		}
		if (portType == ioPortTypeUSBC && node.usbC) || (portType == ioPortTypeMagSafe && !node.usbC) {
			node.contractVoltage = float64(mv) / 1000
			node.contractPower = float64(mw) / 1000
		}
	}
}

// smcMaxPorts bounds the D<n>JV / D<n>JI probe. An M1 Pro MacBook Pro has
// D1-D4; a missing key is skipped.
const smcMaxPorts = 6

// platformLivePortPower reads the SMC power keys, which update every second:
// VD0R, ID0R and PDTR for the DC input, and D<n>JV / D<n>JI for the voltage
// and current USB-C port n supplies (D3 tracked USB-C 3 powering a display,
// 2026-11-09).
func platformLivePortPower() (livePortPower, error) {
	lib, conn, err := openSMC()
	if err != nil {
		return livePortPower{}, err
	}
	defer lib.serviceClose(conn)

	read := func(key string) (float64, bool) {
		typ, data, err := smcReadKey(lib, conn, key)
		if err != nil {
			return 0, false
		}
		return decodeSMCFloat(typ, data)
	}

	live := livePortPower{out: map[int]portOutput{}}
	var okV, okI, okP bool
	live.inVoltage, okV = read("VD0R")
	live.inCurrent, okI = read("ID0R")
	live.inPower, okP = read("PDTR")
	if !okV && !okI && !okP {
		return livePortPower{}, errors.New("SMC has no DC input keys")
	}
	for port := 1; port <= smcMaxPorts; port++ {
		v, okV := read(fmt.Sprintf("D%dJV", port))
		a, okA := read(fmt.Sprintf("D%dJI", port))
		if okV && okA {
			live.out[port] = portOutput{voltage: v, current: a}
		}
	}
	return live, nil
}

// decodeSMCFloat decodes a non-negative Apple Silicon SMC float ("flt ").
// Intel SMCs use other encodings for these keys and fall back to the
// AppleSmartBattery readings.
func decodeSMCFloat(dataType string, data []byte) (float64, bool) {
	if dataType != "flt " || len(data) != 4 {
		return 0, false
	}
	v := float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}
