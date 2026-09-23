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

case "${1:-all}" in
  all)
    for f in network_policy sidecar nodes backup resize oom broken_release quota observability gateway; do $f; done ;;
  *) "${1//-/_}" ;;
esac
