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
| ServiceAccount (hub/spoke + agent) | `serviceaccount.yaml` | да |
| ClusterRole + ClusterRoleBinding (read-only Nodes/workload'ы) | `rbac.yaml` | да (`rbac.create`) |
| Secret (auth, session secret, cluster token, agent token) | `secret.yaml` | да |
| Deployment + Service (hub) | `deployment-hub.yaml`, `service-hub.yaml` | да |
| Ingress (совместимый с Traefik) | `ingress.yaml` | нет (`ingress.enabled`) |
| Deployment + Service (spoke) | `deployment-spoke.yaml`, `service-spoke.yaml` | нет (`spoke.enabled`) |
| DaemonSet (node-agent) | `daemonset-agent.yaml` | да (`agent.enabled`) |
| NetworkPolicy (agent) | `networkpolicy.yaml` | нет (`networkPolicy.enabled`) |

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
| `cluster-token` | `PREPULLER_CLUSTER_TOKEN` | spoke | Bearer-токен, который ожидают `/internal/v1/*`. Пусто отключает авторизацию — только для dev. |
| `agent-token` | `PREPULLER_AGENT_TOKEN` | agent | Bearer-токен HTTP API v1 агента. Пусто отключает авторизацию агента. |
| `users` | `PREPULLER_USERS` | hub | Опциональный список пользователей `name:password:role,...` (роли `viewer`/`operator`); multi-user авторизация работает — hub собирает пользователей из этого списка. |
| — | `PREPULLER_REMOTE_CLUSTERS` | hub | Опциональный список дополнительных spoke `name=url=token,...`. |
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
  --set ingress.enabled=true \
  --set ingress.className=traefik \
  --set 'ingress.hosts[0].host=prepuller.example.com' \
  --set 'ingress.hosts[0].paths[0].path=/' \
  --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
  --set 'ingress.tls[0].secretName=prepuller-tls' \
  --set 'ingress.tls[0].hosts[0]=prepuller.example.com'
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

Spoke — отдельный релиз того же чарта с `spoke.enabled=true`. Разверните его в
кластере, которым он управляет (или в общем кластере), затем направьте на него
hub через `hub.remoteClusters`:

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

`PREPULLER_REMOTE_CLUSTERS` использует записи `name=url=token`, разделённые
запятыми. Hub сохраняет порядок регистрации; первый зарегистрированный кластер —
кластер по умолчанию в UI. В режиме `hub.mock=true` регистрируются три фейковых
кластера, а удалённые spoke игнорируются.

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
  --set networkPolicy.enabled=true

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
| Под агента завис в `ContainerCreating`, событие `hostPath type check failed: ... is not a socket file` | `agent.criSocket` указывает на отсутствующий в этом дистрибутиве путь (k3s держит его в `/run/k3s`) | Задайте реальный сокет в `agent.criSocket` (`docker exec <node> ls -l /run/containerd/containerd.sock /run/k3s/containerd/containerd.sock`). |
| Агент в `CrashLoopBackOff`, `agent config error` или ошибки dial к CRI | Неверный путь к сокету CRI, он не смонтирован или containerd отклоняет соединение | Проверьте `agent.criSocket` и логи агента; для смоука задайте `agent.mock=true` (in-memory образы). |
| Hub достаёт агента из пробы `kubectl exec`, но ноды всё равно offline | Старый под hub сохранил прежний `agent-token` после смены Secret | `kubectl -n <ns> rollout restart deploy/<hub>` (или передеплой), чтобы окружение перечиталось. |
| Агент в `CrashLoopBackOff`, `flag provided but not defined: -pull-timeout` | Чарт из `main` рендерит флаги `agent.pull.*`, которых не знает релизный образ `0.2.0` | Пришпильте `image.tag` к сборке `sha-<commit>` того же коммита ([14.2](#142-привязать-тег-образа-к-коммиту-а-не-к-релизу)) или ставьте релизный OCI-чарт `0.2.0` с образом `0.2.0` |
| Ingress отдаёт 404 на Traefik | Неверный `ingressClassName` или хост | Задайте `ingress.className=traefik` и реальный хост; проверьте `kubectl get ingress`. |
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
платформы, а не здесь. Здесь лежит только шаблон
[deploy/values/talos-example.yaml](../deploy/values/talos-example.yaml);
скопируйте его в GitOps-репозиторий и заполните реальными значениями уже там.

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
| опубликованный OCI-чарт ```0.2.0``` + образ ```0.2.0``` | согласованная релизная пара, но без preflight-пробы и stall watchdog |

Берите тег ```sha-*``` того коммита, который деплоите; никогда не откатывайтесь
на ```latest```.

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

[deploy/values/talos-example.yaml](../deploy/values/talos-example.yaml) включает Ingress чарта с
```ClusterIssuer``` платформы; ingress-shim cert-manager'а выпускает сертификат
для ```ingress.tls```. Следить:

```bash
kubectl -n prepuller get certificate -w
kubectl -n prepuller get order,challenge
```

Если кластер предпочитает нативный ```IngressRoute```, задайте
```ingress.enabled=false``` и примените два этих объекта:

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
  -f deploy/values/talos-example.yaml > /tmp/prepuller.yaml
kubectl apply --dry-run=server -f /tmp/prepuller.yaml
```

Затем сам релиз:

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --namespace prepuller --create-namespace \
  -f deploy/values/talos-example.yaml
```

Что шаблон меняет относительно дефолтов чарта (всё описано инлайном):

- ```image.tag``` — сборка ```sha-*``` того же коммита (14.2);
- ```hub.clusterName``` — имя, которое показывает UI, например ```talos-prod```;
- ```hub.autoWarm=false``` — reconciler включается после проверки первого
  снапшота (14.7);
- ```agent.tolerations``` для ```node-role.kubernetes.io/control-plane``` — без
  него ноды control-plane никогда не получат агента, и снапшот останется
  partial;
- ```agent.pull.*``` оставлены на дефолтах;
- ```ingress.*``` — хост, класс, issuer cert-manager;
- ```agent.metrics.enabled=true``` — Service метрик на ```:9444``` (14.8).

Опубликованный OCI-чарт работает с тем же overlay:

```bash
helm upgrade --install prepuller oci://ghcr.io/vasyakrg/charts/image-prepuller \
  --version <version> --namespace prepuller --create-namespace \
  -f deploy/values/talos-example.yaml
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
endpoints Service — это IP-адреса нод, поэтому ServiceMonitor (или обычные
аннотации ```prometheus.io/*```, которые чарт тоже ставит) работает без
дополнительной обвязки:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: prepuller
  namespace: monitoring
spec:
  namespaceSelector:
    matchNames: [prepuller]
  selector:
    matchLabels:
      app.kubernetes.io/name: image-prepuller
      app.kubernetes.io/instance: prepuller
  endpoints:
    - port: metrics          # hub, 9090
      path: /metrics
      interval: 30s
      scrapeTimeout: 15s     # the hub bounds each snapshot at 3 s, keep headroom
```

Держать ```scrapeTimeout``` на 15 s важно: при дефолтных 10 s hub может не
уложиться в собственный дедлайн, пока ждёт мёртвого агента, и вся цель выпадет
ровно во время инцидента.

Правила алертов для этих серий лежат в этом репозитории шаблоном
```PrometheusRule``` (```prometheusRule.enabled```) и разобраны вместе с
матрицей триангуляции «агент, токен или связь?» в
[docs/alerts.md](alerts.md).

### 14.9 Upgrade, откат, удаление

```bash
# same overlay, new sha tag
helm upgrade prepuller deploy/helm/image-prepuller -n prepuller \
  -f deploy/values/talos-example.yaml --set image.tag=sha-<new-commit>

helm history prepuller -n prepuller
helm rollback prepuller <revision> -n prepuller   # then re-pin image.tag explicitly

helm uninstall prepuller -n prepuller
kubectl delete secret prepuller-image-prepuller-secret -n prepuller   # kept by resource-policy
kubectl delete namespace prepuller
```

Удаление namespace уносит также Ingress, Certificate и DaemonSet; image-кэш
containerd на нодах при uninstall **не** трогается. Если позже релизом владеет
Flux, удалите HelmRelease и дайте Flux сделать prune.
