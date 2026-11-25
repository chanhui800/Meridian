//go:build linux

package main

import (
	"context"
	"net"
	"syscall"
)

func listenAgent(network, address, interfaceName string) (net.Listener, error) {
	config := net.ListenConfig{}
	if interfaceName == "" && network != "tcp6" {
		return config.Listen(context.Background(), network, address)
	}
	config.Control = func(network, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) {
			if interfaceName != "" {
				optionErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, interfaceName)
			}
			if optionErr == nil && network == "tcp6" {
				optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
			}
		}); err != nil {
			return err
		}
		return optionErr
	}
	return config.Listen(context.Background(), network, address)
}
