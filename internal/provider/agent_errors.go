package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Closed reason set for node-agent inventory failures. The values are stable API
// (Prometheus label values): do not rename them without a migration, dashboards
// and alert rules key off them.
const (
	// ReasonUnauthorized means the agent rejected the hub Bearer token (401/403).
	ReasonUnauthorized = "unauthorized"
	// ReasonProtocol means the agent answered in an unexpected shape: an
	// unsupported apiVersion, a snapshot for another node, or a body that does
	// not decode.
	ReasonProtocol = "protocol"
	// ReasonTimeout means the inventory call ran out of time (or was cancelled)
	// before the agent answered.
	ReasonTimeout = "timeout"
	// ReasonUnavailable is the default: the agent is absent, unreachable, or
	// answered with an unexpected HTTP status.
	ReasonUnavailable = "unavailable"
)

// AgentErrorKey identifies one node-agent failure counter. Both fields take
// values from bounded sets (Kubernetes node names and the closed reason set), so
// a counter map keyed by this struct cannot grow with traffic.
type AgentErrorKey struct {
	Node   string
	Reason string
}

// AgentErrorCounter is an OPTIONAL provider capability: providers that talk to
// node agents expose how often each node failed and why. It is deliberately not
// part of the frozen Provider interface (AGENTS.md section 5.1), so MockProvider
// and RemoteProvider are unaffected; the metrics layer type-asserts on it and
// simply emits no series when a provider does not implement it.
type AgentErrorCounter interface {
	AgentErrors() map[AgentErrorKey]uint64
}

// ClassifyAgentError maps one node-agent inventory failure onto the closed reason
// set. The order matters: the inventory transport wraps every failure with
// ErrNodeInventoryUnavailable, so the specific causes are tested first. An
// unknown cause is classified as unavailable, the safe default (it never claims
// a problem the hub can name).
//
// A nil error has no reason and yields "".
func ClassifyAgentError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrAgentUnauthorized):
		return ReasonUnauthorized
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return ReasonTimeout
	case isAgentProtocolError(err):
		return ReasonProtocol
	default:
		return ReasonUnavailable
	}
}

// isAgentProtocolError reports a response the hub cannot trust as agent wire v1.
// The agent wire contract is frozen, so the transport exports no dedicated
// sentinel for these cases; the stable message fragments are matched instead. An
// unknown wording falls through to unavailable, which keeps a future transport
// rewrite from silently mislabelling failures as protocol errors.
func isAgentProtocolError(err error) bool {
	if !errors.Is(err, ErrNodeInventoryUnavailable) {
		return false
	}
	message := err.Error()
	for _, fragment := range []string{
		"apiVersion",        // agent speaks a different wire version
		"answered for node", // agent answered for a different node
		"decode ",           // response body is not valid JSON for the contract
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// agentErrorCounters is a mutex-guarded counter map keyed by (node, reason).
// It lives on the agent transport so the frozen Provider interface and the
// K8sProvider struct layout stay untouched (the metrics layer owns the rest of
// the visibility work).
type agentErrorCounters struct {
	mu     sync.Mutex
	counts map[AgentErrorKey]uint64
}

func (c *agentErrorCounters) add(key AgentErrorKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[AgentErrorKey]uint64)
	}
	c.counts[key]++
}

func (c *agentErrorCounters) snapshot() map[AgentErrorKey]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[AgentErrorKey]uint64, len(c.counts))
	for key, count := range c.counts {
		out[key] = count
	}
	return out
}

// recordAgentError counts one inventory failure for a node. It is a no-op when
// the provider has no agent transport (offline/test providers).
func (p *K8sProvider) recordAgentError(node string, err error) {
	if p == nil || p.agent == nil {
		return
	}
	reason := ClassifyAgentError(err)
	if reason == "" {
		return
	}
	p.agent.agentErrors.add(AgentErrorKey{Node: node, Reason: reason})
}

// AgentErrors returns a copy of the per-(node, reason) inventory failure
// counters. It implements the optional AgentErrorCounter capability; the
// Provider interface itself is unchanged.
func (p *K8sProvider) AgentErrors() map[AgentErrorKey]uint64 {
	if p == nil || p.agent == nil {
		return map[AgentErrorKey]uint64{}
	}
	return p.agent.agentErrors.snapshot()
}
