# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии — [SemVer](https://semver.org/lang/ru/).

### Added

- **Область действия задаётся метками, а не UI-фильтром.** `-workload-selector`
  (`PREPULLER_WORKLOAD_SELECTOR`) уходит в `ListOptions` для Deployment/STS/DS, а
  `-node-selector` (`PREPULLER_NODE_SELECTOR`) — для Nodes: всё, что не попало в
  селектор, не листится, не показывается, не считается в покрытии и не прогревается.
  В чарте — `hub.workloadSelector` (по умолчанию `prepull=true`) и `hub.nodeLabelSelector`;
  у ведомого кластера те же ключи под `spoke.*`. Overlay'и исключают control-plane
  (`!node-role.kubernetes.io/control-plane`) и больше не дают агенту toleration
  control-plane: агенты живут на воркерах, а их отсутствие на control-plane не превращается
  в offline-слоты и вечный `partial`.
- **Live-pull стал асинхронным и отменяемым.** `K8sProvider.TriggerPull` регистрирует
  задачу, запускает загрузку в горутине с собственным `pullTimeout` и сразу возвращает
  `running` — HTTP-запрос больше не висит всю загрузку. `CancelPull` для running-задачи
  отменяет контекст горутины: hub закрывает запрос к агенту, агент отменяет CRI
  `PullImage` на ноде, задача становится `canceled` (не `failed`), счётчик пишется с
  `result=canceled`. Гонка «отмена vs завершение» не перезаписывает состояние.
- **UI: длинная ошибка в Pull Tasks / Activity больше не растягивает таблицу.** Колонка
  Error обрезается многоточием (`title` — полный текст), строка и таблица сохраняют
  высоту.
- **Outbound-режим spoke: управляемый кластер сам ходит к hub'у на его ingress.**
  Прямой режим (hub → spoke по HTTP) требует, чтобы hub дотягивался до кластера; за NAT
  или без ingress в ведомом кластере это невозможно, поэтому появился второй транспорт:
  spoke открывает исходящее соединение к hub'у, пушит состояние
  (`POST /internal/v1/spokes/{cluster}/state`) и long-poll'ит команды
  (`GET /internal/v1/spokes/{cluster}/commands`), а команда — это ровно тот
  HTTP-вызов, который hub сделал бы напрямую, исполняемый собственным handler'ом spoke'а
  (`POST .../commands/{id}/result`). Включение: `-hub-url`/`PREPULLER_HUB_URL` и
  `-hub-sync-interval`/`PREPULLER_HUB_SYNC_INTERVAL` (3 s) на spoke,
  `PREPULLER_MANAGED_CLUSTERS=name=token,...` на hub. Чтения (snapshot, counters, health,
  UI, `/metrics`) обслуживаются из кэша pushed-состояния и не делают round trip;
  молчащий дольше `max(3*syncInterval, 10 s)` spoke = offline
  (`prepuller_cluster_up=0`, `prepuller_snapshot_age_seconds` растёт). State-changing
  вызовы сохраняют прежний маппинг статусов (409/404/502), доставка at-least-once с
  дедупликацией по id команды. Прямой режим (`hub.remoteClusters`) не менялся.
- **Чарт 0.4.0: outbound-режим в values.** `spoke.hub.enabled/url/syncInterval`
  (рендерятся в `-hub-url`/`-hub-sync-interval`), `hub.managedClusters` — строка
  `PREPULLER_MANAGED_CLUSTERS`, она же ключ `managed-clusters` в Secret (hub читает его
  `optional: true`). Рендер падает с понятным сообщением, если `spoke.hub.enabled`
  включён без `spoke.hub.url` или при `spoke.replicaCount > 1` (очередь команд
  single-replica). `managed-cluster.yaml` переведён на outbound, `hub-cluster.yaml`
  регистрирует ведомый кластер в `hub.managedClusters`, оставляя `hub.remoteClusters`
  как прямой вариант.
- **Чарт 0.3.0: компоненты и мониторинг включаются раздельно.** `hub.enabled`
  (agent-only/spoke-only кластер больше не получает Deployment/Service/RBAC hub'а),
  ServiceMonitor для hub и для агента (`serviceMonitor.*`, `agent.serviceMonitor.*`,
  агентский монитор сам ставит `job="prepuller-agent"`), `PrometheusRule` разделён на
  hub/cluster (`prometheusRule.enabled`) и агентский (`agent.prometheusRule.enabled`)
  с собственными labels/annotations/thresholds. Без CRD
  `monitoring.coreos.com/v1` рендер падает с понятным сообщением.
- **Overlay'и на два кластера:** `deploy/values/hub-cluster.yaml` (master: hub + агенты +
  ingress) и `deploy/values/managed-cluster.yaml` (ведомый: spoke + агенты, hub и ingress
  выключены); старый `talos-example.yaml` заменён ими, runbook 14.10 описывает связывание
  через `hub.remoteClusters`.
- `image.tag` резолвится без заглушек: релизный чарт → его версия, чарт из `main` →
  `latest` (с предупреждением в NOTES), явный тег — как есть.
### Changed

- **Чарт 0.6.0: `prepull=true` по умолчанию и control-plane вне области.**
  `hub.workloadSelector`/`spoke.workloadSelector` по умолчанию `prepull=true` —
  поведенческое (ломающее) изменение: workload без метки не виден и не прогревается
  (снять: `--set hub.workloadSelector=`). `hub.nodeLabelSelector`/`spoke.nodeLabelSelector`
  в overlay'ях исключают control-plane; `agent.tolerations` для него убраны.
- **Live-pull больше не мешает outbound-каналу.** Команда `TriggerPull` теперь только
  стартует загрузку, поэтому живой pull на outbound-кластере укладывается в 5-секундный
  бюджет и не отвечает `502`; результат — в `tasks` следующего снапшота.
- **`TriggerWorkloadPull` возвращает число запущенных pull'ов**, а не успешно
  завершённых: загрузки асинхронные, их результат — в `Snapshot.Tasks`.
- **Ломающие изменения values (чарт 0.5.0): мониторинг сгруппирован по компоненту.**
  `serviceMonitor.*` → `hub.serviceMonitor.*`, `prometheusRule.*` →
  `hub.prometheusRule.*` (hub и кластерные серии), `agent.serviceMonitor.*` →
  `agent.metrics.serviceMonitor.*`, `agent.prometheusRule.*` →
  `agent.metrics.prometheusRule.*` (порог `agentNoContact` —
  `agent.metrics.prometheusRule.thresholds.agentNoContact`). Монитор агента и его
  правила требуют `agent.metrics.enabled=true`, монитор hub'а — `hub.enabled=true`:
  иначе рендер падает с понятным сообщением вместо «висящего» объекта. Кластерные
  правила (`hub.prometheusRule`) по-прежнему рендерятся и для release без hub'а,
  если в нём включён spoke (spoke отдаёт те же серии на :9091).
- **Агрегация счётчиков spoke — одна реализация.** `SpokeCounters` собирается в
  `provider.AggregateCounters`, поэтому `GET /internal/v1/counters` и state push
  outbound-канала отдают одинаковые числа (раньше логика жила в `internal/web`).
- **Ломающие изменения values (чарт 0.3.0):** `networkPolicy.enabled` →
  `agent.networkPolicy.enabled`, `ingress.*` → `hub.ingress.*` (ingress относится только
  к hub'у), порог `thresholds.agentNoContact` → `agent.prometheusRule.thresholds.agentNoContact`.
  `hub.tolerations` добавлен в master-overlay (single-node Talos), NetworkPolicy переехал
  в секцию агента.
- **Node-agent отдаёт Prometheus-метрики на отдельном порту `:9444`**
  (`-metrics-addr`/`PREPULLER_AGENT_METRICS_ADDR`, без авторизации; на скрейпе агент не
  ходит в CRI и в сеть, а отдаёт уже собранные данные): `prepuller_agent_up` (dead-man),
  `prepuller_agent_last_contact_timestamp_seconds{endpoint}`,
  `prepuller_agent_requests_total{endpoint,code}` (включая 401 от ротации токена),
  `prepuller_agent_cri_up`, `prepuller_agent_pull_total{result}`,
  `prepuller_agent_pull_in_flight`, ImageFS и `prepuller_agent_images_cached_total`.
  Чарт добавляет Service `agent.metrics.*` (endpoints = IP нод, агент `hostNetwork`) и
  pod-аннотации `prometheus.io/*`.
- **Hub видит причины отказов агентов и не слепнет при partition:**
  `prepuller_node_last_seen_timestamp_seconds`, `prepuller_node_agent_errors_total{reason}`
  (`provider.ClassifyAgentError`, опциональная capability `provider.AgentErrorCounter`,
  интерфейс `Provider` не менялся), `prepuller_snapshot_age_seconds` и
  `prepuller_snapshot_scrape_timeout_total`. Скрейп ограничен 3 s на все кластеры:
  не успевший кластер отдаёт последний успешный снапшот, а не роняет весь таргет
  (раньше partition стоил 5 s × ноды и проваливал скрейп по `scrape_timeout`).
- **Алерты в репозитории:** шаблон `PrometheusRule` (`prometheusRule.enabled`, 13 правил,
  пороги через `prometheusRule.thresholds`) и операторская памятка
  [`docs/alerts.md`](docs/alerts.md) с матрицей «агент / токен / связь».
- CI: полный `helm template` рендерится и с `prometheusRule.enabled=true`.

### Fixed

- **UI больше не падает с «can't access property "map", snap.workloads is null»,
  когда в области действия нет workload'ов.** Контракт обещает массивы, а пустой Go-слайс
  сериализовался как `null` (типичный сценарий — чарт 0.6.0 с `prepull=true` и ни одной
  помеченной workload'ы). `model.Snapshot.Normalize()` заполняет `workloads`/`nodes`/
  `activity`/`tasks` пустыми массивами в провайдерах (K8s, mock, remote/push, offline) и
  на обеих HTTP-границах (hub `/api/v1/snapshot`, `/api/v1/pull-tasks`, spoke
  `/internal/v1/snapshot`); UI дополнительно страхуется `|| []` и вместо пустой таблицы
  подсказывает проверить метку `-workload-selector`.
- Pull в недоступный реестр больше не держит слот image×node до 10 минут. Node-agent
  перед pull делает TCP-пробу реестра (`-registry-probe-timeout`, 5 s) и отдаёт
  `502 registry unreachable` за секунды, а зависший посреди загрузки pull отменяет
  stall watchdog по отсутствию роста ImageFS (`-pull-stall-timeout`, 60 s) с ответом
  `504 pull stalled`. Hub различает это как `provider.ErrAgentPullStalled` → HTTP 504,
  задача сразу переходит в `failed`; `-pull-timeout` (10 m) остаётся страховкой для
  честно долгих загрузок, `WithAgentPullTimeout` позволяет укоротить её на стороне hub.
  Пробу надо выключить (`-registry-probe=false`, Helm `agent.pull.registryProbe=false`),
  если containerd тянет образы через registry mirror.
- e2e: новый шаг 9 — pull в мёртвый реестр (TEST-NET-1 `192.0.2.1:5000`) обязан
  завершиться ошибкой быстрее минуты.

### Security

- Уязвимости в графе модулей (Dependabot: 5 алертов; `govulncheck`: 10 достижимых путей)
  закрыты обновлением зависимостей: `google.golang.org/grpc` v1.80.0 → v1.83.2,
  `golang.org/x/net` v0.49.0 → v0.62.0 (HTTP/2 DoS в `net/http`: гонка HPACK-энкодера,
  двойной flow-control refund, раздувание окна, бесконечный цикл на битом
  `SETTINGS_MAX_FRAME_SIZE`), `golang.org/x/text` v0.33.0 → v0.43.0; транзитивно
  обновлены x/sys, x/term, x/oauth2, genproto/rpc. Границы ADR-001 не менялись.
- `go` в `go.mod`: 1.24.0 → 1.26.11 (требование исправленных версий + патч-уровень
  тулчейна: в стандартной библиотеке Go 1.26.0 числилось 37 уязвимостей, к 1.26.11 они
  закрыты); Dockerfile собирается на `golang:1.26.11-alpine`.
- CI: добавлен шаг `govulncheck` (пин v1.10.0) — регрессии по достижимым уязвимостям
  блокируют сборку.

### Changed

- `Dockerfile`: builder-стадия запускается на нативной архитектуре раннера
  (`--platform=$BUILDPLATFORM`), Go кросс-компилирует под `TARGETARCH`. Сборка образа
  в CI перестала гонять компилятор arm64 под QEMU: multi-arch build на GitHub Actions
  занимает минуты вместо ~15.

### Docs

- **Runbook первого продового кластера (Talos)** — новый §14 в
  `docs/DEPLOY.md`: требования к кластеру, шаблон overlay
  `deploy/values/talos-example.yaml`, DNS/TLS, ожидаемые числа снапшота,
  smoke-проверки pull-пути и особенности окружения. Проверено установкой
  одноразового релиза в живом кластере (агенты по CRI на всех нодах, ~90
  workload'ов, 190 из 375 слотов).
- Раздел «Первый кластер (Talos)» в `README.md` и запись в `AGENTS.md` §4/§8.
- **Публичный репозиторий не хранит данные кластеров:** реальный overlay (домен,
  адреса нод, секрет-стор, `sha-*` тег) остаётся в приватном GitOps-репозитории
  платформы, здесь — только шаблон `deploy/values/talos-example.yaml`.
- **Зафиксирована связка «чарт + образ»**: чарт из `main` передаёт флаги
  `agent.pull.*`, которых нет в релизном образе `0.2.0` — при
  `image.tag=0.2.0` агент падает с `flag provided but not defined:
  -pull-timeout`. В шаблоне overlay и `AGENTS.md` прописано, что `image.tag`
  обязан быть `sha-*` того же коммита (или релизная пара OCI-чарт `0.2.0` +
  образ `0.2.0`).
- На Talos зафиксирована особенность: `imageFSCapacityBytes=0`, поэтому disk-guard
  агента не срабатывает (fail-open) — алертить надо на диск ноды; control-plane
  ноды требуют toleration в DaemonSet.
- CI (`.github/workflows/ci.yaml` и его Gitea-зеркало): `helm lint` и
  `helm template` прогоняются и с `-f deploy/values/talos-example.yaml`, чтобы
  шаблон overlay не разошёлся с чартом.

## [0.2.0] — 2026-11-26 — Stage 2/3: живой путь от Kubernetes до containerd

Контракт заморожен тегом `contract-v2`; изменения ниже — реализация этого контракта.

### Added

- **node-agent** (`prepuller agent`, `internal/agent`): CRI-клиент containerd
  (`k8s.io/cri-api` + grpc), инвентарь образов, ImageFS через CRI + `statfs`, disk-guard
  (порог 0.78), условия runtime, вариант `-mock` без containerd.
- **Agent HTTP/JSON API v1**: `GET /healthz`, `GET /agent/v1/snapshot`,
  `GET /agent/v1/images/{ref}`, `POST /agent/v1/pulls`; опциональный Bearer-токен,
  `409` при `diskPaused`, `502` при ошибке CRI.
- **K8sProvider на client-go** (`internal/provider/k8s_*.go`): list Deployments /
  StatefulSets / DaemonSets (включая init-контейнеры и `imagePullSecrets`), список нод,
  состояния слотов image × node (`present` / `missing` / `paused_disk` / `offline`),
  Kubernetes-дефолты `imagePullPolicy`, policy-warnings, TTL-кэш снапшота 2 s,
  `Health()` через discovery.
- **Reconciler авто-прогрева** (`internal/reconcile`, флаг `-auto-warm`): приоритеты
  (failed > missing, namespace prod > staging > infra), лимиты параллелизма на ноду и
  суммарно с учётом активных задач, дедупликация пар, экспоненциальный backoff,
  счётчики пропусков по причинам в `Result`.
- **UI v2**: вкладка Pull Tasks (журнал задач, отмена, live-длительность), панель ноды
  (кэшированные образы, ImageFS и процент, `diskPaused`, версия агента, условия,
  доступность с клавиатуры), обновление поллингом 5 s.
- **Метрики Stage 2/3**: `prepuller_pull_tasks{state}`, `prepuller_pull_task_duration_seconds`,
  `prepuller_node_ready`, `prepuller_node_imagefs_free_bytes`, `prepuller_images_cached_total`,
  `prepuller_agent_info`, `prepuller_cluster_last_sync_timestamp_seconds`,
  `prepuller_cluster_sync_partial`, `prepuller_build_info`.

- **Надёжный транспорт hub → spoke** (`internal/provider/remote.go`): таймауты, ретраи
  идемпотентных state-changing вызовов с экспоненциальным backoff, ограниченный пул
  соединений, типизированные ошибки (`ErrSpokeUnavailable`/`ErrSpokeUnauthorized` → 502,
  `ErrTaskNotActive` → 409); `GET /internal/v1/health` и `GET /internal/v1/counters`,
  отдающий `SpokeCounters`; отмена завершённой задачи — `409`.
- **Пользователи и роли (RBAC)**: `PREPULLER_USERS` (`name:password:role`), роли `viewer`
  и `operator`, `RequireRole`/`RequireOperator`, rate-limit логина с блокировкой,
  аудит через `auth.Auditor` и `GET /api/v1/audit`; cookie учитывает
  `PREPULLER_COOKIE_SECURE`.
- **Деплой**: `Dockerfile` (distroless, nonroot, amd64+arm64), Helm-чарт
  `deploy/helm/image-prepuller` (hub/spoke/agent DaemonSet, RBAC, NetworkPolicy, ingress),
  workflow сборки образа и публикации чарта в реестр, `docs/DEPLOY.md`.
- **CI на GitHub Actions**: `.github/workflows/ci.yaml` (gofmt/vet/test, сборка под
  amd64+arm64, гейт зависимостей ADR-001, helm lint/template) и
  `.github/workflows/build-image.yaml` (публикация образа и OCI-чарта в GHCR на
  `GITHUB_TOKEN`); PR-Agent ревьюит PR (`.github/workflows/pr-agent.yml`).
- **Локальный e2e**: `hack/e2e-k3d.sh` автоматизирует приёмочный сценарий из
  `docs/DEPLOY.md` §12 (одноразовый k3d-кластер, live-провайдер, агенты по CRI,
  деградация и восстановление, метрики). В CI намеренно не входит — гоняется перед
  пушем в `main`.

### Changed

- Скрейп метрик делает ровно один `Provider.Snapshot()` на кластер вместо шести.
- `prepuller_cluster_up` равен 0 для кластеров в состоянии `offline`/`partial`.
- Hub в live-режиме (без `-mock`) поднимает `K8sProvider` вместо `log.Fatalf`;
  kube-конфиг берётся из in-cluster service account или `$KUBECONFIG`/`~/.kube/config`.

### Fixed

- `RemoteProvider.PullCounters()` больше не возвращает `nil` при живом spoke — данные
  `prepuller_pull_total` для удалённых кластеров.
- Helm: агентский DaemonSet работает с `hostNetwork: true`, иначе hub не может
  достучаться до агента на `Node.InternalIP:9443` (найдено на k3d e2e); для k3s/k3d путь
  CRI-сокета задаётся `agent.criSocket=/run/k3s/containerd/containerd.sock`.
- Helm: сгенерированные секреты переиспользуются на `helm upgrade` (`lookup`). Раньше
  каждый upgrade генерировал новый `agent-token`/`session-secret`: уже запущенные поды
  продолжали работать со старым env, связь hub↔agent рвалась, а сессии сбрасывались.
- Hub-хендлеры pull/cancel/workload-pull отдают `502` при недоступном spoke и `409` при
  отмене завершённой задачи вместо общего `400`.
- Сборка образа и CI: дефолтный `GOPROXY` переключён на `proxy.golang.org` —
  `goproxy.io` не отвечает и вешал `docker build` и job'ы CI.

### Removed

- Заглушка `internal/provider/k8s_stub.go` и `Agent.Run`-заглушка.
