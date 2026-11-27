# Деплой: image-prepuller

Runbook деплоя hub, spoke-бэкенда и node-agent из этого репозитория. Релиз по
умолчанию поднимает **live** `K8sProvider` (client-go) в hub и HTTP/JSON API v1
node-agent на каждой ноде. Для in-memory демо без Kubernetes API задайте
`hub.mock=true`.

- [1. Что деплоится](#1-что-деплоится)
- [2. Предварительные требования](#2-предварительные-требования)
- [3. Секреты и переменные окружения](#3-секреты-и-переменные-окружения)
- [4. Сборка образа](#4-сборка-образа)
- [5. Установка через Helm](#5-установка-через-helm)
- [6. Проверка деплоя](#6-проверка-деплоя)
- [7. Добавление spoke-кластера](#7-добавление-spoke-кластера)
- [8. DaemonSet агента](#8-daemonset-агента)
- [9. Типичные отказы](#9-типичные-отказы)
- [10. Upgrade и откат](#10-upgrade-и-откат)
- [11. Удаление](#11-удаление)
- [12. e2e-смоук на k3d](#12-e2e-смоук-на-k3d)
- [13. Релизные артефакты (v0.2.0)](#13-релизные-артефакты-v020)
- [14. Первый прод-кластер (Talos)](#14-первый-прод-кластер-talos)

## 1. Что деплоится

Чарт `deploy/helm/image-prepuller` рендерит:

| Объект | Шаблон | Включено по умолчанию |
| --- | --- | --- |
| ServiceAccount (hub/spoke) | `serviceaccount.yaml` | да, если `hub.enabled` или `spoke.enabled` |
| ServiceAccount (agent, без прав в API) | `serviceaccount.yaml` | да при `agent.enabled` |
| ClusterRole + ClusterRoleBinding (read-only Nodes/workload'ы) | `rbac.yaml` | да, если `hub.enabled` или `spoke.enabled` (`rbac.create`) |
| Secret (auth, session secret, cluster token, agent token) | `secret.yaml` | да, если включён любой компонент |
| Deployment + Service (hub) | `deployment-hub.yaml`, `service-hub.yaml` | да (`hub.enabled`) |
| HTTP-ручки outbound-канала управляемых кластеров (`/internal/v1/spokes/*`) | маршруты в hub (`server_spoke_hub.go`), отдельных объектов нет | да, если `hub.enabled` и задан `hub.managedClusters` |
| Ingress (совместимый с Traefik) | `ingress.yaml` | нет (`hub.ingress.enabled`; только для hub) |
| Deployment + Service (spoke) | `deployment-spoke.yaml`, `service-spoke.yaml` | нет (`spoke.enabled`) |
| DaemonSet (node-agent) | `daemonset-agent.yaml` | да (`agent.enabled`) |
| Service метрик агента | `service-agent-metrics.yaml` | да при `agent.metrics.enabled` |
| NetworkPolicy (только агент) | `networkpolicy-agent.yaml` | нет (`agent.networkPolicy.enabled`) |
| ServiceMonitor (hub / агент) | `servicemonitor-hub.yaml`, `servicemonitor-agent.yaml` | нет (`serviceMonitor.enabled` / `agent.serviceMonitor.enabled`) |
| PrometheusRule (hub / агент) | `prometheusrule.yaml`, `prometheusrule-agent.yaml` | нет (`prometheusRule.enabled` / `agent.prometheusRule.enabled`) |

`hub.enabled=false` (или `agent.enabled=false`) убирает объекты компонента
целиком — так ставится «только агенты» в кластер без UI и наоборот.

Outbound-канал управляемых кластеров **не добавляет Kubernetes-объектов**: три ручки
`/internal/v1/spokes/{cluster}/{state,commands,commands/{id}/result}` обслуживает тот же
hub Deployment (и тот же ingress, если он включён). Отдельный Service, LoadBalancer или
Ingress для управляемых кластеров не нужен — spoke сам подключается к hub'у.

Один бинарь, три роли: `controller -role=hub` (Web UI + cookie-авторизация +
реестр кластеров + Prometheus), `controller -role=spoke` (headless backend API
с Bearer-токеном), `agent` (CRI-агент на ноде). Порты:

| Роль | HTTP/API | Метрики | Node agent API |
| --- | --- | --- | --- |
| hub | 8080 | 9090 | — |
| spoke | 8081 | 9091 | — |
| agent | 9443 | — | — |

Как этот контур выглядит на уровне двух кластеров — наш софт в каждом, тестовый
Deployment, образ в кэше каждой ноды, матрица `image × node` и поведение при отказе
ноды — на интерактивной схеме
[docs/diagrams/image-prepuller-overview.html](diagrams/image-prepuller-overview.html).

Образ multi-stage (`golang:1.26.11-alpine` builder, `distroless/static` runtime),
`CGO_ENABLED=0`, работает под uid/gid 65532 с root filesystem только для чтения.
`/healthz` и `/readyz` отдаёт controller в обеих ролях — hub и spoke; `/metrics`
слушается только на порту метрик.

## 2. Предварительные требования

- Kubernetes 1.25+ (в чарте `kubeVersion: ">=1.25.0-0"`).
- Helm 3.8+ (поддержка OCI для публикации чарта).
- Container registry, доступный из кластера (Gitea registry, GHCR, Harbor,
  …), и креды для пуша в него.
- `kubectl` с доступом к целевому namespace.
- Опционально: ingress-контроллер (чарт тестируется на Traefik) и
  cert-manager для TLS. Чарт рендерит обычный `networking.k8s.io/v1`
  Ingress, поэтому подойдёт Traefik, nginx или любой другой контроллер.

У чарта **нет внешних зависимостей** и он не использует `vendor/`.

## 3. Секреты и переменные окружения

Controller читает конфигурацию из окружения; чарт целиком берёт её из одного
Secret. Ключи в Secret:

| Ключ Secret | Переменная окружения | Кто использует | Примечания |
| --- | --- | --- | --- |
| `auth-user` | `PREPULLER_AUTH_USER` | hub | Обязателен. Без него hub не стартует. |
| `auth-pass` | `PREPULLER_AUTH_PASS` | hub | Обязателен. |
| `session-secret` | `PREPULLER_SESSION_SECRET` | hub | Обязателен для стабильных сессий. Пусто → случайное значение на каждую установку: после рестарта пользователи разлогинены, multi-replica схемы ломаются. |
| — | `PREPULLER_COOKIE_SECURE` | hub | Флаг `Secure` у cookie; ставится из `hub.cookieSecure` (по умолчанию `true`; выключать только для локальной разработки по plain-HTTP). |
| `cluster-token` | `PREPULLER_CLUSTER_TOKEN` | spoke | Bearer-токен, который ожидают `/internal/v1/*`. В direct-режиме пусто отключает авторизацию (только dev); в outbound-режиме **обязателен** и должен совпадать с токеном этого кластера в `hub.managedClusters` hub'а. |
| `agent-token` | `PREPULLER_AGENT_TOKEN` | agent | Bearer-токен HTTP API v1 агента. Пусто отключает авторизацию агента. |
| `users` | `PREPULLER_USERS` | hub | Опциональный список пользователей `name:password:role,...` (роли `viewer`/`operator`); multi-user авторизация работает — hub собирает пользователей из этого списка. |
| — | `PREPULLER_REMOTE_CLUSTERS` | hub | Direct-режим: опциональный список spoke `name=url=token,...`; hub должен уметь достучаться до URL. |
| `managed-clusters` | `PREPULLER_MANAGED_CLUSTERS` | hub | Outbound-режим: список управляемых кластеров `name=token,...` (токен = `cluster-token` релиза этого spoke). Значение берётся из `hub.managedClusters`; ключ читается с `optional: true`, поэтому старый `existingSecret` без него не ломает hub. Malformed-запись или дубликат имени — hub не стартует. |
| — | `PREPULLER_LOGIN_ATTEMPTS_PER_MINUTE` | hub | Опциональный rate-limit логина (по умолчанию 5/мин); проверяется hub. |
| — | `PREPULLER_LOGIN_BLOCK` | hub | Опциональная длительность блокировки после превышения попыток (Go duration, например `5m`). |
| — | `PREPULLER_NODE_NAME` | agent | Имя ноды; DaemonSet ставит его из `spec.nodeName`. |
| — | `PREPULLER_CRI_SOCKET` | agent | Путь к сокету CRI, по умолчанию `/run/containerd/containerd.sock`. |

Два способа их задать:

**A. Пусть чарт сгенерирует их сам (быстрый старт).** `secret.create=true`
(по умолчанию) и пустые `secret.auth.password`, `secret.sessionSecret`,
`secret.clusterToken` дают случайные значения при установке. На `helm upgrade`
чарт переиспользует значения из существующего Secret (`lookup`), поэтому сессии,
токен spoke и пара токенов hub/agent остаются стабильными; чтобы забрать
управление, задайте `secret.*` явно. На Secret стоит аннотация
`helm.sh/resource-policy: keep`, поэтому он переживает и `helm uninstall`.
Прочитать значения обратно:

```bash
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.auth-user}' | base64 -d; echo
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.auth-pass}' | base64 -d; echo
kubectl -n prepuller get secret prepuller-image-prepuller-secret \
  -o jsonpath='{.data.session-secret}' | base64 -d; echo
```

**B. Свой Secret (production).** Создайте его заранее и задайте
`secret.create=false`, `secret.existingSecret=<name>`:

```bash
kubectl -n prepuller create secret generic prepuller-credentials \
  --from-literal=auth-user=admin \
  --from-literal=auth-pass="$(openssl rand -base64 24)" \
  --from-literal=session-secret="$(openssl rand -base64 48)" \
  --from-literal=cluster-token="$(openssl rand -base64 32)"
```

Креды реестра для pull образа — отдельная история: либо создайте Secret
`kubernetes.io/dockerconfigjson` и задайте `imagePullSecrets`, либо привяжите
его к ServiceAccount (`serviceAccount.annotations` / `kubectl patch sa`).

Пара токенов outbound-канала задаётся на двух сторонах и должна совпадать: на
ведомом кластере это `secret.clusterToken` (`PREPULLER_CLUSTER_TOKEN`), на hub'е —
соответствующая запись `hub.managedClusters="<cluster-name>=<тот же токен>"` (ключ
`managed-clusters` в Secret). В production не храните токены в values: положите
`managed-clusters` в заранее созданный Secret и используйте `secret.existingSecret`.
Флаги `-hub-url` / `-hub-sync-interval` чарт рендерит из `spoke.hub.url` /
`spoke.hub.syncInterval` — в Secret их нет.

Секреты не логируются и никогда не возвращаются API. Единственный секрет в
values релиза Helm — тот, что передан через `--set`; для production
предпочтительнее `--set-file` или внешний secret manager.

> **Замечание про GitOps.** `helm template` и `--dry-run` не могут прочитать
> живой Secret, поэтому на каждом запуске рендерят свежие случайные значения.
> Передавайте `secret.*` явно или используйте `secret.existingSecret`, если
> коммитите отрендеренные манифесты.

> **Multi-user авторизация, rate-limit и аудит.** `cmd/prepuller/main.go`
> подключает `auth.UsersFromEnv()`, `auth.RateLimitConfigFromEnv()` и
> JSON-аудитор, поэтому `PREPULLER_USERS`, `PREPULLER_LOGIN_ATTEMPTS_PER_MINUTE`
> и `PREPULLER_LOGIN_BLOCK` работают как описано. Задайте `secret.users`
> списком `name:password:role,...` (роли `viewer`/`operator`) — он передаётся
> как `PREPULLER_USERS`. Устаревшая пара `auth-user`/`auth-pass` добавляется
> как `operator`. `PREPULLER_COOKIE_SECURE` берётся из `hub.cookieSecure`
> (по умолчанию true — выключать только для локальной разработки по
> plain-HTTP).

## 4. Сборка образа

Локально:

```bash
docker build \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t registry.example.com/ops/image-prepuller:dev .
```

В CI `.gitea/workflows/build-image.yaml` собирает `linux/amd64` + `linux/arm64`
через buildx, пушит `:sha` и `:latest`, кэширует слои в реестре
(`type=registry`), гоняет смоук hub и делает lint/template чарта. Обязательные
переменные репозитория: `REGISTRY`, `IMAGE`; опциональные — `CHART_OCI_REPO`,
`REGISTRY_INSECURE`. Обязательные секреты: `REGISTRY_USERNAME`,
`REGISTRY_PASSWORD`.

> `cmd/prepuller/main.go` пока не определяет переменные `version`/`commit`,
> поэтому Dockerfile **не** передаёт `-X main.version`/`-X main.commit`;
> VERSION и COMMIT запекаются как OCI-метки. Когда интегратор добавит
> переменные, допишите `-X main.version=${VERSION} -X main.commit=${COMMIT}`
> в строку `-ldflags` в Dockerfile.

## 5. Установка через Helm

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag="$(git rev-parse --short=12 HEAD)" \
  --set image.pullPolicy=IfNotPresent \
  --set secret.create=true \
  --set hub.clusterName=talos-prod-eu
```

Эта установка по умолчанию поднимает live-провайдер: hub перечисляет Nodes и
workload'ы через in-cluster ServiceAccount и ходит к DaemonSet агента по
`Node.InternalIP:9443` каждой ноды. Для in-memory демо без доступа к Kubernetes
добавьте `--set hub.mock=true --set agent.enabled=false`.

Production-пример с готовым Secret, ingress и TLS:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag=sha-1a2b3c4d5e6f \
  --set secret.create=false \
  --set secret.existingSecret=prepuller-credentials \
  --set hub.ingress.enabled=true \
  --set hub.ingress.className=traefik \
  --set 'hub.ingress.hosts[0].host=prepuller.example.com' \
  --set 'hub.ingress.hosts[0].paths[0].path=/' \
  --set 'hub.ingress.hosts[0].paths[0].pathType=Prefix' \
  --set 'hub.ingress.tls[0].secretName=prepuller-tls' \
  --set 'hub.ingress.tls[0].hosts[0]=prepuller.example.com'
```

Предпросмотр перед применением:

```bash
helm template prepuller deploy/helm/image-prepuller --namespace prepuller \
  --set image.tag=dev > /tmp/rendered.yaml
kubectl apply --dry-run=server -f /tmp/rendered.yaml
```

## 6. Проверка деплоя

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

UI доступен на `http://127.0.0.1:8080/` (или по хосту ingress'а). Prometheus
скрейпит pod, когда `hub.metrics.annotations=true` (по умолчанию): на поде стоят
`prometheus.io/scrape: "true"`, `prometheus.io/port: "9090"`,
`prometheus.io/path: "/metrics"`.

Для релиза spoke проверяйте его собственные порты 8081/9091 так же. API spoke
защищён Bearer-токеном:

```bash
curl -fsS -H "Authorization: Bearer <cluster-token>" \
  http://127.0.0.1:8081/internal/v1/health
```

## 7. Добавление spoke-кластера

Spoke — отдельный релиз того же чарта с `spoke.enabled=true` в кластере, которым он
управляет. Подключить его к hub'у можно двумя способами, и оба дают в UI кластер с
ролью `remote`:

| Режим | Кто кому ходит | Регистрация на hub'е | Когда использовать |
| --- | --- | --- | --- |
| **outbound** (рекомендуется для NAT/без ingress) | spoke → hub ingress; spoke держит канал и пушит состояние | `hub.managedClusters="name=token"` | hub не может достучаться до кластера: нужен только исходящий HTTPS из управляемого кластера на 443 |
| **direct** | hub → spoke по HTTP | `hub.remoteClusters="name=url=token"` | hub и spoke в одной сети, либо в управляемом кластере опубликован ingress/Internal LoadBalancer |

Готовый шаблон ведомого релиза —
[deploy/values/managed-cluster.yaml](../deploy/values/managed-cluster.yaml): hub и ingress
там выключены, включены spoke и агенты, outbound задан через `spoke.hub.*`.

### 7.1 Outbound: spoke сам подключается к hub

Ничего не публикуйте в управляемом кластере: ingress и внешний Service не нужны.
Обязательны исходящий доступ (DNS + TCP 443) до хоста hub'а и `spoke.replicaCount=1`
(очередь команд hub'а рассчитана на одну реплику spoke).

```bash
# 1. Deploy the spoke in cluster "kz" in outbound mode.
helm upgrade --install prepuller-spoke deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace --kube-context kz \
  --set image.repository=registry.example.com/ops/image-prepuller \
  --set image.tag=sha-1a2b3c4d5e6f \
  --set spoke.enabled=true \
  --set spoke.clusterName=talos-prod-kz \
  --set spoke.hub.enabled=true \
  --set spoke.hub.url=https://prepuller.example.com \
  --set spoke.hub.syncInterval=3s \
  --set spoke.replicaCount=1 \
  --set secret.create=true

# 2. Read the generated cluster token in the managed cluster. It is the token the
#    hub will expect for this cluster; keep it secret.
TOKEN="$(kubectl --context kz -n prepuller get secret prepuller-spoke-image-prepuller-secret \
  -o jsonpath='{.data.cluster-token}' | base64 -d)"

# 3. Register the cluster on the hub. hub.managedClusters format is name=token,
#    comma-separated for several clusters.
helm upgrade prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set "hub.managedClusters=talos-prod-kz=${TOKEN}"
```

В production не передавайте токен через `--set` в истории релиза: положите строку
`managed-clusters` в заранее созданный Secret и задайте
`secret.create=false` + `secret.existingSecret=<name>` (ключ читается с
`optional: true`, так что hub без этого ключа просто не примет outbound-кластеры).

Проверка:

```bash
# hub: кластер зарегистрирован и принял состояние
kubectl -n prepuller logs deploy/prepuller-image-prepuller | grep 'talos-prod-kz'
#   managed cluster "talos-prod-kz" registered for the outbound spoke channel ...
curl -fsS -b /tmp/cookies http://127.0.0.1:8080/api/v1/clusters | jq \
  '.clusters[] | select(.id=="talos-prod-kz")'
#   -> {"role":"remote","endpoint":"outbound",...}

# spoke: канал поднялся
kubectl --context kz -n prepuller logs deploy/prepuller-spoke-image-prepuller-spoke \
  | grep -E 'OUTBOUND MODE|outbound channel'
#   spoke OUTBOUND MODE: dialing hub https://prepuller.example.com as cluster "talos-prod-kz" ...

# hub metrics: кластер онлайн, снапшот не partial и есть отметка последнего синка
curl -fsS http://127.0.0.1:9090/metrics | grep 'prepuller_cluster_up{cluster="talos-prod-kz"'
curl -fsS http://127.0.0.1:9090/metrics | grep 'prepuller_cluster_sync_partial{cluster="talos-prod-kz"'
curl -fsS http://127.0.0.1:9090/metrics | grep 'prepuller_cluster_last_sync_timestamp_seconds{cluster="talos-prod-kz"'
```

Hub сохраняет порядок регистрации; первый зарегистрированный кластер — кластер по
умолчанию в UI. В mock-режиме (`hub.mock=true`) дополнительно регистрируются три
демо-кластера, а outbound-канал продолжает работать: локальную связку удобно проверять
spoke'ом с `-hub-url=http://localhost:8080`.

### 7.2 Direct: hub ходит в spoke по HTTP

Этот вариант требует сетевого маршрута hub → spoke. Разверните spoke (без
`spoke.hub.*`) и опубликуйте его через Ingress или Internal LoadBalancer, затем
зарегистрируйте URL и токен в `hub.remoteClusters`:

```bash
# 1. Deploy the spoke in cluster "kz" (direct mode: no -hub-url).
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
  --set "hub.remoteClusters=talos-prod-kz=https://prepuller-spoke.kz.internal=${TOKEN}"
```

`PREPULLER_REMOTE_CLUSTERS` использует записи `name=url=token`, разделённые
запятыми. В direct-режиме `PREPULLER_CLUSTER_TOKEN` можно оставить пустым (spoke
отключит авторизацию), но публиковать такой spoke вне доверенной сети нельзя.
Одно и то же имя нельзя регистрировать одновременно в `hub.managedClusters` и
`hub.remoteClusters`: в реестре останется только последняя регистрация.

## 8. DaemonSet агента

Агент отдаёт HTTP/JSON wire API v1 на порту 9443 (`GET /healthz`,
`GET /agent/v1/snapshot`, `GET /agent/v1/images/{ref}`, `POST /agent/v1/pulls`).
Включён по умолчанию; live-hub'у он нужен на каждой ноде. Работает под
отдельным ServiceAccount **без** прав в Kubernetes API и не монтирует токен
ServiceAccount.

Чтобы настроить или выключить его:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set agent.enabled=true \
  --set agent.criSocket=/run/containerd/containerd.sock \
  --set agent.networkPolicy.enabled=true

# кластер без UI: только агенты (hub и ingress выключаются явно)
helm upgrade --install prepuller-agents deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  --set hub.enabled=false \
  --set agent.criSocket=/run/containerd/containerd.sock

# demo without a container runtime: in-memory agent, no socket needed
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set agent.mock=true
```

DaemonSet монтирует сокет CRI только для чтения, работает с root filesystem
только для чтения и сбрасывает все capabilities. Сокет должен быть доступен:
по умолчанию контейнер работает под uid 0; в дистрибутивах, где сокет containerd
доступен по группе, задайте `agent.securityContext.runAsUser=65532` и
`agent.securityContext.runAsNonRoot=true`, а также `supplementalGroups` с группой
containerd. NetworkPolicy разрешает ingress только из namespace релиза, а
egress — только к DNS и на TCP 443.

DaemonSet работает с `hostNetwork: true`: hub ходит к агенту на
`Node.InternalIP:9443`, а сокет CRI локальный для хоста. NetworkPolicy не
действует на поды hostNetwork — если сеть не приватная, закройте порт 9443
фаерволом ноды / security groups облака. На k3s/k3d сокет CRI лежит в
`/run/k3s/containerd/containerd.sock`, поэтому передайте
`--set agent.criSocket=/run/k3s/containerd/containerd.sock`; дефолтный
`/run/containerd/containerd.sock` верен для нод kubeadm/Docker/containerd.

### Гигиена pull'ов

Раньше pull в мёртвый реестр держал слот image × node весь
`DefaultAgentPullTimeout` (10 мин) и выглядел в точности как медленная загрузка.
Теперь агент ограничивает его двумя ступенями (значения в `agent.pull`):

| Значение | По умолчанию | Эффект |
| --- | --- | --- |
| `agent.pull.registryProbe` | `true` | TCP-dial реестра перед pull; чёрная дыра падает за секунды с `502 registry unreachable` |
| `agent.pull.registryProbeTimeout` | `5s` | Таймаут пробы |
| `agent.pull.stallWatch` | `true` | Отменять pull, который перестал растить image filesystem (у CRI нет callback'а прогресса) |
| `agent.pull.stallTimeout` | `60s` | Окно без прогресса до отмены pull с `504 pull stalled` |
| `agent.pull.timeout` | `10m` | Страховка для честно долгой загрузки |

Выключайте пробу (`--set agent.pull.registryProbe=false`), когда containerd
тянет через **registry mirror** или **прокси, настроенный в containerd**:
runtime достаёт образ, а прямой dial из агента — нет. Hub сводит оба ответа к
проваленным pull-задачам и отдаёт в UI `502`/`504`, поэтому слот освобождается,
а не висит.

API агента защищено ключом Secret `agent-token` (`PREPULLER_AGENT_TOKEN`);
пусто отключает авторизацию. Клиенту K8sProvider в hub этот же токен нужно
передать при сборке провайдера. Проверка агента напрямую:

```bash
kubectl -n prepuller port-forward ds/prepuller-image-prepuller-agent 9443:9443
curl -fsS http://127.0.0.1:9443/healthz
curl -fsS -H "Authorization: Bearer <agent-token>" \
  http://127.0.0.1:9443/agent/v1/snapshot
```

## 9. Типичные отказы

| Симптом | Причина | Что делать |
| --- | --- | --- |
| `CrashLoopBackOff`, в логах `auth config error` | `PREPULLER_AUTH_USER`/`_PASS` пусты | Заполните `secret.auth.user`/`password` или готовый Secret. |
| `CrashLoopBackOff`, в логах `live Kubernetes provider: ...` | live-путь не может собрать Kubernetes-клиент (нет in-cluster конфига/RBAC) | Проверьте ServiceAccount пода и `rbac.create=true`; для демо задайте `hub.mock=true`. |
| 403 `forbidden` на nodes/deployments | нет ClusterRole/RBAC или `rbac.create=false` | Оставьте `rbac.create=true` или привяжите ServiceAccount hub к эквивалентным read-only правилам. |
| Ноды в UI показываются как `offline`/`degraded` | DaemonSet агента не запущен, сокет не смонтирован, неверный путь к сокету или несовпадение токена агента | Проверьте `kubectl -n <ns> get ds` и логи агента. На k3s/k3d задайте `agent.criSocket=/run/k3s/containerd/containerd.sock`. Убедитесь, что hub и агент используют один `agent-token` (сравните `printenv PREPULLER_AGENT_TOKEN` в обоих подах); после ротации Secret сделайте `kubectl -n <ns> rollout restart deploy/<hub>` и для DaemonSet, чтобы поды подхватили новое значение. |
| `ImagePullBackOff` | Неверные `image.repository`/`tag` или нет кредов реестра | Проверьте тег в реестре; добавьте `imagePullSecrets` или привяжите pull-секрет к ServiceAccount. |
| `CreateContainerConfigError` | Нет упомянутого Secret/ключа | `kubectl -n prepuller describe pod ...`; проверьте `secret.existingSecret` и его ключи. |
| ошибки `read-only file system` | Образ пишет вне `/tmp`; rootfs только для чтения | Ожидаемо для hub/spoke (состояние держат в памяти). Агенту смонтируйте writable `emptyDir` там, где нужен scratch. |
| Поды перезапускаются / пользователей разлогинило после upgrade | `session-secret` перегенерирован | Используйте `secret.existingSecret` (или сохраняйте сгенерированный Secret — у него `resource-policy: keep`). |
| 401 на API UI | Сессионная cookie невалидна или авторизация настроена неверно | Перелогиньтесь; проверьте, что `PREPULLER_SESSION_SECRET` одинаков на всех репликах. |
| Spoke отвечает 401 | Несовпадение токена между hub и spoke | Задайте один и тот же `cluster-token` с обеих сторон. |
| Spoke недоступен, hub показывает `offline` | DNS/Ingress/NetworkPolicy перекрывают путь hub→spoke | `kubectl run curl --rm -it --image=curlimages/curl -- curl -v <spoke-url>/internal/v1/health`; проверьте DNS, хост в Ingress, TLS. |
| Outbound-spoke в цикле получает `401` (в логах `state-push`/`command-poll`), кластер в hub `offline` | Токен не совпал или кластер не зарегистрирован: hub не знает это имя/токен | Сверьте имя из `spoke.clusterName` со списком `hub.managedClusters` и `cluster-token` релиза spoke (`PREPULLER_CLUSTER_TOKEN`); после правки Secret перезапустите поды hub и spoke. |
| Outbound-spoke вообще не доходит до hub (нет ни 401, ни принятого state) | Нет DNS/egress/firewall до ingress'а hub'а, либо внутренний CA не в trust store | Из пода spoke: `curl -v https://<hub-ingress>/healthz`; откройте TCP 443 наружу; для внутреннего CA задайте `SSL_CERT_FILE` или положите CA в trust store образа. |
| `helm template/upgrade` падает: `spoke.hub.enabled=true requires spoke.hub.url` | Guard в `deployment-spoke.yaml`: outbound без адреса hub'а бессмыслен | Задайте `spoke.hub.url` (хост ingress'а мастер-кластера). |
| `helm template/upgrade` падает: `spoke.hub.enabled requires spoke.replicaCount=1` | Guard в `deployment-spoke.yaml`: очередь команд hub'а рассчитана на одну реплику spoke | Оставьте `spoke.replicaCount=1`; для HA нужно менять дизайн канала, а не чарт. |
| Живой Pull на outbound-кластере возвращает `502`, хотя pull на spoke продолжается | Одна попытка команды в outbound-канале ограничена 5 s (как и сетевой таймаут direct-режима), а синхронный pull через node-agent может идти минутами | Это ожидаемо и совпадает с direct-режимом: результат смотрите в следующем снапшоте/`pull-tasks`, задача не потеряна. |
| Hub отвечает `404 unknown managed cluster` на outbound-ручках | Кластер не перечислен в `PREPULLER_MANAGED_CLUSTERS` (или hub запущен без него) | Добавьте `name=token` в `hub.managedClusters` / ключ `managed-clusters` Secret и перезапустите hub. |
| Под агента завис в `ContainerCreating`, событие `hostPath type check failed: ... is not a socket file` | `agent.criSocket` указывает на отсутствующий в этом дистрибутиве путь (k3s держит его в `/run/k3s`) | Задайте реальный сокет в `agent.criSocket` (`docker exec <node> ls -l /run/containerd/containerd.sock /run/k3s/containerd/containerd.sock`). |
| Агент в `CrashLoopBackOff`, `agent config error` или ошибки dial к CRI | Неверный путь к сокету CRI, он не смонтирован или containerd отклоняет соединение | Проверьте `agent.criSocket` и логи агента; для смоука задайте `agent.mock=true` (in-memory образы). |
| Hub достаёт агента из пробы `kubectl exec`, но ноды всё равно offline | Старый под hub сохранил прежний `agent-token` после смены Secret | `kubectl -n <ns> rollout restart deploy/<hub>` (или передеплой), чтобы окружение перечиталось. |
| Агент в `CrashLoopBackOff`, `flag provided but not defined: -pull-timeout` | Чарт из `main` рендерит флаги `agent.pull.*`, которых не знает релизный образ `0.2.0` | Пришпильте `image.tag` к сборке `sha-<commit>` того же коммита ([14.2](#142-привязать-тег-образа-к-коммиту-а-не-к-релизу)) или ставьте релизный OCI-чарт `0.2.0` с образом `0.2.0` |
| Ingress отдаёт 404 на Traefik | Неверный `ingressClassName` или хост | Задайте `hub.ingress.className=traefik` и реальный хост; проверьте `kubectl get ingress`. |
| `helm install` падает: `secret.create=false requires secret.existingSecret` | Guard в `secret.yaml` | Создайте Secret или задайте `secret.create=true`. |

## 10. Upgrade и откат

```bash
helm upgrade prepuller deploy/helm/image-prepuller \
  --namespace prepuller --reuse-values \
  --set image.tag=sha-<new>

helm history prepuller -n prepuller
helm rollback prepuller <revision> -n prepuller
kubectl -n prepuller rollout status deploy/prepuller-image-prepuller
```

Secret переживает upgrade (`helm.sh/resource-policy: keep`), поэтому сессии и
токены spoke не теряются. Откат релиза не откатывает тег образа, если вы меняли
его через `--set image.tag`; передайте предыдущий тег явно. Hub держит всё
состояние в памяти: откат перезапускает поды и очищает in-memory
сессии/реестр, кроме того, что задано через values.

## 11. Удаление

```bash
helm uninstall prepuller -n prepuller
# The generated Secret is annotated "keep" and stays behind:
kubectl -n prepuller delete secret prepuller-image-prepuller-secret
```

Удаление hub не трогает релизы spoke; их удаляют отдельно.

## 12. e2e-смоук на k3d

Сценарий приёмки Stage 2/3 из `docs/PLAN.md` §9. Прогоняйте его перед релизом,
когда есть локальный Docker daemon и `k3d`. Hub работает на **live**
`K8sProvider`; DaemonSet агента общается с containerd ноды по CRI.

Автоматический вариант — те же шаги с проверками, одноразовым кластером и
cleanup-ловушкой:

```bash
bash hack/e2e-k3d.sh              # build image, create cluster, run scenario, delete cluster
KEEP=1 bash hack/e2e-k3d.sh       # keep the cluster for debugging
E2E_SKIP_BUILD=1 bash hack/e2e-k3d.sh   # reuse the already built image
```

Скрипт никогда не трогает ваш kubeconfig по умолчанию: он пишет изолированный
во временный каталог и отказывается продолжать, если `kubectl` смотрит не на
`k3d-<cluster>`. В GitHub Actions его **сознательно** нет — гоняйте локально
перед пушем в `main`. Ручной runbook ниже на случай, если нужно ковыряться в
отдельных шагах:

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

> `agent.mock=true` только для UI-демо: агент тогда отдаёт in-memory образы
> вместо похода в containerd. `hub.mock=true` поднимает весь стек на in-memory
> провайдере без Kubernetes API.

> Под агента работает с `hostNetwork: true` (hub ходит на
> `Node.InternalIP:9443`), поэтому NetworkPolicy к нему не применяется; при
> необходимости закройте порт 9443 фаерволом ноды.

## 13. Релизные артефакты (v0.2.0)

Образ и Helm-чарт публикуются в GHCR workflow
`.github/workflows/build-image.yaml` по тегу `v*` или пушу в `main` — на
встроенном `GITHUB_TOKEN` (`packages: write`), поэтому дополнительных секретов
репозитория не нужно.

| Артефакт | Ссылка для v0.2.0 | Digest |
| --- | --- | --- |
| image | `ghcr.io/vasyakrg/image-prepuller:0.2.0` (multi-arch amd64+arm64; плюс `sha-<12>`; `latest` следует за `main`) | `sha256:5f5527aa5d070d1889b29894faf604e2347d036522256053237668b1913b7fca` |
| chart | `oci://ghcr.io/vasyakrg/charts/image-prepuller` версия `0.2.0` | `sha256:ad84a3f3137cbdfa3e1d97b72f0476e82ad87c4355de56cc9c0d6ed96a27532f` |

Оба пакета **публичные** — pull образа или чарта не требует кредов (проверено
анонимно). Если сделаете их приватными в настройках пакетов GitHub, добавьте
`imagePullSecrets` для подов и `helm registry login ghcr.io` перед установкой.

```bash
helm upgrade --install prepuller oci://ghcr.io/vasyakrg/charts/image-prepuller \
  --version 0.2.0 --namespace prepuller --create-namespace \
  --set secret.auth.user=admin --set secret.auth.password='...' \
  --set secret.sessionSecret="$(openssl rand -hex 32)"
```

> Пакеты GHCR **по умолчанию приватные**, даже для публичного репозитория.
> Чтобы тянуть без кредов, откройте настройки пакета на GitHub
> (`Packages → image-prepuller` и `Packages → charts/image-prepuller`) и
> переключите видимость на public — метка `org.opencontainers.image.source`,
> которую добавляет workflow, связывает пакет с репозиторием. Если оставляете
> приватными, используйте `imagePullSecrets` / `helm registry login`.

Чек-лист релиза:

1. `bash hack/e2e-k3d.sh` — сценарий приёмки на k3d выше должен пройти локально.
2. `Chart.yaml` `version`/`appVersion` совпадают с тегом (`v0.2.0` → `0.2.0`);
   workflow валит job чарта при несовпадении.
3. `git tag v0.2.0 && git push origin v0.2.0` — собирает
   `ghcr.io/…:0.2.0` (amd64+arm64) и пушит чарт.
4. Проверка: `docker buildx imagetools inspect ghcr.io/vasyakrg/image-prepuller:0.2.0`
   и `helm show chart oci://ghcr.io/vasyakrg/charts/image-prepuller --version 0.2.0`.

## 14. Первый прод-кластер (Talos)

Первая раскатка проверена на трёхузловом кластере Talos (Talos v1.13,
Kubernetes v1.36, Cilium, Traefik, cert-manager): ставился одноразовый релиз,
прогонялся и удалялся. Числа в 14.6 взяты из того прогона; они плывут вместе с
workload'ами, так что считайте их ориентировочными диапазонами.

**Этот репозиторий публичный.** Overlay реального кластера (hostname, адреса
нод, secret store, пришпиленные теги) живёт в приватном GitOps-репозитории
платформы, а не здесь. Здесь лежат только шаблоны: [deploy/values/hub-cluster.yaml](../deploy/values/hub-cluster.yaml)
для мастер-кластера (hub + агенты + ingress) и
[deploy/values/managed-cluster.yaml](../deploy/values/managed-cluster.yaml) для ведомого
(spoke + агенты). Скопируйте их в GitOps-репозиторий и заполните реальными
значениями уже там.

### 14.1 Что должен предоставить целевой кластер

| Требование | Примечания |
| --- | --- |
| Kubernetes с CRI v1 | агент общается с containerd по gRPC-сокету CRI |
| сокет containerd на ноде | upstream ```/run/containerd/containerd.sock```; k3s/k3d используют ```/run/k3s/containerd/containerd.sock``` |
| toleration для control-plane taint | без соответствующей записи ```agent.tolerations``` каждый слот control-plane навсегда остаётся ```offline```, а снапшот — постоянно ```partial``` |
| достижимость hub → ```Node.InternalIP:9443``` | агент работает с ```hostNetwork```; hub ходит к нему напрямую, без Service |
| Prometheus скрейпит IP-адреса нод | агент отдаёт ```/metrics``` на ```:9444``` без авторизации (14.8) |
| read-only доступ к Nodes и workload'ам | RBAC даёт чарт; не нужен cluster-admin, PVC или локальный registry |
| ingress + TLS (опционально) | любой Ingress-контроллер плюс cert-manager либо собственная конвенция ```IngressRoute``` кластера |
| реестр, доступный с нод | если containerd тянет через mirror, задайте ```agent.pull.registryProbe=false``` (проба делает dial в upstream напрямую, containerd — нет) |

### 14.2 Привязать тег образа к коммиту, а не к релизу

Чарт передаёт агенту флаги ```agent.pull.*``` (```-pull-timeout```,
```-registry-probe-timeout```, ```-pull-stall-timeout```), которых не знает
релизный **образ 0.2.0**. Смесь чарта из ```main``` с ```image.tag=0.2.0```
роняет агент сразу при старте:

```
flag provided but not defined: -pull-timeout
```

в итоге DaemonSet висит в ```CrashLoopBackOff```, а все ноды показывают
```offline```. Есть две согласованные комбинации:

| Комбинация | Что получаете |
| --- | --- |
| чарт из этого репозитория на коммите + ```image.tag=sha-<same commit>``` | текущий код, гигиена pull'ов включена — **используйте это для первой раскатки** |
| опубликованный OCI-чарт релиза + его образ | согласованная релизная пара; образ из релиза может быть старше кода |

Если ```image.tag``` не задан, чарт подставляет конкретный тег сам, без
незаполненных заглушек:

- релизный чарт (```Chart.version == Chart.appVersion```) → эта версия;
- чарт из ```main``` (версия уже поднята, ```appVersion``` ещё нет) → ```latest```,
  то есть последняя опубликованная сборка; в ```NOTES``` установки будет предупреждение.

Берите тег ```sha-*``` того коммита, который деплоите; ```latest``` — движущаяся
ссылка, для продакшена не годится. Правило согласованности простое: чарт и образ
должны быть из одного билда или из одного релиза.

### 14.3 Namespace и креды

Под агента несовместим с ```restricted```: ```hostNetwork```, сокет
```hostPath``` и uid 0, чтобы его прочитать. Пометьте namespace, чтобы будущее
ужесточение Pod Security по умолчанию не заблокировало DaemonSet:

```bash
kubectl create namespace prepuller
kubectl label namespace prepuller \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/enforce-version=latest
```

Два способа отдать чарту его Secret:

**A. Пусть чарт сгенерирует его сам** (быстро, годится для первой раскатки):
оставьте только ```secret.create=true```; пароль прочитайте потом (раздел 3).

**B. Внешний секрет** (обычная конвенция GitOps): храните запись в secret store
платформы, материализуйте Secret в namespace ```prepuller``` и задайте в overlay
```secret.create=false```, ```secret.existingSecret=<secret-name>```. В Secret
должны быть ```auth-user```, ```auth-pass```, ```session-secret```,
```cluster-token``` и ```agent-token```:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: prepuller-credentials
  namespace: prepuller
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: <platform-secret-store>
  target:
    name: prepuller-credentials
  dataFrom:
    - extract:
        key: <secret-item-name>
```

Держите ```session-secret``` стабильным: им подписываются cookie UI, поэтому
новое значение разлогинит всех пользователей при upgrade.

### 14.4 DNS и TLS

Направьте hostname на ingress-контроллер (A/CNAME-запись; wildcard не
обязателен):

```bash
dig +short <prepuller.example.com>
```

[deploy/values/hub-cluster.yaml](../deploy/values/hub-cluster.yaml) включает Ingress чарта с
```ClusterIssuer``` платформы; ingress-shim cert-manager'а выпускает сертификат
для ```hub.ingress.tls```. Следить:

```bash
kubectl -n prepuller get certificate -w
kubectl -n prepuller get order,challenge
```

Если кластер предпочитает нативный ```IngressRoute```, задайте
```hub.ingress.enabled=false``` и примените два этих объекта:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: prepuller-tls
  namespace: prepuller
spec:
  commonName: <prepuller.example.com>
  dnsNames: [<prepuller.example.com>]
  issuerRef:
    kind: ClusterIssuer
    name: <cluster-issuer>
  secretName: prepuller-tls
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: prepuller
  namespace: prepuller
spec:
  entryPoints: [websecure]
  routes:
    - kind: Rule
      match: Host(```<prepuller.example.com>```)
      services:
        - name: prepuller-image-prepuller
          port: 8080
  tls:
    secretName: prepuller-tls
```

### 14.5 Установка

Сначала предпросмотр — в overlay-шаблоне обычные values, без секретов:

```bash
helm template prepuller deploy/helm/image-prepuller -n prepuller \
  -f deploy/values/hub-cluster.yaml > /tmp/prepuller.yaml
kubectl apply --dry-run=server -f /tmp/prepuller.yaml
```

Затем сам релиз:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  -f deploy/values/hub-cluster.yaml
```

Что шаблон меняет относительно дефолтов чарта (всё описано инлайном):

- ```image.tag``` — сборка ```sha-*``` того же коммита (14.2);
- ```hub.clusterName``` — имя, которое показывает UI, например ```talos-prod```;
- ```hub.autoWarm=false``` — reconciler включается после проверки первого
  снапшота (14.7);
- ```agent.tolerations``` для ```node-role.kubernetes.io/control-plane``` — без
  него ноды control-plane никогда не получат агента, и снапшот останется
  partial;
- ```hub.tolerations``` — та же toleration для hub: если в кластере нет
  untainted worker-нод (небольшой Talos), без неё под hub'а останется Pending;
- ```agent.pull.*``` оставлены на дефолтах;
- ```hub.ingress.*``` — хост, класс, issuer cert-manager;
- ```agent.metrics.enabled=true``` — Service метрик на ```:9444``` (14.8).

Опубликованный OCI-чарт работает с тем же overlay:

```bash
helm upgrade --install prepuller oci://ghcr.io/vasyakrg/charts/image-prepuller \
  --version <version> --namespace prepuller --create-namespace \
  -f deploy/values/hub-cluster.yaml
```

### 14.6 Проверка

```bash
kubectl -n prepuller rollout status deploy/prepuller-image-prepuller --timeout=120s
kubectl -n prepuller rollout status ds/prepuller-image-prepuller-agent --timeout=120s
kubectl -n prepuller get pods -o wide      # hub 1/1 + one agent per node, control-plane included
kubectl -n prepuller logs ds/prepuller-image-prepuller-agent --tail=5
# -> backend=cri:/run/containerd/containerd.sock api=v1 auth=true on every node
```

Затем залогиньтесь и сравните снапшот с ожиданиями самого кластера:

```bash
kubectl -n prepuller port-forward svc/prepuller-image-prepuller 8080:8080 &
curl -fsS http://127.0.0.1:8080/healthz   # ok
curl -fsS -c /tmp/pc -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"<auth-pass>"}'
curl -fsS -b /tmp/pc http://127.0.0.1:8080/api/v1/clusters | jq .
curl -fsS -b /tmp/pc 'http://127.0.0.1:8080/api/v1/snapshot?cluster=<cluster-name>' \
  | jq '{syncStatus, partial, summary, nodes: [.nodes[] | {name, agentUp, cachedImageCount, imageFSUsedBytes, imageFSCapacityBytes}]}'
```

Ориентировочные диапазоны из эталонного прогона (3 ноды, ~90 уникальных
образов):

| Сигнал | Ожидание |
| --- | --- |
| ```clusters[0]``` | ```online=true```, ```readyWorkers=3```, ```totalWorkers=3```, ```totalWorkloads``` ~90, ```coveragePercent``` ~50, ```syncStatus=ok``` |
| ```summary``` | Deployments ~70, StatefulSets ~10, DaemonSets ~10, ~90 уникальных образов, ~190 из ~375 слотов present |
| ```nodes[]``` | ```agentUp=true``` везде; ```cachedImageCount``` различается по нодам (нода control-plane кэширует намного меньше) |
| ```imageFSUsedBytes``` | масштаба GiB и зависит от ноды; ```imageFSCapacityBytes``` может быть ```0``` — см. 14.7 |
| ```syncStatus=ok```, ```partial=false``` | ответили все агенты |

API node-agent доступно на ```Node.InternalIP:9443``` (host network):

```bash
kubectl -n prepuller port-forward ds/prepuller-image-prepuller-agent 9443:9443 &
curl -fsS -H "Authorization: Bearer $(kubectl -n prepuller get secret prepuller-image-prepuller-secret -o jsonpath='{.data.agent-token}' | base64 -d)" \
  http://127.0.0.1:9443/agent/v1/snapshot | jq '{node, imageCount: (.images | length), imageFSUsedBytes}'
```

Смоук pull-пути (достаточно маленького образа; важно поведение по таймаутам):

```bash
# Black-holed target: the preflight dial gives up after 5 s -> 502.
time curl -sS -b /tmp/pc -X POST http://127.0.0.1:8080/api/v1/pull \
  -H 'Content-Type: application/json' \
  -d '{"cluster":"<cluster-name>","image":"192.0.2.1:5000/foo:1","node":"<node>"}'
# -> 502 {"error":"... http 502: registry unreachable: 192.0.2.1:5000: dial tcp ... i/o timeout"}

# Unresolvable host: containerd/probe fails immediately ("no such host").
curl -sS -b /tmp/pc -X POST http://127.0.0.1:8080/api/v1/pull \
  -H 'Content-Type: application/json' \
  -d '{"cluster":"<cluster-name>","image":"registry.invalid/dead:1","node":"<node>"}'

# A real pull through containerd (mirror or direct, whichever the cluster uses).
curl -sS -b /tmp/pc -X POST http://127.0.0.1:8080/api/v1/pull \
  -H 'Content-Type: application/json' \
  -d '{"cluster":"<cluster-name>","image":"docker.io/library/busybox:1.36","node":"<node>"}'
curl -fsS -b /tmp/pc 'http://127.0.0.1:8080/api/v1/pull-tasks?cluster=<cluster-name>' \
  | jq '.tasks[] | {image, state, durationMs}'
```

В live-провайдере pull синхронный через агента, поэтому ответ ```202``` часто уже
несёт терминальное состояние (```state=succeeded```) — в скрипте всё равно
поллите ```/api/v1/pull-tasks```, а не полагайтесь на тело ответа.

### 14.7 Подводные камни кластера

- **Агент на control-plane.** Ноды control-plane помечены taint; DaemonSet
  требует toleration из overlay, иначе каждый слот control-plane навсегда
  ```offline``` и ```partial=true```.
- **```imageFSCapacityBytes=0``` на Talos.** ```ImageFsInfo``` в containerd отдаёт
  только used bytes, поэтому disk-guard (```agent.diskPauseRatio```,
  ```ShouldPause```) никогда не срабатывает: нулевая ёмкость считается
  «не пауза» (fail-open). Used bytes всё равно экспортируются
  (```prepuller_node_imagefs_used_bytes```) — алертите на диск **ноды**
  (```node_filesystem_avail_bytes```), а не полагайтесь на паузы pull'ов.
  Stall watchdog это не затрагивает: он смотрит рост того же значения used
  bytes.
- **Порты 9443 и 9444 на каждой ноде.** Агент использует ```hostNetwork```, а
  NetworkPolicy не может огородить host-network под; если сеть нод не приватная,
  держите порты за фаерволом ноды. ```9443``` требует Bearer-токен
  ```agent-token```, ```9444``` — только счётчики и без авторизации (14.8).
- **Раскатки обновлений.** ```maxUnavailable: 10%``` на маленьком кластере
  округляется вниз до 1, поэтому upgrade агента перезапускает агент по одной
  ноде, и снапшот ненадолго становится ```partial``` — это ожидаемо.
- **Registry mirror'ы.** Если containerd тянет ```docker.io``` через mirror'ы, а
  прямой dial с ноды заблокирован, задайте
  ```agent.pull.registryProbe=false``` (проба делает dial в реестр напрямую,
  containerd — нет).
- **Авто-прогрев.** В overlay ```hub.autoWarm=false```. Включайте только после
  того, как снапшот выглядит правильно: на эталонном кластере при покрытии ~50%
  первая сверка поставила ~185 pull'ов, ограниченных дефолтами reconciler'а
  (2 в полёте на ноду, 4 всего, backoff 30 s -> 15 min). Смотрите
  ```prepuller_pull_total``` и ```activePulls```, прежде чем оставлять
  включённым.
- **Одна реплика hub.** Сессии и список задач живут в памяти; нет PVC и нет
  leader election. Держите ```hub.replicaCount=1``` (и тот же
  ```session-secret```, если когда-нибудь поднимете выше).

### 14.8 Метрики и алерты

Два эндпоинта, оба в сети нод:

| Эндпоинт | Порт | Авторизация | Что отвечает |
| --- | --- | --- | --- |
| hub ```/metrics``` | 9090 | нет (внутри кластера) | снапшот кластера, слоты, pull-задачи, причины ошибок агента; один снапшот на кластер за скрейп, ограничен 3 s |
| node-agent ```/metrics``` | 9444 | нет | локальное здоровье агента: счётчики запросов по эндпоинту/статусу, последний контакт по эндпоинту, статус CRI, счётчики pull'ов, ImageFS |

Чарт рендерит ClusterIP Service для порта метрик агента при
```agent.metrics.enabled=true```; поскольку агент работает с ```hostNetwork```,
endpoints Service — это IP-адреса нод (один endpoint на ноду). ServiceMonitor'ы
чарт умеет создавать сам, отдельные флаги для hub и агента:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller -f deploy/values/hub-cluster.yaml \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=kube-prometheus-stack \
  --set agent.serviceMonitor.enabled=true \
  --set agent.serviceMonitor.labels.release=kube-prometheus-stack
```

- ```serviceMonitor.enabled``` — ServiceMonitor на hub Service, порт ```metrics```
  (9090), ```scrapeTimeout``` по умолчанию 15s;
- ```agent.serviceMonitor.enabled``` — ServiceMonitor на ```<fullname>-agent-metrics```,
  порт ```metrics``` (9444), и он принудительно ставит ```job="prepuller-agent"```,
  чтобы правило ```PrepullerAgentDown``` работало без правки scrape-конфига.

Оба флага требуют CRD ```monitoring.coreos.com/v1```: без него рендер падает с
сообщением, что именно выключить. Альтернатива без оператора — аннотации
```prometheus.io/scrape|port|path``` (```hub.metrics.annotations```,
```agent.metrics.annotations```).

Держать ```scrapeTimeout``` hub'а на 15 s важно: при 10 s hub может не
уложиться в собственный дедлайн, пока ждёт мёртвого агента, и вся цель выпадет
ровно во время инцидента (тот же порог в ```deploy/values/hub-cluster.yaml```).

Правила алертов разделены на два объекта, чтобы включать их независимо:
```prometheusRule.enabled``` — hub/cluster-серии, ```agent.prometheusRule.enabled```
— ```prepuller_agent_*``` с ```:9444```. Матрица триангуляции «агент, токен или связь?»
и все пороги — в [docs/alerts.md](alerts.md).

### 14.9 Upgrade, откат, удаление

```bash
# same overlay, new sha tag
helm upgrade prepuller deploy/helm/image-prepuller -n prepuller \
  -f deploy/values/hub-cluster.yaml --set image.tag=sha-<new-commit>

helm history prepuller -n prepuller
helm rollback prepuller <revision> -n prepuller   # then re-pin image.tag explicitly

helm uninstall prepuller -n prepuller
kubectl delete secret prepuller-image-prepuller-secret -n prepuller   # kept by resource-policy
kubectl delete namespace prepuller
```

Удаление namespace уносит также Ingress, Certificate и DaemonSet; image-кэш
containerd на нодах при uninstall **не** трогается. Если позже релизом владеет
Flux, удалите HelmRelease и дайте Flux сделать prune.

### 14.10 Второй, ведомый кластер

Ведомый кластер получает свой релиз того же чарта: без hub'а и без ingress, с
spoke и агентами. Рекомендуемый режим для отдельного кластера — **outbound**: spoke сам подключается к
ingress'у мастер-кластера и держит канал (push состояния + long-poll команд),
поэтому входящий маршрут в ведомый кластер не нужен, только egress на 443.
Шаблон — [deploy/values/managed-cluster.yaml](../deploy/values/managed-cluster.yaml).

Что важно в этом overlay:

- `hub.enabled=false` — никакого UI, API и ingress в ведомом кластере, и
  hub-RBAC тоже не рендерится;
- `spoke.enabled=true` и `spoke.clusterName` — имя кластера в UI и в метриках;
  оно же идёт в `hub.managedClusters` мастер-кластера;
- `spoke.hub.enabled=true`, `spoke.hub.url=https://prepuller.example.com` (хост
  ingress'а мастер-кластера) и `spoke.hub.syncInterval=3s` — рендерятся во флаги
  `-hub-url` / `-hub-sync-interval`;
- `spoke.replicaCount=1` — обязательно: очередь команд hub'а рассчитана на одну
  реплику spoke, при `>1` чарт падает на рендере;
- `agent.*` — тот же DaemonSet, что и в мастер-кластере (сокет CRI, toleration
  control-plane, метрики на :9444).

Порядок раскатки:

1. **Egress.** Разрешите из ведомого кластера DNS и TCP 443 до хоста ingress'а
   мастер-кластера. Если у hub'а внутренний CA, положите его в trust store образа
   или задайте `SSL_CERT_FILE=/path/to/ca.pem` (Go учитывает эту переменную);
   отдельного флага для CA нет.
2. **Установите ведомый** (outbound уже включён в overlay):
   ```bash
   helm upgrade --install prepuller deploy/helm/image-prepuller \
     --namespace prepuller --create-namespace --kube-context <managed-context> \
     -f deploy/values/managed-cluster.yaml \
     --set spoke.clusterName=talos-stage \
     --set spoke.hub.url=https://prepuller.example.com
   ```
3. **Прочитайте cluster-token** ведомого релиза (или задайте его заранее через
   `secret.clusterToken`, чтобы GitOps не читал Secret обратно):
   ```bash
   TOKEN="$(kubectl --context <managed-context> -n prepuller get secret \
     prepuller-image-prepuller-secret -o jsonpath='{.data.cluster-token}' | base64 -d)"
   ```
4. **Зарегистрируйте ведомый на hub'е** в `hub.managedClusters` (формат
   `name=token`; в `deploy/values/hub-cluster.yaml` уже стоит placeholder):
   ```bash
   helm upgrade prepuller deploy/helm/image-prepuller --namespace prepuller \
     -f deploy/values/hub-cluster.yaml --reuse-values \
     --set "hub.managedClusters=talos-stage=${TOKEN}"
   ```
   В production положите эту строку в Secret под ключом `managed-clusters` и
   используйте `secret.existingSecret` — токен не попадёт в историю Helm.
5. **Проверьте** канал (см. чеклист ниже).

Проверка и диагностика:

| Что проверить | Команда / признак | Вывод |
| --- | --- | --- |
| Канал поднялся на spoke | `kubectl --context <managed-context> -n prepuller logs deploy/prepuller-image-prepuller-spoke \| grep OUTBOUND` | `spoke OUTBOUND MODE: dialing hub ...`; повторяющиеся `state-push`/`command-poll` ошибки — нет связи |
| Hub зарегистрировал кластер | `kubectl -n prepuller logs deploy/prepuller-image-prepuller \| grep talos-stage` | `managed cluster "talos-stage" registered for the outbound spoke channel` |
| Кластер виден в API | `curl -fsS -b /tmp/cookies http://127.0.0.1:8080/api/v1/clusters \| jq` | `role=remote`, `endpoint=outbound` |
| Кластер онлайн и состояние свежее | `/metrics`: `prepuller_cluster_up{cluster="talos-stage"}`, `prepuller_cluster_sync_partial{cluster="talos-stage"}`, `prepuller_snapshot_age_seconds{cluster="talos-stage"}` | `1`, `0` и возраст порядка `syncInterval`; если `cluster_up=0` — смотрите логи spoke и hub, а также health кластера (`/api/v1/health`) |
| 401 в логах spoke | `grep -E '401\|invalid cluster token'` | hub не знает этот кластер или токен не совпал: сверьте имя в `hub.managedClusters` и `cluster-token` релиза |
| Состояния нет вообще | нет ни 401, ни принятого state | DNS/egress/firewall: `curl -v https://prepuller.example.com/healthz` из пода spoke |

Если hub не может достучаться до ведомого (общая сеть или опубликованный spoke),
используйте direct-вариант: уберите `spoke.hub.*`, опубликуйте spoke через Internal
LoadBalancer/Ingress и зарегистрируйте его в `hub.remoteClusters` строкой
`talos-stage=<url>=<token>` ([§7.2](#72-direct-hub-ходит-в-spoke-по-http)).

В UI ведомый появится вторым кластером со своей матрицей `image × node`; если он
недоступен, снапшот помечается `offline`/`partial`, а не роняет мастер-кластер.
