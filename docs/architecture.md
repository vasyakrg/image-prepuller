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

## 2. Компоненты и зависимости пакетов

```mermaid
flowchart TB
    MAIN["cmd/prepuller<br/>разбор argv, env, serve()"]

    subgraph WEB["internal/web"]
        SRV["Server (hub)<br/>mux + requireAuth"]
        SPK["NewSpokeServer<br/>requireToken"]
        ST["static/*<br/>embed.FS"]
    end

    AUTH["internal/auth<br/>Manager"]
    REG["internal/provider<br/>Registry"]
    IFACE["Provider interface"]
    MOCK["MockProvider"]
    REM["RemoteProvider"]
    MET["internal/metrics<br/>Handler(reg)"]
    MODEL["internal/model<br/>Snapshot · Workload · NodeInfo · …"]

    MAIN --> WEB
    MAIN --> AUTH
    MAIN --> REG
    MAIN --> MET
    SRV --> AUTH
    SRV --> REG
    SRV --> ST
    SPK --> IFACE
    REG --> IFACE
    MOCK -.->|реализует| IFACE
    REM -.->|реализует| IFACE
    MET --> REG
    IFACE --> MODEL
    MOCK --> MODEL
    REM --> MODEL
    AUTH --> MODEL
```

`Provider` — единственная точка расширения под Kubernetes:

```go
type Provider interface {
    Snapshot(ctx) model.Snapshot
    TriggerPull(ctx, image, node, actor string) error
    TriggerWorkloadPull(ctx, namespace, kind, name, actor string) (int, error)
    SyncNow(ctx, actor string) error
    PullCounters() []model.PullMetricSample
}
```

Hub, spoke, метрики и UI не знают, mock это или живой кластер. Поэтому новый провайдер
не требует правок ни в роутинге, ни в шаблонах метрик.

## 3. Запуск и регистрация кластеров

```mermaid
flowchart TD
    A["main() → os.Args[1]"] --> B{"controller / agent?"}
    B -->|agent| X["Stage 2 stub<br/>exit 1"]
    B -->|controller| C{"-role"}
    C -->|spoke| D["provider.NewMockProvider<br/>(live → fatal)"]
    D --> E["Registry.Register<br/>role=local"]
    E --> F["web.NewSpokeServer(provider, token)"]
    C -->|hub| G["auth.NewManager<br/>env user/pass/secret"]
    G --> H{"-mock?"}
    H -->|да| I["Register: local + 2 demo remote"]
    H -->|нет| Y["fatal: Stage 3"]
    I --> J["PREPULLER_REMOTE_CLUSTERS<br/>name=url=token"]
    J --> K["Registry.Register<br/>RemoteProvider"]
    K --> L["web.NewServer(reg, auth)"]
    F --> S["serve(httpAddr, metricsAddr)"]
    L --> S
    S --> T["две http.Server:<br/>API :8080 и metrics :9090"]
    T --> U["SIGINT/SIGTERM → Shutdown 5s"]
```

Порядок `Register` важен: `Registry.DefaultID()` — первый зарегистрированный кластер,
именно он открывается в UI по умолчанию. `AllIDs()` сортирует id, поэтому порядок
метрик стабилен и не зависит от порядка регистрации.

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

## 6. Модель данных

```mermaid
erDiagram
    SNAPSHOT ||--|| SUMMARY : summary
    SNAPSHOT ||--o{ WORKLOAD : workloads
    SNAPSHOT ||--o{ NODEINFO : nodes
    SNAPSHOT ||--o{ PULLEVENT : activity
    WORKLOAD ||--o{ WORKLOADIMAGE : images
    WORKLOADIMAGE ||--o{ IMAGENODESTATUS : nodes
    WORKLOADIMAGE ||--o{ POLICYVIOLATION : policyViolations
    NODEINFO ||--o{ CACHEDIMAGE : cachedImages

    SNAPSHOT {
        string clusterName
        bool mockMode
        datetime updatedAt
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
        bool diskPaused
        int64 imageFsUsedBytes
        int64 imageFsCapacityBytes
    }
    PULLEVENT {
        string id
        string node
        string image
        string trigger
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
    C --> D["Registry.TriggerPull(cluster, image, node, actor)"]
    D --> E{"кластер найден?"}
    E -->|нет| F["400 unknown cluster"]
    E -->|да| G{"local / remote"}
    G -->|local| H["Provider.TriggerPull"]
    G -->|remote| I["RemoteProvider → spoke → Provider"]
    H --> J["cell.state = pulling<br/>activePulls++<br/>activity += PullEvent(trigger=manual)"]
    J --> K["goroutine: sleep 1.2–2.5s"]
    K --> L["state = present, sizeBytes, success++"]
    I --> J
    D --> M["202 Accepted"]
```

В Stage 1 «pull» — симуляция задержки; реального CRI-вызова нет. Важные свойства,
которые нужно сохранить в живой реализации:

1. Ответ всегда `202` — операция асинхронная, клиент узнаёт результат из следующего снапшота.
2. Повторный `TriggerPull` по слоту в состоянии `pulling` — no-op, а не ошибка (идемпотентность).
3. Офлайн-нода — ошибка `node ... is offline / agent unreachable`, в UI кнопка disabled.
4. `Pull All` не атомарен: возвращает счётчик успешно запущенных слотов, ошибки отдельных
   слотов проглатываются.

## 9. Метрики

```mermaid
flowchart LR
    SCR["Prometheus scrape<br/>GET /metrics"] --> MH["metrics.Handler(reg)"]
    MH --> IDS["reg.AllIDs() (sorted)"]
    IDS --> LOOP["для каждого cluster id"]
    LOOP --> SNAP["Provider.Snapshot(ctx)"]
    SNAP --> CL["cluster_up, image_present, policy_violation"]
    SNAP --> ND["node_agent_up, disk_paused, imagefs_used/capacity"]
    LOOP --> CNT["Provider.PullCounters()<br/>pull_total"]
    CL --> OUT["text/plain; version=0.0.4"]
    ND --> OUT
    CNT --> OUT
```

Экспозиция собирается руками (`strings.Builder`), без `prometheus/client_golang` —
зависимостей у модуля нет вообще. Учтите: `Handler` вызывает `Snapshot()` отдельно на
каждое семейство метрик (6 раз на кластер за скрейп). Для mock это дёшево, для live
провайдера нужно кэширование внутри провайдера или один снапшот на запрос.

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
2. **K8sProvider** — реализует те же 5 методов: список workload'ов через client-go,
   состояния слотов через аггрегированные ответы агентов, `SyncNow` — внеплановая сверка.
3. **Reconciler** — периодическая сверка «что требует кластер / что уже есть», постановка
   задач прогрева с ограничением параллелизма и приоритетами, backoff на ошибки registry.
4. Hub остаётся без изменений: его контракт — `Provider` + `Registry`, а не детали CRI.

## 11. Эксплуатационные заметки

- Graceful shutdown: 5 секунд на оба сервера; pull-горутины mock при этом завершаются вместе
  с процессом (в живой версии задачи должны переживать рестарт или пересоздаваться reconciler'ом).
- `/healthz` и `/readyz` не проверяют провайдера — readiness говорит лишь о том, что процесс
  поднял HTTP. Для честной проверки нужен отдельный флаг «последний успешный синк».
- Порт метрик слушается отдельным `http.Server` без auth: открывайте его только для Prometheus.
- Один статический пользователь; все действия в activity подписаны его логином (`manual (admin)`).
