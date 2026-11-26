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

## 2. Быстрые команды

```bash
go build -o bin/prepuller ./cmd/prepuller     # сборка
go test ./...                                  # тесты (auth, web, контрактные testutil)
go vet ./...                                   # статический анализ
gofmt -l .                                     # должно быть пусто
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

## 3. Ограничения стека

- **Go 1.24** в `go.mod` (локально может быть новее — не поднимайте директиву `go` без причины).
- **Зависимости — только по ADR-001** ([docs/adr/0001-dependencies.md](docs/adr/0001-dependencies.md)).
  Разрешённые импорты по пакетам:

  | Пакет | Можно импортировать |
  | --- | --- |
  | `internal/agent` | `google.golang.org/grpc`, `k8s.io/cri-api`, stdlib |
  | `internal/provider/k8s*.go` | `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`, stdlib |
  | `internal/web`, `internal/model`, `internal/auth`, `internal/metrics` | только stdlib |
  | `internal/provider/{provider,registry,mock,remote}.go` | только stdlib |
  | `internal/reconcile` | stdlib + `internal/model`, `internal/provider` |
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
| `internal/provider/remote.go` | hub → spoke по `/internal/v1/*` с Bearer |
| `internal/auth/**` | cookie-сессии с ролью (HMAC-SHA256), RBAC (`RequireAuth`/`RequireRole`/`RequireOperator`), rate-limit логина, аудит; `users.go`, `roles.go`, `audit.go`, `ratelimit.go` |
| `internal/auth/middleware.go` | `RequireAuth` (hub cookie), `RequireToken` (spoke Bearer), `RequireRole`/`RequireOperator` |
| `internal/web/server.go` | hub-роутер, hub-хендлеры, `writeJSON` |
| `internal/web/server_spoke.go` | spoke-роутер `/internal/v1/*` (contract v2) |
| `internal/web/static/` | UI: `index.html`, `app.js`, `styles.css` |
| `internal/metrics/prometheus.go` | сборка `/metrics` по всем кластерам |
| `internal/agent/` | node-agent: CRI-клиент (grpc+cri-api), инвентарь образов, disk-guard, HTTP/JSON API v1 |
| `internal/provider/k8s_*.go` | `K8sProvider` на client-go: list Deployments/STS/DS и Node, слоты image×node, policy-warnings, TTL-кэш снапшота; транспорт к node-agent (snapshot + pull, HTTP/JSON v1, Bearer, таймауты) и `NewK8sProviderFromEnv` |
| `internal/reconcile/` | политика авто-прогрева: очередь, приоритеты (failed > missing, prod > staging > infra), лимиты, backoff |
| `internal/testutil/` | фейковый spoke/hub, golden-снапшот v2, контрактные тесты |
| `cmd/prepuller/agent.go` | `runAgent()`: флаги агента, запуск HTTP/JSON сервера, graceful shutdown |
| `.gitea/workflows/ci.yaml` | CI: gofmt, vet, test, build linux/amd64+arm64 |
| `docs/architecture.md` | схемы Mermaid: топология, потоки, состояния, модель данных |
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
5. **Pull-операции асинхронные.** HTTP-ответ `202` (+ `PullTask`), результат — только через
   следующий `Snapshot.Tasks`/`Snapshot.Activity`. Не делайте хендлер блокирующим.
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
    `409`, ошибка CRI — `502`, неизвестный образ — `404`.

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
  агента (auth, коды 200/400/401/404/409/502), mock-бэкенд без containerd.
- `internal/provider/k8s_test.go` — K8sProvider на fake clientset: workload'ы, слоты,
  policy-warnings, TTL-кэш, health, деградация при ошибке list.
- `internal/reconcile/*_test.go` — правила отбора, приоритеты, лимиты, backoff, `Start`.
- `internal/metrics/prometheus_test.go` — новые серии, ровно один `Snapshot()` на кластер,
  offline-кластер.
- `internal/testutil/contract_test.go` — контракт hub↔spoke на фейковом spoke:
  снапшот v2, `TriggerPull`→`PullTask`, идемпотентность, отмена, counters, health,
  заглушки hub-API (`pull-tasks`, `nodes/{node}/images`, `pulls/{id}/cancel`, `health`),
  golden-снапшот `testdata/snapshot_v2.json`.
- Правило: любое изменение поведения API, auth, метрик или контракта сопровождается тестом.
  Метрики проверяйте по подстроке с меткой `cluster="..."`.
- Тесты не должны требовать Kubernetes, сети или внешних бинарей: spoke — `httptest`,
  провайдер — `MockProvider`, фикстуры — `internal/testutil`.

## 8. Подводные камни

- **`metrics.Handler` делает ровно один `Snapshot()` на кластер за скрейп** (`collectClusters` —
  единственная точка вызова; тест `TestHandlerCallsSnapshotOncePerCluster`). Кэш тяжёлого
  снапшота живёт в провайдере (`K8sProvider` — TTL 2s), а не в метриках.
- **`K8sProvider` без node-agent**: слоты `offline`, `syncStatus=degraded`, `partial=true` —
  это ожидаемо (агент не отдал инвентарь), а не баг. Pull в live-режиме идёт через агент.
- **K8sProvider ходит в агент по HTTP на :9443** (`WithAgentPort/Token/Timeout/Scheme`, адрес —
  `Node.status.addresses` InternalIP). Pull синхронный: `CancelPull` для выполняющегося →
  `ErrCancelNotSupported` (409), для завершённого → «already finished», для неизвестного →
  `ErrTaskNotFound` (404). 409 от агента = диск-пауза → failed-задача и
  `errors.Is(err, provider.ErrAgentDiskPaused)`; недоступный агент — `502` (`ErrAgentUnavailable`).
  `k8s_nodes.go` опрашивает ноды последовательно, поэтому при массовом недоступе агентов
  снапшот тормозит до `WithAgentTimeout` (5 s) на ноду.
- **`RemoteProvider.PullCounters()` ходит в `GET /internal/v1/counters`** (contract v2);
  при недоступном spoke возвращает `nil` — это по-прежнему значит «метрик нет», а не «ноль».
- **`RemoteProvider.Snapshot` глотает ошибки**: при недоступном spoke возвращается пустой
  снапшот с `SyncStatus: offline`, `Partial: true` и `time.Now()`. Признак недоступности —
  `TotalWorkers == 0` или `SyncStatus == offline`; не считайте такой кластер «здоровым».
- **Pull-задачи в mock ограничены 200 последними, activity — 100.** Пагинации нет;
  не снимайте лимиты, не добавив её.
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
- **git обязателен.** Работа идёт через ветки и worktree (`docs/PLAN.md` §3, §6.1);
  baseline Stage 1 — коммит `18aaaa5`, ADR-001 — `216e0d2`, контракт Stage 2/3 —
  тег `contract-v2` (от него режутся все feature-ветки).
- **Мок-состояния связаны с именами**: `talos-worker-03` = 80% диска (пауза),
  `talos-worker-04` = NotReady, `ml/scorer` на `worker-02` = `failed` (401). Меняя демо-данные,
  правьте и README/docs, иначе скриншоты и документация расходятся.

## 9. Definition of Done

- [ ] `gofmt -l .` пуст, `go vet ./...` и `go test ./...` зелёные.
- [ ] `go build -o bin/prepuller ./cmd/prepuller` проходит.
- [ ] Изменённое поведение отражено в `README.md` (API/флаги/метрики) и, если затронута
  архитектура, в `docs/architecture.md`.
- [ ] Новые эндпоинты и middleware покрыты тестами; UI-путь проверен вручную
  (`-mock`, логин, pull, переключение кластера).
- [ ] Не добавлено внешних зависимостей вне границ ADR-001 и шагов сборки фронтенда.

## 10. Документация

- Пользовательская — `README.md`: возможности, архитектурная схема, запуск, флаги, env,
  API, метрики, roadmap.
- Техническая — `docs/architecture.md`: топология, компоненты, auth-потоки, модель данных,
  состояния, pull-путь, метрики, план Stage 2–3.
- Архитектурные решения — `docs/adr/`: `0001-dependencies.md` (Accepted: grpc+cri-api
  в агенте, client-go в K8s-провайдере, GOPROXY-зеркало вместо vendor).
  Новая зависимость без ADR не принимается.
- Схемы — только Mermaid в markdown (рендерится в Gitea/GitHub/Obsidian); PNG-вставки и
  внешние редакторы не нужны. Меняя код, проверяйте, что диаграммы не врут.

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
   у всех остальных веток.
7. **Документация:** README/AGENTS/architecture в feature-ветке не правятся; в описании
   PR — готовый «doc snippet» (раздел, строки, текст), применяет интегратор.
8. **Мелкие PR и мерж по одному** через интеграционную ветку; порядок мержа — §9 плана.
