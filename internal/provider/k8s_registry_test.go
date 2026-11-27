package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/local/image-prepuller/internal/model"
)

// dockerConfigSecret builds one kubernetes.io/dockerconfigjson Secret holding a
// single credential.
func dockerConfigSecret(namespace, name, host, username, password string) *corev1.Secret {
	raw, _ := json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]string{
				"username": username,
				"password": password,
				"auth":     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
			},
		},
	})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: raw},
	}
}

func TestImageRegistryHost(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{"nginx:latest", "docker.io"},
		{"library/nginx:1.27", "docker.io"},
		{"docker.io/library/nginx@sha256:abc", "docker.io"},
		{"index.docker.io/library/nginx:1", "docker.io"},
		{"hub.example/tools/app:1.0.0", "hub.example"},
		{"hub.example:5000/tools/app:1.0.0", "hub.example:5000"},
		{"localhost/app:1", "localhost"},
		{"ghcr.io/acme/api@sha256:deadbeef", "ghcr.io"},
		{"", ""},
	} {
		if got := imageRegistryHost(tc.image); got != tc.want {
			t.Errorf("imageRegistryHost(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func TestLookupRegistryAuth(t *testing.T) {
	auths := map[string]dockerConfigEntry{
		"https://index.docker.io/v1/": {Username: "hub-user", Password: "hub-pass"},
		"hub.example":                 {Username: "robot", Password: "s3cret"},
		"empty.example":               {},
	}
	if entry, ok := lookupRegistryAuth(auths, "docker.io"); !ok || entry.Username != "hub-user" {
		t.Fatalf("docker.io lookup = %+v, %v", entry, ok)
	}
	// A reference without a host resolves to Docker Hub first, even though the
	// key is a legacy URL.
	if entry, ok := lookupRegistryAuth(auths, imageRegistryHost("nginx")); !ok || entry.Password != "hub-pass" {
		t.Fatalf("hub lookup = %+v, %v", entry, ok)
	}
	if entry, ok := lookupRegistryAuth(auths, "hub.example"); !ok || entry.Username != "robot" {
		t.Fatalf("host lookup = %+v, %v", entry, ok)
	}
	if _, ok := lookupRegistryAuth(auths, "missing.example"); ok {
		t.Fatal("matched a registry that has no credential")
	}
	if _, ok := lookupRegistryAuth(auths, "empty.example"); ok {
		t.Fatal("matched an entry with no usable credential")
	}
}

func TestParseImagePullSecret(t *testing.T) {
	// dockerconfigjson: the modern type kubelet accepts.
	raw, _ := json.Marshal(map[string]any{"auths": map[string]any{
		"hub.example": map[string]string{"username": "robot", "password": "s3cret"},
	}})
	auths, err := parseImagePullSecret(&corev1.Secret{Data: map[string][]byte{corev1.DockerConfigJsonKey: raw}})
	if err != nil || auths["hub.example"].Username != "robot" {
		t.Fatalf("dockerconfigjson parse = %+v, %v", auths, err)
	}

	// Legacy dockercfg: a bare host map without the "auths" wrapper.
	legacy, _ := json.Marshal(map[string]any{
		"hub.example": map[string]string{"username": "legacy", "password": "p"},
	})
	auths, err = parseImagePullSecret(&corev1.Secret{Data: map[string][]byte{corev1.DockerConfigKey: legacy}})
	if err != nil || auths["hub.example"].Username != "legacy" {
		t.Fatalf("dockercfg parse = %+v, %v", auths, err)
	}

	for name, secret := range map[string]*corev1.Secret{
		"nil":            nil,
		"no docker data": {Data: map[string][]byte{"token": []byte("x")}},
		"broken json":    {Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte("{")}},
		"empty auths":    {Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}},
	} {
		if _, err := parseImagePullSecret(secret); err == nil {
			t.Errorf("%s: parseImagePullSecret = nil error", name)
		}
	}
}

func TestDockerConfigEntryAuth(t *testing.T) {
	// Only the base64 blob: it is decoded so the runtime gets a real login.
	blob := base64.StdEncoding.EncodeToString([]byte("robot:s3cret"))
	auth := dockerConfigEntry{Auth: blob}.registryAuth()
	if auth.Username != "robot" || auth.Password != "s3cret" || auth.Auth != blob {
		t.Fatalf("auth from blob = %+v", auth)
	}
	// Explicit fields win, and the blob is kept as-is.
	auth = dockerConfigEntry{Username: "u", Password: "p", Auth: "opaque"}.registryAuth()
	if auth.Username != "u" || auth.Password != "p" {
		t.Fatalf("explicit auth = %+v", auth)
	}
	// A malformed blob is not fatal: the entry is still forwarded.
	auth = dockerConfigEntry{Auth: "!!!not-base64"}.registryAuth()
	if auth.Username != "" || auth.Auth != "!!!not-base64" {
		t.Fatalf("malformed blob auth = %+v", auth)
	}
}

// --- provider-side resolution ----------------------------------------------

func newRegistryProvider(secrets ...*corev1.Secret) (*K8sProvider, *kubefake.Clientset) {
	objects := make([]runtime.Object, 0, len(secrets))
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	cs := kubefake.NewSimpleClientset(objects...)
	return NewK8sProviderWithClientset("test-cluster", cs), cs
}

func TestRegistryAuthForResolvesWorkloadSecret(t *testing.T) {
	provider, cs := newRegistryProvider(dockerConfigSecret("tools", "hub-registry", "hub.example", "robot", "s3cret"))
	provider.setPullSecretIndex([]model.Workload{{
		Namespace:        "tools",
		Kind:             kindDeployment,
		Name:             "app",
		ImagePullSecrets: []string{"hub-registry"},
		Images:           []model.WorkloadImage{{Image: "hub.example/tools/app:1.0.0"}},
	}})

	auth, hint := provider.registryAuthFor(context.Background(), "hub.example/tools/app:1.0.0")
	if hint != "" {
		t.Fatalf("hint = %q, want empty", hint)
	}
	if auth == nil || auth.Username != "robot" || auth.Password != "s3cret" {
		t.Fatalf("auth = %+v, want the secret credential", auth)
	}

	// Second resolution comes from the cache: one Get, not two.
	if _, _ = provider.registryAuthFor(context.Background(), "hub.example/tools/app:1.0.0"); false {
		t.Fatal("unreachable")
	}
	gets := 0
	for _, action := range cs.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "secrets" {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("secret reads = %d, want 1 (cached after the first pull)", gets)
	}
}

func TestRegistryAuthForHints(t *testing.T) {
	// No workload references a pull secret: the hint says so.
	provider, _ := newRegistryProvider()
	provider.setPullSecretIndex([]model.Workload{{
		Namespace: "tools",
		Images:    []model.WorkloadImage{{Image: "hub.example/tools/app:1.0.0"}},
	}})
	auth, hint := provider.registryAuthFor(context.Background(), "hub.example/tools/app:1.0.0")
	if auth != nil {
		t.Fatalf("auth = %+v, want nil", auth)
	}
	if !strings.Contains(hint, "no imagePullSecrets") {
		t.Fatalf("hint = %q", hint)
	}

	// The workload has a secret, but it holds a credential for another registry.
	provider, _ = newRegistryProvider(dockerConfigSecret("tools", "hub-registry", "other.example", "u", "p"))
	provider.setPullSecretIndex([]model.Workload{{
		Namespace:        "tools",
		ImagePullSecrets: []string{"hub-registry"},
		Images:           []model.WorkloadImage{{Image: "hub.example/tools/app:1.0.0"}},
	}})
	auth, hint = provider.registryAuthFor(context.Background(), "hub.example/tools/app:1.0.0")
	if auth != nil || !strings.Contains(hint, "no credential for hub.example") {
		t.Fatalf("auth/hint = %+v/%q", auth, hint)
	}

	// The secret exists but cannot be read: report the API error, not a 403.
	provider, cs := newRegistryProvider()
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), "hub-registry", errors.New("no permission"))
	})
	provider.setPullSecretIndex([]model.Workload{{
		Namespace:        "tools",
		ImagePullSecrets: []string{"hub-registry"},
		Images:           []model.WorkloadImage{{Image: "hub.example/tools/app:1.0.0"}},
	}})
	auth, hint = provider.registryAuthFor(context.Background(), "hub.example/tools/app:1.0.0")
	if auth != nil || !strings.Contains(hint, "cannot read imagePullSecrets") || !strings.Contains(hint, "tools/hub-registry") {
		t.Fatalf("auth/hint = %+v/%q", auth, hint)
	}
}

// --- end to end through the agent transport --------------------------------

func TestAgentTriggerPullSendsRegistryAuth(t *testing.T) {
	agent := &fakeAgent{images: []agentImage{{Name: "hub.example/tools/app:1.0.0", SizeBytes: 100}}}
	server := startFakeAgent(t, agent)
	objects := []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("tools", "app", 1,
			[]corev1.Container{testContainer("app", "hub.example/tools/app:1.0.0", corev1.PullIfNotPresent)},
			nil, "hub-registry"),
		dockerConfigSecret("tools", "hub-registry", "hub.example", "robot", "s3cret"),
	}
	provider := newAgentProvider(t, server, objects)
	provider.Snapshot(context.Background()) // fills the image -> pull secret index

	task, err := provider.TriggerPull(context.Background(), model.PullRequest{
		Image: "hub.example/tools/app:1.0.0", Node: "worker-01", Actor: "admin", Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	waitAgentTask(t, provider, task.ID, model.TaskSucceeded)

	auths := agent.pulledAuths()
	if len(auths) != 1 || auths[0] == nil {
		t.Fatalf("pull credentials = %+v, want one resolved credential", auths)
	}
	if auths[0].Username != "robot" || auths[0].Password != "s3cret" || auths[0].Auth == "" {
		t.Fatalf("credential = %+v", auths[0])
	}
}

func TestAgentTriggerPullWithoutSecretIsAnonymous(t *testing.T) {
	agent := &fakeAgent{images: []agentImage{{Name: "docker.io/library/nginx:1.27", SizeBytes: 100}}}
	server := startFakeAgent(t, agent)
	objects := []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("web", "nginx", 1,
			[]corev1.Container{testContainer("nginx", "docker.io/library/nginx:1.27", corev1.PullIfNotPresent)}, nil),
	}
	provider := newAgentProvider(t, server, objects)
	provider.Snapshot(context.Background())

	task, err := provider.TriggerPull(context.Background(), model.PullRequest{
		Image: "docker.io/library/nginx:1.27", Node: "worker-01", Actor: "admin", Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	waitAgentTask(t, provider, task.ID, model.TaskSucceeded)
	auths := agent.pulledAuths()
	if len(auths) != 1 || auths[0] != nil {
		t.Fatalf("pull credentials = %+v, want none", auths)
	}
}

func TestAgentTriggerPullRegistryAuthFailure(t *testing.T) {
	agent := &fakeAgent{
		images:     []agentImage{{Name: "hub.example/tools/app:1.0.0", SizeBytes: 100}},
		pullStatus: 502,
		pullError:  "failed to fetch anonymous token: 403 Forbidden",
		pullReason: agentReasonRegistryAuth,
	}
	server := startFakeAgent(t, agent)
	objects := []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("tools", "app", 1,
			[]corev1.Container{testContainer("app", "hub.example/tools/app:1.0.0", corev1.PullIfNotPresent)},
			nil, "hub-registry"),
		// Wrong registry in the secret: the hub resolves nothing and the hint must
		// say exactly that instead of blaming the agent.
		dockerConfigSecret("tools", "hub-registry", "other.example", "u", "p"),
	}
	provider := newAgentProvider(t, server, objects)
	provider.Snapshot(context.Background())

	task, err := provider.TriggerPull(context.Background(), model.PullRequest{
		Image: "hub.example/tools/app:1.0.0", Node: "worker-01", Actor: "admin", Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	failed := waitAgentTask(t, provider, task.ID, model.TaskFailed)
	if !strings.Contains(failed.Error, "registry authentication failed") {
		t.Fatalf("task error = %q, want the registry-auth sentinel", failed.Error)
	}
	if !strings.Contains(failed.Error, "no credential for hub.example") {
		t.Fatalf("task error = %q, want the imagePullSecrets hint", failed.Error)
	}
}
