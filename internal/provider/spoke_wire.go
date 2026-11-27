package provider

import (
	"encoding/json"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Wire contract of the outbound ("pull") spoke channel.
//
// In the direct topology the hub calls the spoke over HTTP (RemoteProvider, see
// remote.go). When the managed cluster cannot be reached from the hub (NAT, no
// ingress there), the direction is reversed: the spoke dials the hub ingress and
// keeps that connection, pushes cluster state periodically and long-polls for
// commands. Each command is one HTTP call the hub would otherwise have made
// directly, so the hub reuses RemoteProvider (and therefore every status/retry
// rule) on top of a mailbox transport (see spoke_hub.go).

// SpokeState is the periodic state push
// (POST /internal/v1/spokes/{cluster}/state). The snapshot and the counters are
// exactly what GET /internal/v1/snapshot and GET /internal/v1/counters answer on
// the direct contract, so the hub can serve reads without a round trip.
type SpokeState struct {
	APIVersion    string         `json:"apiVersion"`
	ClusterName   string         `json:"clusterName"`
	SentAt        time.Time      `json:"sentAt"`
	Version       string         `json:"version,omitempty"`
	UptimeSeconds int64          `json:"uptimeSeconds,omitempty"`
	Snapshot      model.Snapshot `json:"snapshot"`
	Counters      SpokeCounters  `json:"counters"`
}

// SpokeCommand is one hub->spoke call carried over the outbound channel. Path is
// always one of the spoke's /internal/v1/* endpoints, so the spoke executes the
// command through its own API handler and the wire contract stays identical to
// the direct topology.
type SpokeCommand struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Query  string          `json:"query,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// SpokeCommands is the long-poll answer of
// GET /internal/v1/spokes/{cluster}/commands. An empty slice is answered with
// 204 No Content.
type SpokeCommands struct {
	Commands []SpokeCommand `json:"commands"`
}

// SpokeCommandResult is the recorded HTTP answer of the spoke handler for one
// command (POST /internal/v1/spokes/{cluster}/commands/{id}/result). Status and
// Body are the original status code and JSON body, so the hub maps them onto the
// same sentinel errors as the direct transport. Error reports a failure to
// execute the command at all (never a spoke decision).
type SpokeCommandResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// SpokeStateAck is the answer of the state push; Commands is empty and reserved
// for future optimistic delivery (today commands travel over the long poll).
type SpokeStateAck struct {
	APIVersion string `json:"apiVersion"`
	OK         bool   `json:"ok"`
}
