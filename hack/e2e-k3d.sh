#!/usr/bin/env bash
#
# Локальный end-to-end смоук на одноразовом k3d-кластере: приёмка Stage 2/3
# (docs/PLAN.md §9, docs/DEPLOY.md §12). Гоняется ПЕРЕД пушем в main, не в CI.
#
#   bash hack/e2e-k3d.sh              # собрать образ, поднять кластер, прогнать сценарий
#   KEEP=1 bash hack/e2e-k3d.sh       # оставить кластер после прогона (для разбора)
#   E2E_SKIP_BUILD=1 bash hack/e2e-k3d.sh   # переиспользовать уже собранный образ
#
# Требуется: docker, k3d, kubectl, helm, jq, curl. Скрипт всё создаёт сам и
# удаляет кластер на выходе (в том числе при падении), поэтому ничего не
# остаётся ни при успехе, ни при ошибке — если не задан KEEP=1.
#
# Что проверяется:
#   1. образ собирается и импортируется в кластер;
#   2. hub в live-режиме (K8sProvider) + агенты DaemonSet по CRI: agentUp=true,
#      cachedImageCount>0, syncStatus=ok, partial=false;
#   3. policy violations для :latest+Always;
#   4. авто-прогрев ставит pull-задачи;
#   5. pull в мёртвый реестр отвергается быстрее минуты (probe + stall watchdog);
#   6. потеря агента на ноде → offline-слот, partial=true, hub продолжает работать;
#   7. возврат агента → полное восстановление;
#   8. метрики Stage 2/3 на /metrics.
set -euo pipefail

CLUSTER="${E2E_CLUSTER:-prepuller-e2e}"
NAMESPACE="${E2E_NAMESPACE:-prepuller}"
RELEASE="${E2E_RELEASE:-prepuller}"
IMAGE="${E2E_IMAGE:-image-prepuller:e2e}"
IMAGE_REPO="${E2E_IMAGE_REPO:-image-prepuller}"
IMAGE_TAG="${E2E_IMAGE_TAG:-e2e}"
CLUSTER_ID="${E2E_CLUSTER_ID:-talos-prod-eu}"
PORT="${E2E_PORT:-}"
METRICS_PORT="${E2E_METRICS_PORT:-}"
KEEP="${KEEP:-0}"

TMP="$(mktemp -d)"
# Изолированный kubeconfig: у разработчика в KUBECONFIG может лежать список
# боевых кластеров, и скрипт не должен ни читать их, ни тем более менять
# default-context. k3d пишет контекст в этот временный файл.
export KUBECONFIG="$TMP/kubeconfig"
: > "$KUBECONFIG"
COOKIE_JAR="$TMP/cookies.txt"
BASE=""
METRICS=""
PF_PID=""
APF_PID=""
STARTED_AT=$SECONDS

log()  { printf '\n=== %s\n' "$*"; }
ok()   { printf '  ok  %s\n' "$*"; }
note() { printf '      %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }

cleanup() {
  local rc=$?
  set +e
  if [ -n "$PF_PID" ]; then kill "$PF_PID" 2>/dev/null; wait "$PF_PID" 2>/dev/null; fi
  if [ -n "$APF_PID" ]; then kill "$APF_PID" 2>/dev/null; wait "$APF_PID" 2>/dev/null; fi
  rm -rf "$TMP"
  if [ "$KEEP" = "1" ]; then
    printf '\nKEEP=1: кластер %s и namespace %s оставлены\n' "$CLUSTER" "$NAMESPACE"
  else
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1
  fi
  exit "$rc"
}
trap cleanup EXIT

require() { command -v "$1" >/dev/null 2>&1 || fail "нет $1 в PATH"; }

# port_in_use <port> — занят ли TCP-порт на loopback. На хосте разработчика порт
# 18080 может держать посторонний сервис; тогда port-forward не поднимается, а
# curl уходит в чужой процесс (однажды так и вышло: чужой сервис отвечал 200 на
# /healthz, и сценарий проверял не тот сервер).
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# pick_free_port <port> — первый свободный порт, начиная с указанного.
pick_free_port() {
  local p=$1
  while port_in_use "$p"; do p=$((p + 1)); done
  printf '%s' "$p"
}

# --- помощники проверок -------------------------------------------------------

snapshot() { curl -fsS -b "$COOKIE_JAR" "$BASE/api/v1/snapshot?cluster=$CLUSTER_ID"; }

# wait_snapshot <секунды> <jq-выражение> <сообщение>
wait_snapshot() {
  local timeout=$1 expr=$2 msg=$3
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if snapshot 2>/dev/null | jq -e "$expr" >/dev/null 2>&1; then ok "$msg"; return 0; fi
    sleep 3
  done
  snapshot >&2 || true
  fail "$msg — таймаут ${timeout}s (условие: $expr)"
}

assert_snapshot() {
  local expr=$1 msg=$2 snap
  snap=$(snapshot) || fail "$msg — снапшот не получен"
  printf '%s' "$snap" | jq -e "$expr" >/dev/null 2>&1 || {
    # Полный снапшот — сотни строк; печатаем срез, по которому видно причину.
    printf '%s' "$snap" | jq '{
      syncStatus, partial, summary,
      nodes: [.nodes[] | {name, ready, agentUp, diskPaused, cachedImageCount}],
      offlineSlots: [.workloads[].images[].nodes[] | select(.state == "offline")] | length,
      tasks: [.tasks[]? | {image, node, state}]
    }' >&2 || true
    fail "$msg (условие: $expr)"
  }
  ok "$msg"
}

wait_http() {
  local url=$1 timeout=$2
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then return 0; fi
    sleep 1
  done
  fail "$url не ответил за ${timeout}s"
}

# -----------------------------------------------------------------------------

log "0. инструменты"
require docker; require k3d; require kubectl; require helm; require jq; require curl
docker info >/dev/null 2>&1 || fail "docker-демон недоступен"
kubectl version --client=true >/dev/null 2>&1 || fail "kubectl не работает"
helm version --short >/dev/null 2>&1 || fail "helm не работает"
ok "docker, k3d $(k3d --version | head -1 | awk '{print $3}'), kubectl, helm $(helm version --short)"

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

log "1. образ"
if [ "${E2E_SKIP_BUILD:-0}" = "1" ]; then
  docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "нет локального образа $IMAGE, а E2E_SKIP_BUILD=1"
  ok "использую собранный заранее $IMAGE"
else
  sha="$(git rev-parse --short HEAD 2>/dev/null || echo e2e)"
  docker build --progress=plain \
    --build-arg VERSION="${E2E_VERSION:-e2e}" \
    --build-arg COMMIT="$sha" \
    -t "$IMAGE" . >"$TMP/build.log" 2>&1 || { tail -30 "$TMP/build.log" >&2; fail "docker build упал (лог выше)"; }
  ok "образ $IMAGE собран ($(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.1f MB", $1/1024/1024}'))"
fi

log "2. кластер k3d"
k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
k3d cluster create "$CLUSTER" --agents 2 --wait >"$TMP/k3d.log" 2>&1 || { tail -20 "$TMP/k3d.log" >&2; fail "k3d cluster create упал"; }
kubectl config use-context "k3d-$CLUSTER" >/dev/null
# Страховка: дальше все kubectl-команды обязаны идти в одноразовый кластер.
cur="$(kubectl config current-context)"
case "$cur" in k3d-*) ;; *) fail "kubectl смотрит в контекст '$cur', а не в k3d-$CLUSTER — работа остановлена";; esac
ok "кластер $CLUSTER создан, нод: $(kubectl get nodes --no-headers | wc -l | tr -d ' ')"

log "3. импорт образа в узлы"
k3d image import "$IMAGE" -c "$CLUSTER" >"$TMP/import.log" 2>&1 || { tail -20 "$TMP/import.log" >&2; fail "k3d image import упал"; }
ok "образ импортирован"

log "4. helm install (hub live + агенты по CRI)"
helm upgrade --install "$RELEASE" deploy/helm/image-prepuller \
  --namespace "$NAMESPACE" --create-namespace \
  --set image.repository="$IMAGE_REPO" \
  --set image.tag="$IMAGE_TAG" \
  --set image.pullPolicy=Never \
  --set agent.criSocket=/run/k3s/containerd/containerd.sock \
  --set secret.auth.user=admin \
  --set secret.auth.password=admin \
  --set secret.sessionSecret=e2e-session-secret \
  --set hub.cookieSecure=false \
  --wait --timeout 5m >"$TMP/helm.log" 2>&1 || { tail -30 "$TMP/helm.log" >&2; fail "helm upgrade --install упал"; }
kubectl -n "$NAMESPACE" rollout status "deploy/$RELEASE-image-prepuller" --timeout=240s >/dev/null
kubectl -n "$NAMESPACE" rollout status "ds/$RELEASE-image-prepuller-agent" --timeout=240s >/dev/null
ok "hub и агенты в состоянии Ready"

log "4.1 область действия: только workload'ы с label prepull=true"
# Чарт по умолчанию ставит hub.workloadSelector=prepull=true: в UI, в снапшоте и
# в авто-прогреве участвуют только отмеченные workload'ы. Проверяем оба края —
# отмеченный попадает, неотмеченный не появляется вовсе.
kubectl create deployment e2e-in-scope --image="$IMAGE_REPO:$IMAGE_TAG" --replicas=1 >/dev/null 2>&1 || true
kubectl label deployment e2e-in-scope prepull=true --overwrite >/dev/null
kubectl create deployment e2e-out-of-scope --image=ghcr.io/acme/ignored:1 --replicas=1 >/dev/null 2>&1 || true
ok "workload'ы e2e-in-scope (prepull=true) и e2e-out-of-scope созданы"

log "5. port-forward и авторизация"
[ -n "$PORT" ] || PORT=$(pick_free_port 18080)
[ -n "$METRICS_PORT" ] || METRICS_PORT=$(pick_free_port 19090)
BASE="http://127.0.0.1:$PORT"
METRICS="http://127.0.0.1:$METRICS_PORT/metrics"
kubectl -n "$NAMESPACE" port-forward "svc/$RELEASE-image-prepuller" "$PORT:8080" "$METRICS_PORT:9090" >/dev/null 2>&1 &
PF_PID=$!
wait_http "$BASE/healthz" 60 || fail "hub не ответил на /healthz"
kill -0 "$PF_PID" 2>/dev/null || fail "port-forward на $PORT не поднялся (порт занят? задайте E2E_PORT)"
ok "hub отвечает на $BASE"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/clusters")
[ "$code" = "401" ] || fail "без cookie /api/v1/clusters вернул $code, ожидался 401"
ok "защищённые ручки отдают 401 без cookie"

code=$(curl -s -o /dev/null -w '%{http_code}' -c "$COOKIE_JAR" \
  -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}')
[ "$code" = "200" ] || fail "логин вернул $code"
ok "логин operator: 200"

curl -fsS -b "$COOKIE_JAR" "$BASE/api/v1/clusters" | jq -e --arg id "$CLUSTER_ID" '.clusters | map(.id) | index($id) != null' >/dev/null \
  || fail "кластер $CLUSTER_ID не зарегистрирован в hub"
ok "кластер $CLUSTER_ID в реестре hub"

log "6. live-снапшот: агенты, слоты, кэш"
wait_snapshot 180 '.syncStatus == "ok" and .partial == false' "syncStatus=ok, partial=false"
assert_snapshot '[.nodes[] | select(.agentUp == true)] | length == 3' "все 3 агента отвечают (agentUp=true)"
assert_snapshot '[.nodes[].agentVersion] | map(select(. != "")) | length == 3' "агенты отдают версию"
assert_snapshot '.summary.totalWorkers == 3' "totalWorkers=3"
assert_snapshot '[.nodes[] | .cachedImageCount] | add > 0' "агенты видят образы в CRI-кэше (cachedImageCount>0)"
assert_snapshot '.summary.totalSlots > 0 and .summary.presentSlots <= .summary.totalSlots' "слоты посчитаны (present/total)"
assert_snapshot '([.workloads[].name] | index("e2e-in-scope")) != null' "workload с prepull=true попал в снапшот"
assert_snapshot '([.workloads[].name] | index("e2e-out-of-scope")) == null' "workload без prepull=true не показывается"

log "7. policy violations: :latest + Always"
kubectl create deployment e2e-latest --image=ghcr.io/acme/api-gateway:latest --replicas=2 >/dev/null 2>&1 || true
kubectl label deployment e2e-latest prepull=true --overwrite >/dev/null
wait_snapshot 120 '[.workloads[] | .images[] | .policyViolations[]?] | length >= 2' "две policy violations для mutable-тега"
note "violations: $(snapshot | jq -c '[.workloads[].images[].policyViolations[]? | .reason] | unique')"

log "8. авто-прогрев: pull-задачи"
deadline=$((SECONDS + 150))
tasks=''
while [ $SECONDS -lt $deadline ]; do
  tasks=$(curl -fsS -b "$COOKIE_JAR" "$BASE/api/v1/pull-tasks?cluster=$CLUSTER_ID" | jq -c '.tasks')
  [ "$(printf '%s' "$tasks" | jq 'length')" -ge 1 ] && break
  sleep 5
done
[ "$(printf '%s' "$tasks" | jq 'length')" -ge 1 ] || fail "reconciler не поставил ни одной pull-задачи"
ok "pull-задачи есть: $(printf '%s' "$tasks" | jq -r '[.[] | .state] | group_by(.) | map("\(.[0])=\(length)") | join(", ")')"
note "при недоступном реестре задача завершится failed — это ожидаемый сигнал, а не ошибка сценария"

log "9. pull в мёртвый реестр: отказ быстрее минуты"
node=$(snapshot | jq -r '.nodes[] | select(.agentUp == true) | .name' | head -1)
[ -n "$node" ] || fail "нет ноды с живым агентом для проверки мёртвого реестра"
# registry.invalid — TLD из RFC 2606: DNS не разрешается детерминированно в любом
# окружении. Ошибка обязана прийти от preflight-пробы агента ("registry unreachable"),
# а не от CRI: иначе pull снова живёт до DefaultAgentPullTimeout (10 минут).
# Сам чёрный ход (TCP-соединение висит) проверяется юнит-тестами с подменённым
# диалаером — в e2e такой адрес воспроизвести нельзя.
resp=$(curl -sS -m 120 -o "$TMP/deadpull.json" -w '%{http_code} %{time_total}' \
  -b "$COOKIE_JAR" -X POST "$BASE/api/v1/pull" -H 'Content-Type: application/json' \
  -d "{\"cluster\":\"$CLUSTER_ID\",\"image\":\"registry.invalid/dead:1\",\"node\":\"$node\"}") \
  || fail "POST /api/v1/pull для мёртвого реестра не отработал"
code=${resp%% *}; elapsed=${resp##* }
[ "$code" = "502" ] || fail "pull в мёртвый реестр вернул $code, ожидался 502: $(cat "$TMP/deadpull.json")"
grep -q 'registry unreachable' "$TMP/deadpull.json" \
  || fail "502 пришёл не от preflight-пробы агента: $(cat "$TMP/deadpull.json")"
awk -v t="$elapsed" 'BEGIN{exit !(t < 60)}' || fail "pull в мёртвый реестр висел ${elapsed}s — больше минуты"
ok "мёртвый реестр отвергнут за ${elapsed}s пробой агента: $(jq -r '.error' "$TMP/deadpull.json" | cut -c1-90)"

log "10. деградация: убираем агента с одной ноды"
kubectl -n "$NAMESPACE" patch ds "$RELEASE-image-prepuller-agent" --type=json -p \
  '[{"op":"add","path":"/spec/template/spec/affinity","value":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"NotIn","values":["k3d-'"$CLUSTER"'-agent-0"]}]}]}}}}]' >/dev/null
wait_snapshot 240 '[.nodes[] | select(.agentUp == false)] | length >= 1' "нода без агента стала offline"
assert_snapshot '.partial == true' "снапшот помечен partial=true"
assert_snapshot '.syncStatus != "ok"' "syncStatus перестал быть ok"
code=$(curl -s -o /dev/null -w '%{http_code}' -b "$COOKIE_JAR" "$BASE/api/v1/snapshot?cluster=$CLUSTER_ID")
[ "$code" = "200" ] || fail "hub перестал отдавать снапшот при потере агента ($code)"
ok "hub продолжает отвечать (200) без одного агента"
assert_snapshot '[.workloads[].images[].nodes[] | select(.state == "offline")] | length >= 1' "слоты потерянной ноды помечены offline"

log "11. восстановление"
kubectl -n "$NAMESPACE" patch ds "$RELEASE-image-prepuller-agent" --type=json \
  -p '[{"op":"remove","path":"/spec/template/spec/affinity"}]' >/dev/null
kubectl -n "$NAMESPACE" rollout status "ds/$RELEASE-image-prepuller-agent" --timeout=300s >/dev/null
wait_snapshot 300 '.syncStatus == "ok" and .partial == false' "после возврата агента syncStatus=ok, partial=false"
assert_snapshot '[.nodes[] | select(.agentUp == true)] | length == 3' "все агенты снова отвечают"

log "12. метрики"
curl -fsS "$METRICS" -o "$TMP/metrics.txt" || fail "метрики недоступны на $METRICS"
missing=''
for series in prepuller_build_info prepuller_cluster_up prepuller_cluster_sync_partial \
              prepuller_cluster_last_sync_timestamp_seconds prepuller_agent_info \
              prepuller_node_agent_up prepuller_node_ready \
              prepuller_node_imagefs_capacity_bytes prepuller_images_cached_total \
              prepuller_node_last_seen_timestamp_seconds prepuller_snapshot_age_seconds \
              prepuller_snapshot_scrape_timeout_total \
              prepuller_workload_policy_violation prepuller_pull_tasks; do
  grep -q "^$series" "$TMP/metrics.txt" || missing="$missing $series"
done
[ -z "$missing" ] || fail "нет серий метрик:$missing"
ok "все серии метрик на месте ($(grep -c '^prepuller_' "$TMP/metrics.txt") строк)"
# prepuller_pull_total рендерится по завершённым pull-задачам: running счётчиков не
# инкрементирует, а pull внешнего образа живёт до DefaultAgentPullTimeout (10 минут).
# Поэтому серию с данными требуем только при терминальном состоянии задачи — метрика
# при этом всё равно должна быть объявлена в экспозиции.
grep -q '^# TYPE prepuller_pull_total counter' "$TMP/metrics.txt" || fail "метрика prepuller_pull_total не объявлена"
terminal=$(curl -fsS -b "$COOKIE_JAR" "$BASE/api/v1/pull-tasks?cluster=$CLUSTER_ID" \
  | jq '[.tasks[] | select(.state == "succeeded" or .state == "failed" or .state == "canceled")] | length')
if [ "$terminal" -gt 0 ]; then
  grep -q '^prepuller_pull_total{' "$TMP/metrics.txt" || fail "pull-задача завершена, но счётчиков нет"
  ok "счётчики pull_total есть (завершённых задач: $terminal)"
else
  note "pull-задачи ещё не завершились (внешний реестр), серия счётчиков не рендерится — это ожидаемо"
fi
grep -q 'cluster="'"$CLUSTER_ID"'"' "$TMP/metrics.txt" || fail "метрики без метки cluster=\"$CLUSTER_ID\""
ok "метрики размечены cluster=\"$CLUSTER_ID\""

log "13. метрики node-agent (:9444)"
AGENT_METRICS_PORT="${E2E_AGENT_METRICS_PORT:-$(pick_free_port 19444)}"
kubectl -n "$NAMESPACE" port-forward "svc/$RELEASE-image-prepuller-agent-metrics" \
  "$AGENT_METRICS_PORT:9444" >/dev/null 2>&1 &
APF_PID=$!
sleep 2
kill -0 "$APF_PID" 2>/dev/null || fail "port-forward метрик агента не поднялся (задайте E2E_AGENT_METRICS_PORT)"
curl -fsS "http://127.0.0.1:$AGENT_METRICS_PORT/metrics" -o "$TMP/agent-metrics.txt" \
  || fail "метрики агента недоступны на :$AGENT_METRICS_PORT"
kill "$APF_PID" 2>/dev/null || true
wait "$APF_PID" 2>/dev/null || true
APF_PID=""
missing=''
for series in prepuller_agent_up prepuller_agent_build_info \
              prepuller_agent_last_contact_timestamp_seconds \
              prepuller_agent_requests_total prepuller_agent_cri_up \
              prepuller_agent_pull_total prepuller_agent_pull_in_flight \
              prepuller_agent_imagefs_used_bytes prepuller_agent_images_cached_total; do
  grep -q "^$series" "$TMP/agent-metrics.txt" || missing="$missing $series"
done
[ -z "$missing" ] || fail "нет серий метрик агента:$missing"
grep -q '^prepuller_agent_cri_up{.*} 1$' "$TMP/agent-metrics.txt" \
  || fail "агент не отчитался о живом CRI"
grep -q '^prepuller_agent_last_contact_timestamp_seconds{.*endpoint="snapshot".*} ' "$TMP/agent-metrics.txt" \
  || fail "нет last_contact для snapshot"
ok "метрики агента на месте ($(grep -c '^prepuller_agent_' "$TMP/agent-metrics.txt") строк)"

printf '\n=== PASS за %ss (кластер %s будет удалён%s)\n' "$((SECONDS - STARTED_AT))" "$CLUSTER" "$([ "$KEEP" = "1" ] && echo ', KEEP=1 — оставлен')"
