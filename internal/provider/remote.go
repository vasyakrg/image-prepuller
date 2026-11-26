package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// RemoteProvider connects a Hub controller to a headless Spoke controller
// over the internal API authenticated with a Bearer token. Wire contract v2:
// see docs/PLAN.md section 10 and internal/web/server_spoke.go.
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

// HTTPError is a non-2xx spoke response; callers map specific statuses onto
// provider sentinel errors (for example 404 -> ErrTaskNotFound).
type HTTPError struct {
	Cluster    string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("spoke %s returned HTTP %d", e.Cluster, e.StatusCode)
}

// doJSON performs one spoke request and decodes the JSON response.
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
	req.Header.Set("X-Prepuller-APIVersion", model.APIVersion)
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &HTTPError{Cluster: p.clusterName, StatusCode: resp.StatusCode}
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

// apiEnvelope adds the wire apiVersion to a request body.
func apiEnvelope(req any) any {
	return struct {
		APIVersion string `json:"apiVersion"`
		Request    any    `json:"request"`
	}{APIVersion: model.APIVersion, Request: req}
}

func (p *RemoteProvider) Snapshot(ctx context.Context) model.Snapshot {
	var snap model.Snapshot
	if err := p.doJSON(ctx, http.MethodGet, "/internal/v1/snapshot", nil, &snap); err != nil {
		return model.Snapshot{
			ClusterName: p.clusterName,
			APIVersion:  model.APIVersion,
			UpdatedAt:   time.Now(),
			SyncStatus:  model.SyncOffline,
			Partial:     true,
		}
	}
	if snap.ClusterName == "" {
		snap.ClusterName = p.clusterName
	}
	if snap.APIVersion == "" {
		snap.APIVersion = model.PreviousAPIVersion
	}
	if snap.SyncStatus == "" {
		snap.SyncStatus = model.SyncUnknown
	}
	return snap
}

func (p *RemoteProvider) TriggerPull(ctx context.Context, req model.PullRequest) (model.PullTask, error) {
	body := struct {
		APIVersion string            `json:"apiVersion"`
		Image      string            `json:"image"`
		Node       string            `json:"node"`
		Actor      string            `json:"actor,omitempty"`
		Trigger    model.PullTrigger `json:"trigger,omitempty"`
		Policy     model.PullPolicy  `json:"policy,omitempty"`
	}{APIVersion: model.APIVersion, Image: req.Image, Node: req.Node, Actor: req.Actor, Trigger: req.Trigger, Policy: req.Policy}

	var task model.PullTask
	if err := p.doJSON(ctx, http.MethodPost, "/internal/v1/pull", body, &task); err != nil {
		return model.PullTask{}, err
	}
	if task.ID == "" {
		// Spoke running the previous contract answered {"ok":true}.
		task = model.PullTask{
			Cluster:   p.clusterName,
			Image:     req.Image,
			Node:      req.Node,
			State:     model.TaskRunning,
			Attempt:   1,
			Trigger:   req.Trigger,
			Actor:     req.Actor,
			CreatedAt: time.Now(),
		}
	}
	if task.Cluster == "" {
		task.Cluster = p.clusterName
	}
	return task, nil
}

func (p *RemoteProvider) TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error) {
	var out struct {
		Triggered int `json:"triggered"`
	}
	body := struct {
		APIVersion string            `json:"apiVersion"`
		Namespace  string            `json:"namespace"`
		Kind       string            `json:"kind"`
		Name       string            `json:"name"`
		Actor      string            `json:"actor,omitempty"`
		Trigger    model.PullTrigger `json:"trigger,omitempty"`
	}{APIVersion: model.APIVersion, Namespace: req.Namespace, Kind: req.Kind, Name: req.Name, Actor: req.Actor, Trigger: req.Trigger}
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/workloads/pull", body, &out)
	return out.Triggered, err
}

func (p *RemoteProvider) CancelPull(ctx context.Context, taskID string) error {
	body := struct {
		APIVersion string `json:"apiVersion"`
	}{APIVersion: model.APIVersion}
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/pulls/"+taskID+"/cancel", body, nil)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	return err
}

func (p *RemoteProvider) SyncNow(ctx context.Context, actor string) error {
	body := struct {
		APIVersion string `json:"apiVersion"`
		Actor      string `json:"actor,omitempty"`
	}{APIVersion: model.APIVersion, Actor: actor}
	return p.doJSON(ctx, http.MethodPost, "/internal/v1/reconcile", body, nil)
}

func (p *RemoteProvider) Health(ctx context.Context) model.Health {
	started := time.Now()
	health := model.Health{CheckedAt: started}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/healthz", nil)
	if err != nil {
		health.Error = err.Error()
		return health
	}
	resp, err := p.client.Do(req)
	if err != nil {
		health.Error = err.Error()
		health.LatencyMs = time.Since(started).Milliseconds()
		return health
	}
	defer resp.Body.Close()
	health.LatencyMs = time.Since(started).Milliseconds()
	health.Online = resp.StatusCode == http.StatusOK
	health.Version = resp.Header.Get("X-Prepuller-Version")
	if !health.Online {
		health.Error = fmt.Sprintf("spoke health returned HTTP %d", resp.StatusCode)
	}
	return health
}

func (p *RemoteProvider) PullCounters() []model.PullMetricSample {
	var out []model.PullMetricSample
	if err := p.doJSON(context.Background(), http.MethodGet, "/internal/v1/counters", nil, &out); err != nil {
		return nil
	}
	return out
}
