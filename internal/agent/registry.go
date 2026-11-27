package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Registry reachability probe.
//
// A CRI pull into a black-holed registry (connection accepted but never
// answered, or silently dropped packets) holds the pull for the whole
// Config.PullTimeout and keeps the image x node slot busy, which is
// indistinguishable from a slow download in the UI. The agent therefore dials
// the registry endpoint before it asks the runtime to pull: a dead registry
// fails the pull in seconds instead of minutes.
const (
	// DefaultRegistryProbeTimeout bounds the preflight dial to the registry.
	DefaultRegistryProbeTimeout = 5 * time.Second
	// defaultRegistryPort is the TLS port of a registry whose reference carries
	// no explicit port. Plain-HTTP registries are always written host:port.
	defaultRegistryPort = "443"
	// dockerHubRegistry is the registry a reference without a host resolves to.
	dockerHubRegistry = "docker.io"
)

// ErrRegistryUnreachable reports that the image registry could not be dialed
// within the probe timeout. The image is not touched and the pull task fails
// fast instead of occupying a slot.
var ErrRegistryUnreachable = errors.New("registry unreachable")

// registryDial is the dialer used by ProbeRegistry; swappable in tests so a
// black hole can be simulated without real network timeouts.
var registryDial = (&net.Dialer{Timeout: DefaultRegistryProbeTimeout}).DialContext

// RegistryAddress resolves the host:port that serves the registry of an image
// reference. It follows the distribution reference rules: the first path
// element is a registry host only when it contains a dot or a colon or is
// "localhost"; otherwise the reference lives on Docker Hub ("nginx" and
// "library/nginx" both resolve to docker.io).
func RegistryAddress(image string) (string, error) {
	ref := strings.TrimSpace(image)
	if ref == "" {
		return "", errors.New("agent: empty image reference")
	}
	// Drop the digest, then the tag of the last path element ("repo/app:1.2").
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		if j := strings.IndexByte(ref[i+1:], ':'); j >= 0 {
			ref = ref[:i+1+j]
		}
	}
	host := dockerHubRegistry
	if i := strings.Index(ref, "/"); i >= 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host = first
		}
	}
	if host == "" {
		return "", fmt.Errorf("agent: image reference %q has no registry host", image)
	}
	if strings.Contains(host, ":") {
		return host, nil
	}
	return net.JoinHostPort(host, defaultRegistryPort), nil
}

// ProbeRegistry dials the registry endpoint of image and fails when it is not
// reachable within timeout. It answers reachability only: an authenticated or
// empty registry still accepts the TCP connection, so a passing probe never
// means the pull itself will succeed.
func ProbeRegistry(ctx context.Context, image string, timeout time.Duration) error {
	addr, err := RegistryAddress(image)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = DefaultRegistryProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := registryDial(probeCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrRegistryUnreachable, addr, err)
	}
	_ = conn.Close()
	return nil
}
