package main

import (
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port power comes from three sources that refresh at different rates:
//
//   - The IORegistry port nodes (platformPortNodes): connection state and the
//     power contract a charger won, updated the moment a cable goes in or out.
//   - The SMC power keys (platformLivePortPower): DC input and per-port output
//     volts and amps, updated every second.
//   - AppleSmartBattery: the same readings plus each output port's offered
//     maximum, refreshed only about once a minute (UpdateTime advances in 60 s
//     steps), so it cannot follow a plug or unplug.
//
// The full collection keeps AppleSmartBattery's latest parse here for what
// does not change while a device stays connected; the fast path's port read
// (livePortsForTick) takes everything else live.
var (
	batteryPortPowerMu   sync.Mutex
	lastBatteryPortPower batteryPortPower
)

// Hardware readers, swapped in tests. The platform implementations live in
// metrics_ports_darwin.go and metrics_ports_other.go.
var (
	readPortNodes     = platformPortNodes
	readLivePortPower = platformLivePortPower
)

// powerRefreshInterval spaces the fast path's power reads (ports, battery and
// charger). The sources update every second, but a once-a-second read was
// more churn than the cards are worth; five seconds still follows a plug or
// unplug.
const powerRefreshInterval = 5 * time.Second

// lastPowerRefresh is when the fast path last read power, or zero before the
// first read.
func (c *Collector) lastPowerRefresh() time.Time {
	c.livePowerMu.Lock()
	defer c.livePowerMu.Unlock()
	return c.lastPowerAt
}

// livePower is what the fast path shows on the Power and Ports cards.
type livePower struct {
	ports      []PortPower
	battery    liveBattery
	hasBattery bool
}

// livePowerForTick returns the power readings for a fast tick, reading them
// again only when powerRefreshInterval has passed since the last attempt (or
// the clock went back). A failed read keeps the previous reading; until one
// has succeeded the fields stay empty, so the snapshot keeps the full
// collection's values.
func (c *Collector) livePowerForTick(now time.Time) livePower {
	c.livePowerMu.Lock()
	defer c.livePowerMu.Unlock()
	if c.lastPowerAt.IsZero() || now.Sub(c.lastPowerAt) >= powerRefreshInterval || now.Before(c.lastPowerAt) {
		c.lastPowerAt = now
		if ports, ok := collectLivePorts(); ok {
			c.cachedPower.ports = ports
		}
		if battery, err := readLiveBattery(); err == nil {
			c.cachedPower.battery, c.cachedPower.hasBattery = battery, true
		}
	}
	return c.cachedPower
}

// livePortPower is the SMC's per-second power data: the DC input (VD0R,
// ID0R, PDTR) and each USB-C port's output (D<n>JV, D<n>JI).
type livePortPower struct {
	inVoltage, inCurrent, inPower float64
	out                           map[int]portOutput
}

// collectPorts is the full collection's port read. batteryOut is the
// AppleSmartBattery ioreg output collectThermal already read.
func collectPorts(batteryOut string) []PortPower {
	power := parseBatteryPortPower(batteryOut)
	batteryPortPowerMu.Lock()
	lastBatteryPortPower = power
	batteryPortPowerMu.Unlock()
	ports, _ := collectLivePorts()
	return ports
}

// collectLivePorts lists the ports and the power through each as of now. It
// starts no external command, so the fast collection may call it.
// It reports false when the ports could not be read, so the caller keeps the
// last good list instead of showing every port as gone.
func collectLivePorts() ([]PortPower, bool) {
	nodes, err := readPortNodes()
	if err != nil || len(nodes) == 0 {
		return nil, false
	}
	batteryPortPowerMu.Lock()
	power := lastBatteryPortPower
	batteryPortPowerMu.Unlock()
	if live, err := readLivePortPower(); err == nil {
		power = power.withLive(live)
	}
	return mergePortPower(nodes, power), true
}

// withLive returns p with the SMC's live readings in place of the minute-old
// ones. It copies the output map, which the cached value shares.
func (p batteryPortPower) withLive(live livePortPower) batteryPortPower {
	merged := batteryPortPower{
		inVoltage: live.inVoltage,
		inCurrent: live.inCurrent,
		inPower:   live.inPower,
		out:       make(map[int]portOutput, len(p.out)+len(live.out)),
	}
	maps.Copy(merged.out, p.out)
	for port, reading := range live.out {
		entry := merged.out[port]
		entry.voltage, entry.current = reading.voltage, reading.current
		merged.out[port] = entry
	}
	return merged
}

// portNode is one port as the IORegistry describes it.
type portNode struct {
	name      string
	usbC      bool
	number    int
	connected bool
	// The power source option the port won, set only while something powers
	// the Mac through it.
	contractVoltage float64
	contractPower   float64
}

// newPortNode recognises a port node name such as "Port-USB-C@3" or
// "Port-MagSafe 3@1". HDMI, SD and other ports cannot carry power.
func newPortNode(nodeName string) (portNode, bool) {
	kind, numText, found := strings.Cut(nodeName, "@")
	if !found {
		return portNode{}, false
	}
	number, err := strconv.Atoi(strings.TrimSpace(numText))
	if err != nil {
		return portNode{}, false
	}
	switch {
	case kind == "Port-USB-C":
		return portNode{name: "USB-C " + strconv.Itoa(number), usbC: true, number: number}, true
	case strings.HasPrefix(kind, "Port-MagSafe"):
		name := "MagSafe"
		if number > 1 {
			name += " " + strconv.Itoa(number)
		}
		return portNode{name: name, number: number}, true
	default:
		return portNode{}, false
	}
}

// batteryPortPower is the power data AppleSmartBattery reports.
type batteryPortPower struct {
	// Input from PowerTelemetryData: the source that powers the Mac.
	inVoltage, inCurrent, inPower float64
	// PowerOutDetails by PortIndex, which numbers USB-C ports like their
	// "Port-USB-C@N" node.
	out map[int]portOutput
}

type portOutput struct {
	voltage, current, configuredVoltage, maxPower float64
}

func parseBatteryPortPower(out string) batteryPortPower {
	result := batteryPortPower{out: map[int]portOutput{}}
	for line := range strings.Lines(out) {
		switch {
		case strings.Contains(line, `"PowerTelemetryData"`):
			if mv, ok := parseIORegFloatValue(line, "SystemVoltageIn"); ok {
				result.inVoltage = mv / 1000
			}
			if ma, ok := parseIORegFloatValue(line, "SystemCurrentIn"); ok {
				result.inCurrent = ma / 1000
			}
			if mw, ok := parseIORegFloatValue(line, "SystemPowerIn"); ok {
				result.inPower = mw / 1000
			}
		case strings.Contains(line, `"PowerOutDetails"`):
			_, body, _ := strings.Cut(line, "=")
			for entry := range strings.SplitSeq(body, "},{") {
				index, ok := parseIORegFloatValue(entry, "PortIndex")
				if !ok {
					continue
				}
				var reading portOutput
				if mv, ok := parseIORegFloatValue(entry, "AdapterVoltage"); ok {
					reading.voltage = mv / 1000
				}
				if ma, ok := parseIORegFloatValue(entry, "Current"); ok {
					reading.current = ma / 1000
				}
				if mv, ok := parseIORegFloatValue(entry, "ConfiguredVoltage"); ok {
					reading.configuredVoltage = mv / 1000
				}
				if mw, ok := parseIORegFloatValue(entry, "PDPowermW"); ok {
					reading.maxPower = mw / 1000
				}
				result.out[int(index)] = reading
			}
		}
	}
	return result
}

// mergePortPower joins the port list with the power data. The input readings
// are system-wide, so they are attributed to an input port only when exactly
// one port is powering the Mac.
func mergePortPower(nodes []portNode, power batteryPortPower) []PortPower {
	inputs := 0
	for _, node := range nodes {
		if node.connected && node.contractPower > 0 {
			inputs++
		}
	}

	ports := make([]PortPower, 0, len(nodes))
	for _, node := range nodes {
		port := PortPower{Name: node.name, Connected: node.connected}
		output, hasOutput := power.out[node.number]
		switch {
		case node.connected && node.contractPower > 0:
			port.Direction = "in"
			port.ContractVoltage = node.contractVoltage
			port.MaxPower = node.contractPower
			if inputs == 1 && power.inPower > 0 {
				port.Voltage, port.Current, port.Power = power.inVoltage, power.inCurrent, power.inPower
			}
		// A port at 0 V is connected but not powering anything.
		case node.connected && node.usbC && hasOutput && output.voltage > 0:
			port.Direction = "out"
			port.Voltage, port.Current = output.voltage, output.current
			port.Power = output.voltage * output.current
			port.ContractVoltage = output.configuredVoltage
			port.MaxPower = output.maxPower
		}
		ports = append(ports, port)
	}
	return ports
}
