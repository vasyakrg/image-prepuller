# AGENTS.md

Правила работы над **image-prepuller** для людей и AI-агентов. Читать целиком перед
первым изменением кода.

## 1. Что это

Сервис прогрева image-кэша на worker-нодах Kubernetes. Один Go-бинарь, две роли:
`hub` (Web UI + cookie-авторизация + реестр кластеров + Prometheus) и `spoke`
(headless backend API рядом с кластером, Bearer-токен).

Текущий этап — **Stage 2/3 (integration)**: `MockProvider` работает всегда через
`-mock`; live-путь собран из `K8sProvider` (client-go: workload'ы, слоты, TTL-кэш)
и `node-agent` (`internal/agent`: CRI, инвентарь образов, disk-guard, HTTP/JSON
API v1), транспорт провайдер→агент — `internal/provider/k8s_agent.go`. Авто-прогрев —
`internal/reconcile`, включается флагом `-auto-warm`. Контракт заморожен тегом
`contract-v2`; agent wire v1 — HTTP/JSON (`/healthz`, `/agent/v1/snapshot`,
`/agent/v1/images/{ref}`, `/agent/v1/pulls`).

Креды приватного реестра контроллер резолвит сам: агент тянет через CRI, а CRI не
применяет `imagePullSecrets` пода, поэтому провайдер читает их из workload'а и
передаёт агенту в запросе pull (`internal/provider/k8s_registry.go`,
`internal/agent/registry_auth.go`); иначе приватный реестр отвечает анонимный `403`.

Область действия ограничена метками: `-workload-selector`/`PREPULLER_WORKLOAD_SELECTOR`
(в чарте по умолчанию `prepull=true`) и `-node-selector`/`PREPULLER_NODE_SELECTOR`
(чарт по умолчанию `!node-role.kubernetes.io/control-plane`, потому что DaemonSet
агента ставится только на воркеры). Всё, что не попало в селектор, не листится, не
показывается, не считается в покрытии и не прогревается. Флаги бинаря по умолчанию
пустые (все ноды/workload'ы) — дефолт области действия задаёт именно чарт.

Управляемый кластер подключается к hub'у двумя способами: **прямой** (hub → spoke по
HTTP, `hub.remoteClusters`/`PREPULLER_REMOTE_CLUSTERS`) и **outbound** (spoke сам ходит
в hub на его ingress, `-hub-url` + `PREPULLER_MANAGED_CLUSTERS`). Второй нужен, когда
hub не может достучаться до кластера (NAT, нет ingress) — тогда наружу нужен только
исходящий HTTPS из управляемого кластера.

## 2. Быстрые команды

```bash
go build -o bin/prepuller ./cmd/prepuller     # сборка
go test ./...                                  # тесты (auth, web, контрактные testutil)
go vet ./...                                   # статический анализ
gofmt -l .                                     # должно быть пусто
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...  # достижимые уязвимости (0 = ok)
./release.sh 1.1.0 -m "short description"      # релиз: бамп версии, CHANGELOG, тег, push (§8)
```

В песочнице `$HOME` может быть read-only (`operation not permitted` на
`~/Library/Caches/go-build`, `~/go/pkg/mod`, `~/go/pkg/sumdb`). Тогда задайте кэши
внутри рабочего каталога — они в `.gitignore`:

```bash
export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod
go mod download && go test ./...
```

С contract-v2 у модуля есть внешние зависимости, поэтому первый `go build/test`
в новом worktree требует `go mod download` (сеть или прогретый `GOMODCACHE`).

Запуск для ручной проверки:

```bash
PREPULLER_AUTH_USER=admin PREPULLER_AUTH_PASS=admin \
  ./bin/prepuller controller -role=hub -mock
# http://localhost:8080  (UI)   http://localhost:9090/metrics
```

Spoke:

```bash
PREPULLER_CLUSTER_TOKEN=spoke-secret \
  ./bin/prepuller controller -role=spoke -mock -http-addr=:8081 -cluster-name=talos-prod-kz
```

Тот же spoke в outbound-режиме (hub должен быть запущен с
`PREPULLER_MANAGED_CLUSTERS=talos-prod-kz=spoke-secret`):

```bash
PREPULLER_CLUSTER_TOKEN=spoke-secret \
  ./bin/prepuller controller -role=spoke -mock -http-addr=:8081 -cluster-name=talos-prod-kz \
  -hub-url=http://localhost:8080
```

E2E на одноразовом k3d-кластере (docker+kubectl+k3d+helm, ~2 минуты) — гоняется
локально перед пушем в `main`, в CI его намеренно нет:

```bash
bash hack/e2e-k3d.sh        # собрать образ, поднять кластер, прогнать сценарий, удалить
KEEP=1 bash hack/e2e-k3d.sh # оставить кластер для разбора
```

## 3. Ограничения стека

- **Go 1.26.11** в `go.mod` (локально может быть новее — не поднимайте директиву `go` без причины).
  Директива поднята с 1.24 в 2026-11 вместе с патчами x/net и grpc, патч-уровень
  зафиксирован отдельно: `govulncheck` сверяет стандартную библиотеку тулчейна с базой
  уязвимостей, а 1.26.0 содержал 37 известных проблем (см. ADR-001, «Обновление»).
- **Зависимости — только по ADR** ([ADR-001](docs/adr/0001-dependencies.md) — CRI/client-go,
  [ADR-002](docs/adr/0002-postgres-statistics.md) — драйвер PostgreSQL для статистики).
  Разрешённые импорты по пакетам:

  | Пакет | Можно импортировать |
  | --- | --- |
  | `internal/agent` | `google.golang.org/grpc`, `k8s.io/cri-api`, stdlib |
  | `internal/provider/k8s*.go` | `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`, stdlib |
  | `internal/web`, `internal/model`, `internal/auth`, `internal/metrics` | только stdlib |
  | `internal/provider/{provider,registry,mock,remote}.go` | только stdlib |
  | `internal/provider/{spoke_hub,spoke_wire,counters}.go` | только stdlib |
  | `internal/spokeclient` | stdlib + `internal/model`, `internal/provider` |
  | `internal/reconcile` | stdlib + `internal/model`, `internal/provider` |
  | `internal/tasks` | stdlib + `internal/model` |
  | `internal/stats` | stdlib (`database/sql`) + `internal/model` + `github.com/jackc/pgx/v5/stdlib` (только в `postgres.go`): запись, SQL-чтение для вкладки Stats и `Service` |
  | `internal/testutil` | stdlib + внутренние пакеты |

  `go.mod`/`go.sum` — зона интегратора: feature-ветки их не меняют, новая зависимость
  без ADR не принимается. `vendor/` не используется: CI и разработка тянут модули через
  `GOPROXY`-зеркало, `go.sum` коммитится. `prometheus/client_golang`, фреймворки
  роутинга и UI-библиотеки по-прежнему запрещены — метрики и UI пишутся руками.
- Фронтенд — статические `internal/web/static/*`, вшитые через `//go:embed`.
  Никаких npm/Vite/esbuild шагов. `app.js` — ES2020+ в браузере, без модулей и сборки.
- Метрики пишутся вручную в формате Prometheus text exposition 0.0.4.

## 4. Карта кода

| Путь | Ответственность |
| --- | --- |
| `cmd/prepuller/main.go` | `argv`, флаги, env, регистрация кластеров, `serve()`, graceful shutdown |
| `internal/model/types.go` | доменные типы и JSON-контракт API (camelCase-теги) |
| `internal/provider/provider.go` | интерфейс `Provider` — главная точка расширения |
| `internal/provider/registry.go` | потокобезопасный реестр кластеров, `DefaultID`, `AllIDs` |
| `internal/provider/mock.go` | in-memory кластер: 4 ноды, 5 workload'ов, симуляция pull |
| `internal/provider/remote.go` | hub → spoke по `/internal/v1/*`: Bearer, таймауты, ретраи state-changing вызовов, типизированные ошибки (`ErrSpokeUnavailable`/`ErrSpokeUnauthorized`/`ErrTaskNotActive`), health и counters; тем же кодом пользуется outbound-канал (`newRemoteProviderWithTransport`) |
| `internal/provider/spoke_hub.go` | hub-сторона outbound-канала: `SpokeHub` (токены кластеров, кэш pushed-состояния, mailbox команд, staleness), `PushProvider` (чтения из кэша, state-changing — команды через `RemoteProvider` поверх mailbox-`RoundTripper`) |
| `internal/provider/spoke_wire.go` | wire-контракт outbound-канала: `SpokeState`, `SpokeCommand`, `SpokeCommandResult` |
| `internal/provider/counters.go` | `AggregateCounters` — одна реализация `SpokeCounters` для `GET /internal/v1/counters` и для state push |
| `internal/spokeclient/` | spoke-сторона outbound-канала: push состояния, long-poll команд, исполнение через собственный handler, дедупликация по id, backoff при обрыве |
| `internal/auth/**` | cookie-сессии с ролью (HMAC-SHA256), RBAC (`RequireAuth`/`RequireRole`/`RequireOperator`), rate-limit логина, аудит; `users.go`, `roles.go`, `audit.go`, `ratelimit.go` |
| `internal/auth/middleware.go` | `RequireAuth` (hub cookie), `RequireToken` (spoke Bearer), `RequireRole`/`RequireOperator` |
| `internal/web/server.go` | hub-роутер, hub-хендлеры, `writeJSON`, опции сервера (`WithSpokeHub`) |
| `internal/web/server_spoke.go` | spoke-роутер `/internal/v1/*` (contract v2) |
| `internal/web/server_stats.go` | read-only ручки `/api/v1/stats/*` для вкладки Stats: порт `StatsSource` (web не импортирует `internal/stats`, чтобы не тянуть `pgx`), парсинг `window`/`limit`/`offset`/`state`/`trigger`/`q`, таймауты 5 s/3 s, состояния disabled/unavailable |
| `internal/web/server_spoke_hub.go` | hub-эндпоинты управляемых кластеров `/internal/v1/spokes/{cluster}/{state,commands,commands/{id}/result}` (Bearer-токен кластера, `auth.RequireSpokeToken`) |
| `internal/web/static/` | UI: `index.html`, `app.js`, `styles.css` |
| `internal/metrics/prometheus.go` | сборка `/metrics` по всем кластерам |
| `internal/agent/` | node-agent: CRI-клиент (grpc+cri-api), инвентарь образов, disk-guard, preflight-проба реестра и stall-watchdog pull (`registry.go`), классификация registry-auth ошибок pull (`registry_auth.go`), HTTP/JSON API v1 |
| `internal/provider/k8s_*.go` | `K8sProvider` на client-go: list Deployments/STS/DS и Node в области действия (`WithLabelSelector`/`WithNodeSelector`), слоты image×node, policy-warnings, TTL-кэш снапшота; транспорт к node-agent (snapshot + асинхронный pull с отменой, HTTP/JSON v1, Bearer, таймауты) и `NewK8sProviderFromEnv` |
| `internal/provider/k8s_registry.go` | резолв кредов реестра из `imagePullSecrets` workload'ов: `.dockerconfigjson`/`.dockercfg`, матчинг хоста по правилам distribution, кэш Secret (5 мин / 1 мин на ошибку), индекс image → secret'ы из снапшота |
| `internal/reconcile/` | политика авто-прогрева: очередь, приоритеты (failed > missing, prod > staging > infra), лимиты, backoff |
| `internal/tasks/` | hub-история pull-задач: upsert по cluster+id, фильтр по состоянию, пагинация, ограничение ёмкости (активные не вытесняются), персистентность JSONL (атомная компакция, битые строки пропускаются) |
| `internal/stats/` | опциональная статистика в PostgreSQL (ADR-002): срез по кластерам, diff присутствия образов на нодах, события сборщика мусора, перепулы с лагом «удалён → перетянут», лог pull-задач, samples нод; `Collector` (чистый diff), `Postgres` (SQL), `Run` (цикл hub'а, best effort), `query.go` (`Querier` — чтение для вкладки Stats), `service.go` (`Service`: connect/backoff, состояние enabled/ok, делегирование чтения) |
| `internal/model/stats.go` | типы вкладки Stats: `StatsInfo`, `StatsOverview`, срезы нод/образов/GC/перепулов/задач, страницы и фильтры. Hub-UI контракт, не часть spoke v2 |
| `internal/testutil/` | фейковый spoke/hub, golden-снапшот v2, контрактные тесты |
| `cmd/prepuller/agent.go` | `runAgent()`: флаги агента, запуск HTTP/JSON сервера, graceful shutdown |
| `.github/workflows/ci.yaml` | **Единственный** CI на GitHub: гейты `test` (gofmt, vet, test, build linux/amd64+arm64, гейт зависимостей ADR-001, `govulncheck`) и `helm` (lint/template), затем `image` (buildx amd64+arm64, публикация в GHCR на `GITHUB_TOKEN`, теги `sha-*`/latest/версия, `needs: [test, helm]`) и `chart` (OCI-чарт, только с релизного тега `v*` и `needs: [test, helm, image]`) |
| `hack/e2e-k3d.sh` | Локальный e2e-смоук на k3d: live-провайдер, агенты по CRI, policy violations, деградация/восстановление, метрики |
| `.gitea/workflows/ci.yaml` | CI для Gitea (зеркало `.github/`): те же гейты `test` + `helm`, затем `image` (buildx, публикация в реестр по `vars.REGISTRY`/`vars.IMAGE`) и `chart` с релизного тега `v*` после образа (если задан `vars.CHART_OCI_REPO`) |
| `.github/workflows/pr-agent.yml` | Ревью pull request'ов моделью (PR-Agent): `review` на открытие PR, `command` на комментарий со слэшем (только владелец), `doctor` вручную. Секреты — `OPENAI_API_BASE`/`OPENAI_KEY`; подробности в `docs/pr-agent.md` |
| `Dockerfile` | multi-stage образ: golang:1.26.11 builder → distroless nonroot (65532) |
| `deploy/helm/image-prepuller/` | Helm-чарт: hub (`hub.enabled`), spoke (прямой режим или `spoke.hub.*` outbound), agent DaemonSet, RBAC, ingress, Service метрик агента, `agent.networkPolicy`, `ServiceMonitor` и `PrometheusRule` раздельно для hub и агента, `hub.managedClusters` |
| `deploy/values/hub-cluster.yaml` | шаблон overlay мастер-кластера (Talos): hub + агенты, ingress/cert-manager, toleration control-plane, `autoWarm=false`, `hub.managedClusters` (outbound) и `hub.remoteClusters` (прямой режим) |
| `deploy/values/managed-cluster.yaml` | шаблон overlay ведомого кластера (Talos): `hub.enabled=false`, spoke в outbound-режиме (`spoke.hub.enabled/url`) + агенты, токен для `hub.managedClusters`; реальные overlays — в приватном GitOps-репо |
| `docs/DEPLOY.md` | чеклист деплоя: секреты, k3d-стенд, e2e-сценарий, общий runbook Talos-кластера (§14) |
| `docs/architecture.md` | схемы Mermaid: топология, потоки, состояния, модель данных |
| `docs/diagrams/` | интерактивная схема продукта (archify): `image-prepuller-overview.html` + исходный `image-prepuller-overview.json` + PNG-скриншот для README; генерируется из JSON, не правится руками |
| `docs/alerts.md` | операторская памятка: что скрейпить, правила алертов, триангуляция «агент/токен/связь», пороги |
| `docs/PLAN.md` | план Stage 2–3: workstreams, зоны записи, порядок мержа, гейты |

## 5. Инварианты, которые нельзя ломать

1. **`Provider` v2 реализуют все провайдеры.** Контракт: `Snapshot`,
   `TriggerPull(ctx, PullRequest) (PullTask, error)`, `TriggerWorkloadPull(ctx, WorkloadPullRequest)`,
   `CancelPull(ctx, taskID)`, `SyncNow`, `Health(ctx)`, `PullCounters`. Добавили метод —
   обновите `MockProvider`, `RemoteProvider`, `K8sProvider` (stub) и тесты. Компилятор
   поймает только часть: `RemoteProvider` должен ещё отправить запрос на spoke, а
   spoke-хендлер — принять его.
2. **Hub и spoke разделяют JSON-контракт v2** (`model.APIVersion = "v2"`). Изменили поле
   в `model` — проверьте `server.go`, `server_spoke.go`, `remote.go` и `app.js`.
   Hub обновляется раньше spoke; spoke принимает `apiVersion` `v1`/`v2`/пусто один релиз.
3. **Реестр сохраняет порядок регистрации.** `DefaultID()` — первый зарегистрированный;
   это поведение UI («кластер по умолчанию»). `AllIDs()` сортирует — порядок метрик стабилен.
4. **`TriggerPull` идемпотентен.** Активная задача на ту же пару image+node
   возвращается как есть (тот же `PullTask.ID`), слот в состоянии `pulling` — no-op
   без ошибки. Офлайн-нода — ошибка, а не тихий успех. `CancelPull` отменяет только
   `queued`/`running`; завершённая задача — ошибка, неизвестный id — `provider.ErrTaskNotFound`.
   Отмена `running` в live-режиме закрывает запрос к агенту, агент отменяет CRI
   `PullImage`, задача становится `canceled` (не `failed`), счётчик — `result=canceled`.
5. **Pull-операции асинхронные, включая live-путь.** HTTP-ответ `202` (+ `PullTask`
   `state=running`), результат — только через следующий `Snapshot.Tasks`/`Snapshot.Activity`.
   `K8sProvider.TriggerPull` запускает горутину со своим deadline (`pullTimeout`) и сразу
   возвращает задачу; контекст HTTP-запроса не переживает загрузку. Не делайте хендлер
   блокирующим и не привязывайте pull к `r.Context()`.
6. **Все защищённые ручки hub идут через `requireAuth`.** Публичны только `/healthz`,
   `/readyz`, логин/логаут и статика. `/api/v1/auth/me` проверяет cookie сам.
7. **Spoke-ручки идут через `requireToken`.** Пустой `PREPULLER_CLUSTER_TOKEN` отключает
   проверку — это осознанное поведение для локальной разработки, не «фича для прода».
8. **Секреты и пароли не логируются** и не попадают в ответы API; аудит пишется через `auth.Auditor`, `AuditEvent` не имеет свободных полей. Сравнения — constant-time
   (`crypto/subtle`), как в `auth.go` и spoke-middleware.
9. **Состояние слотов вычисляется в одном месте** (сейчас `MockProvider`, позже
   `K8sProvider`). UI только отображает `PullState`, не выводит статусы сам.
10. **Coverage = `presentSlots / totalSlots`**, считается в провайдере (`Snapshot.Summary`),
    а не в UI.
11. **Agent wire — HTTP/JSON v1.** Пути и поля контракта агента
    (`/healthz`, `/agent/v1/snapshot`, `/agent/v1/images/{ref}`, `/agent/v1/pulls`)
    фиксированы в `internal/agent/server.go`; меняя их, обновите клиент в
    `internal/provider/k8s_agent.go` и тесты обеих сторон. Pull при `diskPaused` —
    `409`, ошибка CRI — `502`, неизвестный образ — `404`. Тело pull-запроса несёт
    необязательный `auth` (`RegistryAuth`: username/password/auth/identityToken/
    registryToken) — креды из `imagePullSecrets` workload'а; пусто = анонимный pull,
    как у пода без секрета. Отклонённые реестром креды агент отдаёт как `502` с
    `{"error":...,"reason":"registry_auth"}`, и провайдер превращает их в
    `provider.ErrAgentRegistryAuth` (не «node agent unavailable»). Добавили поле в
    запрос/ответ — обновите обе стороны и `internal/provider/k8s_registry_test.go`.
12. **Outbound-канал (spoke → hub) — транспорт, а не второй API.** Команда — это ровно тот
    HTTP-вызов, который hub сделал бы напрямую (`SpokeCommand{method,path,body}`),
    и исполняет её собственный handler spoke'а, поэтому контракт `/internal/v1/*` один
    на оба режима: меняя spoke-эндпоинт, вы меняете и его. Hub никогда не аутентифицирует
    пустой токен (в отличие от spoke'а, где пустой токен = «auth выключена»), неизвестный
    кластер — `404`; чтения (snapshot/counters/health) обслуживаются из кэша
    pushed-состояния; доставка команд at-least-once, поэтому исполнение обязано быть
    идемпотентным (spoke дедуплицирует по id команды, `TriggerPull` — по image+node).
    Outbound-режим предполагает ровно одну реплику spoke.

13. **Вкладка Stats — только чтение, опциональна и не тянет pgx в web.**
    `/api/v1/stats/*` ходят через узкий порт `web.StatsSource`; `internal/web` не
    импортирует `internal/stats` (иначе `pgx` попал бы в web транзитивно), типы ответа
    живут в `internal/model/stats.go`. Выключенный DSN — это `200 {enabled:false}` на
    `/stats/status` и `503` с понятным текстом на data-ручках, а не `500`; сырые ошибки
    драйвера наружу не отдаются. У каждой ручки свой таймаут (`5 s` запрос, `3 s` ping),
    `window` клампится в [5m, 90d], `limit` — в 1..200, ответы всегда несут массивы.
    Новый stats-запрос добавляется в `stats.Querier`, `stats.Service`, `web.StatsSource`,
    ручку и тесты (stats + web), а UI берёт его из `/api/v1/stats/*`.
    **Фильтр по кластеру сужает всю вкладку, а не только списки:** `/stats/overview`
    принимает `cluster` и считает по нему все KPI (`model.StatsOverviewQuery`, ответ
    эхом несёт `cluster`), `/stats/nodes` принимает тот же `cluster` и фильтрует на
    сервере; `clusters` в overview остаётся полным списком — селекту нужны все
    варианты, пока один выбран. Не переносите фильтрацию нод в браузер и не
    оставляйте KPI «по всем кластерам»: так уже было, и это читалось как баг.

## 6. Соглашения по коду

- `gofmt` обязателен; имена пакетов короткие, ошибки в нижнем регистре без точки
  (`fmt.Errorf("unknown cluster %q", id)`).
- JSON-теги — camelCase; поля, которые могут отсутствовать, — `omitempty`.
- Комментарии в коде — по-английски и только там, где неочевиден контракт
  (например, комментарии к `PullState`, `Trigger`, `Result` в `model`).
- UI-строки — по-русски (аудитория — русскоязычные ops), технические подписи
  (Present, Pulling, ImageFS) — как в продукте, не переводить.
- В `app.js`: любой пользовательский текст проходит через `esc()` перед вставкой в
  `innerHTML`; обработчики навешиваются после рендера; состояние — в объекте `state`.
- Новые эндпоинты: сначала хендлер в `server.go`, затем метод в `Registry` (если нужен
  выбор кластера), затем вызов из `app.js`, затем строка в README.
- Ошибки API — `{"error": "..."}` через `writeJSON`; статус подбирается по смыслу
  (400 — плохой ввод, 401 — нет сессии, 404 — нет кластера/страницы, 500 — внутренняя).

## 7. Тесты

- `internal/auth/*_test.go` — cookie-flow: валидные/невалидные креды, подпись, подделка; роли (401 vs 403), v1-cookie, rate-limit на фейковых часах, аудит без секретов, `-race`.
- `internal/web/server_test.go` — 401 без cookie, логин, список кластеров, снапшот
  удалённого кластера, Bearer-защита spoke, counters/cancel/apiVersion, наличие
  кластерных метрик. `internal/web/rbac_test.go` — wiring RBAC: viewer 403 на
  state-changing ручки, operator 202, `/api/v1/audit` и 429 при превышении лимита логина.
- `internal/agent/*_test.go` — CRI-адаптер на фейках, инвентарь и disk-guard, HTTP-сервер
  агента (auth, коды 200/400/401/404/409/502), mock-бэкенд без containerd;
  `registry_auth_test.go` — классификация 401/403 от реестра (по словам, а не по
  цифрам в ошибке) и `RegistryAuth.Empty`.
- `internal/provider/remote_test.go` — транспорт hub→spoke на `httptest`: ретраи с backoff, таймауты, типизированные ошибки (502/401/409), health и counters.
- `internal/provider/k8s_test.go` — K8sProvider на fake clientset: workload'ы, слоты,
  policy-warnings, TTL-кэш, health, деградация при ошибке list.
- `internal/provider/k8s_registry_test.go` — резолв кредов из `imagePullSecrets`:
  dockerconfigjson и legacy dockercfg, матчинг хоста (включая Docker Hub), кэш Secret,
  подсказки при ненайденных/нечитаемых секретах, сквозной pull с `auth` и без.
- `internal/reconcile/*_test.go` — правила отбора, приоритеты, лимиты, backoff, `Start`.
- `internal/provider/spoke_hub_test.go` — mailbox outbound-канала: авторизация кластера,
  приём state и staleness, redelivery без ack, отмена ожидающего вызова, маппинг
  HTTP-статусов в sentinel-ошибки, spurious result.
- `internal/web/server_spoke_hub_test.go` — hub-эндпоинты `/internal/v1/spokes/*`:
  401 без/с неверным токеном, 404 для незарегистрированного кластера, приём state,
  выдача команды и её результата, 204 на пустом long-poll, отсутствие маршрутов без
  `WithSpokeHub`.
- `internal/spokeclient/client_test.go` — канал целиком: push состояния, pull через
  hub-провайдер, `SyncNow`, остановка по отмене контекста.
- `cmd/prepuller/main_test.go` — парсинг `PREPULLER_MANAGED_CLUSTERS` (включая токен с
  `=`) и интервала из env.
- `internal/metrics/prometheus_test.go` — новые серии, ровно один `Snapshot()` на кластер,
  offline-кластер.
- `internal/tasks/store_test.go` — upsert и вытеснение (активная задача не удаляется),
  фильтр/пагинация, перезагрузка файла (побеждает последняя версия), битые строки,
  компакция, memory-only режим, конкурентные Observe/Query, отсутствие каталога.
- `internal/web/server_test.go` — дополнительно к API: пагинация и state-фильтр
  `/api/v1/pull-tasks` из истории, fallback на снапшот без истории, 400 на плохие
  `limit`/`state`, `handleSnapshot` пополняет историю.
- `internal/stats/query_test.go` — SQL-чтение на фейковом `database/sql`-драйвере
  с `QueryContext`/rows: `Overview` (несколько запросов, NULL → 0/nil), `Nodes`,
  `Images` (open/removed/поиск/пагинация/lifetime), `GCEvents`, `Repulls`,
  `Tasks` (active/trigger/search); проверяются текст запроса, аргументы (`$n`, без
  интерполяции) и пустые срезы вместо `nil`.
- `internal/stats/service_test.go` — состояния disabled/unavailable/ok, ping в `Info`,
  делегирование чтения, attach/detach в `Service.Run` (фейковый opener).
- `internal/web/server_stats_test.go` — статистика: 401 без cookie, disabled (status 200
  `enabled=false`, data 503), unavailable (enabled+not ok), парсинг
  `window`/`limit`/`offset`/`state`/`trigger`, массивы вместо `null`, отсутствие
  утечки текста ошибки БД.
- `internal/testutil/contract_test.go` — контракт hub↔spoke на фейковом spoke:
  снапшот v2, `TriggerPull`→`PullTask`, идемпотентность, отмена, counters, health,
  заглушки hub-API (`pull-tasks`, `nodes/{node}/images`, `pulls/{id}/cancel`, `health`),
  golden-снапшот `testdata/snapshot_v2.json`. `internal/testutil/reliability_test.go` —
  надёжность spoke: битый JSON, обрыв соединения, context cancel, повторная отмена.
- Правило: любое изменение поведения API, auth, метрик или контракта сопровождается тестом.
  Метрики проверяйте по подстроке с меткой `cluster="..."`.
- Тесты не должны требовать Kubernetes, сети или внешних бинарей: spoke — `httptest`,
  провайдер — `MockProvider`, фикстуры — `internal/testutil`.

## 8. Подводные камни

- **`metrics.Handler` делает ровно один `Snapshot()` на кластер за скрейп** (`(*collector).collect` —
  единственная точка вызова; тест `TestHandlerCallsSnapshotOncePerCluster`). Кэш тяжёлого
  снапшота живёт в провайдере (`K8sProvider` — TTL 2s), а не в метриках.
- **`metrics.Handler` не блокируется дольше 3 s и не слепнет при partition.** `Handler`
  создаёт stateful-коллектор один раз: каждый `Provider.Snapshot` зовётся в своей горутине, все
  кластеры параллельно, общий дедлайн `snapshotDeadline = 3 s` (`internal/metrics/prometheus.go`).
  Не успел — инкремент `prepuller_snapshot_scrape_timeout_total{cluster}`, в вывод идёт последний
  успешный снапшот кластера (со своим `LastSyncAt`, поэтому `prepuller_snapshot_age_seconds`
  растёт — это и есть сигнал «hub ослеп»); кластер, который ни разу не отвечал, не получает
  ни одной серии (никаких нулей), кроме счётчика таймаутов. `PullCounters` качается в той же
  горутине и в тот же дедлайн; не успел — берутся прошлые counters. Брошенная горутина пишет
  в буферизованный канал и получает отменённый после возврата хендлера `r.Context()`, поэтому
  не течёт и быстро отпускает мьютекс провайдера. Замер: мёртвый провайдер — скрейп
  возвращается за 3.00 s (было 5 s × ноды и провал всего скрейпа по `scrape_timeout`).
- **Ошибки node-agent видны на hub: `prepuller_node_agent_errors_total{cluster,node,reason}`.**
  `provider.ClassifyAgentError` сводит ошибку инвентаря к закрытому набору
  `unauthorized|protocol|timeout|unavailable`; счётчики — в `K8sProvider.AgentErrors()`
  (опциональный `provider.AgentErrorCounter`, в замороженный `Provider` его сознательно не
  добавляли). Эмитятся только реально наблюдавшиеся reason'ы. `MockProvider`/`RemoteProvider`
  capability не реализуют — серий нет, это норма. Транспорт оборачивает ошибку
  `http.Client.Do` через `%w`, иначе `context.DeadlineExceeded` терялся и любой таймаут
  классифицировался бы как `unavailable`. `prepuller_node_last_seen_timestamp_seconds` не
  эмитится при нулевом `LastSeen`, `prepuller_snapshot_age_seconds` — при нулевом `LastSyncAt`
  (для ни разу не синхронизировавшегося кластера используйте `prepuller_cluster_up`).
- **Метрики агента — отдельный порт `:9444` без авторизации** (`-metrics-addr`,
  `PREPULLER_AGENT_METRICS_ADDR`; пусто выключает листенер). На скрейпе агент не ходит в CRI и
  в сеть: `prepuller_agent_cri_up` и ImageFS/кэш — значения последнего успешного снапшота,
  `prepuller_agent_last_contact_timestamp_seconds{endpoint}` инициализируются временем старта
  процесса (поэтому «к агенту никто не ходит» алертится сразу), `prepuller_agent_up` — dead-man
  (серия исчезает вместе с процессом). `prepuller_agent_requests_total{code}` считает и 401 от
  `withAuth`, что и отличает ротацию токена от сетевой аварии. Порт открыт на всех нодах
  (`hostNetwork`), NetworkPolicy его не закрывает — это ожидаемо, см. `docs/alerts.md`.
- **`K8sProvider` без node-agent**: слоты `offline`, `syncStatus=degraded`, `partial=true` —
  это ожидаемо (агент не отдал инвентарь), а не баг. Pull в live-режиме идёт через агент.
- **K8sProvider ходит в агент по HTTP на :9443** (`WithAgentPort/Token/Timeout/Scheme`, адрес —
  `Node.status.addresses` InternalIP). Pull асинхронный: `TriggerPull` регистрирует задачу,
  запускает горутину с `pullTimeout` и сразу отдаёт `running`; `CancelPull` для running
  отменяет контекст горутины (агентский HTTP-запрос закрывается, CRI `PullImage` на ноде
  отменяется), для завершённой задачи → «already finished», для неизвестного →
  `ErrTaskNotFound` (404). 409 от агента = диск-пауза → failed-задача и
  `errors.Is(err, provider.ErrAgentDiskPaused)`; недоступный агент — `502` (`ErrAgentUnavailable`).
  `finishTask` переводит задачу только из активного состояния, поэтому гонка «отмена vs
  завершение» не перезаписывает `canceled` на `failed` и не пишет лишний счётчик.
  `k8s_nodes.go` опрашивает ноды последовательно, поэтому при массовом недоступе агентов
  снапшот тормозит до `WithAgentTimeout` (5 s) на ноду.
- **Приватный реестр: креды едут в запросе pull, а не в CRI-конфиге ноды.** Агент
  тянет через CRI, поэтому `imagePullSecrets` пода к нему не применяются и pull
  анонимен — приватный реестр отвечает `403`, образ оказывается `present` только на
  той ноде, где его уже тянул kubelet, а на остальных `missing` с пачкой failed-задач
  `failed to fetch anonymous token … 403 Forbidden`. Провайдер строит индекс
  image → `imagePullSecrets` в `buildSnapshot` (без лишних list'ов) и резолвит
  Secret в namespace workload'а: `.dockerconfigjson` и legacy `.dockercfg`, матчинг
  хоста по правилам distribution (Docker Hub — в любом написании, включая
  `https://index.docker.io/v1/`), кэш 5 мин (ошибки — 1 мин). Креды уезжают в
  `POST /agent/v1/pulls` и в CRI `AuthConfig`. Нужны RBAC `get` на secrets
  (`rbac.readSecrets`, по умолчанию `true`, только get — без list/watch) и новый
  образ у hub/spoke **и** у агента: старый агент неизвестное поле `auth` игнорирует и
  тянет анонимно. Секреты в логах и в ответах API не появляются: наружу уходит только
  текст ошибки и подсказка, какие Secret'ы не нашлись/не прочитались.
- **Pull-гигиена агента: probe реестра + stall watchdog.** Pull синхронный, и CRI не даёт
  прогресса, поэтому зависший реестр раньше держал слот до `DefaultAgentPullTimeout`
  (10 min) и выглядел в UI как «медленная загрузка». Теперь агент сначала делает TCP-пробу
  реестра (`Config.RegistryProbeTimeout`, 5 s; `-registry-probe=false` выключает — нужно при
  pull через registry mirror или containerd-прокси: агент ходит напрямую, containerd — нет) и
  отдаёт `502` с `ErrRegistryUnreachable`; затем, если pull идёт,
  watchdog сэмплирует `ImageFS.UsedBytes` и через `PullStallTimeout` (60 s) без роста отменяет
  pull и отдаёт `504` с `errPullStalled`. `DefaultPullTimeout` (10 min) остаётся страховкой для
  честно долгих загрузок. Hub: 504 → `provider.ErrAgentPullStalled` → HTTP 504
  (`pullErrorStatus`), задача сразу `failed`. Watchdog fail-open: если `ImageFS` не отвечает,
  pull не валится. Сэмплирование `ImageFS` — единственный сигнал: не выключайте
  `-pull-stall-watch`, не предложив другой; e2e проверяет отказ по мёртвому реестру
  (`hack/e2e-k3d.sh` §9, < 60 s).
- **Чарт и приложение версионируются одной версией.** `Chart.version == appVersion`,
  и релизный тег обязан совпасть с обоими (`v1.0.0` → `1.0.0`): helm-гейт CI ловит
  расхождение на PR, job'ы `image`/`chart` — на теге. Образ по умолчанию —
  `Chart.AppVersion` (`ip.imageTag`: `values.image.tag` → `appVersion`), фолбэка на
  `latest` в чарте нет. Чарт из `main` передаёт агенту флаги `agent.pull.*`
  (`-pull-timeout`, `-registry-probe-timeout`, `-pull-stall-timeout`), которых нет
  в образе прошлого релиза: такая пара — `CrashLoopBackOff` с
  `flag provided but not defined: -pull-timeout` и все ноды `offline`. Поэтому
  раскатка с `main` всегда пиннит `image.tag=sha-<тот же коммит>`, а релизная
  пара — OCI-чарт релиза + его образ. OCI-чарт публикуется только с релизного
  тега (`needs: [test, helm, image]`), пуш в `main` даёт только образ
  (`sha-<12>` + `latest`); в Helm-релизе видны обе версии — chart и app version
  (`app.kubernetes.io/version`). Между релизами `Chart.version` не бампайте:
  `main` несёт номер ближайшего релиза, бамп обоих полей — в релизном коммите
  (опубликованные чарт-теги 0.2.0/0.3.0/0.6.0 старые, нумерация не должна
  убывать: текущая линия — 1.1.0/1.1.0, следующий релиз — 1.1.1 или 1.2.0).
  Бампать вручную не нужно: `./release.sh X.Y.Z` (корень репозитория) проверяет
  чистоту дерева и синхронность `main` с `origin`, поднимает `version`/
  `appVersion`/`internal/agent.Version`, закрывает `[Unreleased]` в CHANGELOG,
  гоняет локальные гейты и только потом коммитит, тегирует и пушит.
  `internal/agent.Version` идёт той же линией и печатается по
  `prepuller agent -version`; на `main` он несёт номер ближайшего релиза.
- **Outbound-канал: «сейчас» в UI отстаёт на syncInterval.** Hub отдаёт UI и метрикам
  последний pushed-снапшот (по умолчанию каждые 3 s, плюс TTL провайдера 2 s), а не делает
  запрос на каждый скрейп. Молчащий дольше `max(3*syncInterval, 10 s)` spoke считается
  offline: `prepuller_cluster_up=0`, `prepuller_cluster_sync_partial=1`, `PullCounters`
  = `nil`. `prepuller_snapshot_age_seconds` у замолчавшего outbound-кластера **не растёт**:
  `PushProvider.Snapshot` сразу отдаёт offline-заглушку с нулевым `LastSyncAt`, а такие
  серии не эмитятся (растёт только кэш последнего успешного снапшота при таймауте скрейпа).
  `Health.LatencyMs` на этом канале всегда 0 — RTT hub не меряет, сигнал — возраст push и
  `PushProvider.SyncAge()`.
- **Команда — 5 s на попытку.** `SpokeHubConfig.CommandTimeout` — таймаут одного
  round trip (очередь long-poll + исполнение), он играет роль 5-секундного сетевого
  таймаута прямого режима, а retry-политика state-changing вызовов остаётся сверху (до 3
  попыток). Живой pull на spoke стартует асинхронно (202 + running-задача) и в бюджет
  укладывается; загрузка идёт в фоне, поэтому командный транспорт её больше не ограничивает.
  Не растягивайте таймаут до минут — web-хендлер ждёт round trip.
- **Redelivery at-least-once.** Неподтверждённая команда переотправляется через 10 s;
  spoke помнит результаты последних 512 id и повторно их только переотправляет. Меняете
  формат команды — правьте обе стороны (`spoke_wire.go`, `spokeclient`), иначе получите
  двойное исполнение.
- **Прямой и outbound режимы сосуществуют, но не для одного кластера.**
  `hub.remoteClusters` (`name=url=token`) — прямой; `hub.managedClusters` (`name=token`)
  вместе с `spoke.hub.*` — outbound. Одно имя в обоих списках даст два провайдера на один
  UI-id (последняя регистрация перезапишет первую).
- **Область действия — это фильтр на входе, а не скрытие в UI.** `WithLabelSelector`
  уходит в `ListOptions` для Deployments/STS/DS, `WithNodeSelector` — для Nodes, поэтому
  вне области не существует ни workload'ов, ни слотов: не показывать «на всякий случай»
  нельзя, их просто нет в снапшоте. Метка ставится на объект workload'а (Deployment/STS/DS),
  не на pod template. `MockProvider` демо-данные не фильтрует — это только UI-режим.
  Пустая область — это **пустые массивы, а не `null`**: `model.Snapshot.Normalize()`
  вызывается в провайдерах и на HTTP-границах (hub и spoke), потому что UI делает
  `.map`/`.length` по `workloads`/`nodes` и падает на `null`. Новый провайдер или
  новый сериализатор снапшота обязан сохранить это свойство (тесты
  `TestSnapshotNormalizeJSON`, `TestK8sProviderEmptyScopeKeepsArrays`,
  `TestSnapshotHandlerEmitsArrays`).
- **Чарт 0.6.0: `hub.workloadSelector`/`spoke.workloadSelector` по умолчанию `prepull=true`.**
  Это поведенческое (ломающее) изменение: без метки workload'ы не видны и не прогреваются.
  Снять ограничение — `--set hub.workloadSelector=`.
- **Чарт 1.0.0: область действия нод и `mock` заданы дефолтами чарта.**
  `hub.nodeLabelSelector` и `spoke.nodeLabelSelector` по умолчанию
  `!node-role.kubernetes.io/control-plane` (агенты — только на воркерах): без этого
  control-plane ноды всю жизнь висели в UI как offline-слоты, а снапшот — `partial`.
  Overlay'и оставляют значение явно только как напоминание. `spoke.mock` по умолчанию
  `false`: раскатка не поднимает демо-данные; `true` — только витрина/UI-разработка.
  Helm-гейт CI проверяет, что дефолтный рендер несёт оба селектора и нигде нет `-mock`.
  Нужен control-plane — `agent.tolerations` **и** пустой селектор.
- **Ломающие изменения values 0.4.0 → 0.5.0**: мониторинг переехал в раздел
  компонента, чьи серии он покрывает — `serviceMonitor.*` → `hub.serviceMonitor.*`,
  `prometheusRule.*` → `hub.prometheusRule.*`, `agent.serviceMonitor.*` →
  `agent.metrics.serviceMonitor.*`, `agent.prometheusRule.*` →
  `agent.metrics.prometheusRule.*` (порог `agentNoContact` — в
  `agent.metrics.prometheusRule.thresholds`). Монитор агента и его правила требуют
  `agent.metrics.enabled=true`, монитор hub'а — `hub.enabled=true`; без этого рендер
  падает с сообщением. Кластерные правила (`hub.prometheusRule`) по-прежнему
  рендерятся и для spoke-only release: spoke отдаёт те же серии на `:9091`.
- **Значения чарта 0.3.0 → 0.4.0 только добавлены** (`hub.managedClusters`,
  `spoke.hub.enabled/url/syncInterval`, ключ `managed-clusters` в Secret) — старые
  overrides продолжают работать. `hub.managedClusters` — строка формата
  `PREPULLER_MANAGED_CLUSTERS`, а не map; `existingSecret` должен содержать ключ
  `managed-clusters`, иначе принимать outbound-кластеры нечем.
- **Ограничения outbound-чарта проверяются на рендере:** `spoke.hub.enabled` без
  `spoke.hub.url` и `spoke.hub.enabled` при `spoke.replicaCount > 1` падают с понятным
  сообщением. В outbound-режиме Service spoke'а нужен только для метрик и проб — ingress
  и внешняя экспозиция не нужны.
- **Values чарта ломаются между 0.2.0 и 0.3.0**: `networkPolicy.enabled`
  переехал в `agent.networkPolicy.enabled`, `ingress.*` — в `hub.ingress.*`
  (ingress выставляет только hub), `prometheusRule` разделён на `prometheusRule`
  (hub/cluster) и `agent.prometheusRule`, добавлены `hub.enabled`,
  `serviceMonitor`, `agent.serviceMonitor`. Шаблоны overlay'ев переехали в
  `deploy/values/hub-cluster.yaml` и `deploy/values/managed-cluster.yaml`.
  Старые overrides молча не применятся — это повод один раз пройтись по ним.
- **Talos: `imageFSCapacityBytes=0`.** `ImageFsInfo` в containerd на Talos отдаёт
  только used. Провайдер компенсирует это для отображения: если агент вернул
  ёмкость 0, `k8s_nodes.go` берёт `ephemeral-storage` из `Node.status.capacity` и
  выставляет `ImageFSCapacityEstimated=true` (в UI — `~` и бейдж `est.`); значение
  агента всегда важнее, а без `AgentUp` фолбэк не включается. Disk-guard **агента**
  (`ShouldPause`/`UsedRatio`) от этого не меняется и на Talos по-прежнему
  fail-open (нулевая ёмкость = «не пауза»), так что пауза авто-прогрева там не
  срабатывает. `prepuller_node_imagefs_used_bytes` есть — алертить надо на диск
  ноды, а не на паузу pull'ов; stall watchdog не страдает (он тоже смотрит на used).
  Control-plane ноды в overlay вне области действия (`hub.nodeLabelSelector:
  "!node-role.kubernetes.io/control-plane"`, агенты — только на воркерах), поэтому
  их taint и отсутствие агента не дают `offline`-слотов. Нужен control-plane —
  верните `agent.tolerations` **и** очистите `nodeLabelSelector`.
- **`RemoteProvider.PullCounters()` ходит в `GET /internal/v1/counters`** (contract v2);
  при недоступном spoke возвращает `nil` — это по-прежнему значит «метрик нет», а не «ноль».
- **Транспорт hub→spoke ретраит только идемпотентные state-changing вызовы** (pull, workload-pull, cancel, reconcile): 3 попытки, backoff 150 ms → 2 s, таймаут 5 s на попытку. `Snapshot`, `counters` и `health` ходят одним запросом, чтобы мёртвый spoke не тормозил UI и скрейп. `ErrSpokeUnavailable`/`ErrSpokeUnauthorized` → `502` (`pullErrorStatus`), `ErrTaskNotActive` → `409`.
- **`RemoteProvider.Snapshot` глотает ошибки**: при недоступном spoke возвращается пустой
  снапшот с `SyncStatus: offline`, `Partial: true` и `time.Now()`. Признак недоступности —
  `TotalWorkers == 0` или `SyncStatus == offline`; не считайте такой кластер «здоровым».
- **История pull-задач — hub'ская, не провайдерская.** `internal/tasks.Store`
  (`-pull-history-file`, `-pull-history-limit`, чарт `hub.pullHistory`) хранит
  последние N задач по всем кластерам в памяти + JSONL, кормится каждым
  `/api/v1/snapshot` и `/api/v1/pull-tasks` (upsert по cluster+id, поэтому
  переходы состояния не дублируются) и отдаёт страницы через
  `/api/v1/pull-tasks?limit=&offset=&state=`. Провайдер по-прежнему держит свои
  ~200 задач — это не замена истории, а источник для неё: если hub перезапустили и
  файла нет, более старые задачи исчезнут. Запись best-effort: ошибки файла только
  логируются, hub стартует с `memory only`. `readOnlyRootFilesystem: true` требует
  монтирования каталога — чарт монтирует emptyDir/PVC в `/var/lib/prepuller`.
- **Pull-задачи в mock ограничены 200 последними, activity — 100.** Пагинации нет;
  не снимайте лимиты, не добавив её (hub-история выше снимает это ограничение для UI).
- **`apiVersion` в spoke-запросах**: пусто и `v1` принимаются один релиз, всё остальное —
  `400 unsupported apiVersion`. Меняя `model.APIVersion`, обновите `validAPIVersion`
  в `server_spoke.go` и `RemoteProvider`.
- **Модульные кэши**: в песочнице `$HOME` read-only — используйте
  `GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod`.
  `.gopath/` в `.gitignore`, в историю не коммитится.
- **`auth.NewManager` падает без логина/пароля** — hub без `PREPULLER_USERS` или
  `PREPULLER_AUTH_USER`/`_PASS` не стартует. Это ожидаемо.
- **Случайный session secret** без `PREPULLER_SESSION_SECRET` означает разлогин после
  рестарта и несовместимость нескольких реплик.
- **Без `-mock` запускается live-путь**: провайдер собирается `NewK8sProviderFromEnv`
  (in-cluster конфиг → `$KUBECONFIG` → `~/.kube/config`) и падает на старте, если конфига
  нет — это ожидаемо, а не баг. Для UI-разработки по-прежнему `-mock`.
- **`.gocache/`, `bin/`, `.gomodcache/` в корне** — локальные кэши сборки и модулей,
  они в `.gitignore` и в историю не попадают; не коммитьте их принудительно и не удаляйте
  вслепую, если в них завязан прогон. `bin/prepuller` — артефакт сборки.
- **Вкладка Stats: disabled ≠ unavailable.** Нет DSN — `/stats/status` отвечает
  200 `{enabled:false}` (UI: «Сбор статистики выключен администратором»), и data-ручки
  намеренно отдают 503: не превращайте это в 500 и не пытайтесь ходить в БД «на всякий
  случай». DSN есть, но база молчит — `enabled=true, ok=false` + `error` для UI,
  данные не отдаются; сырой текст ошибки драйвера (host/DSN) наружу не выпускается.
  `Service.Info` пингует БД, поэтому вызывается только из `/stats/status` с таймаутом 3 s.
- **Stats-окно и лимиты — это защита от тяжёлых запросов.** `window` клампится в
  [5m, 90d] (по умолчанию 24h), `limit` — в 1..200 (по умолчанию 50), `q` обрезается до
  200 символов; всё это делает HTTP-слой до SQL (`internal/web/server_stats.go`), а SQL
  дополнительно защищается от отрицательных limit/offset. Не снимайте клампы, не добавив
  индексы: таблицы маленькие только пока retention 90 дней.
- **git обязателен.** Работа идёт через ветки и worktree (`docs/PLAN.md` §3, §6.1);
  baseline Stage 1 — коммит `18aaaa5`, ADR-001 — `216e0d2`, контракт Stage 2/3 —
  тег `contract-v2` (от него режутся все feature-ветки).
- **Мок-состояния связаны с именами**: `talos-worker-03` = 80% диска (пауза,
  `DiskPressure=True`), `talos-worker-04` = NotReady, `ml/scorer` на `worker-02` =
  `failed` (401). Меняя демо-данные, правьте и README/docs, иначе скриншоты и
  документация расходятся. Условия нод в mock намеренно разные по полярности
  (`Ready=True` — хорошо, `DiskPressure=True` — плохо): UI красит их по типу условия,
  а не по значению.

## 9. Definition of Done

- [ ] `gofmt -l .` пуст, `go vet ./...` и `go test ./...` зелёные.
- [ ] `go build -o bin/prepuller ./cmd/prepuller` проходит.
- [ ] `helm lint deploy/helm/image-prepuller` (defaults, `deploy/values/hub-cluster.yaml`, `deploy/values/managed-cluster.yaml`) и `helm template` проходят: defaults, полный набор (`spoke`, `agent`, `hub.ingress`, `agent.networkPolicy`, `hub.serviceMonitor`, `hub.prometheusRule`, `agent.metrics.serviceMonitor`, `agent.metrics.prometheusRule`, `--api-versions monitoring.coreos.com/v1`), область действия (`-workload-selector=prepull=true`, `-node-selector=!node-role.kubernetes.io/control-plane`; пустые селекторы не рендерят флаг), а также `hub.enabled=false`, `agent.enabled=false`, оба overlay'я и outbound-вариант (`spoke.enabled=true`, `spoke.hub.enabled=true`, `spoke.hub.url` задан, `hub.enabled=false`) — компоненты должны реально исчезать из рендера, managed-кластер не рендерить ни Ingress, ни hub-контейнер, outbound-рендер содержать `-hub-url`, а пустой `spoke.hub.url` и `spoke.replicaCount>1` — падать.
- [ ] Изменённое поведение отражено в `README.md` (API/флаги/метрики) и, если затронута
  архитектура, в `docs/architecture.md`.
- [ ] Новые эндпоинты и middleware покрыты тестами; UI-путь проверен вручную
  (`-mock`, логин, pull, переключение кластера).
- [ ] Не добавлено внешних зависимостей вне границ ADR-001/ADR-002 и шагов сборки фронтенда.
- [ ] Статистика (если тронута): `go test ./internal/stats/` зелёный без реальной БД,
  `helm template --set hub.stats.enabled=true --set hub.stats.dsn=...` содержит env/флаги,
  а `hub.stats.enabled=true` без DSN падает на рендере.
- [ ] Вкладка Stats (если тронута): `go test ./internal/web/ ./internal/stats/` зелёные;
  при выключенном DSN UI показывает «Сбор статистики выключен администратором», при
  недоступной БД — баннер с «Повторить»; все stats-ответы содержат массивы, а не `null`;
  `node --check internal/web/static/app.js` чистый.

## 10. Документация

- Пользовательская — `README.md`: возможности, архитектурная схема, запуск, флаги, env,
  API, метрики, roadmap.
- Техническая — `docs/architecture.md`: топология, компоненты, auth-потоки, модель данных,
  состояния, pull-путь, метрики, план Stage 2–3.
- Архитектурные решения — `docs/adr/`: `0001-dependencies.md` (Accepted: grpc+cri-api
  в агенте, client-go в K8s-провайдере, GOPROXY-зеркало вместо vendor) и
  `0002-postgres-statistics.md` (Accepted: `pgx/v5` только в `internal/stats` для
  опциональной статистики). Новая зависимость без ADR не принимается.
- Схемы — только Mermaid в markdown (рендерится в Gitea/GitHub/Obsidian); PNG-вставки
  допустимы только для главной схемы в README (см. исключение ниже) и должны
  пересобираться из JSON, а не рисоваться вручную. Меняя код, проверяйте, что диаграммы
  не врут.
- Исключение — `docs/diagrams/`: интерактивные HTML-схемы, собранные из JSON
  skill'ом `archify` (`node bin/archify.mjs deliver architecture <json> <html>`).
  Правится только JSON, HTML пересобирается; JSON и HTML коммитятся парой.
  Главная схема README — та же HTML-схема, снятая скриншотом
  (`archify visual-check`, кадр `*.visual-check.2048x1320.light.png`, обрезка панели
  и карточек) в `docs/diagrams/image-prepuller-overview.png`. Меняя диаграмму,
  пересоберите и PNG: `node bin/archify.mjs validate|deliver architecture
  docs/diagrams/image-prepuller-overview.json` → `visual-check` → обрезать кадр.

## 11. Параллельная работа в нескольких worktree

Подробный план — [docs/PLAN.md](docs/PLAN.md). Если работа идёт в отдельной сессии
и отдельном worktree, действуют дополнительные правила:

1. **Одна сессия — одна зона записи** из таблицы §5 плана (WS-A…WS-H). Файлы чужой
   зоны не трогать: нужна правка — задача интегратору (`lead`), а не «попутный фикс».
2. **Общие файлы принадлежат интегратору:** `internal/model/**`,
   `internal/provider/provider.go`, `internal/provider/registry.go`,
   `internal/web/server.go`, `cmd/prepuller/main.go`, `go.mod`, `go.sum`,
   `README.md`, `AGENTS.md`, `docs/architecture.md`.
3. **Contract freeze.** После тега `contract-v2` изменения контракта — отдельный PR
   от интегратора; feature-ветки ребейзятся на него, а не «договариваются».
4. **Ветка и worktree:** в песочнице DSH писать можно только внутри workspace, поэтому
   worktree создаются внутри репозитория: `git worktree add .worktrees/ip-<ws> -b ws/<ws>`
   от `main` (`.worktrees/` в `.gitignore`); ежедневно `git rebase main`; живёт ≤ 3 дней.
   В каждом worktree перед первой сборкой:
   `export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOMODCACHE=$PWD/.gopath/pkg/mod && go mod download`.
5. **Гейты ветки:** `go build ./... && go vet ./... && go test ./...`,
   `gofmt -l $(git diff --name-only main... -- '*.go')` пусто, новых зависимостей нет
   (`go list -m all` сверяется с `main`).
6. **Не форматировать чужие файлы** и не запускать `gofmt -w ./...` — это конфликты