//go:build !darwin

package main

import "errors"

var errPortsUnsupported = errors.New("port power is only read on macOS")

func platformPortNodes() ([]portNode, error) {
	return nil, errPortsUnsupported
}

func platformLivePortPower() (livePortPower, error) {
	return livePortPower{}, errPortsUnsupported
}

func platformLiveBattery() (liveBattery, error) {
	return liveBattery{}, errPortsUnsupported
}
