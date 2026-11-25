//go:build !linux

package main

import (
	"context"
	"net"
)

func listenAgent(network, address string) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(context.Background(), network, address)
}
