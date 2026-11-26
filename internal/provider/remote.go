package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// RemoteProvider connects a Hub controller to a headless Spoke controller
// over internal HTTPS/HTTP API authenticated with a Bearer token.
type RemoteProvider struct {
	clusterName string
	baseURL     string
	token       string
	client      *http.Client
}

func NewRemoteProvider(clusterName, baseURL, token string) *RemoteProvider {
	return &RemoteProvider{
		clusterName: clusterName,
		baseURL:     strings.TrimRight(baseURL, "/"),
		token:       token,
		client:      &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *RemoteProvider) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader *bytes.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("spoke %s returned HTTP %d", p.clusterName, resp.StatusCode)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

func (p *RemoteProvider) Snapshot(ctx context.Context) model.Snapshot {
	var snap model.Snapshot
	if err := p.doJSON(ctx, http.MethodGet, "/internal/v1/snapshot", nil, &snap); err != nil {
		return model.Snapshot{
			ClusterName: p.clusterName,
			UpdatedAt:   time.Now(),
		}
	}
	if snap.ClusterName == "" {
		snap.ClusterName = p.clusterName
	}
	return snap
}

func (p *RemoteProvider) TriggerPull(ctx context.Context, image, node, actor string) error {
	return p.doJSON(ctx, http.MethodPost, "/internal/v1/pull", map[string]string{
		"image": image,
		"node":  node,
		"actor": actor,
	}, nil)
}

func (p *RemoteProvider) TriggerWorkloadPull(ctx context.Context, namespace, kind, name, actor string) (int, error) {
	var out struct {
		Triggered int `json:"triggered"`
	}
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/workloads/pull", map[string]string{
		"namespace": namespace,
		"kind":      kind,
		"name":      name,
		"actor":     actor,
	}, &out)
	return out.Triggered, err
}

func (p *RemoteProvider) SyncNow(ctx context.Context, actor string) error {
	return p.doJSON(ctx, http.MethodPost, "/internal/v1/reconcile", map[string]string{
		"actor": actor,
	}, nil)
}

func (p *RemoteProvider) PullCounters() []model.PullMetricSample {
	return nil
}
