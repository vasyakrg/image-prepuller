# AGENTS.md

Правила работы над **image-prepuller** для людей и AI-агентов. Читать целиком перед
первым изменением кода.

## 1. Что это

Сервис прогрева image-кэша на worker-нодах Kubernetes. Один Go-бинарь, две роли:
`hub` (Web UI + cookie-авторизация + реестр кластеров + Prometheus) и `spoke`
(headless backend API рядом с кластером, Bearer-токен).

Текущий этап — **Stage 1**: единственный рабочий провайдер — `MockProvider`.
Живой Kubernetes/CRI — Stage 2–3 (`cmd/prepuller/main.go` намеренно падает с
`live Kubernetes provider is Stage 3` при запуске без `-mock`).

## 2. Быстрые команды

```bash
go build -o bin/prepuller ./cmd/prepuller     # сборка
go test ./...                                  # тесты
go vet ./...                                   # статический анализ
gofmt -l .                                     # должно быть пусто
GOCACHE=$PWD/.gocache go test ./...            # если GOCACHE недоступен (песочница/права)
GOCACHE=$PWD/.gocache go build -o bin/prepuller ./cmd/prepuller
```

Если сборка падает с `operation not permitted` при обращении к `~/Library/Caches/go-build`,
используйте `GOCACHE=$PWD/.gocache` для всех команд `go`.

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
- **Только стандартная библиотека.** Ноль внешних зависимостей, нет `go.sum`, нет
  `node_modules`, нет сборки фронтенда. Не добавляйте client-go, prometheus/client_golang,
  фреймворки роутинга или UI-библиотеки, пока задача прямо этого не требует.
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
| `internal/auth/auth.go` | cookie-сессии: HMAC-SHA256, TTL, constant-time |
| `internal/web/server.go` | hub- и spoke-роутеры, middleware, JSON-хелперы |
| `internal/web/static/` | UI: `index.html`, `app.js`, `styles.css` |
| `internal/metrics/prometheus.go` | сборка `/metrics` по всем кластерам |
| `docs/architecture.md` | схемы Mermaid: топология, потоки, состояния, модель данных |
| `docs/PLAN.md` | план Stage 2–3: workstreams, зоны записи, порядок мержа, гейты |

## 5. Инварианты, которые нельзя ломать

1. **`Provider` реализуют все провайдеры.** Добавили метод — обновите `MockProvider`,
   `RemoteProvider` и тесты. Компилятор поймает только часть: `RemoteProvider` должен
   ещё отправить соответствующий запрос на spoke.
2. **Hub и spoke разделяют JSON-контракт.** Изменили поле в `model` — проверьте
   `internal/web/server.go` (spoke-хендлеры) и `app.js`.
3. **Реестр сохраняет порядок регистрации.** `DefaultID()` — первый зарегистрированный;
   это поведение UI («кластер по умолчанию»). `AllIDs()` сортирует — порядок метрик стабилен.
4. **`TriggerPull` идемпотентен.** Слот в состоянии `pulling` — no-op без ошибки.
   Офлайн-нода — ошибка, а не тихий успех.
5. **Pull-операции асинхронные.** HTTP-ответ `202`/счётчик, результат — только через
   следующий `Snapshot`. Не делайте хендлер блокирующим.
6. **Все защищённые ручки hub идут через `requireAuth`.** Публичны только `/healthz`,
   `/readyz`, логин/логаут и статика. `/api/v1/auth/me` проверяет cookie сам.
7. **Spoke-ручки идут через `requireToken`.** Пустой `PREPULLER_CLUSTER_TOKEN` отключает
   проверку — это осознанное поведение для локальной разработки, не «фича для прода».
8. **Секреты и пароли не логируются** и не попадают в ответы API. Сравнения — constant-time
   (`crypto/subtle`), как в `auth.go` и spoke-middleware.
9. **Состояние слотов вычисляется в одном месте** (сейчас `MockProvider`, позже
   `K8sProvider`). UI только отображает `PullState`, не выводит статусы сам.
10. **Coverage = `presentSlots / totalSlots`**, считается в провайдере (`Snapshot.Summary`),
    а не в UI.

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

- `internal/auth/auth_test.go` — cookie-flow: валидные/невалидные креды, подпись, подделка.
- `internal/web/server_test.go` — 401 без cookie, логин, список кластеров, снапшот
  удалённого кластера, Bearer-защита spoke, наличие кластерных метрик.
- Правило: любое изменение поведения API, auth или метрик сопровождается тестом.
  Метрики проверяйте по подстроке с меткой `cluster="..."`.
- Тесты не должны требовать Kubernetes, сети или внешних бинарей.

## 8. Подводные камни

- **`metrics.Handler` вызывает `Snapshot()` 6 раз на кластер за скрейп.** Для mock дёшево;
  в live-реализации добавьте кэш в провайдере, а не в метриках.
- **`RemoteProvider.PullCounters()` возвращает `nil`** — у удалённых кластеров нет
  `prepuller_pull_total`. Чтобы это исправить, нужен метод в spoke API.
- **`RemoteProvider.Snapshot` глотает ошибки**: при недоступном spoke возвращается пустой
  снапшот с `time.Now()`. Признак недоступности — `TotalWorkers == 0`; не считайте такой
  кластер «здоровым» бездумно.
- **`auth.NewManager` падает без логина/пароля** — hub без `PREPULLER_AUTH_USER`/`_PASS`
  не стартует. Это ожидаемо.
- **Случайный session secret** без `PREPULLER_SESSION_SECRET` означает разлогин после
  рестарта и несовместимость нескольких реплик.
- **`activity` в mock ограничен 100 последними событиями** (`m.activity = ...`), не снимайте
  лимит без пагинации.
- **`-mock` обязателен**: без него оба режима завершаются `log.Fatalf`. Не «чините» это
  заглушкой, пока не реализован живой провайдер.
- **`.gocache/`, `bin/`, `.gomodcache/` в корне** — локальные кэши сборки и модулей,
  они в `.gitignore` и в историю не попадают; не коммитьте их принудительно и не удаляйте
  вслепую, если в них завязан прогон. `bin/prepuller` — артефакт сборки.
- **git обязателен.** Работа идёт через ветки и worktree (`docs/PLAN.md` §3, §6.1);
  baseline Stage 1 — коммит `18aaaa5`, ADR-001 — `216e0d2`.
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
- [ ] Не добавлено внешних зависимостей и шагов сборки фронтенда без явного требования.

## 10. Документация

- Пользовательская — `README.md`: возможности, архитектурная схема, запуск, флаги, env,
  API, метрики, roadmap.
- Техническая — `docs/architecture.md`: топология, компоненты, auth-потоки, модель данных,
  состояния, pull-путь, метрики, план Stage 2–3.
- Архитектурные решения — `docs/adr/`: `0001-dependencies.md` (границы зависимостей Stage 2/3).
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
4. **Ветка и worktree:** `git worktree add ../ip-<ws> -b ws/<ws>` от `main`;
   ежедневно `git rebase main`; живёт ≤ 3 дней.
5. **Гейты ветки:** `go build ./... && go vet ./... && go test ./...`,
   `gofmt -l $(git diff --name-only main... -- '*.go')` пусто, новых зависимостей нет
   (`go list -m all` сверяется с `main`).
6. **Не форматировать чужие файлы** и не запускать `gofmt -w ./...` — это конфликты
   у всех остальных веток.
7. **Документация:** README/AGENTS/architecture в feature-ветке не правятся; в описании
   PR — готовый «doc snippet» (раздел, строки, текст), применяет интегратор.
8. **Мелкие PR и мерж по одному** через интеграционную ветку; порядок мержа — §9 плана.
