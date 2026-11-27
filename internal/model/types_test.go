package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// A provider with nothing to list (e.g. no workload matches the label selector)
// must still serialise collections as arrays: the UI calls .map on them.
func TestSnapshotNormalizeJSON(t *testing.T) {
	var zero Snapshot
	raw, err := json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"workloads":null`, `"nodes":null`, `"activity":null`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("un-normalized snapshot = %s, want %s", raw, want)
		}
	}

	zero.Normalize()
	raw, err = json.Marshal(&zero)
	if err != nil {
		t.Fatalf("marshal normalized: %v", err)
	}
	for _, want := range []string{`"workloads":[]`, `"nodes":[]`, `"activity":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("normalized snapshot = %s, want %s", raw, want)
		}
	}
	if strings.Contains(string(raw), ":null") {
		t.Fatalf("normalized snapshot still has null fields: %s", raw)
	}
	// Tasks keep omitempty: an empty list is simply absent, never null.
	if strings.Contains(string(raw), `"tasks":null`) {
		t.Fatalf("tasks serialised as null: %s", raw)
	}
}
