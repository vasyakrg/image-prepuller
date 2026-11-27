package agent

import (
	"errors"
	"strings"
)

// Registry credential handling on the pull path.
//
// The agent pulls through CRI, so the kubelet's imagePullSecrets never apply to
// it: CRI PullImage is anonymous unless the caller passes an AuthConfig. The hub
// resolves the workload's pull secret (internal/provider/k8s_registry.go) and
// sends it in the POST /agent/v1/pulls body; the agent only forwards it to the
// runtime and classifies the failure.

// reasonRegistryAuth is the machine-readable reason the agent returns when the
// registry rejected a pull for lack of (or with wrong) credentials. The hub maps
// it onto provider.ErrAgentRegistryAuth.
const reasonRegistryAuth = "registry_auth"

// ErrRegistryAuth reports a CRI pull the registry rejected as unauthenticated:
// no credential was sent, or the one sent is wrong/expired. It is a registry
// answer, not an agent or node failure, so the hub must not blame the agent.
var ErrRegistryAuth = errors.New("registry authentication failed")

// registryAuthMarkers are the wordings containerd, CRI-O and the distribution
// client use when auth is missing or rejected. Matching words instead of status
// codes is deliberate: an image digest can contain "401", and a bare number in
// an error string says nothing about the cause.
var registryAuthMarkers = []string{
	"unauthorized",
	"unauthenticated",
	"authentication required",
	"no basic auth credentials",
	"failed to authorize",
	"insufficient_scope",
	"denied",
	"forbidden",
}

// IsRegistryAuthError reports whether err looks like a registry credential
// rejection rather than a transport, disk or runtime failure.
func IsRegistryAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range registryAuthMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
