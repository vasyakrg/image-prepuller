# Алерты prepuller

Операторская памятка: что скрейпить, какие правила включены, как читать
одновременные симптомы и как включить правила. Метрики пишутся вручную в
формате Prometheus text exposition 0.0.4 (`/metrics` у hub и у node-agent);
отдельных exporters/ServiceMonitor чарт не содержит.

## 1. Что скрейпить

| Цель | Адрес | Авторизация | Примечание |
| --- | --- | --- | --- |
| node-agent | `prepuller_agent_*` на `agent.metrics.port` (по умолчанию `9444`) | нет | агент работает с `hostNetwork: true`, поэтому эндпоинты Service — это IP-адреса нод |
| hub | `/metrics` на `hub.service.metricsPort` (по умолчанию `9090`) | нет | hub-метрики агрегируют все зарегистрированные кластеры |
| spoke | `/metrics` на `spoke.service.metricsPort` (по умолчанию `9091`) | нет | те же серии, что у hub, для одного кластера |

Агент отдаёт метрики из памяти: на скрейпе он не ходит в CRI и не делает
сетевых вызовов. Hub тоже ограничивает каждый снапшот кластера 3 секундами.
Поэтому `scrape_timeout` нужно держать **не меньше 15s**: более короткий
таймаут даст ложные пропуски при медленном API кластера, но не защитит ни от
чего — обе стороны уже ограничены изнутри. `scrape_interval` 15–30s достаточно.

Service агента (`<fullname>-agent-metrics`, ключ `agent.metrics.service.name`)
селектит поды DaemonSet'а; так как поды hostNetwork, его endpoints — это
`<node-ip>:9444`. Отдельный headless Service не нужен. Второй вариант
обнаружения — pod-аннотации `prometheus.io/scrape|port|path`, они включаются
`agent.metrics.annotations`.

Ключевое соглашение: job scrape-конфига агента должен называться
`prepuller-agent`, иначе правило `PrepullerAgentDown` не сработает (выражение
использует `up{job="prepuller-agent"}`). Это единственное место, которое
оператор обязан адаптировать.

## 2. Правила

| Alert | Выражение | for | Severity | Что значит | Первое действие |
| --- | --- | --- | --- | --- | --- |
| `PrepullerAgentDown` | `up{job="prepuller-agent"} == 0` | 2m | critical | скрейп-цель агента недоступна (dead-man switch) | `kubectl -n <ns> get pod -l app.kubernetes.io/component=agent -o wide`, логи пода |
| `PrepullerAgentNotContacted` | `time() - prepuller_agent_last_contact_timestamp_seconds{endpoint="snapshot"} > <agentNoContact>` | 5m | warning | агент жив, но hub давно не присылал снапшот-запрос: сломан путь hub → агент | проверить hub: логи, NetworkPolicy, `PREPULLER_AGENT_TOKEN`, доступность адреса ноды |
| `PrepullerAgentUnauthorized` | `increase(prepuller_agent_requests_total{code=~"401\|403"}[10m]) > 0` | 0m | warning | несовпадение токена, а не сетевая авария | сверить `PREPULLER_AGENT_TOKEN` на hub и агентах, ротировать |
| `PrepullerAgentCRIDown` | `prepuller_agent_cri_up == 0` | 5m | critical | агент не может достучаться до CRI: инвентарь и pull'ы деградированы | проверить сокет CRI (`agent.criSocket`), containerd на ноде |
| `PrepullerAgentPullStalled` | `increase(prepuller_agent_pull_total{result="stalled"}[15m]) > 0` | 0m | warning | watchdog отменил pull: ImageFS не растёт | реестр/containerd; проверить `agent.pull.stallTimeout` |
| `PrepullerNodeAgentDown` | `prepuller_node_agent_up == 0 and on(cluster,node) prepuller_node_ready == 1` | <nodeAgentDown>s | critical | агент недоступен на **Ready** ноде | под DaemonSet'а, tolerations, сеть до ноды |
| `PrepullerNodeAgentAuthErrors` | `increase(prepuller_node_agent_errors_total{reason="unauthorized"}[15m]) > 0` | 0m | warning | hub получает 401 от агентов | токен агента на hub и на нодах |
| `PrepullerClusterPartial` | `prepuller_cluster_sync_partial == 1` | <clusterPartial>s | warning | снапшот кластера долго partial: не все агенты отдали инвентарь | ожидаемо при намеренно исключённой ноде (`nodeSelector`); иначе искать агента |
| `PrepullerClusterDown` | `prepuller_cluster_up == 0` | 2m | critical | hub не получает консистентный снапшот кластера | spoke/провайдер, логи hub |
| `PrepullerSnapshotStale` | `prepuller_snapshot_age_seconds > <snapshotAge>` | 5m | warning | hub давно не синхронизировался, UI показывает старые данные | логи hub, доступность API кластера |
| `PrepullerSnapshotScrapeTimeouts` | `increase(prepuller_snapshot_scrape_timeout_total[15m]) > 0` | 0m | warning | hub упирается в собственный таймаут снапшота: часть кластера недоступна | какая часть нод/узлов не отвечает |
| `PrepullerNodeImageFSHigh` | `prepuller_node_imagefs_capacity_bytes > 0 and (prepuller_node_imagefs_used_bytes / prepuller_node_imagefs_capacity_bytes) > <imagefsUsedRatio>` | 5m | warning | ImageFS заполнен выше порога | почистить неиспользуемые образы / расширить диск |
| `PrepullerPullFailures` | `sum by (cluster) (increase(prepuller_pull_total{result=~"error\|failed"}[15m])) > 0` | 0m | warning | есть проваленные pull'ы | логи агента и реестра |

`<...>` — пороги из `prometheusRule.thresholds`; правила с `for: 0m`
срабатывают сразу, потому что сам счётчик уже накоплен за окно `increase`.

Оговорки, зашитые в выражения:

- `PrepullerAgentDown` — классический dead-man switch по `up`: если scrape-job
  целиком убрать из Prometheus, серия `up{job="prepuller-agent"}` исчезнет и
  правило не сработает. Держите job и scrape-конфиг синхронно.
- `PrepullerNodeAgentDown` использует `and on(cluster,node)`, чтобы не будить
  дежурного на cordon/drain и NotReady нодах: отсутствие агента там ожидаемо.
- `PrepullerNodeImageFSHigh` не сработает там, где runtime не отдаёт ёмкость
  ImageFS (серия отсутствует). На таких нодах нужен алерт по диску ноды, а не
  по prepuller.
- `PrepullerPullFailures`: провайдер сейчас пишет `result="error"`; `failed`
  оставлен в regex, чтобы правило пережило переименование значения результата.

## 3. Триангуляция

Один симптом почти всегда неоднозначен; смотрите на пару метрик сразу.

| Наблюдение | Вывод | Куда смотреть |
| --- | --- | --- |
| hub `prepuller_node_agent_up{node=X} == 0`, при этом агент на X скрейпится (`up{job="prepuller-agent"} == 1`) | сломан путь hub → агент (сеть, токен, адрес), агент жив | NetworkPolicy/firewall до ноды, токен, `Node.status.addresses` |
| hub `prepuller_node_agent_up == 0` и `up{job="prepuller-agent"}` отсутствует/0 для той же ноды | агент мёртв либо не скрейпится | под DaemonSet'а на ноде, tolerations, скрейп-конфиг |
| hub `prepuller_node_agent_up == 0` и `prepuller_node_agent_errors_total{reason="unauthorized"}` растёт | токен, не сеть | `PREPULLER_AGENT_TOKEN` на обеих сторонах |
| `prepuller_cluster_up == 0`, но агенты healthy (`prepuller_agent_up == 1`) | сломан канал hub ↔ spoke (или API кластера) | `/internal/v1/*`, сеть до spoke, токен кластера |
| `prepuller_agent_up == 1`, но `prepuller_agent_last_contact_timestamp_seconds{endpoint="snapshot"}` старый | агент жив, hub перестал к нему ходить | hub: рестарты, логи, ошибки снапшота |
| `prepuller_agent_cri_up == 0` при `prepuller_agent_up == 1` | процесс жив, CRI сломан | сокет/containerd на ноде |

`prepuller_node_last_seen_timestamp_seconds{cluster,node}` (последний успешный
контакт hub → агент по ноде) удобно накладывать на
`prepuller_agent_last_contact_timestamp_seconds`: расхождение направлений
показывает, кто именно перестал видеть другого.

## 4. Как включить

По умолчанию правила выключены: `prometheusRule.enabled: false`. Включайте,
только если в кластере установлен prometheus-operator и существует CRD
`monitoring.coreos.com/v1`; иначе объект PrometheusRule не примется API.

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --set prometheusRule.enabled=true \
  --set prometheusRule.labels.release=kube-prometheus-stack
```

`prometheusRule.labels` должны попадать под `ruleSelector` вашего Prometheus,
иначе оператор не заберёт правила. `prometheusRule.annotations` — аннотации на
сам объект (не на алерты).

Все пороги — значения чарта, форкать чарт не нужно:

| Ключ | По умолчанию | Смысл |
| --- | --- | --- |
| `thresholds.agentNoContact` | `180` | секунд без snapshot-запроса hub к агенту |
| `thresholds.nodeAgentDown` | `120` | секунд недоступности агента на Ready ноде |
| `thresholds.clusterPartial` | `600` | секунд в состоянии partial |
| `thresholds.snapshotAge` | `120` | секунд с последней успешной синхронизации |
| `thresholds.imagefsUsedRatio` | `0.85` | доля занятого ImageFS |

## 5. Переносимость

Ни одно выражение, аннотация правила и значение чарта не содержит
cluster-specific данных: имён кластеров, доменов, IP-адресов, namespace'ов или
организационных меток. Метки `cluster` и `node` приходят из hub-метрик,
значения `severity` — стандартные. Оператору нужно адаптировать только одно —
имя job'а scrape-конфига (`prepuller-agent`) и, при необходимости, labels
селектора PrometheusRule.
