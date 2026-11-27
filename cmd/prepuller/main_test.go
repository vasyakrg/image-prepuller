package main

import (
	"testing"
	"time"
)

func TestParseManagedClusters(t *testing.T) {
	clusters, err := parseManagedClusters(" talos-kz=tok-1 , talos-eu=tok=2 ")
	if err != nil {
		t.Fatalf("parseManagedClusters: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("got %d clusters, want 2: %+v", len(clusters), clusters)
	}
	if clusters[0].name != "talos-kz" || clusters[0].token != "tok-1" {
		t.Fatalf("first cluster = %+v", clusters[0])
	}
	// A token may legitimately contain '=': only the first one separates.
	if clusters[1].name != "talos-eu" || clusters[1].token != "tok=2" {
		t.Fatalf("second cluster = %+v", clusters[1])
	}

	for _, raw := range []string{"", " , ", "talos-kz", "talos-kz=", "=tok", "talos-kz=tok,talos-kz=other"} {
		if _, err := parseManagedClusters(raw); err == nil {
			t.Errorf("parseManagedClusters(%q) = nil error, want an error", raw)
		}
	}
}

func TestHubSyncIntervalFromEnv(t *testing.T) {
	t.Setenv("PREPULLER_HUB_SYNC_INTERVAL", "")
	if got := hubSyncIntervalFromEnv(); got != 3*time.Second {
		t.Fatalf("default sync interval = %s, want 3s", got)
	}
	t.Setenv("PREPULLER_HUB_SYNC_INTERVAL", "10s")
	if got := hubSyncIntervalFromEnv(); got != 10*time.Second {
		t.Fatalf("configured sync interval = %s, want 10s", got)
	}
	for _, bad := range []string{"soon", "0s", "-5s"} {
		t.Setenv("PREPULLER_HUB_SYNC_INTERVAL", bad)
		if got := hubSyncIntervalFromEnv(); got != 3*time.Second {
			t.Fatalf("invalid value %q resolved to %s, want the 3s fallback", bad, got)
		}
	}
}

func TestPullHistoryLimitFromEnv(t *testing.T) {
	t.Setenv("PREPULLER_PULL_HISTORY_LIMIT", "")
	if got := pullHistoryLimitFromEnv(); got != 2000 {
		t.Fatalf("default history limit = %d, want 2000", got)
	}
	t.Setenv("PREPULLER_PULL_HISTORY_LIMIT", "500")
	if got := pullHistoryLimitFromEnv(); got != 500 {
		t.Fatalf("configured history limit = %d, want 500", got)
	}
	for _, bad := range []string{"many", "0", "-10"} {
		t.Setenv("PREPULLER_PULL_HISTORY_LIMIT", bad)
		if got := pullHistoryLimitFromEnv(); got != 2000 {
			t.Fatalf("invalid value %q resolved to %d, want the 2000 fallback", bad, got)
		}
	}
}
