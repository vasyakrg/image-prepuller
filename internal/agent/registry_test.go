package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRegistryAddress(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  string
	}{
		{image: "nginx:latest", want: "docker.io:443"},
		{image: "library/nginx", want: "docker.io:443"},
		{image: "library/nginx:1.27", want: "docker.io:443"},
		{image: "ghcr.io/acme/api-gateway:1.14.2", want: "ghcr.io:443"},
		{image: "ghcr.io/acme/api-gateway@sha256:" + strings.Repeat("a", 64), want: "ghcr.io:443"},
		{image: "registry.internal:5000/fin/payments:2.3.0", want: "registry.internal:5000"},
		{image: "localhost:5000/app:1", want: "localhost:5000"},
		{image: "localhost/app", want: "localhost:443"},
		{image: "[::1]:5000/app:1", want: "[::1]:5000"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			got, err := RegistryAddress(tc.image)
			if err != nil {
				t.Fatalf("RegistryAddress(%q) error: %v", tc.image, err)
			}
			if got != tc.want {
				t.Fatalf("RegistryAddress(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
	if _, err := RegistryAddress("   "); err == nil {
		t.Fatal("RegistryAddress(empty) = nil error")
	}
}

// stubRegistryDial replaces the probe dialer for one test and returns a restore
// function.
func stubRegistryDial(t *testing.T, fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	previous := registryDial
	registryDial = fn
	t.Cleanup(func() { registryDial = previous })
}

func TestProbeRegistryReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if err := ProbeRegistry(context.Background(), "127.0.0.1:"+portOf(t, ln)+"/app:1", time.Second); err != nil {
		t.Fatalf("ProbeRegistry(reachable) = %v, want nil", err)
	}
}

func TestProbeRegistryBlackHoleRespectsTimeout(t *testing.T) {
	stubRegistryDial(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	started := time.Now()
	err := ProbeRegistry(context.Background(), "192.0.2.1:5000/dead:1", 50*time.Millisecond)
	if !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("ProbeRegistry(black hole) = %v, want ErrRegistryUnreachable", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe took %s, want it bounded by the timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "192.0.2.1:5000") {
		t.Fatalf("error %q does not name the registry address", err)
	}
}

func TestProbeRegistryRefused(t *testing.T) {
	// Port 1 on loopback is closed, so the dial fails immediately.
	err := ProbeRegistry(context.Background(), "127.0.0.1:1/app:1", time.Second)
	if !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("ProbeRegistry(closed port) = %v, want ErrRegistryUnreachable", err)
	}
}

func TestConfigPullTimingDefaults(t *testing.T) {
	cfg := testConfig().withDefaults()
	if cfg.PullTimeout != DefaultPullTimeout {
		t.Fatalf("PullTimeout = %s, want %s", cfg.PullTimeout, DefaultPullTimeout)
	}
	if cfg.RegistryProbeTimeout != DefaultRegistryProbeTimeout {
		t.Fatalf("RegistryProbeTimeout = %s, want %s", cfg.RegistryProbeTimeout, DefaultRegistryProbeTimeout)
	}
	if cfg.PullStallTimeout != DefaultPullStallTimeout {
		t.Fatalf("PullStallTimeout = %s, want %s", cfg.PullStallTimeout, DefaultPullStallTimeout)
	}
	if DefaultPullStallTimeout > time.Minute {
		t.Fatalf("default stall window %s is longer than a minute", DefaultPullStallTimeout)
	}
}

func TestNewRejectsNegativePullTimings(t *testing.T) {
	cfg := testConfig()
	cfg.Mock = true
	cfg.PullStallTimeout = -time.Second
	if _, err := New(cfg); err == nil {
		t.Fatal("New(negative stall timeout) = nil error")
	}
}

func portOf(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	return port
}
