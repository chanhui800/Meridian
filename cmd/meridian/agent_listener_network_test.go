package main

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgentListenerNetworkAddressFollowsPublishFamily(t *testing.T) {
	tests := []struct {
		family string
		port   int
		want   []agentListenerSpec
	}{
		{family: "auto", port: 80, want: []agentListenerSpec{{network: "tcp4", address: "0.0.0.0:80"}, {network: "tcp6", address: "[::]:80"}}},
		{family: "auto", port: 443, want: []agentListenerSpec{{network: "tcp4", address: "0.0.0.0:443"}, {network: "tcp6", address: "[::]:443"}}},
		{family: "v4", port: 80, want: []agentListenerSpec{{network: "tcp4", address: "0.0.0.0:80"}}},
		{family: "v4", port: 443, want: []agentListenerSpec{{network: "tcp4", address: "0.0.0.0:443"}}},
		{family: "v6", port: 80, want: []agentListenerSpec{{network: "tcp6", address: "[::]:80"}}},
		{family: "v6", port: 443, want: []agentListenerSpec{{network: "tcp6", address: "[::]:443"}}},
	}
	for _, test := range tests {
		t.Run(test.family+"/"+strconv.Itoa(test.port), func(t *testing.T) {
			got, err := agentListenerSpecs(test.family, test.port)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("listeners = %#v, want %#v", got, test.want)
			}
			for index := range test.want {
				if got[index] != test.want[index] {
					t.Fatalf("listener[%d] = %#v, want %#v", index, got[index], test.want[index])
				}
			}
		})
	}
	if _, err := agentListenerSpecs("invalid", 443); err == nil {
		t.Fatal("invalid listener family was accepted")
	}
}

func TestAgentStartServerPassesSelectedFamilyAndInterface(t *testing.T) {
	port := freePort(t)
	releasePort(port)
	var gotNetwork, gotAddress, gotInterface string
	runtime := &edgeAgentRuntime{listen: func(network, address, interfaceName string) (net.Listener, error) {
		gotNetwork, gotAddress, gotInterface = network, address, interfaceName
		return net.Listen(network, address)
	}}
	if err := runtime.startServer(port, "v6", "eth6"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.stopServer() })
	if gotNetwork != "tcp6" || gotAddress != net.JoinHostPort("::", strconv.Itoa(port)) || gotInterface != "eth6" {
		t.Fatalf("listener call = %q %q interface=%q", gotNetwork, gotAddress, gotInterface)
	}
	if runtime.listenFamily != "v6" || runtime.listenInterface != "eth6" {
		t.Fatalf("runtime listener state = family %q interface %q", runtime.listenFamily, runtime.listenInterface)
	}
}

func TestAgentStartServerAutoBindsBothListeners(t *testing.T) {
	port := freePort(t)
	releasePort(port)
	var got []agentListenerSpec
	runtime := &edgeAgentRuntime{listen: func(network, address, _ string) (net.Listener, error) {
		got = append(got, agentListenerSpec{network: network, address: address})
		return net.Listen(network, address)
	}}
	if err := runtime.startServer(port, "auto", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.stopServer() })
	want, err := agentListenerSpecs("auto", port)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("auto listener calls = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("auto listener[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestAgentIPv6OnlyListenerLeavesIPv4PortAvailableOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the reported wildcard port collision is specific to Linux dual-stack sockets")
	}
	port := freePort(t)
	releasePort(port)
	specs, err := agentListenerSpecs("v6", port)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenAgent(specs[0].network, specs[0].address, "")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ipv4, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("IPv6-only listener still occupies IPv4 port %d: %v", port, err)
	}
	_ = ipv4.Close()
}

func TestAgentIPv4OnlyListenerLeavesIPv6PortAvailableOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this verifies the Linux Agent single-family socket behavior")
	}
	port := freePort(t)
	releasePort(port)
	v4Specs, err := agentListenerSpecs("v4", port)
	if err != nil {
		t.Fatal(err)
	}
	ipv4, err := listenAgent(v4Specs[0].network, v4Specs[0].address, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ipv4.Close()

	v6Specs, err := agentListenerSpecs("v6", port)
	if err != nil {
		t.Fatal(err)
	}
	ipv6, err := listenAgent(v6Specs[0].network, v6Specs[0].address, "")
	if err != nil {
		t.Fatalf("IPv4-only listener still occupies IPv6 port %d: %v", port, err)
	}
	defer ipv6.Close()

	for _, target := range []struct{ network, address string }{
		{network: "tcp4", address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		{network: "tcp6", address: net.JoinHostPort("::1", strconv.Itoa(port))},
	} {
		connection, dialErr := net.DialTimeout(target.network, target.address, time.Second)
		if dialErr != nil {
			t.Fatalf("dual-family listeners did not accept %s at %s: %v", target.network, target.address, dialErr)
		}
		_ = connection.Close()
	}
}

func TestAgentAutoListenerBindsBothAddressFamiliesOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this verifies the Linux Agent dual-listener socket behavior")
	}
	port := freePort(t)
	releasePort(port)
	specs, err := agentListenerSpecs("auto", port)
	if err != nil {
		t.Fatal(err)
	}
	listeners := make([]net.Listener, 0, len(specs))
	for _, spec := range specs {
		listener, listenErr := listenAgent(spec.network, spec.address, "")
		if listenErr != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			t.Fatal(listenErr)
		}
		listeners = append(listeners, listener)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, target := range []struct{ network, address string }{
		{network: "tcp4", address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		{network: "tcp6", address: net.JoinHostPort("::1", strconv.Itoa(port))},
	} {
		connection, dialErr := net.DialTimeout(target.network, target.address, time.Second)
		if dialErr != nil {
			t.Fatalf("dual-stack listener did not accept %s at %s: %v", target.network, target.address, dialErr)
		}
		_ = connection.Close()
	}
}

func TestAgentSelectedIPv6InterfaceDoesNotOccupyIPv4PortOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_BINDTODEVICE runtime behavior requires Linux")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	selected := ""
	for _, iface := range interfaces {
		if !edgeNetworkInterfaceEligible(iface) || strings.EqualFold(iface.Name, "lo") {
			continue
		}
		addresses, addressErr := iface.Addrs()
		if addressErr != nil {
			continue
		}
		hasIPv4, hasIPv6 := false, false
		for _, raw := range addresses {
			text, _, _ := strings.Cut(raw.String(), "/")
			ip := net.ParseIP(text)
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			if ip.To4() != nil {
				hasIPv4 = true
			} else if ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				hasIPv6 = true
			}
		}
		if hasIPv4 && hasIPv6 {
			selected = iface.Name
			break
		}
	}
	if selected == "" {
		t.Skip("no up dual-stack interface with IPv4 and IPv6 addresses")
	}
	port := freePort(t)
	releasePort(port)
	specs, err := agentListenerSpecs("v6", port)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenAgent(specs[0].network, specs[0].address, selected)
	if err != nil {
		t.Fatalf("bind IPv6-only listener to selected interface %s: %v", selected, err)
	}
	defer listener.Close()
	ipv4, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("selected IPv6 interface listener occupies IPv4 port %d: %v", port, err)
	}
	_ = ipv4.Close()
}
