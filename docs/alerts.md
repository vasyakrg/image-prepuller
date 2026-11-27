# Алерты prepuller

Операторская памятка: что скрейпить, какие правила включены, как читать
одновременные симптомы и как включить правила. Метрики пишутся вручную в
формате Prometheus text exposition 0.0.4 (`/metrics` у hub и у node-agent);
экспортёров чарт не содержит, а ServiceMonitor и правила он умеет создавать сам
(флаги ниже) — либо скрейпите через `prometheus.io/*`-аннотации.

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
`<node-ip>:9444`. Отдельный headless Service не нужен. Варианты обнаружения:

- `agent.metrics.serviceMonitor.enabled=true` — чарт создаёт ServiceMonitor
  (`<fullname>-agent`, порт `metrics`, job принудительно `prepuller-agent`);
- `hub.serviceMonitor.enabled=true` — ServiceMonitor для hub (`<fullname>`, порт
  `metrics`, `scrapeTimeout` по умолчанию 15s);
- pod-аннотации `prometheus.io/scrape|port|path` — `hub.metrics.annotations` /
  `agent.metrics.annotations`.

Оба ServiceMonitor-флага требуют CRD `monitoring.coreos.com/v1`: без него
рендер падает с понятным сообщением, а не молча пропускает объект.

Мониторы и правила лежат в разделе того компонента, чьи серии они покрывают:
`hub.serviceMonitor` / `hub.prometheusRule` — hub и кластерные серии (правила
рендерятся и для release без hub'а, если в нём есть spoke: spoke отдаёт те же
серии на своём порту метрик); `agent.metrics.serviceMonitor` /
`agent.metrics.prometheusRule` — `prepuller_agent_*` с `:9444`, поэтому без
`agent.metrics.enabled` они не рендерятся (чарт падает с сообщением).

Ключевое соглашение: job scrape-конфига агента должен называться
`prepuller-agent`, иначе правило `PrepullerAgentDown` не сработает (выражение
использует `up{job="prepuller-agent"}`). ServiceMonitor чарта выставляет эту
метку сам (relabeling `job: prepuller-agent`); при ручном scrape-конфиге имя
job'а придётся задать самому.

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

`<...>` — пороги из `hub.prometheusRule.thresholds` (кроме `agentNoContact`:
он в `agent.metrics.prometheusRule.thresholds`); правила с `for: 0m` срабатывают
сразу, потому что сам счётчик уже накоплен за окно `increase`.

Каждое правило несёт аннотацию `title` с именем приложения, чьи серии оно покрывает:
`prepuller-hub` в `<fullname>-rules` и `prepuller-agent` в `<fullname>-agent-rules`
(это же имя в `job` агента). По нему группируют алерты Alertmanager-шаблоны,
Karma и дежурные дашборды; аннотация рендерится чартом и не настраивается, чтобы
новое правило не появилось без неё (CI сверяет число `alert` и число `title`).

Правила разделены на два объекта, чтобы их можно было включать независимо:

| Объект | Флаг | Правила |
| --- | --- | --- |
| `<fullname>-rules` | `hub.prometheusRule.enabled` | всё, что считается по hub/spoke-сериям: `PrepullerNodeAgentDown`, `PrepullerNodeAgentAuthErrors`, `PrepullerClusterPartial`, `PrepullerClusterDown`, `PrepullerSnapshotStale`, `PrepullerSnapshotScrapeTimeouts`, `PrepullerNodeImageFSHigh`, `PrepullerPullFailures` |
| `<fullname>-agent-rules` | `agent.metrics.prometheusRule.enabled` | всё, что считается по `prepuller_agent_*` с `:9444`: `PrepullerAgentDown`, `PrepullerAgentNotContacted`, `PrepullerAgentUnauthorized`, `PrepullerAgentCRIDown`, `PrepullerAgentPullStalled` |

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
| `prepuller_cluster_up{cluster=X} == 0` для outbound-кластера (X в `hub.managedClusters`), при этом агенты в X живы | spoke в X перестал пушить состояние: нет egress до hub, сменился токен, hub потерял сессию после рестарта | логи пода spoke (`spoke outbound channel: state-push/command-poll`), 401 на hub, DNS/egress 443 до hub ingress, совпадение кластерного токена с `hub.managedClusters` |

`prepuller_node_last_seen_timestamp_seconds{cluster,node}` (последний успешный
контакт hub → агент по ноде) удобно накладывать на
`prepuller_agent_last_contact_timestamp_seconds`: расхождение направлений
показывает, кто именно перестал видеть другого.

Для кластеров в **outbound-режиме** (`-hub-url` на spoke +
`hub.managedClusters` на hub) направление обратное: соединение открывает spoke,
а hub лишь отдаёт UI и метрикам последнее присланное состояние. Поэтому
«hub ослеп» здесь означает «spoke перестал пушить»: свежесть данных равна
`-hub-sync-interval` (3 s по умолчанию, плюс TTL провайдера 2 s), а молчание
дольше `max(3*syncInterval, 10 s)` переводит кластер в offline
(`prepuller_cluster_up == 0`, `prepuller_snapshot_age_seconds` растёт,
`PullCounters` пустеет). Правила `PrepullerClusterDown` и
`PrepullerSnapshotStale` работают без изменений; отдельного правила «spoke
давно не выходил на связь» не нужно — это те же серии.

## 4. Как включить

По умолчанию всё выключено: `hub.prometheusRule.enabled: false`,
`agent.metrics.prometheusRule.enabled: false`, `hub.serviceMonitor.enabled: false`,
`agent.metrics.serviceMonitor.enabled: false`. Включайте, только если в кластере
установлен prometheus-operator и существует CRD `monitoring.coreos.com/v1`;
без CRD рендер падает с сообщением вида
`monitoring.coreos.com/v1 CRDs are not installed: install prometheus-operator or set hub.prometheusRule.enabled=false`.

```bash
helm upgrade --install prepuller deploy/helm/image-prepuller \
  --set hub.serviceMonitor.enabled=true \
  --set hub.serviceMonitor.labels.release=kube-prometheus-stack \
  --set hub.prometheusRule.enabled=true \
  --set hub.prometheusRule.labels.release=kube-prometheus-stack \
  --set agent.metrics.serviceMonitor.enabled=true \
  --set agent.metrics.serviceMonitor.labels.release=kube-prometheus-stack \
  --set agent.metrics.prometheusRule.enabled=true \
  --set agent.metrics.prometheusRule.labels.release=kube-prometheus-stack
```

`*.labels` должны попадать под селекторы вашего Prometheus (`ruleSelector` для
правил, `serviceMonitorSelector` для мониторов), иначе оператор не заберёт
объекты. `*.annotations` — аннотации на сам объект (не на алерты).
`hub.serviceMonitor.interval` / `scrapeTimeout` и
`agent.metrics.serviceMonitor.interval` / `scrapeTimeout` — параметры скрейпа;
`scrapeTimeout` hub'а по умолчанию 15s (см. §1), у агента — 10s.

Все пороги — значения чарта, форкать чарт не нужно:

| Ключ | По умолчанию | Смысл |
| --- | --- | --- |
| `agent.metrics.prometheusRule.thresholds.agentNoContact` | `180` | секунд без snapshot-запроса hub к агенту |
| `hub.prometheusRule.thresholds.nodeAgentDown` | `120` | секунд недоступности агента на Ready ноде |
| `hub.prometheusRule.thresholds.clusterPartial` | `600` | секунд в состоянии partial |
| `hub.prometheusRule.thresholds.snapshotAge` | `120` | секунд с последней успешной синхронизации |
| `hub.prometheusRule.thresholds.imagefsUsedRatio` | `0.85` | доля занятого ImageFS |

## 5. Переносимость

Ни одно выражение, аннотация правила и значение чарта не содержит
cluster-specific данных: имён кластеров, доменов, IP-адресов, namespace'ов или
организационных меток. Метки `cluster` и `node` приходят из hub-метрик,
значения `severity` — стандартные. Оператору нужно адаптировать только labels
селекторов (`*.labels`) и, если ServiceMonitor не включён, имя job'а
scrape-конфига (`prepuller-agent`).
