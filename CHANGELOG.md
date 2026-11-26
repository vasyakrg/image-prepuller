# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии — [SemVer](https://semver.org/lang/ru/).

## [Unreleased]

## [0.2.0] — Stage 2/3: живой путь от Kubernetes до containerd

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

### Changed

- Hub-хендлеры pull/cancel/workload-pull отдают `502` при недоступном spoke и `409`
  при отмене завершённой задачи вместо общего `400`.
- Скрейп метрик делает ровно один `Provider.Snapshot()` на кластер вместо шести.
- `prepuller_cluster_up` равен 0 для кластеров в состоянии `offline`/`partial`.
- Hub в live-режиме (без `-mock`) поднимает `K8sProvider` вместо `log.Fatalf`;
  kube-конфиг берётся из in-cluster service account или `$KUBECONFIG`/`~/.kube/config`.

### Fixed

- `RemoteProvider.PullCounters()` больше не возвращает `nil` при живом spoke — данные
  `prepuller_pull_total` для удалённых кластеров.

### Removed

- Заглушка `internal/provider/k8s_stub.go` и `Agent.Run`-заглушка.
