#!/usr/bin/env bash
# E2E-проверки «продвинутых» фич в настоящем кластере. По секции на фичу:
#   ./scripts/features-test.sh network-policy|sidecar|nodes|backup|resize|oom|broken-release|quota|observability|gateway
# или все подряд: ./scripts/features-test.sh all
set -euo pipefail

URL="${URL:-http://localhost:30080}"
NS="${NS:-kube-demo}"
k() { kubectl -n "$NS" "$@"; }
fail() { echo "❌ $*" >&2; k get pods -o wide >&2 || true; exit 1; }
ok() { echo "✅ $*"; }
# json <ключ>: первое строковое значение ключа из JSON на stdin (без jq и без SIGPIPE).
json() { local v; v=$(grep -o "\"$1\":\"[^\"]*\"" || true); v=${v%%$'\n'*}; v=${v#*:\"}; echo "${v%\"}"; }
retry() { # retry <секунд> <описание> <команда...>
  local timeout=$1 what=$2; shift 2
  for _ in $(seq 1 "$timeout"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  fail "не дождались: $what"
}
# contains <текст> <подстрока>. Не `cmd | grep -q`: при pipefail grep -q закрывает
# pipe на первом совпадении, cmd получает SIGPIPE, и проверка ложно падает.
contains() { grep -qF -- "$2" <<<"$1"; }
app_pod() { k get pods -l app=kube-demo --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}'; }

network_policy() {
  echo "→ NetworkPolicy"
  kubectl apply -f k8s/extras/network-policy.yaml
  sleep 5
  if k run np-deny --rm -i --restart=Never --image=postgres:16-alpine --command -- pg_isready -h postgres -t 3; then
    fail "под без метки postgres-access достучался до базы"
  fi
  k run np-allow --rm -i --restart=Never --labels=postgres-access=true --image=postgres:16-alpine --command -- pg_isready -h postgres -t 5 \
    || fail "под с меткой postgres-access не достучался до базы"
  contains "$(curl -sf "$URL/api/hello")" '"storage":"postgres"' || fail "приложение потеряло доступ к базе"
  ok "к базе пускают только поды с меткой postgres-access=true"
}

sidecar() {
  echo "→ нативный sidecar"
  for _ in $(seq 1 30); do curl -sf "$URL/api/hello" >/dev/null; done
  for pod in $(k get pods -l app=kube-demo -o name); do
    ready=$(k get "$pod" -o jsonpath='{.status.initContainerStatuses[?(@.name=="access-log")].ready}')
    [ "$ready" = true ] || fail "$pod: sidecar access-log не ready"
  done
  contains "$(k logs -l app=kube-demo -c access-log --tail=100)" "GET /api/hello 200" || fail "в логах sidecar'а нет запросов"
  ok "sidecar работает во всех подах, access-лог виден через kubectl logs -c access-log"
}

nodes() {
  echo "→ DaemonSet node-agent"
  k rollout status daemonset/node-agent --timeout=120s
  want=$(kubectl get nodes --no-headers | wc -l)
  got=$(k get pods -l app=node-agent --no-headers | wc -l)
  [ "$got" = "$want" ] || fail "агентов $got, нод $want"
  n=$(curl -sf "$URL/api/nodes" | grep -o '"kernel"' | wc -l)
  [ "$n" = "$want" ] || fail "/api/nodes вернул $n нод из $want"
  ok "по агенту на каждой из $want нод (включая control-plane), приложение опрашивает всех"
}

backup() {
  echo "→ CronJob бэкапа"
  job=$(curl -sf -X POST "$URL/api/k8s/backup" | json job)
  [ -n "$job" ] || fail "приложение не создало Job"
  k wait --for=condition=complete "job/$job" --timeout=180s || { k logs "job/$job" || true; fail "Job $job не завершился"; }
  logs=$(k logs "job/$job"); echo "$logs"
  contains "$logs" "backup ok" || fail "в логах Job нет 'backup ok'"
  ok "Job $job из CronJob сделал дамп базы"
}

resize() {
  echo "→ in-place resize"
  pod=$(app_pod)
  before=$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="app")].restartCount}')
  curl -sf -X PUT "$URL/api/k8s/pods/$pod/cpu" -d '{"limitMilli":1000}' | json command
  retry 60 "cgroup cpu.max = 1 CPU" sh -c "kubectl -n $NS exec $pod -c app -- cat /sys/fs/cgroup/cpu.max | grep -q '^100000 100000'"
  after=$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="app")].restartCount}')
  [ "$before" = "$after" ] || fail "контейнер перезапустился ($before -> $after)"
  k get pod "$pod" -o jsonpath='{.spec.containers[?(@.name=="app")].resources}'; echo
  ok "$pod: лимит CPU 500m -> 1000m без рестарта (restartCount $after)"
}

oom() {
  echo "→ OOMKilled"
  pod=$(curl -sf -X POST "$URL/api/chaos/oom" | json pod)
  [ -n "$pod" ] || fail "не получили имя пода"
  retry 90 "OOMKilled у $pod" sh -c \
    "kubectl -n $NS get pod $pod -o jsonpath='{.status.containerStatuses[?(@.name==\"app\")].lastState.terminated.reason}' | grep -q OOMKilled"
  code=$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="app")].lastState.terminated.exitCode}')
  k wait --for=condition=Ready "pod/$pod" --timeout=90s
  ok "$pod: OOMKilled (exit $code), kubelet перезапустил контейнер"
}

broken_release() {
  echo "→ сломанный релиз"
  curl -sf -X POST "$URL/api/k8s/break" | json command
  if k rollout status deployment/kube-demo --timeout=150s; then
    fail "rollout сломанного релиза не должен был завершиться"
  fi
  reason=$(k get deployment kube-demo -o jsonpath='{.status.conditions[?(@.type=="Progressing")].reason}')
  [ "$reason" = ProgressDeadlineExceeded ] || fail "ожидали ProgressDeadlineExceeded, получили $reason"
  for _ in $(seq 1 30); do curl -sf "$URL/api/hello" >/dev/null || fail "сервис лёг во время сломанного релиза"; done
  curl -sf -X POST "$URL/api/k8s/undo" | json command
  k rollout status deployment/kube-demo --timeout=180s
  ok "сломанный релиз застрял (ProgressDeadlineExceeded), сервис работал, rollout undo всё вернул"
}

quota() {
  echo "→ ResourceQuota"
  kubectl apply -f k8s/extras/quota.yaml
  k describe quota kube-demo
  curl -sf -X PUT "$URL/api/k8s/replicas" -d '{"replicas":10}' | json command
  retry 60 "событие exceeded quota" sh -c "kubectl -n $NS get events | grep -q 'exceeded quota'"
  n=$(k get pods -l app=kube-demo --no-headers | wc -l)
  [ "$n" -lt 10 ] || fail "квота не сработала: подов $n"
  k get events --field-selector reason=FailedCreate | tail -3
  curl -sf -X PUT "$URL/api/k8s/replicas" -d '{"replicas":3}' >/dev/null
  kubectl delete -f k8s/extras/quota.yaml
  k rollout status deployment/kube-demo --timeout=180s
  ok "при квоте pods=10 создалось только $n подов приложения из 10"
}

observability() {
  echo "→ метрики и Prometheus"
  contains "$(curl -sf "$URL/metrics")" 'kube_demo_http_requests_total{' || fail "/metrics без счётчиков"
  kubectl apply -f k8s/extras/observability/prometheus.yaml
  k rollout status deployment/prometheus --timeout=180s
  retry 90 "Prometheus видит 3+ пода" sh -c \
    "curl -sf 'http://localhost:30090/api/v1/query?query=count(up%7Bjob%3D%22kube-demo%22%7D%3D%3D1)' | grep -Eq '\"value\":\[[^,]+,\"([3-9]|[1-9][0-9])\"'"
  curl -sf 'http://localhost:30090/api/v1/query?query=up%7Bjob%3D%22kube-demo%22%7D' | head -c 600; echo
  ok "Prometheus сам нашёл поды по аннотациям и собирает /metrics"
}

gateway() {
  echo "→ Gateway API (Traefik) + canary"
  kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
  helm repo add traefik https://traefik.github.io/charts >/dev/null
  helm upgrade --install traefik traefik/traefik --version 41.6.0 -n traefik --create-namespace \
    -f k8s/extras/gateway/traefik-values.yaml
  gateway_debug() {
    kubectl -n traefik get all,gateway,events -o wide || true
    kubectl -n traefik describe pods || true
    kubectl -n traefik logs deploy/traefik --tail=80 || true
    kubectl get gatewayclass -o yaml || true
    kubectl -n traefik get gateway -o yaml || true
    k get httproute -o yaml || true
  }
  kubectl -n traefik rollout status deployment/traefik --timeout=180s || { gateway_debug; fail "Traefik не поднялся"; }
  kubectl -n traefik wait --for=condition=Programmed gateway/traefik-gateway --timeout=120s || { gateway_debug; fail "Gateway не Programmed"; }
  kubectl apply -f k8s/extras/gateway/canary.yaml -f k8s/extras/gateway/httproute.yaml
  k rollout status deployment/kube-demo-canary --timeout=180s
  # Traefik подхватывает HTTPRoute асинхронно: ждём 10 успешных ответов подряд.
  streak=0
  for _ in $(seq 1 240); do
    if curl -sf -o /dev/null http://localhost:30081/api/hello; then
      streak=$((streak+1)); [ "$streak" -ge 10 ] && break
    else
      streak=0
    fi
    sleep 0.5
  done
  [ "$streak" -ge 10 ] || { gateway_debug; fail "через Gateway нет стабильных ответов"; }

  canary=0; main=0; errors=""
  for _ in $(seq 1 60); do
    body=$(curl -s -w '\n%{http_code}' http://localhost:30081/api/hello || true)
    code=${body##*$'\n'}
    if [ "$code" != 200 ]; then errors+=" $code"; continue; fi
    pod=$(json pod <<<"$body")
    case "$pod" in kube-demo-canary-*) canary=$((canary+1)) ;; kube-demo-*) main=$((main+1)) ;; esac
  done
  echo "   из 60 запросов: основная версия $main, canary $canary, ошибки:${errors:- нет}"
  [ -z "$errors" ] || { gateway_debug; fail "ошибки при запросах через Gateway:$errors"; }
  [ "$canary" -ge 2 ] && [ "$main" -ge 30 ] || fail "веса 80/20 не соблюдаются"
  for _ in $(seq 1 10); do
    pod=$(curl -s -H 'X-Canary: always' http://localhost:30081/api/hello | json pod)
    [[ "$pod" == kube-demo-canary-* ]] || fail "X-Canary: always привёл на '$pod', а не на canary"
  done
  ok "Gateway делит трафик ~80/20, заголовок X-Canary: always ведёт на canary"
}

replicas() { k get deployment kube-demo -o jsonpath='{.spec.replicas}'; }

rps_autoscale() {
  echo "→ автоскейлинг по RPS (KEDA + Prometheus)"
  kubectl apply -f k8s/extras/observability/prometheus.yaml >/dev/null
  k rollout status deployment/prometheus --timeout=180s
  helm repo add kedacore https://kedacore.github.io/charts >/dev/null
  helm upgrade --install keda kedacore/keda --version 2.21.0 -n keda --create-namespace >/dev/null
  kubectl -n keda rollout status deployment/keda-operator --timeout=180s
  kubectl -n keda rollout status deployment/keda-operator-metrics-apiserver --timeout=180s
  kubectl apply -f k8s/extras/autoscaling/keda-rps.yaml
  retry 120 "KEDA создала HPA" kubectl -n "$NS" get hpa keda-hpa-kube-demo-rps
  retry 180 "без нагрузки реплик = minReplicaCount (2)" sh -c "[ \"\$(kubectl -n $NS get deployment kube-demo -o jsonpath='{.spec.replicas}')\" = 2 ]"
  ok "без нагрузки: $(replicas) реплики"

  # ~50 rps: 6 «клиентов» по ~9 запросов в секунду
  load_pids=""
  for _ in 1 2 3 4 5 6; do
    ( while true; do curl -s -o /dev/null --max-time 2 "$URL/api/hello"; sleep 0.1; done ) &
    load_pids+=" $!"
  done
  trap 'kill $load_pids 2>/dev/null || true' EXIT
  start=$(date +%s)
  retry 180 "под нагрузкой реплик >= 4" sh -c "[ \$(kubectl -n $NS get deployment kube-demo -o jsonpath='{.spec.replicas}') -ge 4 ]"
  echo "   под нагрузкой: $(replicas) реплик через $(( $(date +%s) - start )) с"
  k get hpa keda-hpa-kube-demo-rps
  state=$(curl -sf "$URL/api/k8s/state")
  contains "$state" '"name":"keda-hpa-kube-demo-rps"' || fail "API приложения не видит HPA"
  grep -o '"metrics":\["[^]]*\]' <<<"$state" | head -1

  kill $load_pids 2>/dev/null || true; trap - EXIT
  start=$(date +%s)
  retry 240 "после нагрузки реплик снова 2" sh -c "[ \"\$(kubectl -n $NS get deployment kube-demo -o jsonpath='{.spec.replicas}')\" = 2 ]"
  echo "   нагрузку сняли: 2 реплики через $(( $(date +%s) - start )) с"

  kubectl delete -f k8s/extras/autoscaling/keda-rps.yaml
  curl -sf -X PUT "$URL/api/k8s/replicas" -d '{"replicas":3}' >/dev/null
  k rollout status deployment/kube-demo --timeout=180s
  ok "KEDA масштабирует по RPS: 2 → больше под нагрузкой → снова 2"
}

# served_by_slot <green|blue> — все последние 10 ответов от нужной версии?
served_by_slot() {
  local want=$1 pod
  for _ in $(seq 1 10); do
    pod=$(curl -s --max-time 2 "$URL/api/hello" | json pod)
    case "$want:$pod" in
      green:kube-demo-green-*) ;;
      blue:kube-demo-green-*) return 1 ;;
      blue:kube-demo-*) ;;
      *) return 1 ;;
    esac
  done
}

bluegreen() {
  echo "→ blue/green"
  kubectl apply -f k8s/extras/bluegreen/green.yaml
  k rollout status deployment/kube-demo-green --timeout=180s

  # непрерывная нагрузка на всё время переключений — ошибок быть не должно
  log=$(mktemp)
  ( while true; do curl -s -o /dev/null --max-time 3 -w '%{http_code}\n' "$URL/api/hello" >>"$log" || true; sleep 0.05; done ) &
  loader=$!
  trap 'kill $loader 2>/dev/null || true' EXIT
  sleep 2

  curl -sf -X POST "$URL/api/k8s/bluegreen/green" | json command
  retry 30 "весь трафик на green" served_by_slot green
  ok "трафик переключён на green"
  curl -sf -X POST "$URL/api/k8s/bluegreen/blue" | json command
  retry 30 "весь трафик снова на blue" served_by_slot blue
  ok "откат на blue"
  sleep 2
  kill $loader 2>/dev/null || true; trap - EXIT
  total=$(wc -l <"$log"); errors=$(grep -vc '^200$' "$log" || true)
  echo "   во время переключений: запросов $total, ошибок $errors"
  [ "$errors" -eq 0 ] || fail "при переключении blue/green были ошибки"

  # защита: неготовый green не принимает трафик
  k scale deployment/kube-demo-green --replicas=0
  retry 60 "green без подов" sh -c "[ -z \"\$(kubectl -n $NS get deployment kube-demo-green -o jsonpath='{.status.readyReplicas}')\" ]"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/api/k8s/bluegreen/green")
  [ "$code" = 409 ] || fail "переключение на пустой green должно давать 409, получили $code"
  served_by_slot blue || fail "трафик ушёл с blue"
  kubectl delete -f k8s/extras/bluegreen/green.yaml
  ok "переключение на неготовый green отклонено (409), трафик остался на blue"
}

# app_pods_table: NODE DELETED READY для каждого пода приложения (<none> = поля нет)
app_pods_table() {
  k get pods -l app=kube-demo --no-headers \
    -o custom-columns='NODE:.spec.nodeName,DEL:.metadata.deletionTimestamp,READY:.status.conditions[?(@.type=="Ready")].status'
}

node_failure() {
  echo "→ drain и отказ ноды"
  pg_node=$(k get pod postgres-0 -o jsonpath='{.spec.nodeName}')
  victim=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o name | sed 's#node/##' | grep -vx "$pg_node" | head -1)
  [ -n "$victim" ] || fail "нет воркера без postgres-0"
  echo "   жертва: $victim (postgres-0 живёт на $pg_node)"

  # 1. drain через API приложения
  curl -sf -X POST "$URL/api/k8s/nodes/$victim/drain" | json command
  retry 120 "поды приложения ушли с $victim" sh -c \
    "[ \$(kubectl -n $NS get pods -l app=kube-demo --field-selector spec.nodeName=$victim --no-headers 2>/dev/null | wc -l) -eq 0 ]"
  [ "$(kubectl get node "$victim" -o jsonpath='{.spec.unschedulable}')" = true ] || fail "$victim не в cordon"
  k rollout status deployment/kube-demo --timeout=120s
  ok "drain: поды приложения выселены с $victim, нода в cordon"

  # 2. uncordon и перераскатка, чтобы на ноде снова были поды
  curl -sf -X POST "$URL/api/k8s/nodes/$victim/uncordon" | json command
  curl -sf -X POST "$URL/api/k8s/restart" >/dev/null
  k rollout status deployment/kube-demo --timeout=180s
  on_victim=$(app_pods_table | awk -v v="$victim" '$1==v && $2=="<none>"' | wc -l)
  echo "   после uncordon на $victim подов приложения: $on_victim"
  [ "$on_victim" -ge 1 ] || fail "после uncordon на $victim не оказалось подов"

  # 3. роняем ноду
  docker pause "$victim"
  trap 'docker unpause "$victim" >/dev/null 2>&1 || true' EXIT
  start=$(date +%s)
  retry 150 "нода $victim перестала быть Ready" sh -c \
    "[ \"\$(kubectl get node $victim -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}')\" != True ]"
  echo "   $victim NotReady через $(( $(date +%s) - start )) с"
  contains "$(curl -sf "$URL/api/k8s/state")" "\"name\":\"$victim\",\"role\":\"worker\",\"ready\":\"Unknown\"" \
    || fail "API приложения не видит, что $victim не отвечает"

  retry 240 "3 Ready-пода приложения на живых нодах" sh -c \
    "[ \$(kubectl -n $NS get pods -l app=kube-demo --no-headers -o custom-columns='NODE:.spec.nodeName,DEL:.metadata.deletionTimestamp,READY:.status.conditions[?(@.type==\"Ready\")].status' | awk -v v=$victim '\$1!=v && \$2==\"<none>\" && \$3==\"True\"' | wc -l) -ge 3 ]"
  echo "   замена поднялась через $(( $(date +%s) - start )) с после отказа"
  k get pods -o wide

  streak=0
  for _ in $(seq 1 120); do
    if curl -sf -o /dev/null --max-time 2 "$URL/api/hello"; then streak=$((streak+1)); [ "$streak" -ge 20 ] && break; else streak=0; fi
    sleep 0.5
  done
  [ "$streak" -ge 20 ] || fail "после отказа ноды сервис не стабилен"
  ok "нода $victim упала, поды переехали, сервис отвечает"

  # 4. возвращаем ноду
  docker unpause "$victim"; trap - EXIT
  kubectl wait --for=condition=Ready "node/$victim" --timeout=180s
  ok "нода $victim вернулась"
}

case "${1:-all}" in
  all)
    for f in network_policy sidecar nodes backup resize oom broken_release quota observability rps_autoscale bluegreen gateway node_failure; do $f; done ;;
  *) "${1//-/_}" ;;
esac
