# Image Prepuller

Консоль и API для прогрева image-кэша на worker-нодах Kubernetes: показывает, какие образы
из workload'ов реально лежат на каждой ноде, позволяет дотянуть недостающие, следит за
заполнением ImageFS и отдаёт метрики Prometheus для multi-cluster контура.

> Текущее состояние — **Stage 1 (mock) + contract-v2**: работают Web UI, авторизация,
> multi-cluster реестр, headless spoke API и Prometheus-метрики на in-memory провайдере.
> Контракт Stage 2/3 заморожен (тег `contract-v2`): Provider v2, pull-задачи, отмена,
> counters на spoke, скелет node-agent и K8s-провайдера. Живые CRI/Kubernetes появятся
> в Stage 2–3 (см. [Roadmap](#roadmap)); внешние зависимости и их границы — в
> [ADR-001](docs/adr/0001-dependencies.md).

## Что умеет

- **Workloads view** — Deployment / StatefulSet / DaemonSet, их образы (включая init-контейнеры),
  PullPolicy, покрытие каждого образа по нодам.
- **Матрица image × node** — состояние каждого слота: `present`, `pulling`, `missing`,
  `failed`, `paused_disk`, `offline`; размер, время последней проверки, текст ошибки.
- **Ручной прогрев** — отдельный слот (image + node) или все образы workload'а сразу на все
  онлайн-ноды.
- **Nodes view** — ImageFS (used/capacity), порог паузы авто-прогрева (78%) и порог kubelet GC
  (85%), активные pull'ы, список закэшированных образов.
- **Policy warnings** — `:latest`/отсутствие иммутабельного тега и `imagePullPolicy: Always`
  (ломают offline failover).
- **Activity** — журнал pull-событий с триггером (auto / manual), актором (логин оператора),
  длительностью и ошибкой.
- **Multi-cluster** — hub агрегирует локальный и удалённые spoke-кластеры через единый интерфейс.
- **Prometheus** — отдельный порт `:9090/metrics`, метрики по кластерам, нодам, слотам и pull'ам.

## Архитектура

Сервис — один бинарь с двумя ролями. Hub даёт Web UI и публичный API с cookie-сессией;
spoke — headless-бэкенд рядом с кластером, общается с hub по Bearer-токену.

```mermaid
flowchart TB
    subgraph UserSide["Браузер оператора"]
        UI["Web UI<br/>index.html · app.js<br/>polling 5s"]
    end

    subgraph Prom["Мониторинг"]
        PR["Prometheus<br/>scrape :9090/metrics"]
    end

    subgraph HubBox["Hub controller — --role=hub"]
        direction TB
        AUTH["Session auth<br/>HMAC-SHA256 cookie"]
        API["/api/v1/*<br/>clusters · snapshot · pull · reconcile"]
        REG["Cluster registry<br/>local + remote"]
        HUBMET["Metrics handler<br/>:9090"]
        STATIC["Embedded static UI"]
    end

    subgraph SpokeA["Spoke — --role=spoke (cluster KZ)"]
        SAPI_A["/internal/v1/*<br/>Bearer token"]
        SPROV_A["Provider"]
    end

    subgraph SpokeB["Spoke — --role=spoke (cluster RU)"]
        SAPI_B["/internal/v1/*"]
        SPROV_B["Provider"]
    end

    subgraph LocalK8s["Локальный кластер (hub)"]
        LPROV["Provider<br/>mock → K8s (Stage 3)"]
        NODES["Worker nodes<br/>containerd image store"]
    end

    UI -->|"HTTPS + cookie"| AUTH
    AUTH --> API
    API --> STATIC
    API --> REG
    REG --> LPROV
    REG -->|"RemoteProvider<br/>HTTPS + Bearer"| SAPI_A
    REG -->|"RemoteProvider<br/>HTTPS + Bearer"| SAPI_B
    SAPI_A --> SPROV_A
    SAPI_B --> SPROV_B
    LPROV --> NODES
    SPROV_A -->|"CRI / node agent"| NODES
    PR -->|"HTTP GET"| HUBMET
    HUBMET --> REG
```

Пунктиром мысленно отметьте то, чего пока нет: CRI-вызовы и node-agent — Stage 2/3,
сейчас всё это заменяет `MockProvider`.

### Поток ручного прогрева

```mermaid
sequenceDiagram
    autonumber
    actor OP as Оператор
    participant UI as Web UI
    participant HUB as Hub API
    participant REG as Registry
    participant RP as RemoteProvider
    participant SP as Spoke API
    participant P as Provider

    OP->>UI: «Pull» в ячейке image × node
    UI->>HUB: POST /api/v1/pull {cluster, image, node}
    HUB->>HUB: проверка cookie-сессии
    HUB->>REG: Get(cluster)
    alt кластер локальный
        REG->>P: TriggerPull(image, node, actor)
    else кластер удалённый
        REG->>RP: TriggerPull(...)
        RP->>SP: POST /internal/v1/pull + Bearer
        SP->>P: TriggerPull(image, node, actor)
    end
    P-->>UI: 202 Accepted (pull идёт асинхронно)
    loop каждые 5 секунд
        UI->>HUB: GET /api/v1/snapshot?cluster=…
        HUB->>REG: Snapshot()
        REG-->>UI: состояния слотов, ноды, activity
    end
```

### Жизненный цикл образа на ноде

```mermaid
stateDiagram-v2
    [*] --> missing: образ нужен workload'у
    missing --> pulling: trigger (auto/manual)
    present --> pulling: trigger (manifest обновился)
    pulling --> present: pull успешен
    pulling --> failed: registry/auth/сеть
    failed --> pulling: retry
    missing --> offline: нода NotReady / agent down
    present --> offline: нода NotReady / agent down
    offline --> missing: нода вернулась
    present --> paused_disk: ImageFS ≥ 78% → авто-прогрев стоп
    missing --> paused_disk: ImageFS ≥ 78% → авто-прогрев стоп
    paused_disk --> missing: ImageFS ниже порога
```

Полная схема компонентов, режимы hub/spoke и модель данных — в
[docs/architecture.md](docs/architecture.md).

## Быстрый старт

Нужен Go 1.24+ и доступ к модульному proxy (или прогретый `GOMODCACHE`): с contract-v2
у модуля есть внешние зависимости — grpc, cri-api и client-go (границы и версии —
[ADR-001](docs/adr/0001-dependencies.md)).

```bash
# сборка
go build -o bin/prepuller ./cmd/prepuller

# hub + Web UI на http://localhost:8080 в mock-режиме
export PREPULLER_AUTH_USER=admin
export PREPULLER_AUTH_PASS=admin
export PREPULLER_SESSION_SECRET="$(openssl rand -hex 32)"   # иначе секрет генерится на старте
./bin/prepuller controller -role=hub -mock

# отдельно — headless spoke
export PREPULLER_CLUSTER_TOKEN=spoke-secret
./bin/prepuller controller -role=spoke -mock -http-addr=:8081 -cluster-name=talos-prod-kz
```

Откройте http://localhost:8080, войдите под `PREPULLER_AUTH_USER` / `PREPULLER_AUTH_PASS`.
В mock-режиме поднимаются три кластера: `talos-prod-eu` (hub), `talos-prod-kz`,
`talos-stage-ru`; у `talos-worker-03` ImageFS заполнен на 80% (пауза авто-прогрева),
`talos-worker-04` — NotReady, часть образов в статусе `failed` / `paused_disk`.

### Проверка

```bash
curl -s localhost:8080/healthz                 # ok
curl -s localhost:8080/readyz                  # ok
curl -s localhost:9090/metrics | head
curl -s -c /tmp/pc -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}'
curl -s -b /tmp/pc localhost:8080/api/v1/clusters | jq
curl -s -b /tmp/pc 'localhost:8080/api/v1/snapshot?cluster=talos-prod-kz' | jq '.summary'
```

## Флаги

| Флаг | По умолчанию | Назначение |
| --- | --- | --- |
| `-role` | `hub` | `hub` — Web UI + API + auth; `spoke` — headless backend API |
| `-http-addr` | `:8080` | Адрес HTTP (UI/API) |
| `-metrics-addr` | `:9090` | Адрес Prometheus-метрик (только `GET /metrics`) |
| `-mock` | `false` | In-memory провайдер, Kubernetes не нужен |
| `-cluster-name` | `talos-prod-eu` | Имя локального кластера |

Подкоманда `agent` зарезервирована под Stage 2 (CRI `/run/containerd/containerd.sock`)
и пока завершается с сообщением об ошибке.

## Переменные окружения

| Переменная | Роль | Обязательна | Описание |
| --- | --- | --- | --- |
| `PREPULLER_AUTH_USER` | hub | да | Логин оператора (единственный статический пользователь) |
| `PREPULLER_AUTH_PASS` | hub | да | Пароль оператора |
| `PREPULLER_SESSION_SECRET` | hub | нет | Ключ подписи cookie; без него генерируется случайный (сессии живут до рестарта) |
| `PREPULLER_CLUSTER_TOKEN` | spoke | нет | Bearer-токен для `/internal/v1/*`; пустое значение = API без авторизации |
| `PREPULLER_REMOTE_CLUSTERS` | hub | нет | Дополнительные spoke: `name=url=token,name2=url2=token2` |

## HTTP API

Все защищённые ручки hub требуют cookie `prepuller_session` (HttpOnly, SameSite=Lax,
`Secure` при TLS или `X-Forwarded-Proto: https`). Health-ручки открыты.

| Метод и путь | Доступ | Описание |
| --- | --- | --- |
| `GET /healthz` | открыт | Liveness, `ok` |
| `GET /readyz` | открыт | Readiness, `ok` |
| `POST /api/v1/auth/login` | открыт | `{username,password}` → ставит cookie |
| `POST /api/v1/auth/logout` | открыт | Сбрасывает cookie |
| `GET /api/v1/auth/me` | cookie | `{username}` или 401 |
| `GET /api/v1/clusters` | cookie | `{defaultCluster, apiVersion, clusters[]}` |
| `GET /api/v1/snapshot?cluster=ID` | cookie | Полный снимок кластера (`model.Snapshot`, contract v2) |
| `POST /api/v1/pull` | cookie | `{cluster,image,node}` → 202 `{status,image,node,task}` |
| `POST /api/v1/workloads/pull` | cookie | `{cluster,namespace,kind,name}` → 202 `{triggered}` |
| `POST /api/v1/reconcile` | cookie | `{cluster}` → 200, принудительный синк |
| `GET /api/v1/pull-tasks?cluster=ID` | cookie | `{cluster, tasks[]}` — асинхронные pull-задачи (v2) |
| `POST /api/v1/pulls/{id}/cancel?cluster=ID` | cookie | Отмена queued/running задачи → 202; 404 — нет задачи (v2) |
| `GET /api/v1/nodes/{node}/images?cluster=ID` | cookie | Кэш образов и ImageFS одной ноды (v2) |
| `GET /api/v1/health?cluster=ID` | cookie | `model.Health` провайдера кластера (v2) |

Spoke (только если задан `PREPULLER_CLUSTER_TOKEN` — Bearer):

| Метод и путь | Описание |
| --- | --- |
| `GET /internal/v1/snapshot` | Снимок кластера (contract v2) |
| `POST /internal/v1/pull` | `{apiVersion,image,node,actor,trigger,policy}` → 202 `model.PullTask` |
| `POST /internal/v1/workloads/pull` | `{apiVersion,namespace,kind,name,actor}` → 202 `{triggered}` |
| `POST /internal/v1/reconcile` | `{apiVersion,actor}` → 200 |
| `GET /internal/v1/counters` | `[]model.PullMetricSample` — источник `prepuller_pull_total` для hub |
| `POST /internal/v1/pulls/{id}/cancel` | Отмена задачи → 202; 404 — задача не найдена |

Поле `apiVersion` в запросах обязательно только для новых клиентов: пустое значение и
`v1` принимаются в течение одного релиза (см. [docs/PLAN.md](docs/PLAN.md) §10).

## Метрики

Экспортируются на отдельном порту (`-metrics-addr`, по умолчанию `:9090`) в текстовом
формате Prometheus:

| Метрика | Тип | Метки |
| --- | --- | --- |
| `prepuller_cluster_up` | gauge | `cluster`, `role` |
| `prepuller_image_present` | gauge | `cluster`, `namespace`, `workload_kind`, `workload_name`, `image`, `node` |
| `prepuller_workload_policy_violation` | gauge | те же + `reason` |
| `prepuller_node_agent_up` | gauge | `cluster`, `node` |
| `prepuller_node_disk_paused` | gauge | `cluster`, `node` |
| `prepuller_node_imagefs_used_bytes` | gauge | `cluster`, `node` |
| `prepuller_node_imagefs_capacity_bytes` | gauge | `cluster`, `node` |
| `prepuller_pull_total` | counter | `cluster`, `image`, `node`, `result` |

С contract-v2 `prepuller_pull_total` заполняется и для удалённых кластеров:
`RemoteProvider.PullCounters()` ходит в `GET /internal/v1/counters` своего spoke.

## Структура репозитория

```
cmd/prepuller/          точка входа, controller-подкоманда, runAgent(), запуск/graceful shutdown
internal/agent/         node-agent: Config, DialCRI (grpc + cri-api), заглушка gRPC-сервера
internal/auth/          cookie-сессии и middleware (RequireAuth, RequireToken)
internal/model/         доменные типы и JSON-контракт API v2 (Snapshot, PullTask, ImageRef, …)
internal/provider/      Provider v2, MockProvider, RemoteProvider, K8sProvider (stub), Registry
internal/web/           hub-роутер (server.go) + spoke-роутер (server_spoke.go), embed статики
internal/web/static/    index.html, app.js, styles.css — без сборки и npm
internal/metrics/       ручная генерация Prometheus text exposition
internal/testutil/      фейковый spoke/hub, golden-снапшот v2, контрактные тесты hub↔spoke
docs/                   architecture.md, PLAN.md, ADR-001
.gitea/workflows/       CI: gofmt, vet, test, build linux/amd64+arm64
```

## Разработка

```bash
go test ./...            # auth, web, контрактные тесты hub↔spoke (internal/testutil)
go vet ./...
gofmt -l .
go build -o bin/prepuller ./cmd/prepuller
```

Если `GOCACHE`/`GOMODCACHE` недоступны (песочница, read-only HOME), задайте их внутри
рабочего каталога:

```bash
export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod
go mod download && go test ./...
```

`.gocache/`, `.gopath/`, `.gomodcache/` и `bin/` — в `.gitignore`. Правила, инварианты
и зоны записи для агентов — в [AGENTS.md](AGENTS.md) и [docs/PLAN.md](docs/PLAN.md).

## Roadmap

Подробный план работ с разбивкой на параллельные потоки, зоны записи и порядок мержа —
в [docs/PLAN.md](docs/PLAN.md).

| Этап | Содержание |
| --- | --- |
| Stage 1 (готово) | Mock-провайдер, hub/spoke, Web UI, auth, multi-cluster, метрики |
| contract-v2 (готово) | Provider v2 (PullTask/CancelPull/Health), counters+cancel на spoke, заглушки API, testutil, CI-каркас |
| Stage 2 | `agent`-режим: gRPC-сервер, CRI containerd, реальный pull, инвентаризация образов на ноде |
| Stage 3 | `K8sProvider` (client-go): список workload'ов, состояния слотов с нод, фоновый reconciler и авто-прогрев |
| Далее | RBAC/несколько пользователей, аудит-лог, Helm-чарт, алерты по покрытию и ImageFS |

## Известные ограничения

- Один статический пользователь, без rate-limit и блокировки после неудачных попыток.
- Автоматический prepull-цикл не реализован: `SyncNow` только обновляет `lastSeen`/`lastChecked`
  в mock-провайдере; реальная сверка появится в Stage 3.
- `Missing` в mock почти не возникает: все слоты инициализируются `present`/офлайн/паузой;
  `missing` появляется только после отмены pull-задачи (`POST /api/v1/pulls/{id}/cancel`).
- `tasks` в mock ограничены 200 последними, `activity` — 100; пагинации пока нет.
- Один экземпляр hub: состояние (сессии, реестр) in-memory, реплики не разделяют данные.
- Hub при старте без `-mock` завершается с `live Kubernetes provider is Stage 3` — это
  ожидаемое поведение, а не сломанная конфигурация.
