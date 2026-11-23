package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestDynamicPinnedDialReservesTimeForHealthyAlternate(t *testing.T) {
	target := mustStructuredURL(t, "https://cdn.example.com/video.mp4")
	pins := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}
	calls := 0
	transport, err := newDynamicTransportWithDialerTimeout(target, pins, func(ctx context.Context, _, address string) (net.Conn, error) {
		calls++
		host, _, _ := net.SplitHostPort(address)
		if host == "1.1.1.1" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return client, nil
	}, newTestDynamicSelfTargetPolicy(t), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	started := time.Now()
	conn, err := transport.DialContext(context.Background(), "tcp", "cdn.example.com:443")
	if err != nil {
		t.Fatalf("reachable alternate pin was not tried inside total budget: calls=%d elapsed=%s err=%v", calls, time.Since(started), err)
	}
	_ = conn.Close()
	if calls != 2 || time.Since(started) > time.Second {
		t.Fatalf("unexpected bounded fallback: calls=%d elapsed=%s", calls, time.Since(started))
	}
	t.Logf("healthy alternate connected: pins=%d calls=%d elapsed=%s total_budget=100ms", len(pins), calls, time.Since(started))
}

func TestDynamicPinnedDialCancellationStopsAlternateAttempts(t *testing.T) {
	target := mustStructuredURL(t, "https://cdn.example.com/video.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	transport, err := newDynamicTransportWithDialerTimeout(target, []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}, func(attempt context.Context, _, _ string) (net.Conn, error) {
		calls++
		cancel()
		<-attempt.Done()
		return nil, attempt.Err()
	}, newTestDynamicSelfTargetPolicy(t), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if conn, err := transport.DialContext(ctx, "tcp", "cdn.example.com:443"); conn != nil || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation continued pin attempts: conn=%v err=%v calls=%d", conn, err, calls)
	}
}

func TestDynamicPinnedDialCapsFirstAttemptWithLongTotalBudget(t *testing.T) {
	target := mustStructuredURL(t, "https://cdn.example.com/video.mp4")
	var firstWindow time.Duration
	calls := 0
	transport, err := newDynamicTransportWithDialerTimeout(target, []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}, func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls++
		if calls == 1 {
			deadline, _ := ctx.Deadline()
			firstWindow = time.Until(deadline)
		}
		return nil, errors.New("fixture connection refusal")
	}, newTestDynamicSelfTargetPolicy(t), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	_, _ = transport.DialContext(context.Background(), "tcp", "cdn.example.com:443")
	if firstWindow <= 0 || firstWindow > 2*time.Second || calls != 2 {
		t.Fatalf("first failed pin retained a long startup stall: window=%s calls=%d", firstWindow, calls)
	}
}

func TestDynamicPinnedDialClosesConnectionReturnedAfterAttemptDeadline(t *testing.T) {
	target := mustStructuredURL(t, "https://cdn.example.com/video.mp4")
	lateClient, latePeer := net.Pipe()
	defer latePeer.Close()
	transport, err := newDynamicTransportWithDialerTimeout(target, []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}, func(ctx context.Context, _, address string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(address)
		if host == "1.1.1.1" {
			<-ctx.Done()
			return lateClient, nil
		}
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return client, nil
	}, newTestDynamicSelfTargetPolicy(t), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	conn, err := transport.DialContext(context.Background(), "tcp", "cdn.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = latePeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := latePeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("late failed-attempt connection was leaked: %v", err)
	}
}
