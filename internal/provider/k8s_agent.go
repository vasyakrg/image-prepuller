package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/local/image-prepuller/internal/model"
)

// Agent wire contract v1 (frozen together with WS-A, see internal/agent/server.go).
// The hub must not import internal/agent (ADR-001 dependency direction), so the
// tiny response structs are mirrored here; bump agentAPIVersion only with a
// contract PR on both sides.
const (
	agentAPIVersion         = "v1"
	agentSnapshotPath       = "/agent/v1/snapshot"
	agentPullsPath          = "/agent/v1/pulls"
	agentReasonRegistryAuth = "registry_auth"
	maxAgentResponseBytes   = 8 << 20
	agentTaskHistoryLimit   = 200
	nodeAddressCacheTTL     = time.Minute
)

const (
	// DefaultAgentPort is the node-agent HTTP port (agents listen on :9443).
	DefaultAgentPort = 9443
	// DefaultAgentTimeout bounds one request to the agent. The agent pull call
	// itself may take minutes (CRI PullImage), so TriggerPull derives its own,
	// longer context deadline from the caller instead of this value.
	DefaultAgentTimeout = 5 * time.Second
	// DefaultAgentScheme is the agent transport scheme. WS-A serves plain HTTP
	// on :9443; TLS/mTLS terminates in front of the agent (WS-F).
	DefaultAgentScheme = "http"
	// DefaultAgentPullTimeout bounds one synchronous CRI pull through the agent.
	// It is a backstop, not the stall detector: the agent itself fails a dead
	// registry in seconds (registry probe) and cancels a pull that stops making
	// progress after a minute (stall watchdog), see internal/agent/server.go.
	DefaultAgentPullTimeout = 10 * time.Minute
)

var (
	// ErrAgentUnavailable reports that a node agent could not be reached, timed
	// out, answered with an unexpected status (5xx/404/...) or returned a
	// malformed/incompatible body. Snapshot degrades the node to offline slots;
	// TriggerPull returns it as a failed pull.
	ErrAgentUnavailable = errors.New("node agent unavailable")
	// ErrAgentUnauthorized reports an agent 401/403: the hub Bearer token is
	// missing or wrong (WS-F will harden this with mTLS).
	ErrAgentUnauthorized = errors.New("node agent unauthorized")
	// ErrAgentDiskPaused reports an agent 409: auto-prepull is paused because the
	// node ImageFS is above the disk-pause threshold. The image is not touched.
	ErrAgentDiskPaused = errors.New("node agent paused: image filesystem above threshold")
	// ErrCancelNotSupported reports CancelPull for a running pull on an older
	// provider/spoke that cannot cancel it (the hub maps it to 409). K8sProvider
	// cancels running pulls by canceling the agent request, so it no longer
	// returns this sentinel.
	ErrCancelNotSupported = errors.New("cancel is not supported for synchronous node-agent pulls")
	// ErrAgentPullStalled reports an agent 504: the pull timed out or stopped
	// making progress and was cancelled by the agent. The node is healthy, the
	// registry (or the layer download) is not, so the slot is released and the
	// task fails fast instead of hanging until DefaultAgentPullTimeout.
	ErrAgentPullStalled = errors.New("node agent pull stalled or timed out")
	// ErrAgentRegistryAuth reports an agent pull the image registry rejected for
	// lack of (or with wrong) credentials: the agent answers 502 with
	// reason=registry_auth. The node agent is healthy; the credential is not, so
	// the failure must not be reported as "node agent unavailable".
	ErrAgentRegistryAuth = errors.New("registry authentication failed")
)

// Agent wire v1 response types (subset the hub needs).
type agentSnapshot struct {
	APIVersion   string       `json:"apiVersion"`
	NodeName     string       `json:"node"`
	AgentVersion string       `json:"agentVersion"`
	GeneratedAt  time.Time    `json:"generatedAt"`
	DiskPaused   bool         `json:"diskPaused"`
	ImageFS      agentImageFS `json:"imageFs"`
	Images       []agentImage `json:"images"`
}

type agentImageFS struct {
	UsedBytes     int64 `json:"usedBytes"`
	CapacityBytes int64 `json:"capacityBytes"`
}

type agentImage struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Tags      []string `json:"tags"`
	Digests   []string `json:"digests"`
	SizeBytes int64    `json:"sizeBytes"`
}

// agentPullRequest is the body of POST /agent/v1/pulls. Auth is the credential
// resolved from the workload's imagePullSecrets; nil means an anonymous pull.
type agentPullRequest struct {
	Image string             `json:"image"`
	Auth  *agentRegistryAuth `json:"auth,omitempty"`
}

type agentPullResult struct {
	APIVersion string    `json:"apiVersion"`
	Image      string    `json:"image"`
	ImageRef   string    `json:"imageRef"`
	PulledAt   time.Time `json:"pulledAt"`
}

type agentErrorBody struct {
	Error string `json:"error"`
	// Reason classifies the failure on the agent side ("registry_auth" today).
	// Older agents omit it, so the message stays the only signal then.
	Reason string `json:"reason"`
}

// cachedNodeAddress is a resolved agent base URL for one node.
type cachedNodeAddress struct {
	baseURL   string
	expiresAt time.Time
}

// agentTransport is the hub-side HTTP client for the node-agent API v1. It
// resolves the agent address from the Kubernetes Node object (InternalIP), keeps
// one keep-alive connection pool for all nodes and tracks the pull tasks it
// created so CancelPull/PullCounters and Snapshot.Tasks have provider-side state.
type agentTransport struct {
	clientset  kubernetes.Interface
	httpClient *http.Client

	port        int
	scheme      string
	token       string
	timeout     time.Duration
	pullTimeout time.Duration

	addrMu    sync.Mutex
	addresses map[string]cachedNodeAddress

	taskMu    sync.Mutex
	tasks     map[string]*model.PullTask
	taskOrder []string
	active    map[string]string             // "image|node" -> in-flight task id
	cancels   map[string]context.CancelFunc // task id -> cancel of the in-flight pull
	taskSeq   atomic.Uint64

	counters sync.Map // "image|node|result" -> *atomic.Uint64

	// agentErrors counts inventory failures per (node, reason); see
	// agent_errors.go. Optional Provider capability, exposed by K8sProvider.
	agentErrors agentErrorCounters
}

// newAgentTransport builds the default inventory/pull transport.
func newAgentTransport(clientset kubernetes.Interface) *agentTransport {
	return &agentTransport{
		clientset: clientset,
		httpClient: &http.Client{
			// No proxy: hub -> node agent is cluster-internal traffic.
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   DefaultAgentTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
		port:        DefaultAgentPort,
		scheme:      DefaultAgentScheme,
		timeout:     DefaultAgentTimeout,
		pullTimeout: DefaultAgentPullTimeout,
		addresses:   make(map[string]cachedNodeAddress),
		tasks:       make(map[string]*model.PullTask),
		active:      make(map[string]string),
		cancels:     make(map[string]context.CancelFunc),
	}
}

// --- construction options ---------------------------------------------------

// WithAgentPort sets the node-agent HTTP port (default 9443).
func WithAgentPort(port int) K8sOption {
	return func(p *K8sProvider) {
		if p.agent != nil && port > 0 {
			p.agent.port = port
		}
	}
}

// WithAgentToken sets the Bearer token sent to every node agent (empty disables
// auth, matching the agent's development mode).
func WithAgentToken(token string) K8sOption {
	return func(p *K8sProvider) {
		if p.agent != nil {
			p.agent.token = token
		}
	}
}

// WithAgentTimeout bounds one agent request (default 5s). Pulls use a separate,
// longer deadline (DefaultAgentPullTimeout) derived from the caller context.
func WithAgentTimeout(timeout time.Duration) K8sOption {
	return func(p *K8sProvider) {
		if p.agent != nil && timeout > 0 {
			p.agent.timeout = timeout
		}
	}
}

// WithAgentPullTimeout bounds one synchronous pull through the agent (default
// DefaultAgentPullTimeout). The agent fails a dead registry long before this
// backstop; shorten it only when the hub must not hold a slot that long.
func WithAgentPullTimeout(timeout time.Duration) K8sOption {
	return func(p *K8sProvider) {
		if p.agent != nil && timeout > 0 {
			p.agent.pullTimeout = timeout
		}
	}
}

// WithAgentScheme sets the agent URL scheme, http (default) or https when a TLS
// terminator sits in front of the agent.
func WithAgentScheme(scheme string) K8sOption {
	return func(p *K8sProvider) {
		if p.agent == nil {
			return
		}
		scheme = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(scheme, "://")))
		if scheme == "http" || scheme == "https" {
			p.agent.scheme = scheme
		}
	}
}

// NewK8sProviderFromEnv builds a provider without the caller touching client-go:
// in-cluster configuration first, then $KUBECONFIG (or ~/.kube/config outside a
// cluster). It never contacts the apiserver during construction.
//
// The out-of-cluster fallback parses the kubeconfig itself instead of using
// k8s.io/client-go/tools/clientcmd: that package requires
// github.com/spf13/pflag, which is in go.sum but not in the integrator-owned
// go.mod (feature branches must not touch go.mod). The supported subset is the
// one the hub needs: clusters (server, CA file/data, insecure, TLS server name,
// proxy-url), contexts and users (token, tokenFile, client cert/key file/data,
// basic auth). If go.mod ever gains pflag, this can be swapped for clientcmd.
func NewK8sProviderFromEnv(clusterID string, opts ...K8sOption) (*K8sProvider, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = restConfigFromKubeconfigEnv()
		if err != nil {
			return nil, fmt.Errorf("k8s provider: kube config: %w", err)
		}
	}
	return NewK8sProvider(clusterID, cfg, opts...)
}

// --- kubeconfig fallback (out-of-cluster only) ------------------------------

// kubeconfigFile is the subset of the kubeconfig schema the hub reads.
type kubeconfigFile struct {
	CurrentContext string         `yaml:"current-context"`
	Clusters       []namedCluster `yaml:"clusters"`
	Contexts       []namedContext `yaml:"contexts"`
	Users          []namedUser    `yaml:"users"`
}

type namedCluster struct {
	Name    string      `yaml:"name"`
	Cluster kubeCluster `yaml:"cluster"`
}

type kubeCluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	TLSServerName            string `yaml:"tls-server-name"`
	ProxyURL                 string `yaml:"proxy-url"`
}

type namedContext struct {
	Name    string      `yaml:"name"`
	Context kubeContext `yaml:"context"`
}

type kubeContext struct {
	Cluster   string `yaml:"cluster"`
	User      string `yaml:"user"`
	Namespace string `yaml:"namespace"`
}

type namedUser struct {
	Name string   `yaml:"name"`
	User kubeUser `yaml:"user"`
}

type kubeUser struct {
	Token                 string `yaml:"token"`
	TokenFile             string `yaml:"tokenFile"`
	ClientCertificate     string `yaml:"client-certificate"`
	ClientCertificateData string `yaml:"client-certificate-data"`
	ClientKey             string `yaml:"client-key"`
	ClientKeyData         string `yaml:"client-key-data"`
	Username              string `yaml:"username"`
	Password              string `yaml:"password"`
}

// restConfigFromKubeconfigEnv resolves $KUBECONFIG (a path list) or
// ~/.kube/config and converts it into a rest.Config.
func restConfigFromKubeconfigEnv() (*rest.Config, error) {
	path, err := kubeconfigPath()
	if err != nil {
		return nil, err
	}
	return restConfigFromKubeconfig(path)
}

func kubeconfigPath() (string, error) {
	raw := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	var candidates []string
	if raw != "" {
		candidates = filepath.SplitList(raw)
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = []string{filepath.Join(home, ".kube", "config")}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("no kubeconfig: set KUBECONFIG or create ~/.kube/config")
	}
	return "", fmt.Errorf("no readable kubeconfig in %v", candidates)
}

// restConfigFromKubeconfig parses one kubeconfig file into a rest.Config.
func restConfigFromKubeconfig(path string) (*rest.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig %s: %w", path, err)
	}
	var file kubeconfigFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse kubeconfig %s: %w", path, err)
	}

	contextName := file.CurrentContext
	if contextName == "" && len(file.Contexts) == 1 {
		contextName = file.Contexts[0].Name
	}
	var current *kubeContext
	for i := range file.Contexts {
		if file.Contexts[i].Name == contextName {
			current = &file.Contexts[i].Context
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("kubeconfig %s: current context %q not found", path, contextName)
	}

	var cluster *kubeCluster
	for i := range file.Clusters {
		if file.Clusters[i].Name == current.Cluster {
			cluster = &file.Clusters[i].Cluster
			break
		}
	}
	if cluster == nil {
		return nil, fmt.Errorf("kubeconfig %s: cluster %q not found", path, current.Cluster)
	}
	if strings.TrimSpace(cluster.Server) == "" {
		return nil, fmt.Errorf("kubeconfig %s: cluster %q has no server", path, current.Cluster)
	}

	baseDir := filepath.Dir(path)
	cfg := &rest.Config{Host: strings.TrimSpace(cluster.Server)}
	cfg.TLSClientConfig = rest.TLSClientConfig{
		Insecure:   cluster.InsecureSkipTLSVerify,
		ServerName: cluster.TLSServerName,
	}
	switch {
	case cluster.CertificateAuthorityData != "":
		data, decodeErr := decodeKubeconfigData(cluster.CertificateAuthorityData)
		if decodeErr != nil {
			return nil, fmt.Errorf("kubeconfig %s: certificate-authority-data: %w", path, decodeErr)
		}
		cfg.TLSClientConfig.CAData = data
	case cluster.CertificateAuthority != "":
		cfg.TLSClientConfig.CAFile = resolveKubeconfigPath(baseDir, cluster.CertificateAuthority)
	}
	if cluster.ProxyURL != "" {
		proxyURL, parseErr := url.Parse(cluster.ProxyURL)
		if parseErr != nil {
			return nil, fmt.Errorf("kubeconfig %s: proxy-url: %w", path, parseErr)
		}
		cfg.Proxy = http.ProxyURL(proxyURL)
	}

	if current.User != "" {
		for i := range file.Users {
			if file.Users[i].Name != current.User {
				continue
			}
			user := file.Users[i].User
			cfg.BearerToken = user.Token
			if cfg.BearerToken == "" && user.TokenFile != "" {
				token, readErr := os.ReadFile(resolveKubeconfigPath(baseDir, user.TokenFile))
				if readErr != nil {
					return nil, fmt.Errorf("kubeconfig %s: tokenFile: %w", path, readErr)
				}
				cfg.BearerToken = strings.TrimSpace(string(token))
			}
			cfg.Username = user.Username
			cfg.Password = user.Password
			switch {
			case user.ClientCertificateData != "":
				data, decodeErr := decodeKubeconfigData(user.ClientCertificateData)
				if decodeErr != nil {
					return nil, fmt.Errorf("kubeconfig %s: client-certificate-data: %w", path, decodeErr)
				}
				cfg.TLSClientConfig.CertData = data
			case user.ClientCertificate != "":
				cfg.TLSClientConfig.CertFile = resolveKubeconfigPath(baseDir, user.ClientCertificate)
			}
			switch {
			case user.ClientKeyData != "":
				data, decodeErr := decodeKubeconfigData(user.ClientKeyData)
				if decodeErr != nil {
					return nil, fmt.Errorf("kubeconfig %s: client-key-data: %w", path, decodeErr)
				}
				cfg.TLSClientConfig.KeyData = data
			case user.ClientKey != "":
				cfg.TLSClientConfig.KeyFile = resolveKubeconfigPath(baseDir, user.ClientKey)
			}
			break
		}
	}
	return cfg, nil
}

func decodeKubeconfigData(encoded string) ([]byte, error) {
	compact := strings.Join(strings.Fields(encoded), "")
	return base64.StdEncoding.DecodeString(compact)
}

func resolveKubeconfigPath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

// --- NodeInventorySource ----------------------------------------------------

// Inventory implements NodeInventorySource over GET /agent/v1/snapshot. Any
// failure is wrapped with ErrNodeInventoryUnavailable (and the specific agent
// sentinel) so the provider degrades the node instead of failing the snapshot.
func (a *agentTransport) Inventory(ctx context.Context, node string) (NodeInventory, error) {
	var snapshot agentSnapshot
	if err := a.getJSON(ctx, node, agentSnapshotPath, &snapshot); err != nil {
		return NodeInventory{}, fmt.Errorf("%w: %w", ErrNodeInventoryUnavailable, err)
	}
	if snapshot.APIVersion != "" && snapshot.APIVersion != agentAPIVersion {
		return NodeInventory{}, fmt.Errorf("%w: %w: node %q reports apiVersion %q, want %q",
			ErrNodeInventoryUnavailable, ErrAgentUnavailable, node, snapshot.APIVersion, agentAPIVersion)
	}
	if snapshot.NodeName != "" && snapshot.NodeName != node {
		return NodeInventory{}, fmt.Errorf("%w: %w: node %q answered for node %q",
			ErrNodeInventoryUnavailable, ErrAgentUnavailable, node, snapshot.NodeName)
	}
	return NodeInventory{
		Images:               cachedImagesFromAgent(snapshot.Images),
		ImageFSUsedBytes:     snapshot.ImageFS.UsedBytes,
		ImageFSCapacityBytes: snapshot.ImageFS.CapacityBytes,
		AgentVersion:         snapshot.AgentVersion,
		ActivePulls:          0, // the agent does not report its pull queue yet
		LastSeen:             snapshot.GeneratedAt,
	}, nil
}

// cachedImagesFromAgent flattens CRI image entries into the reference-oriented
// list the slot matcher consumes: one model.CachedImage per tag/digest/name.
// A workload spec matches any of them, exactly like kubelet resolves an image by
// any of its references.
func cachedImagesFromAgent(images []agentImage) []model.CachedImage {
	out := make([]model.CachedImage, 0, len(images))
	seen := make(map[string]struct{}, len(images))
	add := func(ref string, size int64) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		if _, ok := seen[ref]; ok {
			return
		}
		seen[ref] = struct{}{}
		out = append(out, model.CachedImage{Image: ref, SizeBytes: size})
	}
	for _, image := range images {
		// Name is the agent's primary reference; tags/digests widen matching.
		add(image.Name, image.SizeBytes)
		for _, tag := range image.Tags {
			add(tag, image.SizeBytes)
		}
		for _, digest := range image.Digests {
			add(digest, image.SizeBytes)
		}
		if image.Name == "" && len(image.Tags) == 0 && len(image.Digests) == 0 {
			add(image.ID, image.SizeBytes)
		}
	}
	return out
}

// --- pull / cancel / counters ----------------------------------------------

// TriggerPull starts one asynchronous pull through the node agent and returns
// the running task immediately (Provider invariant 5). POST /agent/v1/pulls
// blocks until CRI PullImage returns, so the pull runs in its own goroutine with
// its own deadline: the caller context only has to survive the start, and the
// HTTP request that triggered the pull no longer hangs for the whole download.
// CancelPull cancels that goroutine's context, which closes the agent request and
// aborts the CRI pull on the node (the agent runs PullImage under the request
// context). A 409 disk-paused does not touch the image and ends the task failed.
func (p *K8sProvider) TriggerPull(ctx context.Context, req model.PullRequest) (model.PullTask, error) {
	if p.agent == nil {
		return model.PullTask{}, fmt.Errorf("k8s provider: TriggerPull: %w", ErrNotImplemented)
	}
	image := strings.TrimSpace(req.Image)
	node := strings.TrimSpace(req.Node)
	if image == "" || node == "" {
		return model.PullTask{}, errors.New("k8s provider: TriggerPull: image and node are required")
	}
	req.Image, req.Node = image, node
	if err := ctx.Err(); err != nil {
		return model.PullTask{}, err
	}

	task, existing := p.agent.beginTask(p.clusterID, req)
	if existing != nil {
		// Idempotency: an in-flight pull for the same image+node is returned as is.
		return *existing, nil
	}

	// Resolve the workload's imagePullSecrets before the pull starts. A node
	// agent pulls through CRI and is anonymous otherwise, so a private registry
	// would answer 403 even though the workload has a pull secret
	// (k8s_registry.go). Resolution reads the index of the last snapshot and a
	// cached Secret, never the cluster listing.
	auth, authHint := p.registryAuthFor(ctx, image)

	pullCtx, cancel := context.WithTimeout(context.Background(), p.agent.pullTimeout)
	if !p.agent.registerCancel(task.ID, cancel) {
		// The task was canceled between beginTask and here: do not start a pull.
		cancel()
		return p.agent.taskCopy(task.ID), nil
	}
	go p.runAgentPull(pullCtx, task.ID, image, node, auth, authHint)
	return *task, nil
}

// runAgentPull performs one asynchronous agent pull and records its result. Any
// error from the transport ends the task; a canceled task is already terminal, so
// finishTask reports false and the goroutine leaves counters untouched.
func (p *K8sProvider) runAgentPull(ctx context.Context, taskID, image, node string, auth *agentRegistryAuth, authHint string) {
	raw, err := p.agent.request(ctx, node, http.MethodPost, agentPullsPath, agentPullRequest{Image: image, Auth: auth}, p.agent.pullTimeout)
	if err != nil {
		message := err.Error()
		// Only a credential rejection gets the hint: for a disk-full or a dead
		// agent, "no imagePullSecrets" would send the operator down the wrong path.
		if errors.Is(err, ErrAgentRegistryAuth) && authHint != "" {
			message += " (" + authHint + ")"
		}
		p.finishAgentPull(taskID, image, node, model.TaskFailed, message)
		return
	}

	var result agentPullResult
	if err := json.Unmarshal(raw, &result); err != nil {
		wrapped := fmt.Errorf("%w: node %q: decode pull response: %v", ErrAgentUnavailable, node, err)
		p.finishAgentPull(taskID, image, node, model.TaskFailed, wrapped.Error())
		return
	}
	if result.APIVersion != "" && result.APIVersion != agentAPIVersion {
		wrapped := fmt.Errorf("%w: node %q: pull apiVersion %q, want %q",
			ErrAgentUnavailable, node, result.APIVersion, agentAPIVersion)
		p.finishAgentPull(taskID, image, node, model.TaskFailed, wrapped.Error())
		return
	}
	p.finishAgentPull(taskID, image, node, model.TaskSucceeded, "")
}

// finishAgentPull terminates a task and, only when the transition actually
// happened, updates the counters and drops the cached snapshot: a canceled task
// must not be counted as a success or failure.
func (p *K8sProvider) finishAgentPull(taskID, image, node string, state model.TaskState, errMessage string) {
	if !p.agent.finishTask(taskID, state, errMessage) {
		return
	}
	result := "error"
	if state == model.TaskSucceeded {
		result = "success"
	}
	p.agent.recordPull(image, node, result)
	// Succeeded: the image is on the node now, so drop the cached snapshot and let
	// the next scrape see a present slot without waiting for the TTL. Failed:
	// drop it too, so a retry is not masked by a stale "missing" cell.
	p.invalidateSnapshot()
}

// TriggerWorkloadPull starts a pull for every image of one workload on every node
// in the snapshot and returns how many tasks were started (or were already
// active). The pulls run asynchronously. Nodes without a live agent are not
// skipped: their task is still created and fails asynchronously with
// ErrAgentUnavailable, so an operator-initiated warm-up always leaves visible
// Pull Tasks instead of silently doing nothing. The cached snapshot is reused
// even past its TTL: building a fresh one polls every node agent sequentially,
// which does not fit the 5 s outbound spoke command budget. A fresh snapshot is
// built only when nothing has been cached yet.
func (p *K8sProvider) TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error) {
	if p.agent == nil {
		return 0, fmt.Errorf("k8s provider: TriggerWorkloadPull: %w", ErrNotImplemented)
	}

	snapshot, cached := p.cachedSnapshot()
	if !cached {
		snapshot = p.Snapshot(ctx)
	}

	var images []string
	found := false
	seen := make(map[string]struct{})
	for _, workload := range snapshot.Workloads {
		if workload.Namespace != req.Namespace ||
			!strings.EqualFold(workload.Kind, req.Kind) ||
			workload.Name != req.Name {
			continue
		}
		found = true
		for _, image := range workload.Images {
			if _, ok := seen[image.Image]; ok {
				continue
			}
			seen[image.Image] = struct{}{}
			images = append(images, image.Image)
		}
	}
	if !found {
		return 0, fmt.Errorf("workload %s/%s/%s not found", req.Namespace, req.Kind, req.Name)
	}
	if len(images) == 0 {
		return 0, fmt.Errorf("workload %s/%s/%s has no images", req.Namespace, req.Kind, req.Name)
	}

	triggered := 0
	for _, image := range images {
		for _, node := range snapshot.Nodes {
			_, err := p.TriggerPull(ctx, model.PullRequest{
				Image:   image,
				Node:    node.Name,
				Actor:   req.Actor,
				Trigger: triggerOrDefault(req.Trigger),
			})
			if err == nil {
				triggered++
			}
		}
	}
	return triggered, nil
}

// CancelPull cancels a queued or running pull. A running pull is canceled through
// its context: the hub closes the agent request and the agent aborts the CRI pull
// it started for that request. Unknown ids are ErrTaskNotFound; a finished task
// reports the MockProvider-style "already finished" error (Provider invariant 4).
func (p *K8sProvider) CancelPull(_ context.Context, taskID string) error {
	if p.agent == nil {
		return fmt.Errorf("k8s provider: CancelPull: %w", ErrNotImplemented)
	}
	if err := p.agent.cancelTask(taskID); err != nil {
		return err
	}
	p.invalidateSnapshot()
	return nil
}

// PullCounters returns provider-local pull counters (image x node x result),
// filled by TriggerPull; nil is never returned so the metrics layer renders an
// empty series instead of "no counters".
func (p *K8sProvider) PullCounters() []model.PullMetricSample {
	if p.agent == nil {
		return []model.PullMetricSample{}
	}
	return p.agent.pullCounters()
}

// --- HTTP plumbing ----------------------------------------------------------

func (a *agentTransport) getJSON(ctx context.Context, node, path string, out any) error {
	raw, err := a.request(ctx, node, http.MethodGet, path, nil, a.timeout)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: node %q: decode %s: %v", ErrAgentUnavailable, node, path, err)
	}
	return nil
}

// request performs one agent call with the given timeout (a.timeout for control
// calls, a longer deadline for CRI pulls) and maps HTTP status codes onto the
// provider sentinel errors.
func (a *agentTransport) request(ctx context.Context, node, method, path string, payload any, timeout time.Duration) ([]byte, error) {
	baseURL, err := a.nodeBaseURL(ctx, node)
	if err != nil {
		return nil, err
	}

	var body io.Reader
	if payload != nil {
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, fmt.Errorf("%w: encode request: %v", ErrAgentUnavailable, marshalErr)
		}
		body = bytes.NewReader(raw)
	}

	if timeout <= 0 {
		timeout = a.timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrAgentUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		// The cached address may be stale (node re-registered with a new IP).
		a.forgetAddress(node)
		// %w keeps context.DeadlineExceeded/context.Canceled in the chain so
		// ClassifyAgentError can distinguish a timeout from an unreachable agent.
		return nil, fmt.Errorf("%w: node %q: %w", ErrAgentUnavailable, node, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAgentResponseBytes))
	if readErr != nil {
		return nil, fmt.Errorf("%w: node %q: read response: %v", ErrAgentUnavailable, node, readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}

	message := agentErrorMessage(raw)
	if agentErrorReason(raw) == agentReasonRegistryAuth {
		return nil, fmt.Errorf("%w: node %q: %s", ErrAgentRegistryAuth, node, message)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: node %q: http %d: %s", ErrAgentUnauthorized, node, resp.StatusCode, message)
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: node %q: %s", ErrAgentDiskPaused, node, message)
	case http.StatusGatewayTimeout:
		return nil, fmt.Errorf("%w: node %q: %s", ErrAgentPullStalled, node, message)
	default:
		return nil, fmt.Errorf("%w: node %q: http %d: %s", ErrAgentUnavailable, node, resp.StatusCode, message)
	}
}

// agentErrorReason returns the machine-readable reason of an agent error body
// (empty for older agents that only send "error").
func agentErrorReason(raw []byte) string {
	var body agentErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

func agentErrorMessage(raw []byte) string {
	var body agentErrorBody
	if err := json.Unmarshal(raw, &body); err == nil && strings.TrimSpace(body.Error) != "" {
		return strings.TrimSpace(body.Error)
	}
	message := strings.TrimSpace(string(raw))
	const maxMessage = 256
	if len(message) > maxMessage {
		message = message[:maxMessage] + "..."
	}
	if message == "" {
		message = "empty error body"
	}
	return message
}

// nodeBaseURL resolves the agent base URL for a node: Node.status.addresses
// InternalIP plus the configured port, cached for a minute.
func (a *agentTransport) nodeBaseURL(ctx context.Context, node string) (string, error) {
	if a.clientset == nil {
		return "", fmt.Errorf("%w: no Kubernetes clientset to resolve node %q", ErrAgentUnavailable, node)
	}
	now := time.Now()
	a.addrMu.Lock()
	if cached, ok := a.addresses[node]; ok && now.Before(cached.expiresAt) {
		a.addrMu.Unlock()
		return cached.baseURL, nil
	}
	a.addrMu.Unlock()

	nodeObject, err := a.clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: resolve node %q address: %v", ErrAgentUnavailable, node, err)
	}
	ip := nodeInternalIP(nodeObject)
	if ip == "" {
		return "", fmt.Errorf("%w: node %q has no InternalIP address", ErrAgentUnavailable, node)
	}
	baseURL := a.scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(a.port))

	a.addrMu.Lock()
	a.addresses[node] = cachedNodeAddress{baseURL: baseURL, expiresAt: now.Add(nodeAddressCacheTTL)}
	a.addrMu.Unlock()
	return baseURL, nil
}

func (a *agentTransport) forgetAddress(node string) {
	a.addrMu.Lock()
	delete(a.addresses, node)
	a.addrMu.Unlock()
}

func nodeInternalIP(node *corev1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type != corev1.NodeInternalIP {
			continue
		}
		if ip := strings.TrimSpace(address.Address); ip != "" {
			return ip
		}
	}
	return ""
}

// --- pull task bookkeeping --------------------------------------------------

// beginTask registers a running task, or returns an existing in-flight task for
// the same image+node (Provider idempotency invariant).
func (a *agentTransport) beginTask(cluster string, req model.PullRequest) (created, existing *model.PullTask) {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()

	key := req.Image + "|" + req.Node
	if id, ok := a.active[key]; ok {
		if task, ok := a.tasks[id]; ok && task.State.Active() {
			duplicate := *task
			return nil, &duplicate
		}
		delete(a.active, key)
	}

	now := time.Now()
	started := now
	task := &model.PullTask{
		ID:        fmt.Sprintf("task-%d", a.taskSeq.Add(1)),
		Cluster:   cluster,
		Image:     req.Image,
		Node:      req.Node,
		State:     model.TaskRunning,
		Attempt:   1,
		Trigger:   triggerOrDefault(req.Trigger),
		Actor:     req.Actor,
		CreatedAt: now,
		StartedAt: &started,
	}
	a.tasks[task.ID] = task
	a.taskOrder = append(a.taskOrder, task.ID)
	a.active[key] = task.ID
	a.pruneTasksLocked()

	copied := *task
	return &copied, nil
}

// registerCancel stores the cancel func of a freshly started pull. It reports
// false when the task is already terminal (canceled between beginTask and the
// goroutine start), in which case the caller must not start the pull at all.
func (a *agentTransport) registerCancel(taskID string, cancel context.CancelFunc) bool {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()

	task, ok := a.tasks[taskID]
	if !ok || !task.State.Active() {
		return false
	}
	a.cancels[taskID] = cancel
	return true
}

// finishTask moves an active task to a terminal state and reports whether the
// transition happened: a task that was canceled first is left untouched, so the
// pull goroutine cannot overwrite "canceled" with "failed" or count a counter.
func (a *agentTransport) finishTask(taskID string, state model.TaskState, errMessage string) bool {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()

	task, ok := a.tasks[taskID]
	if !ok || !task.State.Active() {
		return false
	}
	finished := time.Now()
	task.State = state
	task.Error = errMessage
	task.FinishedAt = &finished
	if task.StartedAt != nil {
		task.DurationMs = finished.Sub(*task.StartedAt).Milliseconds()
	}
	key := task.Image + "|" + task.Node
	if a.active[key] == taskID {
		delete(a.active, key)
	}
	delete(a.cancels, taskID)
	return true
}

// cancelTask cancels a queued or running pull. The task is marked canceled under
// the same lock that owns the registry, then the pull context is canceled: the
// in-flight agent request is closed and the agent aborts the CRI pull.
func (a *agentTransport) cancelTask(taskID string) error {
	a.taskMu.Lock()
	task, ok := a.tasks[taskID]
	if !ok {
		a.taskMu.Unlock()
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	if !task.State.Active() {
		state := task.State
		a.taskMu.Unlock()
		return fmt.Errorf("pull task %s is already finished (%s)", taskID, state)
	}
	finished := time.Now()
	task.State = model.TaskCanceled
	task.Error = "canceled by operator"
	task.FinishedAt = &finished
	if task.StartedAt != nil {
		task.DurationMs = finished.Sub(*task.StartedAt).Milliseconds()
	}
	image, node := task.Image, task.Node
	key := image + "|" + node
	if a.active[key] == taskID {
		delete(a.active, key)
	}
	cancel := a.cancels[taskID]
	delete(a.cancels, taskID)
	a.taskMu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.recordPull(image, node, "canceled")
	return nil
}

func (a *agentTransport) taskCopy(taskID string) model.PullTask {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if task, ok := a.tasks[taskID]; ok {
		return *task
	}
	return model.PullTask{}
}

// tasksSnapshot returns the tracked tasks newest first, bounded like MockProvider.
func (a *agentTransport) tasksSnapshot() []model.PullTask {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	out := make([]model.PullTask, 0, len(a.taskOrder))
	for _, id := range a.taskOrder {
		if task, ok := a.tasks[id]; ok {
			out = append(out, *task)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// pruneTasksLocked drops the oldest finished tasks above the history limit; an
// active task is never dropped (its id is still cancellable).
func (a *agentTransport) pruneTasksLocked() {
	for len(a.taskOrder) > agentTaskHistoryLimit {
		oldest := a.taskOrder[0]
		task, ok := a.tasks[oldest]
		if ok && task.State.Active() {
			return
		}
		delete(a.tasks, oldest)
		a.taskOrder = a.taskOrder[1:]
	}
}

// --- pull counters ----------------------------------------------------------

func (a *agentTransport) recordPull(image, node, result string) {
	key := image + "|" + node + "|" + result
	value, _ := a.counters.LoadOrStore(key, new(atomic.Uint64))
	value.(*atomic.Uint64).Add(1)
}

func (a *agentTransport) pullCounters() []model.PullMetricSample {
	out := make([]model.PullMetricSample, 0)
	a.counters.Range(func(key, value any) bool {
		parts := strings.Split(key.(string), "|")
		if len(parts) != 3 {
			return true
		}
		out = append(out, model.PullMetricSample{
			Image:  parts[0],
			Node:   parts[1],
			Result: parts[2],
			Count:  value.(*atomic.Uint64).Load(),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Image != out[j].Image {
			return out[i].Image < out[j].Image
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Result < out[j].Result
	})
	return out
}

// invalidateSnapshot drops the cached snapshot (same effect as SyncNow) so a
// just-finished pull is reflected on the next scrape.
func (p *K8sProvider) invalidateSnapshot() {
	p.mu.Lock()
	p.cache = snapshotCache{}
	p.mu.Unlock()
}

// tasksSnapshot exposes the transport tasks to the snapshot builder.
func (p *K8sProvider) tasksSnapshot() []model.PullTask {
	if p.agent == nil {
		return nil
	}
	return p.agent.tasksSnapshot()
}
