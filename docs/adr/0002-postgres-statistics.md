# ADR-002: PostgreSQL-статистика образов, сборщика мусора и pull-задач

- **Статус:** Accepted 2026-11-27
- **Дата:** 2026-11-27
- **Затрагивает:** internal/stats, go.mod/go.sum, cmd/prepuller/main.go, deploy/helm/image-prepuller, docs/DEPLOY.md
- **Связанные решения:** [ADR-001](0001-dependencies.md) (границы зависимостей и правило «новая зависимость — только через ADR»)

## Контекст

Оператору нужно понимать, как ведёт себя Talos/containerd на worker-нодах:

- как часто image-collector (GC) чистит образы и сколько байт освобождает;
- сколько живёт образ на ноде между появлением и удалением;
- как часто агентам приходится повторно тянуть образ (repull) и с какой задержкой
  после удаления сборщиком мусора;
- какие pull-задачи реально исполнялись, с какими переходами состояний.

Данные нужны внешние и переживающие перезапуск hub'а, поэтому их место — PostgreSQL,
а не JSONL рядом с процессом. При этом статистика не должна влиять на основной путь:
hub обязан работать без DSN, а при недоступной БД — не блокироваться и не падать.
Приоритет — прогрев и UI; статистика best effort.

Ключевые ограничения:

- hub и spoke — один бинарь; статистика живёт только в hub-роли;
- DDL должен быть идемпотентным (несколько реплик/перезапусков);
- тесты не должны требовать настоящего PostgreSQL (CI без внешних сервисов);
- retention задаётся оператором, а не зашит в код;
- DSN — секрет, его нельзя логировать.

## Варианты

1. **Только Prometheus-метрики.** Счётчики по образам/GC/repull можно вывести в
   `/metrics`. Отклонено: метрики не дают истории и точечных задержек «удалён →
   повторно скачан», а именно эти вопросы задаёт оператор. Плюс кардинальность
   image×node×cluster слишком велика для labels.
2. **JSONL-файлы рядом с hub'ом** (как `internal/tasks`). Отклонено: нет SQL-запросов,
   нет дедупликации по ключу, ручная ротация/retention, файл не общий между репликами.
3. **Собственный протокол поверх TCP + рукописный wire protocol.** Отклонено:
   это отдельная зависимость/код ради одной задачи, нет параметризованных запросов,
   нет пула соединений, нет TLS/аутентификации «из коробки». Драйвер БД даёт это
   бесплатно и проверен годами.
4. **PostgreSQL через `github.com/jackc/pgx/v5/stdlib`.** Принято: один драйвер,
   `database/sql`-совместимый, параметризованные запросы, пул соединений, поддержка
   `context` и таймаутов. Версия зафиксирована `v5.9.2` (поднята с `v5.7.6`:
   `GO-2026-5004`, SQL-инъекция через путаницу плейсхолдеров с dollar-quoted
   строковыми литералами, достижима из `database/sql`-пути `pgx`).

## Решение

Добавляем пакет `internal/stats` — необязательный, best-effort конвейер статистики.

Публичный контракт (его использует `cmd/prepuller/main.go` и чарт):

```go
type Config struct { Interval, SnapshotTimeout, Retention, PruneInterval time.Duration }
type Cluster struct { ID string; Snapshot func(context.Context) model.Snapshot }
type Store interface { Apply(context.Context, Batch) error; Prune(context.Context, time.Time) error; Close() error }
func NewCollector() *Collector
func (c *Collector) Step(cluster string, snap model.Snapshot, at time.Time) Batch
func (c *Collector) Commit(cluster string, b Batch)
func OpenPostgres(ctx context.Context, dsn string) (Store, error)
func Run(ctx context.Context, cfg Config, clusters []Cluster, store Store, logf func(string, ...any))
```

Разделение ответственности:

- `Collector` — чистая логика diff'а: сравнивает снапшот с закоммиченным состоянием,
  формирует `Batch` (upsert задач, samples нод, открытие/закрытие интервалов
  присутствия, GC-события, repull'ы). `Step` не мутирует состояние; `Commit`
  применяется только после успешного `Store.Apply`, поэтому при ошибке БД тот же
  diff повторяется на следующем тике.
- `Postgres` — SQL-слой: идемпотентный DDL, `Apply` в одной транзакции, `Prune` по
  retention-horizon, `Close`. Драйвер импортируется только здесь.
- `Run` — фоновый цикл hub'а: один снапшот на кластер за `Interval`, `Apply` → `Commit`,
  `Prune` раз в `PruneInterval`. Ошибки логируются и не прерывают цикл.

### Схема

Все таблицы содержат `cluster text` (id кластера из реестра hub'а):

| Таблица | Назначение | Ключ/идемпотентность |
| --- | --- | --- |
| `prepuller_tasks` | лог исполненных pull-задач и переходов состояний | PK `(cluster, task_id)`, upsert `ON CONFLICT` |
| `prepuller_image_presence` | интервалы жизни образа на ноде | открытый интервал `removed_at IS NULL`; уникальный индекс по `(cluster,node,image)` среди открытых |
| `prepuller_gc_events` | активность image-collector по нодам | PK `(cluster,node,occurred_at)`; `removed_count`, `removed_bytes`, `window_seconds` |
| `prepuller_repulls` | повторные скачивания после удаления | PK `(cluster, task_id)`; `lag_seconds = finished_at - removed_at` |
| `prepuller_node_samples` | сырые samples нод | PK `(cluster,node,sampled_at)` |

DDL (`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`) выполняется при
`OpenPostgres` и лениво в `Apply`; повторный запуск ничего не ломает.

### Семантика diff'а

- Множество присутствия на `(cluster,node)` — `NodeInfo.CachedImages` **только при
  `AgentUp`**. Пропавший ref закрывает открытый интервал (`removed_at = at`) и
  добавляет вклад в per-node GC-событие за шаг (`removed_count`, `removed_bytes`,
  `window_seconds = at - предыдущий шаг этой ноды`).
- Успешная задача (`succeeded`), чей образ на старте отсутствовал (последний закрытый
  интервал имеет `removed_at <= finished_at`), даёт repull-строку с
  `lag_seconds = finished_at - removed_at`.
- `Prune` удаляет строки старше `now - Retention`; открытые интервалы присутствия не
  удаляются.

## Границы зависимостей

`go.mod`/`go.sum` — зона интегратора. Новый пакет и правило импорта:

| Пакет | Можно импортировать |
| --- | --- |
| `internal/stats` | stdlib + `internal/model` + `github.com/jackc/pgx/v5/stdlib` |

`pgx` используется только в SQL-файле пакета; остальные файлы (`types`, `collector`,
`run`) зависят только от stdlib и `internal/model`. Это позволяет тестировать diff и
цикл без БД и без драйвера.

## Последствия

Положительные:

- оператор получает историю GC, время жизни образов, частоту и задержку repull'ов,
  а также лог pull-задач с переходами состояний;
- hub не зависит от доступности БД: без DSN статистика выключена, при ошибке —
  лог и повтор на следующем тике, основной путь не блокируется;
- DDL идемпотентен, upsert'ы идемпотентны — можно перезапускать hub и повторять тик;
- retention настраивается, а не зашит в код.

Отрицательные и принимаемые:

- **+1 внешняя зависимость** (`github.com/jackc/pgx/v5` и её транзитивные модули) и
  рост `go.sum`; это осознанное исключение из ADR-001, зафиксированное здесь;
- **статистика best effort**: при недоступной БД часть тиков теряется, дырки в
  истории допустимы; гарантий exactly-once нет — только идемпотентные upsert'ы;
- **тесты без БД обязательны**: SQL-слой проверяется фейковым `database/sql`-драйвером,
  зарегистрированным в тесте (assert statements/args и идемпотентные upsert'ы);
  реальный PostgreSQL в CI не поднимается. Правило: `go test ./internal/stats/`
  зелёный без сети и без БД;
- **DSN никогда не логируется** и не попадает в текст ошибок; при ошибке соединения
  пишется только факт/причина без строки подключения;
- retention по умолчанию задаёт чарт; слишком маленький retention удалит историю,
  слишком большой — увеличит БД. Открытые интервалы присутствия не прунятся, чтобы
  не потерять факт «образ ещё лежит на ноде»;
- схема пишется в ту же БД, что и остальные данные оператора; префикс таблиц
  `prepuller_` снижает риск коллизий.

## Что должен сделать интегратор (вне пакета)

- `cmd/prepuller/main.go`: флаги/env для DSN, `Interval`, `SnapshotTimeout`,
  `Retention`, `PruneInterval`; `OpenPostgres` при непустом DSN, `stats.Run` в
  отдельной горутине, graceful shutdown по общему контексту. Ошибка открытия БД —
  лог и повтор с backoff до минуты (hub стартует и работает без статистики), не
  фатально.
- Чарт: `hub.stats.enabled`, `hub.stats.dsn` (через Secret), `hub.stats.interval`,
  `hub.stats.snapshotTimeout`, `hub.stats.retention`, `hub.stats.pruneInterval`;
  без `enabled` env/флаги не выставляются.
- `README.md`/`docs/DEPLOY.md`: раздел про необязательную статистику и retention.

## Compliance

- `go test ./internal/stats/` зелёный без реальной БД; `go vet` и `gofmt` чистые.
- Импорт `pgx` только в `internal/stats`; в `internal/model`, `internal/web`,
  `internal/provider` и остальных пакетах он запрещён.
- Любое изменение публичного контракта (`Config`, `Cluster`, `Store`, `Collector`,
  `Run`) согласуется с `cmd/prepuller/main.go` и чартом.
