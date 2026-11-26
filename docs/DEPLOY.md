# DEPLOY: image-prepuller

Runbook for deploying the hub, the spoke backend and the node-agent from this
repository. The default release runs the **live** `K8sProvider` (client-go) in
the hub and the node-agent HTTP/JSON API v1 on every node. Set `hub.mock=true`
for an in-memory demo without a Kubernetes API.

- [1. What gets deployed](#1-what-gets-deployed)
- [2. Prerequisites](#2-prerequisites)
- [3. Secrets and environment variables](#3-secrets-and-environment-variables)
- [4. Build the image](#4-build-the-image)
- [5. Install with Helm](#5-install-with-helm)
- [6. Verify the deployment](#6-verify-the-deployment)
- [7. Add a spoke cluster](#7-add-a-spoke-cluster)
- [8. Enable the agent DaemonSet](#8-enable-the-agent-daemonset)
- [9. Typical failures](#9-typical-failures)
- [10. Upgrade and rollback](#10-upgrade-and-rollback)
- [11. Uninstall](#11-uninstall)

## 1. What gets deployed

The chart `deploy/helm/image-prepuller` renders:

| Object | Template | Enabled by default |
| --- | --- | --- |
| ServiceAccount (hub/spoke + agent) | `serviceaccount.yaml` | yes |
| ClusterRole + ClusterRoleBinding (read-only Nodes/workloads) | `rbac.yaml` | yes (`rbac.create`) |
| Secret (auth, session secret, cluster token, agent token) | `secret.yaml` | yes |
| Deployment + Service (hub) | `deployment-hub.yaml`, `service-hub.yaml` | yes |
| Ingress (Traefik-compatible) | `ingress.yaml` | no (`ingress.enabled`) |
| Deployment + Service (spoke) | `deployment-spoke.yaml`, `service-spoke.yaml` | no (`spoke.enabled`) |
| DaemonSet (node-agent) | `daemonset-agent.yaml` | yes (`agent.enabled`) |
| NetworkPolicy (agent) | `networkpolicy.yaml` | no (`networkPolicy.enabled`) |

One binary, three roles: `controller -role=hub` (Web UI + cookie auth +
multi-cluster registry + Prometheus), `controller -role=spoke` (headless backend
API with a Bearer token), `agent` (per-node CRI agent). Ports:

| Role | HTTP/API | Metrics | Node agent API |
| --- | --- | --- | --- |
| hub | 8080 | 9090 | — |
| spoke | 8081 | 9091 | — |
| agent | 9443 | — | — |

The image is multi-stage (`golang:1.26-alpine` builder, `distroless/static`
runtime), `CGO_ENABLED=0`, runs as uid/gid 65532 with a read-only root
filesystem. `/healthz` and `/readyz` are served by the controller in both hub
and spoke roles; `/metrics` is served on the metrics port only.

## 2. Prerequisites

- Kubernetes 1.25+ (the chart sets `kubeVersion: ">=1.25.0-0"`).
- Helm 3.8+ (OCI support for chart publishing).
- A container registry reachable from the cluster (Gitea registry, GHCR, Harbor,
  …) and credentials to push to it.
- `kubectl` with access to the target namespace.
- Optional: an ingress controller (Traefik is what the chart is tested with) and
  cert-manager for TLS. The chart renders a plain `networking.k8s.io/v1`
  Ingress, so Traefik, nginx or any other controller works.

The chart has **no external chart dependencies** and does not use `vendor/`.

## 3. Secrets and environment variables

The controller reads configuration from the environment; the chart maps all of
it from a single Secret. Keys in the Secret:

| Secret key | Environment variable | Used by | Notes |
| --- | --- | --- | --- |
| `auth-user` | `PREPULLER_AUTH_USER` | hub | Required. The hub refuses to start without it. |
| `auth-pass` | `PREPULLER_AUTH_PASS` | hub | Required. |
| `session-secret` | `PREPULLER_SESSION_SECRET` | hub | Required for stable sessions. Random per install if empty → users are logged out on restart and multi-replica setups break. |
| — | `PREPULLER_COOKIE_SECURE` | hub | Cookie `Secure` flag; set from `hub.cookieSecure` (default `true`; disable only for plain-HTTP local development). |
| `cluster-token` | `PREPULLER_CLUSTER_TOKEN` | spoke | Bearer token expected by `/internal/v1/*`. Empty disables auth — dev only. |
| `agent-token` | `PREPULLER_AGENT_TOKEN` | agent | Bearer token of the agent HTTP API v1. Empty disables agent auth. |
| `users` | `PREPULLER_USERS` | hub | Optional multi-user list `name:password:role,...` (roles `viewer`/`operator`); multi-user auth works — the hub builds users from this list. |
| — | `PREPULLER_REMOTE_CLUSTERS` | hub | Optional `name=url=token,...` list of extra spokes. |
| — | `PREPULLER_LOGIN_ATTEMPTS_PER_MINUTE` | hub | Optional login rate limit (default 5/min); enforced by the hub. |
| — | `PREPULLER_LOGIN_BLOCK` | hub | Optional block duration after too many attempts (Go duration, e.g. `5m`). |
| — | `PREPULLER_NODE_NAME` | agent | Node name; the DaemonSet sets it from `spec.nodeName`. |
| — | `PREPULLER_CRI_SOCKET` | agent | CRI socket path, default `/run/containerd/containerd.sock`. |

Two ways to provide them:

**A. Let the chart generate them (quick start).** `secret.create=true` (default)
and empty `secret.auth.password`, `secret.sessionSecret`, `secret.clusterToken`
generate random values on install. On `helm upgrade` the chart reuses the values
stored in the existing Secret (`lookup`), so sessions, the spoke token and the
hub/agent token pair stay stable; set `secret.*` explicitly to take over. The
Secret is annotated `helm.sh/resource-policy: keep`, so it also survives
`helm uninstall`. Read them back:

```bash
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.auth-user}' | base64 -d; echo
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.auth-pass}' | base64 -d; echo
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.session-secret}' | base64 -d; echo
```

**B. Bring your own Secret (production).** Create it first and set
`secret.create=false`, `secret.existingSecret=<name>`:

```bash
kubectl -n prepuller create secret generic prepuller-credentials \
  --from-literal=auth-user=admin \
  --from-literal=auth-pass="$(openssl rand -base64 24)" \
  --from-literal=session-secret="$(openssl rand -base64 48)" \
  --from-literal=cluster-token="$(openssl rand -base64 32)"
```

Registry credentials for pulling the image are separate: either create a
`kubernetes.io/dockerconfigjson` Secret and set `imagePullSecrets`, or attach it
to the ServiceAccount (`serviceAccount.annotations` / `kubectl patch sa`).

Secrets are never logged and never returned by the API. The only secret in the
Helm release values is what you pass with `--set`; prefer `--set-file` or an
external secret manager for production.

> **GitOps note.** `helm template` and `--dry-run` cannot read the live Secret,
> so they render fresh random values on every run. Pass `secret.*` explicitly or use
> `secret.existingSecret` when committing rendered manifests.

> **Multi-user auth, rate limit and audit.** `cmd/prepuller/main.go` wires
> `auth.UsersFromEnv()`, `auth.RateLimitConfigFromEnv()` and the JSON auditor,
> so `PREPULLER_USERS`, `PREPULLER_LOGIN_ATTEMPTS_PER_MINUTE` and
> `PREPULLER_LOGIN_BLOCK` work as documented. Set `secret.users` to a
> `name:password:role,...` list (roles `viewer`/`operator`); it is passed as
> `PREPULLER_USERS`. The legacy `auth-user`/`auth-pass` pair is merged in as an
> `operator`. `PREPULLER_COOKIE_SECURE` comes from `hub.cookieSecure` (default
> true — disable only for plain-HTTP local development).

## 4. Build the image

Locally:

```bash
docker build \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t registry.example.com/ops/image-prepuller:dev .
```

In CI, `.gitea/workflows/build-image.yaml` builds `linux/amd64` + `linux/arm64`
with buildx, pushes `:sha` and `:latest`, caches layers in the registry
(`type=registry`), runs a hub smoke test and lints/templates the chart. Required
repository variables: `REGISTRY`, `IMAGE`; optional `CHART_OCI_REPO`,
`REGISTRY_INSECURE`. Required secrets: `REGISTRY_USERNAME`,
`REGISTRY_PASSWORD`.

> `cmd/prepuller/main.go` does not define `version`/`commit` variables yet, so
> the Dockerfile does **not** pass `-X main.version`/`-X main.commit`; VERSION and
> COMMIT are baked as OCI labels. When the integrator adds the variables, add
> `-X main.version=${VERSION} -X main.commit=${COMMIT}` to the `-ldflags` line in
> the Dockerfile.

## 5. Install with Helm

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag="$(git rev-parse --short=12 HEAD)" \
  --set image.pullPolicy=IfNotPresent \
  --set secret.create=true \
  --set hub.clusterName=talos-prod-eu
```

This default install runs the live provider: the hub lists Nodes and workloads
through the in-cluster ServiceAccount and talks to the agent DaemonSet on each
node's InternalIP:9443. For an in-memory demo without Kubernetes access add
`--set hub.mock=true --set agent.enabled=false`.

Production example with an existing Secret, ingress and TLS:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag=sha-1a2b3c4d5e6f \
  --set secret.create=false \
  --set secret.existingSecret=prepuller-credentials \
  --set ingress.enabled=true \
  --set ingress.className=traefik \
  --set 'ingress.hosts[0].host=prepuller.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
  --set 'ingress.tls[0].secretName=prepuller-tls' \
  --set 'ingress.tls[0].hosts[0]=prepuller.example.com'
```

Preview before applying:

```bash
helm template prepuller deploy/helm/image-prepuller --namespace prepuller \
  --set image.tag=dev > /tmp/rendered.yaml
kubectl apply --dry-run=server -f /tmp/rendered.yaml
```

## 6. Verify the deployment

```bash
kubectl -n prepuller rollout status deploy/prepuller-image-prepuller --timeout=120s
kubectl -n prepuller get pods,svc,ingress

# Health endpoints (public, no auth).
kubectl -n prepuller port-forward svc/prepuller-image-prepuller 8080:8080 9090:9090 &
curl -fsS http://127.0.0.1:8080/healthz   # -> ok
curl -fsS http://127.0.0.1:8080/readyz    # -> ok
curl -fsS http://127.0.0.1:9090/metrics | head

# Login and check the API.
curl -fsS -c /tmp/cookies -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<auth-pass>"}'
curl -fsS -b /tmp/cookies http://127.0.0.1:8080/api/v1/clusters
```

The UI is at `http://127.0.0.1:8080/` (or the ingress host). Prometheus should
scrape the pod when `hub.metrics.annotations=true` (default): the pod carries
`prometheus.io/scrape: "true"`, `prometheus.io/port: "9090"`,
`prometheus.io/path: "/metrics"`.

For a spoke release, check its own ports 8081/9091 the same way. The spoke API
is protected by the Bearer token:

```bash
curl -fsS -H "Authorization: Bearer <cluster-token>" \
  http://127.0.0.1:8081/internal/v1/health
```

## 7. Add a spoke cluster

A spoke is a separate release of the same chart with `spoke.enabled=true`.
Deploy it in the cluster it manages (or in a shared cluster), then point the hub
at it via `hub.remoteClusters`:

```bash
# 1. Deploy the spoke in cluster "kz".
helm upgrade --install prepuller-spoke deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace --kube-context kz \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag=sha-1a2b3c4d5e6f \
  --set spoke.enabled=true \
  --set spoke.clusterName=talos-prod-kz \
  --set secret.create=true

# 2. Read the generated token in the spoke cluster.
TOKEN="$(kubectl --context kz -n prepuller get secret prepuller-spoke-image-prepuller-secret \
  -o jsonpath='{.data.cluster-token}' | base64 -d)"

# 3. Expose the spoke to the hub (Ingress or an internal LoadBalancer), then
#    register it on the hub. Use the hub release's hub.remoteClusters value:
helm upgrade prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set "hub.remoteClusters=talos-prod-kz=https://prepuller-spoke.kz.internal=<token>"
```

`PREPULLER_REMOTE_CLUSTERS` uses `name=url=token` entries separated by commas.
The hub keeps the registration order; the first registered cluster is the UI
default. In `hub.mock=true` mode three fake clusters are registered and remote
spokes are ignored.

## 8. The agent DaemonSet

The agent serves the HTTP/JSON wire API v1 on port 9443 (`GET /healthz`,
`GET /agent/v1/snapshot`, `GET /agent/v1/images/{ref}`, `POST /agent/v1/pulls`).
It is enabled by default; the live hub needs it on every node. It runs under a
dedicated ServiceAccount with **no** Kubernetes API permissions and does not
mount a ServiceAccount token.

To tune or disable it:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set agent.enabled=true \
  --set agent.criSocket=/run/containerd/containerd.sock \
  --set networkPolicy.enabled=true

# demo without a container runtime: in-memory agent, no socket needed
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set agent.mock=true
```

The DaemonSet mounts the CRI socket read-only, runs with a read-only root
filesystem and drops all capabilities. It needs access to the socket: by default
it runs as uid 0; on distros where the containerd socket is group-accessible,
set `agent.securityContext.runAsUser=65532` and
`agent.securityContext.runAsNonRoot=true` plus `supplementalGroups` for the
containerd group. The NetworkPolicy allows ingress only from the release
namespace and egress only to DNS and TCP 443.

The DaemonSet runs with `hostNetwork: true`: the hub dials the agent on
`Node.InternalIP:9443`, and the CRI socket is host-local. NetworkPolicy does not
apply to host-network pods — restrict port 9443 with the node firewall / cloud
security groups if the network is not private. On k3s/k3d the CRI socket lives at
`/run/k3s/containerd/containerd.sock`, so pass
`--set agent.criSocket=/run/k3s/containerd/containerd.sock`; the default
`/run/containerd/containerd.sock` is right for kubeadm/Docker/containerd nodes.

The agent API is protected by the `agent-token` Secret key
(`PREPULLER_AGENT_TOKEN`); empty disables auth. The hub must be given the same
token when its K8sProvider client is wired in. Check the agent directly:

```bash
kubectl -n prepuller port-forward ds/prepuller-image-prepuller-agent 9443:9443
curl -fsS http://127.0.0.1:9443/healthz
curl -fsS -H "Authorization: Bearer <agent-token>" \
  http://127.0.0.1:9443/agent/v1/snapshot
```

## 9. Typical failures

| Symptom | Cause | Fix |
| --- | --- | --- |
| `CrashLoopBackOff`, logs `auth config error` | `PREPULLER_AUTH_USER`/`_PASS` empty | Fill `secret.auth.user`/`password` or the existing Secret. |
| `CrashLoopBackOff`, logs `live Kubernetes provider: ...` | The live path cannot build a Kubernetes client (no in-cluster config/RBAC) | Check the pod ServiceAccount and `rbac.create=true`; for a demo set `hub.mock=true`. |
| 403 `forbidden` on nodes/deployments | ClusterRole/RBAC missing or `rbac.create=false` | Keep `rbac.create=true` or bind the hub ServiceAccount to equivalent read-only rules. |
| Nodes show `offline`/`degraded` in the UI | Agent DaemonSet not running, socket not mounted, wrong socket path, or agent token mismatch | Check `kubectl -n <ns> get ds` and agent logs. On k3s/k3d set `agent.criSocket=/run/k3s/containerd/containerd.sock`. Confirm hub and agent share `agent-token` (compare `printenv PREPULLER_AGENT_TOKEN` in both pods); after rotating the Secret, `kubectl -n <ns> rollout restart deploy/<hub>` and the DaemonSet so pods pick up the new value. |
| `ImagePullBackOff` | Wrong `image.repository`/`tag`, or no registry credentials | Check the tag in the registry; add `imagePullSecrets` or attach the pull secret to the ServiceAccount. |
| `CreateContainerConfigError` | Referenced Secret/key missing | `kubectl -n prepuller describe pod ...`; check `secret.existingSecret` and its keys. |
| `read-only file system` errors | The image writes outside `/tmp`; the rootfs is read-only | Expected for hub/spoke (they keep state in memory). For the agent, mount a writable `emptyDir` where it needs scratch space. |
| Pods restart / users logged out after upgrade | `session-secret` regenerated | Use `secret.existingSecret` (or keep the generated Secret — it has `resource-policy: keep`). |
| 401 on the UI API | Session cookie invalid or auth misconfigured | Re-login; check `PREPULLER_SESSION_SECRET` is stable across replicas. |
| Spoke returns 401 | Token mismatch between hub and spoke | Set the same `cluster-token` on both sides. |
| Spoke unreachable, hub shows `offline` | DNS/Ingress/NetworkPolicy blocks the hub→spoke path | `kubectl run curl --rm -it --image=curlimages/curl -- curl -v <spoke-url>/internal/v1/health`; check DNS, Ingress host, TLS. |
| Agent pod stuck `ContainerCreating`, event `hostPath type check failed: ... is not a socket file` | `agent.criSocket` points at a missing path for this distro (k3s keeps it under `/run/k3s`) | Set `agent.criSocket` to the real socket (`docker exec <node> ls -l /run/containerd/containerd.sock /run/k3s/containerd/containerd.sock`). |
| Agent `CrashLoopBackOff`, `agent config error` or CRI dial errors | CRI socket path wrong, not mounted, or containerd rejects the connection | Check `agent.criSocket` and agent logs; for a smoke test set `agent.mock=true` (in-memory images). |
| Hub can reach the agent from a `kubectl exec` probe but nodes still show offline | Old hub pod kept the previous `agent-token` after the Secret changed | `kubectl -n <ns> rollout restart deploy/<hub>` (or redeploy) so the env is re-read. |
| Ingress 404 with Traefik | Wrong `ingressClassName` or host | Set `ingress.className=traefik` and the real host; check `kubectl get ingress`. |
| `helm install` fails: `secret.create=false requires secret.existingSecret` | Guard in `secret.yaml` | Create the Secret or set `secret.create=true`. |

## 10. Upgrade and rollback

```bash
helm upgrade prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set image.tag=sha-<new>

helm history prepuller -n prepuller
helm rollback prepuller <revision> -n prepuller
kubectl -n prepuller rollout status deploy/prepuller-image-prepuller
```

The Secret is kept across upgrades (`helm.sh/resource-policy: keep`), so sessions
and spoke tokens survive an upgrade. A rollback of the release does not roll back
the image tag if you changed it with `--set image.tag`; pass the previous tag
explicitly. The hub holds all state in memory: a rollback restarts pods and
clears in-memory sessions/registry except what is configured via values.

## 11. Uninstall

```bash
helm uninstall prepuller -n prepuller
# The generated Secret is annotated "keep" and stays behind:
kubectl -n prepuller delete secret prepuller-image-prepuller-secret
```

Uninstalling the hub does not touch spoke releases; remove them separately.
## 12. k3d end-to-end smoke test

The Stage 2/3 acceptance scenario from `docs/PLAN.md` §9. Run it before a release when a
local Docker daemon and `k3d` are available. The hub runs the **live** `K8sProvider`;
the agent DaemonSet talks to the node containerd over CRI.

Automated variant — the same steps with assertions, a throw-away cluster and a
cleanup trap:

```bash
bash hack/e2e-k3d.sh              # build image, create cluster, run scenario, delete cluster
KEEP=1 bash hack/e2e-k3d.sh       # keep the cluster for debugging
E2E_SKIP_BUILD=1 bash hack/e2e-k3d.sh   # reuse the already built image
```

The script never touches your default kubeconfig: it writes an isolated one into a temp
dir and refuses to continue if `kubectl` points at anything but `k3d-<cluster>`. It is
deliberately **not** part of GitHub Actions — run it locally before pushing to `main`.
Manual runbook below, in case you need to poke at individual steps:

```bash
# 1. Cluster + local image (no registry push needed).
k3d cluster create prepuller --agents 2
docker build -t image-prepuller:e2e .
k3d image import image-prepuller:e2e -c prepuller

# 2. Install hub + agent DaemonSet. On k3s/k3d the CRI socket lives under /run/k3s.
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set image.repository=image-prepuller --set image.tag=e2e \
  --set image.pullPolicy=Never \
  --set agent.criSocket=/run/k3s/containerd/containerd.sock \
  --set secret.auth.user=admin --set secret.auth.password=admin \
  --set secret.sessionSecret=e2e-session-secret

# 3. Wait for the rollout and port-forward the hub.
kubectl -n prepuller rollout status deploy/prepuller-image-prepuller
kubectl -n prepuller rollout status ds/prepuller-image-prepuller-agent
kubectl -n prepuller port-forward svc/prepuller-image-prepuller 8080:8080 9090:9090 &

# 4. Workload with a mutable tag and Always: two policy violations.
kubectl create deployment api-gateway --image=ghcr.io/acme/api-gateway:latest --replicas=2
curl -s -c /tmp/pc -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin"}'
curl -s -b /tmp/pc 'localhost:8080/api/v1/snapshot?cluster=talos-prod-eu' \
  | jq '.workloads[].images[].policyViolations'

# 5. Nodes and slots come from the agents: agentUp=true, cachedImageCount>0,
#    syncStatus=ok and partial=false once every agent has answered.
curl -s -b /tmp/pc 'localhost:8080/api/v1/snapshot?cluster=talos-prod-eu' \
  | jq '.nodes, .syncStatus, .partial'

# 6. Auto-warm: a missing slot produces an auto pull task. Without registry
#    access the pull ends failed — that is the expected "task failed" signal.
curl -s -b /tmp/pc 'localhost:8080/api/v1/pull-tasks?cluster=talos-prod-eu' \
  | jq '.tasks[] | {image, state}'

# 7. Failure injection: keep the DaemonSet off one node. The hub keeps serving,
#    that node turns offline and the snapshot becomes partial.
kubectl -n prepuller patch ds prepuller-image-prepuller-agent --type=json -p \
  '[{"op":"add","path":"/spec/template/spec/affinity","value":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"NotIn","values":["k3d-prepuller-agent-0"]}]}]}}}}]'
curl -s -b /tmp/pc 'localhost:8080/api/v1/snapshot?cluster=talos-prod-eu' \
  | jq '.syncStatus, .partial, [.nodes[] | {name, agentUp}]'
kubectl -n prepuller patch ds prepuller-image-prepuller-agent --type=json \
  -p '[{"op":"remove","path":"/spec/template/spec/affinity"}]'

# 8. Metrics: Stage 2/3 series and the per-node agent gauge.
curl -fsS http://localhost:9090/metrics | grep -E 'prepuller_(pull_tasks|pull_task_duration|node_ready|node_imagefs|agent_info|cluster_last_sync)'

# 9. Tear down.
k3d cluster delete prepuller
```

> `agent.mock=true` is only for UI demos: the agent then serves in-memory images instead of
> talking to containerd. `hub.mock=true` runs the whole stack on the in-memory provider
> without a Kubernetes API.

> The agent pod uses `hostNetwork: true` (the hub dials `Node.InternalIP:9443`), so the
> NetworkPolicy does not apply to it; restrict port 9443 with the node firewall when needed.

## 13. Released artifacts (v0.2.0)

The image and the Helm chart are published to GHCR by
`.github/workflows/build-image.yaml` on a `v*` tag or a push to `main` — on the
built-in `GITHUB_TOKEN` (`packages: write`), so no extra repository secrets are needed.

| Artifact | Reference for v0.2.0 | Digest |
| --- | --- | --- |
| image | `ghcr.io/vasyakrg/image-prepuller:0.2.0` (multi-arch amd64+arm64; plus `sha-<12>`; `latest` follows `main`) | `sha256:5f5527aa5d070d1889b29894faf604e2347d036522256053237668b1913b7fca` |
| chart | `oci://ghcr.io/vasyakrg/charts/image-prepuller` version `0.2.0` | `sha256:ad84a3f3137cbdfa3e1d97b72f0476e82ad87c4355de56cc9c0d6ed96a27532f` |

Both packages are **public** — pulling an image or chart needs no credentials (verified
anonymously). If you flip them to private in the GitHub package settings, add
`imagePullSecrets` for the pods and `helm registry login ghcr.io` before install.

```bash
helm upgrade --install prepuller oci://ghcr.io/vasyakrg/charts/image-prepuller \
  --version 0.2.0 --namespace prepuller --create-namespace \
  --set secret.auth.user=admin --set secret.auth.password='...' \
  --set secret.sessionSecret="$(openssl rand -hex 32)"
```

> GHCR packages are **private by default**, even for a public repository. To pull without
> credentials, open the package settings on GitHub (`Packages → image-prepuller` and
> `Packages → charts/image-prepuller`) and switch visibility to public — the
> `org.opencontainers.image.source` label added by the workflow links the package to the
> repository. Keep them private and use `imagePullSecrets` / `helm registry login`
> instead.

Release checklist:

1. `bash hack/e2e-k3d.sh` — the k3d acceptance scenario above must pass locally.
2. `Chart.yaml` `version`/`appVersion` match the tag (`v0.2.0` → `0.2.0`); the workflow
   fails the chart job on a mismatch.
3. `git tag v0.2.0 && git push origin v0.2.0` — builds `ghcr.io/…:0.2.0` (amd64+arm64)
   and pushes the chart.
4. Verify: `docker buildx imagetools inspect ghcr.io/vasyakrg/image-prepuller:0.2.0` and
   `helm show chart oci://ghcr.io/vasyakrg/charts/image-prepuller --version 0.2.0`.
