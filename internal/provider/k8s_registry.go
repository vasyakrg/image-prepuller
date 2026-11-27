package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/local/image-prepuller/internal/model"
)

// imagePullSecret resolution on the hub side.
//
// A node agent pulls through CRI, not through the kubelet, and Kubernetes
// applies imagePullSecrets only to the pods it creates itself. An agent pull is
// therefore anonymous, and a registry that requires a login answers 403 on every
// node where the image is not cached yet: the image looks "present" only on the
// node whose kubelet already pulled it, and "missing" everywhere else.
//
// The provider is the only component with a clientset, so it resolves the
// credential from the imagePullSecrets of the in-scope workloads that reference
// the image and hands it to the agent inside the pull request. The agent never
// talks to the apiserver, so the extra privilege stays in one ServiceAccount:
// "get" on secrets for hub/spoke (chart RBAC, docs/DEPLOY.md).
const (
	// defaultRegistryAuthTTL bounds the parsed-secret cache: a rotated
	// dockerconfigjson is picked up within five minutes and the apiserver is not
	// hit on every pull.
	defaultRegistryAuthTTL = 5 * time.Minute
	// registryAuthErrorTTL is shorter so a fixed secret or newly granted RBAC is
	// retried quickly instead of waiting out the success TTL.
	registryAuthErrorTTL = time.Minute
	// dockerHubHost is the canonical Docker Hub name used when matching a
	// credential against an image reference.
	dockerHubHost = "docker.io"
)

// agentRegistryAuth is the credential sent to the node agent with the pull
// request. It mirrors agent.RegistryAuth: the hub must not import internal/agent
// (ADR-001), so the tiny struct is duplicated like the rest of the agent wire
// types in k8s_agent.go. No credential means an anonymous pull.
type agentRegistryAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
	RegistryToken string `json:"registryToken,omitempty"`
}

// dockerConfig is the schema of a kubernetes.io/dockerconfigjson Secret.
type dockerConfig struct {
	Auths map[string]dockerConfigEntry `json:"auths"`
}

// dockerConfigEntry is one registry credential. dockerconfigjson spells the two
// token fields without dashes.
type dockerConfigEntry struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// registryAuthCacheEntry caches one parsed Secret together with its error.
type registryAuthCacheEntry struct {
	auths     map[string]dockerConfigEntry
	err       error
	expiresAt time.Time
}

// secretRef identifies one imagePullSecret in a workload's namespace.
type secretRef struct {
	namespace string
	name      string
}

// registryAuthFor resolves the credential to use for one pull of image. It
// returns a human-readable hint when no credential could be resolved; the caller
// appends that hint to a failed task so the operator reads "check
// imagePullSecrets" instead of a bare registry 403.
func (p *K8sProvider) registryAuthFor(ctx context.Context, image string) (*agentRegistryAuth, string) {
	if p.clientset == nil {
		return nil, ""
	}
	host := imageRegistryHost(image)
	if host == "" {
		return nil, ""
	}
	refs := p.imagePullSecretsFor(image)
	if len(refs) == 0 {
		return nil, "no imagePullSecrets on the workloads that use this image"
	}

	var (
		unreadable []string
		names      []string
	)
	for _, ref := range refs {
		names = append(names, ref.namespace+"/"+ref.name)
		auths, err := p.cachedSecretAuths(ctx, ref.namespace, ref.name)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s/%s: %v", ref.namespace, ref.name, err))
			continue
		}
		entry, ok := lookupRegistryAuth(auths, host)
		if !ok {
			continue
		}
		return entry.registryAuth(), ""
	}
	if len(unreadable) > 0 {
		return nil, "cannot read imagePullSecrets: " + strings.Join(unreadable, ", ")
	}
	return nil, fmt.Sprintf("no credential for %s in imagePullSecrets %s", host, strings.Join(names, ", "))
}

// imagePullSecretsFor returns the imagePullSecrets of the workloads that
// reference image. The index is built by buildSnapshot, so resolving a pull
// secret never re-lists the cluster and a pull cannot recursively trigger a
// snapshot.
func (p *K8sProvider) imagePullSecretsFor(image string) []secretRef {
	key := strings.TrimSpace(image)
	p.authMu.Lock()
	defer p.authMu.Unlock()
	return append([]secretRef(nil), p.secretIndex[key]...)
}

// setPullSecretIndex records the image -> imagePullSecrets mapping of one
// snapshot. Only workloads with pull secrets are indexed; an image nobody
// references has no entry and is pulled anonymously.
func (p *K8sProvider) setPullSecretIndex(workloads []model.Workload) {
	index := make(map[string][]secretRef)
	for _, workload := range workloads {
		if len(workload.ImagePullSecrets) == 0 {
			continue
		}
		for _, image := range workload.Images {
			key := strings.TrimSpace(image.Image)
			if key == "" {
				continue
			}
			refs := index[key]
			for _, name := range workload.ImagePullSecrets {
				if name == "" {
					continue
				}
				ref := secretRef{namespace: workload.Namespace, name: name}
				if containsSecretRef(refs, ref) {
					continue
				}
				refs = append(refs, ref)
			}
			index[key] = refs
		}
	}
	p.authMu.Lock()
	p.secretIndex = index
	p.authMu.Unlock()
}

func containsSecretRef(refs []secretRef, want secretRef) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

// cachedSecretAuths reads and parses one imagePullSecret, caching the result
// (including failures) for a short TTL.
func (p *K8sProvider) cachedSecretAuths(ctx context.Context, namespace, name string) (map[string]dockerConfigEntry, error) {
	key := namespace + "/" + name
	now := p.pullSecretNow()

	p.authMu.Lock()
	cached, ok := p.authCache[key]
	p.authMu.Unlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.auths, cached.err
	}

	auths, err := p.readSecretAuths(ctx, namespace, name)
	ttl := defaultRegistryAuthTTL
	if err != nil {
		ttl = registryAuthErrorTTL
	}
	p.authMu.Lock()
	if p.authCache == nil {
		p.authCache = make(map[string]registryAuthCacheEntry)
	}
	p.authCache[key] = registryAuthCacheEntry{auths: auths, err: err, expiresAt: now.Add(ttl)}
	p.authMu.Unlock()
	return auths, err
}

func (p *K8sProvider) pullSecretNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// readSecretAuths fetches one Secret and parses its docker credential map.
func (p *K8sProvider) readSecretAuths(ctx context.Context, namespace, name string) (map[string]dockerConfigEntry, error) {
	secret, err := p.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseImagePullSecret(secret)
}

// parseImagePullSecret understands both types kubelet accepts:
// kubernetes.io/dockerconfigjson (key ".dockerconfigjson", body {"auths": ...})
// and the legacy kubernetes.io/dockercfg (key ".dockercfg", a bare host map).
func parseImagePullSecret(secret *corev1.Secret) (map[string]dockerConfigEntry, error) {
	if secret == nil {
		return nil, fmt.Errorf("nil secret")
	}
	if raw, ok := secret.Data[corev1.DockerConfigJsonKey]; ok && len(raw) > 0 {
		var cfg dockerConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigJsonKey, err)
		}
		if len(cfg.Auths) == 0 {
			return nil, fmt.Errorf("%s has no auths", corev1.DockerConfigJsonKey)
		}
		return cfg.Auths, nil
	}
	if raw, ok := secret.Data[corev1.DockerConfigKey]; ok && len(raw) > 0 {
		var auths map[string]dockerConfigEntry
		if err := json.Unmarshal(raw, &auths); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigKey, err)
		}
		if len(auths) == 0 {
			return nil, fmt.Errorf("%s has no entries", corev1.DockerConfigKey)
		}
		return auths, nil
	}
	return nil, fmt.Errorf("secret has neither %s nor %s", corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

// lookupRegistryAuth returns the credential that matches host. dockerconfigjson
// keys are either a bare host ("ghcr.io") or a URL ("https://index.docker.io/v1/"),
// so both spellings are normalised before the comparison. Keys are iterated in
// sorted order to keep the choice deterministic when several entries fold onto
// the same host.
func lookupRegistryAuth(auths map[string]dockerConfigEntry, host string) (dockerConfigEntry, bool) {
	want := canonicalRegistryHost(host)
	if want == "" || len(auths) == 0 {
		return dockerConfigEntry{}, false
	}
	keys := make([]string, 0, len(auths))
	for key := range auths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := auths[key]
		if entry.empty() || canonicalRegistryHost(key) != want {
			continue
		}
		return entry, true
	}
	return dockerConfigEntry{}, false
}

// canonicalRegistryHost maps a registry host or a dockerconfigjson key onto one
// comparable name: lower-case host without scheme, path or trailing slash, with
// every Docker Hub spelling folded to "docker.io".
func canonicalRegistryHost(value string) string {
	host := strings.ToLower(strings.TrimSpace(value))
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "docker.io":
		return dockerHubHost
	}
	return host
}

// imageRegistryHost resolves the registry host of an image reference with the
// distribution rules: the first path element is a registry only when it contains
// a dot or a colon or is "localhost"; everything else lives on Docker Hub.
func imageRegistryHost(image string) string {
	ref := strings.TrimSpace(image)
	if ref == "" {
		return ""
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		if j := strings.IndexByte(ref[i+1:], ':'); j >= 0 {
			ref = ref[:i+1+j]
		}
	}
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || strings.EqualFold(first, "localhost") {
			return canonicalRegistryHost(first)
		}
	}
	return dockerHubHost
}

// registryAuth converts a dockerconfigjson entry into the wire credential. When
// only the base64 "auth" blob is present it is decoded too: containerd accepts
// the blob, but explicit username/password is what most registries log.
func (e dockerConfigEntry) registryAuth() *agentRegistryAuth {
	out := &agentRegistryAuth{
		Username:      e.Username,
		Password:      e.Password,
		Auth:          e.Auth,
		IdentityToken: e.IdentityToken,
		RegistryToken: e.RegistryToken,
	}
	if out.Username == "" && out.Password == "" && out.Auth != "" {
		if user, password, ok := decodeDockerAuth(out.Auth); ok {
			out.Username, out.Password = user, password
		}
	}
	return out
}

// empty reports whether the entry carries no usable credential.
func (e dockerConfigEntry) empty() bool {
	return e.Username == "" && e.Password == "" && e.Auth == "" &&
		e.IdentityToken == "" && e.RegistryToken == ""
}

// decodeDockerAuth splits the base64 "user:password" blob of an entry.
func decodeDockerAuth(encoded string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	user, password, ok := strings.Cut(string(raw), ":")
	if !ok || user == "" {
		return "", "", false
	}
	return user, password, true
}
