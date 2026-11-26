# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии — [SemVer](https://semver.org/lang/ru/).

## [Unreleased]

### Security

- Уязвимости в графе модулей (Dependabot: 5 алертов; `govulncheck`: 10 достижимых путей)
  закрыты обновлением зависимостей: `google.golang.org/grpc` v1.80.0 → v1.83.2,
  `golang.org/x/net` v0.49.0 → v0.62.0 (HTTP/2 DoS в `net/http`: гонка HPACK-энкодера,
  двойной flow-control refund, раздувание окна, бесконечный цикл на битом
  `SETTINGS_MAX_FRAME_SIZE`), `golang.org/x/text` v0.33.0 → v0.43.0; транзитивно
  обновлены x/sys, x/term, x/oauth2, genproto/rpc. Границы ADR-001 не менялись.
- `go` в `go.mod`: 1.24.0 → 1.26.0 (требование исправленных версий); Dockerfile собирается
  на `golang:1.26-alpine`.
- CI: добавлен шаг `govulncheck` (пин v1.10.0) — регрессии по достижимым уязвимостям
  блокируют сборку.

### Changed

- `Dockerfile`: builder-стадия запускается на нативной архитектуре раннера
  (`--platform=$BUILDPLATFORM`), Go кросс-компилирует под `TARGETARCH`. Сборка образа
  в CI перестала гонять компилятор arm64 под QEMU: multi-arch build на GitHub Actions
  занимает минуты вместо ~15.

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
