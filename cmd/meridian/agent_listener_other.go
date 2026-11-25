//go:build !linux

package main

import (
	"context"
	"errors"
	"net"
)

func listenAgent(network, address, interfaceName string) (net.Listener, error) {
	if interfaceName != "" {
		return nil, errors.New("network interface binding is supported on Linux Agents only")
	}
	var config net.ListenConfig
	return config.Listen(context.Background(), network, address)
}
