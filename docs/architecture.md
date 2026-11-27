# Архитектура Image Prepuller

Документ описывает текущую (Stage 1, mock) архитектуру и заложенные точки расширения.
Все схемы — Mermaid, рендерятся в GitHub/Gitea/GitLab и Obsidian.

## 1. Роли и топология

Один бинарь `prepuller` с двумя ролями:

| | hub | spoke |
| --- | --- | --- |
| Флаг | `-role=hub` (по умолчанию) | `-role=spoke` |
| Web UI | да (embed static) | нет |
| Авторизация | cookie-сессия оператора | Bearer-токен кластера |
| Публичный API | `/api/v1/*` | — |
| Внутренний API | — | `/internal/v1/*` |
| Кластеры | локальный + удалённые через `RemoteProvider` | ровно один локальный |
| Метрики | `:9090/metrics` по всем кластерам | `:9090/metrics` по своему |
| Требует K8s в Stage 1 | нет (`-mock`) | нет (`-mock`) |

```mermaid
flowchart LR
    subgraph Clients["Клиенты"]
        B["Браузер оператора"]
        PM["Prometheus / Alertmanager"]
    end

    subgraph Hub["Hub (master cluster)"]
        direction TB
        WEB["web.Server<br/>embed static + /api/v1"]
        AM["auth.Manager<br/>cookie HMAC"]
        RG["provider.Registry<br/>ordered map"]
        MH["metrics.Handler"]
    end

    subgraph Spoke1["Spoke A (регион KZ)"]
        SW1["web.NewSpokeServer<br/>/internal/v1"]
        RP1["provider.RemoteProvider"]
    end

    subgraph Spoke2["Spoke B (регион RU)"]
        SW2["web.NewSpokeServer<br/>/internal/v1"]
    end

    subgraph Local["Локальный кластер hub"]
        MP["MockProvider<br/>→ K8sProvider (Stage 3)"]
        AN["node-agent<br/>(Stage 2)"]
    end

    B -->|"HTTP + cookie"| WEB
    WEB --> AM
    WEB --> RG
    RG --> MP
    RG -->|"HTTP + Bearer"| RP1
    RP1 -->|"GET snapshot / POST pull"| SW1
    SW1 --> AN
    RG -->|"HTTP + Bearer"| SW2
    PM -->|"scrape :9090"| MH
    MH --> RG
```

Интерактивная схема продукта — два кластера (`talos-prod-kz`, `talos-stage-ru`), наш
софт в каждом, тестовый Deployment, матрица `image × node` с прогретым на всех воркерах
образом и пример отказа ноды:
[docs/diagrams/image-prepuller-overview.html](diagrams/image-prepuller-overview.html)
(исходник — `docs/diagrams/image-prepuller-overview.json`, PNG для README —
`docs/diagrams/image-prepuller-overview.png`). Mermaid-схемы ниже остаются
каноническими для markdown-рендера; HTML-артефакт нужен для чтения схемы с зумом
и подсветкой связей.

## 2. Компоненты и зависимости пакетов

```mermaid
flowchart TB
    MAIN["cmd/prepuller<br/>разбор argv, env, serve()"]

    subgraph WEB["internal/web"]
        SRV["Server (hub)<br/>mux + requireAuth"]
        SPK["NewSpokeServer<br/>requireToken"]
        ST["static/*<br/>embed.FS"]
    end

    AUTH["internal/auth<br/>Manager + middleware"]
    REG["internal/provider<br/>Registry"]
    IFACE["Provider interface v2"]
    MOCK["MockProvider"]
    REM["RemoteProvider"]
    K8SP["K8sProvider<br/>client-go · TTL-кэш 2s"]
    AGC["internal/provider/k8s_agent.go<br/>HTTP-клиент агента v1"]
    AG["internal/agent<br/>CRI · HTTP/JSON API v1"]
    REC["internal/reconcile<br/>очередь · приоритеты · backoff"]
    MET["internal/metrics<br/>Handler(reg)"]
    MODEL["internal/model<br/>Snapshot · PullTask · ImageRef · …"]
    TU["internal/testutil<br/>fake spoke · golden v2"]

    MAIN --> WEB
    MAIN --> AUTH
    MAIN --> REG
    MAIN --> MET
    MAIN --> AG
    MAIN --> REC
    REC --> IFACE
    K8SP --> AGC
    AGC -.->|"/agent/v1/*"| AG
    SRV --> AUTH
    SRV --> REG
    SRV --> ST
    SPK --> IFACE
    REG --> IFACE
    MOCK -.->|реализует| IFACE
    REM -.->|реализует| IFACE
    K8SP -.->|реализует| IFACE
    MET --> REG
    IFACE --> MODEL
    MOCK --> MODEL
    REM --> MODEL
    K8SP --> MODEL
    AUTH --> MODEL
    TU -.->|контрактные тесты| WEB
    TU -.->|контрактные тесты| IFACE
```

`Provider` v2 — единственная точка расширения под Kubernetes (contract-v2):

```go
type Provider interface {
    Snapshot(ctx) model.Snapshot
    TriggerPull(ctx, model.PullRequest) (model.PullTask, error)
    TriggerWorkloadPull(ctx, model.WorkloadPullRequest) (int, error)
    CancelPull(ctx, taskID string) error
    SyncNow(ctx, actor string) error
    Health(ctx) model.Health
    PullCounters() []model.PullMetricSample
}
```

Hub, spoke, метрики и UI не знают, mock это или живой кластер. Поэтому новый провайдер
не требует правок ни в роутинге, ни в шаблонах метрик. Границы зависимостей
(grpc+cri-api только в агенте, client-go только в `internal/provider/k8s*.go`,
hub-обвязка на stdlib) зафиксированы в [ADR-001](adr/0001-dependencies.md) и проверяются
на ревью; `internal/testutil` даёт фейковый spoke и golden-снапшот для контрактных тестов.

`K8sProvider` берёт инвентарь образов ноды через интерфейс `NodeInventorySource`
(`internal/provider/k8s_inventory.go`); его рабочая реализация — HTTP-клиент к node-agent
(`internal/provider/k8s_agent.go`), который ходит в `/agent/v1/*`. Пока агент недоступен,
включён `UnavailableInventorySource`: слоты получают `offline`, снапшот — `degraded`/`partial`.

## 3. Запуск и регистрация кластеров

```mermaid
flowchart TD
    A["main() → os.Args[1]"] --> B{"controller / agent?"}
    B -->|agent| X["runAgent()<br/>флаги · CRI · HTTP/JSON сервер :9443"]
    B -->|controller| C{"-role"}
    C -->|spoke| D["provider.NewMockProvider<br/>(live → K8sProvider)"]
    D --> E["Registry.Register<br/>role=local"]
    E --> F["web.NewSpokeServer(provider, token)"]
    C -->|hub| G["auth.NewManager<br/>env user/pass/secret"]
    G --> H{"-mock?"}
    H -->|да| I["Register: local + 2 demo remote"]
    H -->|нет| Y["provider.NewK8sProviderFromEnv<br/>client-go + agent-клиент"]
    I --> J["PREPULLER_REMOTE_CLUSTERS<br/>name=url=token"]
    J --> K["Registry.Register<br/>RemoteProvider"]
    K --> M{"-auto-warm?"}
    M -->|да| N["reconcile.Start<br/>по локальным кластерам"]
    M -->|нет| L["web.NewServer(reg, auth)"]
    N --> L
    F --> S["serve(httpAddr, metricsAddr)"]
    L --> S
    S --> T["две http.Server:<br/>API :8080 и metrics :9090"]
    T --> U["SIGINT/SIGTERM → Shutdown 5s"]
```

Порядок `Register` важен: `Registry.DefaultID()` — первый зарегистрированный кластер,
именно он открывается в UI по умолчанию. `AllIDs()` сортирует id, поэтому порядок
метрик стабилен и не зависит от порядка регистрации.

В live-режиме (без `-mock`) hub поднимает `K8sProvider`: kube-конфиг берётся из
in-cluster service account, иначе из `$KUBECONFIG`/`~/.kube/config`; токен агента —
`PREPULLER_AGENT_TOKEN`. С флагом `-auto-warm` для каждого локального кластера
запускается свой `reconcile.Reconciler` под тем же signal-контекстом, что и HTTP-серверы;
удалённые кластеры пропускаются — reconciler рядом с ними запускает spoke.

## 4. Аутентификация

### Hub — cookie-сессия

```mermaid
sequenceDiagram
    autonumber
    participant B as Браузер
    participant S as Hub Server
    participant A as auth.Manager

    B->>S: POST /api/v1/auth/login {username, password}
    S->>A: ValidateCredentials (subtle.ConstantTimeCompare)
    alt неверно
        A-->>B: 401 {"error":"неверный логин или пароль"}
    else верно
        S->>A: IssueCookie
        A-->>B: Set-Cookie prepuller_session<br/>base64(user)|exp.HMAC
        B->>S: GET /api/v1/snapshot + cookie
        S->>A: AuthenticateRequest
        A->>A: split('.') → HMAC(payload) → exp → user
        A-->>S: (user, true)
        S-->>B: 200 snapshot
    end
```

Свойства: HttpOnly, SameSite=Lax, `Secure` при TLS или `X-Forwarded-Proto: https`,
TTL 24 часа, подпись HMAC-SHA256, сравнение подписи и пароля — constant-time.
Секрет берётся из `PREPULLER_SESSION_SECRET`; если пуст — 32 случайных байта на процесс,
то есть после рестарта все сессии инвалидируются.

### Spoke — Bearer-токен

```mermaid
sequenceDiagram
    participant RP as RemoteProvider (hub)
    participant SP as Spoke mux
    participant P as Provider

    RP->>SP: POST /internal/v1/pull<br/>Authorization: Bearer <token>
    SP->>SP: subtle.ConstantTimeCompare(token)
    alt токен неверен
        SP-->>RP: 401 invalid cluster token
    else ок
        SP->>P: TriggerPull(...)
        P-->>SP: error | nil
        SP-->>RP: 202 / 400
    end
```

Если `PREPULLER_CLUSTER_TOKEN` пуст, проверка отключается — такой spoke нельзя
публиковать вне доверенной сети.

## 5. Поток данных снапшота

```mermaid
sequenceDiagram
    autonumber
    actor OP as Оператор
    participant UI as app.js
    participant HUB as /api/v1
    participant REG as Registry
    participant PR as Provider
    participant RP as RemoteProvider
    participant SP as Spoke

    Note over UI: boot → GET /api/v1/auth/me
    UI->>HUB: GET /api/v1/clusters
    HUB->>REG: List(ctx)
    REG->>PR: Snapshot()
    REG-->>HUB: []ClusterSummary
    HUB-->>UI: {defaultCluster, clusters}
    UI->>HUB: GET /api/v1/snapshot?cluster=ID
    HUB->>REG: Get(ID)
    alt local
        REG->>PR: Snapshot()
    else remote
        REG->>RP: Snapshot()
        RP->>SP: GET /internal/v1/snapshot
        SP-->>RP: model.Snapshot
    end
    REG-->>HUB: Snapshot
    HUB-->>UI: Snapshot (summary, workloads, nodes, activity)
    Note over UI: setInterval 5s, пока включён Auto 5s
```

UI не кэширует ничего, кроме последнего снапшота: каждые 5 секунд — `clusters` и
`snapshot` выбранного кластера. Переключение кластера в селекте меняет только
`state.activeCluster` и сразу перезапрашивает снапшот.

### Надёжность транспорта hub → spoke

`RemoteProvider` держит один `http.Client` с ограниченным пулом соединений и таймаутом
5 s на попытку. Идемпотентные state-changing вызовы (`pull`, `workloads/pull`, `cancel`,
`reconcile`) повторяются до 3 раз с экспоненциальным backoff (150 ms → 2 s); чтения
(`snapshot`, `counters`, `health`) — ровно один запрос, чтобы мёртвый spoke не
тормозил страницу и скрейп метрик. Ошибки типизированы: `ErrSpokeUnavailable`/
`ErrSpokeUnauthorized` → `502`, `ErrTaskNotActive` → `409`; `Snapshot` по-прежнему
деградирует в offline-снапшот, а `PullCounters` возвращает `nil`.

## 6. Модель данных

```mermaid
erDiagram
    SNAPSHOT ||--|| SUMMARY : summary
    SNAPSHOT ||--o{ WORKLOAD : workloads
    SNAPSHOT ||--o{ NODEINFO : nodes
    SNAPSHOT ||--o{ PULLEVENT : activity
    SNAPSHOT ||--o{ PULLTASK : tasks
    WORKLOAD ||--o{ WORKLOADIMAGE : images
    WORKLOADIMAGE ||--o{ IMAGENODESTATUS : nodes
    WORKLOADIMAGE ||--o{ POLICYVIOLATION : policyViolations
    WORKLOADIMAGE ||--|| IMAGEREF : ref
    NODEINFO ||--o{ CACHEDIMAGE : cachedImages
    NODEINFO ||--o{ NODECONDITION : conditions

    SNAPSHOT {
        string clusterName
        string apiVersion
        bool mockMode
        datetime updatedAt
        datetime lastSyncAt
        string syncStatus
        bool partial
    }
    PULLTASK {
        string id
        string image
        string node
        string state
        int attempt
        string trigger
        string actor
        string error
    }
    IMAGEREF {
        string image
        string tag
        string digest
    }
    NODECONDITION {
        string type
        string status
        string reason
    }
    SUMMARY {
        int totalWorkloads
        int uniqueImages
        int presentSlots
        int totalSlots
        float coveragePercent
        int policyWarnings
        int diskPausedNodes
    }
    WORKLOAD {
        string namespace
        string kind
        string name
        int32 replicas
    }
    WORKLOADIMAGE {
        string image
        string containerName
        bool isInit
        string pullPolicy
    }
    IMAGENODESTATUS {
        string node
        string state
        int64 sizeBytes
        datetime lastChecked
        string error
    }
    NODEINFO {
        string name
        bool ready
        bool agentUp
        string agentVersion
        bool diskPaused
        int64 imageFsUsedBytes
        int64 imageFsCapacityBytes
    }
    PULLEVENT {
        string id
        string node
        string image
        string trigger
        string actor
        string result
        int64 durationMs
    }
```

Ключевая метрика — **coverage**: `presentSlots / totalSlots × 100`, где слот — пара
«образ workload'а × worker-нода». Она же уезжает в `prepuller_image_present`.

## 7. Состояния слота и политика диска

```mermaid
stateDiagram-v2
    direction LR
    [*] --> present: mock init (образ уже есть)
    [*] --> missing: требуется прогрев
    [*] --> offline: нода NotReady / agent down
    missing --> pulling: TriggerPull
    present --> pulling: TriggerPull
    failed --> pulling: retry
    pulling --> present: success (+sizeBytes, lastChecked)
    pulling --> failed: registry 401, сеть, CRI error
    present --> offline: нода пропала
    missing --> offline: нода пропала
    present --> paused_disk: ImageFS ≥ 78%
    missing --> paused_disk: ImageFS ≥ 78%
    paused_disk --> missing: ImageFS ниже порога
    offline --> missing: нода вернулась
```

Порядок вычисления состояния в `MockProvider` (и будущем `K8sProvider`):
`offline` (нет ноды) → `paused_disk` (ImageFS ≥ `diskPauseRatio`) → `failed` (ошибка pull)
→ `present`/`missing`. Порог паузы 78% ниже порога kubelet GC 85% намеренно: прогрев
должен остановиться до того, как kubelet начнёт вычищать образы.

Policy-проверки (не блокируют pull, только предупреждают):

- `pull_policy_always` — `imagePullPolicy: Always` ломает offline failover;
- `latest_tag` — нет иммутабельного тега/digest (`:latest`, пустой тег; `@sha256:` — ок).

## 8. Pull-операции

```mermaid
flowchart TD
    A["UI: Pull / Pull All"] --> B["POST /api/v1/pull<br/>или /workloads/pull"]
    B --> C["requireAuth → actor = username"]
    C --> D["Registry.TriggerPull(cluster, PullRequest)"]
    D --> E{"кластер найден?"}
    E -->|нет| F["400 unknown cluster"]
    E -->|да| G{"local / remote"}
    G -->|local| H["Provider.TriggerPull → PullTask"]
    G -->|remote| I["RemoteProvider → spoke → Provider"]
    H --> J["PullTask{id,state=running}<br/>cell.state = pulling<br/>activePulls++"]
    J --> K["goroutine: sleep 1.2–2.5s"]
    K --> L["PullTask.state=succeeded<br/>state = present, success++"]
    I --> J
    D --> M["202 Accepted + PullTask"]
    N["POST /api/v1/pulls/{id}/cancel"] --> O["Provider.CancelPull"]
    O --> P["PullTask.state=canceled<br/>activePulls--, cell откатывается"]
```

В Stage 1 «pull» — симуляция задержки; реального CRI-вызова нет. Важные свойства,
которые нужно сохранить в живой реализации:

1. Ответ всегда `202` + `PullTask` — операция асинхронная, клиент узнаёт результат из
   следующего снапшота (`Snapshot.Tasks`/`Snapshot.Activity`) или из `/api/v1/pull-tasks`.
2. Повторный `TriggerPull` по активной паре image+node возвращает существующую задачу
   (тот же `id`) — идемпотентность, а не ошибка.
3. Офлайн-нода — ошибка `node ... is offline / agent unreachable`, в UI кнопка disabled.
4. `CancelPull` отменяет только `queued`/`running`; завершённая задача — ошибка,
   неизвестный id — `provider.ErrTaskNotFound` (hub отдаёт 404).
5. `Pull All` не атомарен: возвращает счётчик успешно запущенных слотов, ошибки отдельных
   слотов проглатываются.
6. **Зависший реестр не занимает слот.** В live-пути pull синхронный (hub → агент →
   CRI `PullImage`), и CRI не даёт прогресса, поэтому агент ограничивает его сам:
   preflight TCP-проба реестра (`-registry-probe-timeout`, 5 s) отдаёт
   `502 registry unreachable`, а stall watchdog по отсутствию роста ImageFS
   (`-pull-stall-timeout`, 60 s) — `504 pull stalled`. Hub превращает 504 в
   `provider.ErrAgentPullStalled` (HTTP 504), задача сразу `failed`, слот
   освобождается; `DefaultAgentPullTimeout` (10 min) остаётся страховкой для
   действительно долгих загрузок. Подробности — AGENTS.md §8.
## 9. Метрики

```mermaid
flowchart LR
    SCR["Prometheus scrape<br/>GET /metrics"] --> MH["metrics.Handler(reg)<br/>stateful collector"]
    MH --> PAR["каждый кластер — своя горутина"]
    PAR --> SNAP["ровно один Provider.Snapshot(ctx)<br/>на кластер за скрейп"]
    PAR --> DL{"общий дедлайн 3 s?"}
    DL -->|успел| OUT["text/plain; version=0.0.4"]
    DL -->|не успел| TMO["snapshot_scrape_timeout_total++<br/>последний успешный снапшот"]
    TMO --> OUT
    SNAP --> CL["cluster_up · sync_partial · last_sync_timestamp · snapshot_age"]
    SNAP --> ND["node_ready · agent_up · last_seen · agent_errors{reason}<br/>disk_paused · imagefs used/capacity/free · images_cached"]
    SNAP --> TS["pull_tasks{state} · pull_task_duration_seconds · agent_info"]
    SNAP --> IMG["image_present · workload_policy_violation"]
    PAR --> CNT["Provider.PullCounters()<br/>pull_total"]
    CL --> OUT
    ND --> OUT
    TS --> OUT
    IMG --> OUT
    CNT --> OUT
```

Экспозиция собирается руками (`strings.Builder`), без `prometheus/client_golang` —
эта зависимость запрещена ADR-001, как и любые UI/роутинг-фреймворки. Внешние
зависимости модуля ограничены агентом (grpc+cri-api) и K8s-провайдером (client-go).

**Скрейп не слепнет при аварии.** `Handler` создаёт один stateful-коллектор на всё
время жизни процесса: каждый `Provider.Snapshot` вызывается в своей горутине, все
кластеры параллельно, общий дедлайн `snapshotDeadline = 3 s`
(`internal/metrics/prometheus.go`). Не успевший кластер получает
`prepuller_snapshot_scrape_timeout_total{cluster}` и в вывод идёт его последний
успешный снапшот из коллектора — со своим `LastSyncAt`, поэтому
`prepuller_snapshot_age_seconds` растёт (это и есть сигнал «hub ослеп»). Кластер,
который ни разу не отвечал, не получает ни одной серии, кроме счётчика таймаутов:
нули выглядели бы как здоровый кластер. `PullCounters` качается в той же горутине и
в тот же дедлайн; не успел — берутся прошлые counters. Брошенная горутина пишет в
буферизованный канал, а после возврата хендлера `r.Context()` отменяется, поэтому
снапшот провайдера быстро отпускает мьютекс. Инвариант «ровно один `Snapshot()` на
кластер за скрейп» сохранён (`TestHandlerCallsSnapshotOncePerCluster`). До этого
изменения partition стоил 5 s × число нод и ронял весь скрейп по `scrape_timeout`.

Семантика: `prepuller_cluster_up=0` при `syncStatus=offline` или `partial=true`;
`prepuller_cluster_last_sync_timestamp_seconds` отсутствует, если `LastSyncAt`
неизвестен (нулевое время), и никогда не подменяется текущим временем;
`prepuller_node_last_seen_timestamp_seconds` отсутствует при нулевом `LastSeen`.
`pull_task_duration_seconds` — gauge со средним по терминальным задачам
(гистограмма в text 0.0.4 без client_golang потребовала бы bucket-серий и
нескольких sample с одинаковым label-set, что невалидно).

**Ошибки агента на hub:** `prepuller_node_agent_errors_total{cluster,node,reason}`
заполняет `K8sProvider` — единственный, кто реализует опциональную capability
`provider.AgentErrorCounter` (в замороженный интерфейс `Provider` её сознательно не
добавляли). `provider.ClassifyAgentError` сводит ошибку инвентаря к закрытому набору
`unauthorized|protocol|timeout|unavailable`; эмитятся только реально наблюдавшиеся
reason'ы. У mock и remote кластеров серии нет — это норма.

**Метрики node-agent (отдельный порт).** Агент поднимает второй листенер
(`-metrics-addr`, по умолчанию `:9444`, без авторизации) и отдаёт только
счётчики/gauge'и, которые уже знает: запросы по `endpoint`/`code`, время последнего
контакта по каждому endpoint (инициализируется стартом процесса, поэтому «к агенту
никто не ходил» алертится сразу), статус CRI по последнему снапшоту, pull-счётчики
по результату, ImageFS и число образов из последнего успешного снапшота. На скрейпе
агент не ходит в CRI и в сеть, поэтому скрейп не может заблокироваться. Вместе с
`prepuller_node_agent_up` и `prepuller_node_last_seen_timestamp_seconds` это даёт
триангуляцию «агент умер / токен / связь hub→агент» — таблица в `docs/alerts.md`.

## 10. Расширение до живого кластера (Stage 2–3)

```mermaid
flowchart LR
    HUB["Hub<br/>K8sProvider"] -->|"list Deployments / STS / DS"| KAPI["Kubernetes API"]
    HUB -->|"gRPC ListImages / PullImage"| AG["node-agent<br/>(DaemonSet)"]
    AG -->|"CRI"| CD["containerd.sock"]
    AG -->|"ImageFS stats"| KUBELET["/stats/summary<br/>kubelet"]
    HUB -->|"SSE / push (опционально)"| UI["Web UI"]
```

План:

1. **node-agent** (`prepuller agent`) — DaemonSet на каждой ноде: gRPC-сервер, список
   локальных образов и размеров, `PullImage` через CRI, чтение заполнения ImageFS;
   agent сам ставит авто-прогрев на паузу при превышении порога.
2. **K8sProvider** — реализует те же методы Provider v2: список workload'ов через
   client-go (list/watch, кэш), состояния слотов через агрегированные ответы агентов,
   `SyncNow` — внеплановая сверка, `PullCounters`/`Health` — для метрик и UI.
   Скелет с clientset уже в `internal/provider/k8s_stub.go` (WS-B заменяет заглушки).
3. **Reconciler** — периодическая сверка «что требует кластер / что уже есть», постановка
   задач прогрева с ограничением параллелизма и приоритетами, backoff на ошибки registry.
4. Hub остаётся без изменений: его контракт — `Provider` + `Registry`, а не детали CRI.

## 11. Эксплуатационные заметки

- Graceful shutdown: 5 секунд на оба сервера; pull-горутины mock при этом завершаются вместе
  с процессом (в живой версии задачи должны переживать рестарт или пересоздаваться reconciler'ом).
- `/healthz` и `/readyz` не проверяют провайдера — readiness говорит лишь о том, что процесс
  поднял HTTP. Для честной проверки нужен отдельный флаг «последний успешный синк».
- Порт метрик слушается отдельным `http.Server` без auth: открывайте его только для Prometheus.
- Один статический пользователь; в activity действия подписаны парой
  `trigger: manual` + `actor: <логин>` (contract v2 разделил триггер и актора).