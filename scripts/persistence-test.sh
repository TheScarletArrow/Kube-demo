#!/usr/bin/env bash
# E2E: управление кластером через API приложения + сохранность данных при смене подов.
#   1. scale через приложение (аналог kubectl scale) — проверяем kubectl'ом;
#   2. пишем сообщения и делаем снимок (кол-во + SHA-256);
#   3. убиваем под-автора снимка, postgres-0 и перезапускаем все поды;
#   4. новый под сверяет данные со снимком.
#   ./scripts/persistence-test.sh [http://localhost:30080]
set -euo pipefail

URL="${1:-http://localhost:30080}"
NS="${NS:-kube-demo}"

fail() { echo "❌ $*" >&2; exit 1; }
json() { grep -o "\"$1\":[^,}]*" | head -1 | cut -d: -f2- | tr -d '"'; }

echo "→ scale до 4 реплик через API приложения"
curl -sf -X PUT "$URL/api/k8s/replicas" -d '{"replicas":4}' | json command
kubectl -n "$NS" rollout status deployment/kube-demo --timeout=120s
[ "$(kubectl -n "$NS" get deployment kube-demo -o jsonpath='{.spec.replicas}')" = 4 ] || fail "replicas != 4"
curl -sf -X PUT "$URL/api/k8s/replicas" -d '{"replicas":3}' >/dev/null
kubectl -n "$NS" rollout status deployment/kube-demo --timeout=120s

echo "→ RBAC: чужие действия запрещены"
kubectl -n "$NS" auth can-i delete deployments --as="system:serviceaccount:$NS:kube-demo" && fail "SA не должен удалять deployments"

echo "→ пишем сообщения и делаем снимок"
for i in 1 2 3 4 5; do
  curl -sf -X POST "$URL/api/messages" -d "{\"author\":\"e2e\",\"text\":\"сообщение $i\"}" >/dev/null
done
snap=$(curl -sf -X POST "$URL/api/snapshots")
id=$(json id <<<"$snap"); writer=$(json pod <<<"$snap"); count=$(json messages <<<"$snap")
echo "   снимок #$id: $count сообщений, автор $writer"

echo "→ убиваем автора снимка (через API приложения), базу и все остальные поды"
curl -sf -X DELETE "$URL/api/k8s/pods/$writer" | json command
kubectl -n "$NS" delete pod postgres-0 --wait=false
curl -sf -X POST "$URL/api/k8s/restart" | json command
sleep 2
kubectl -n "$NS" rollout status statefulset/postgres --timeout=180s
kubectl -n "$NS" rollout status deployment/kube-demo --timeout=180s
kubectl -n "$NS" get pods -o wide

echo "→ проверяем снимок новым подом"
for i in $(seq 1 60); do
  check=$(curl -s --max-time 3 "$URL/api/snapshots/$id" || true)
  if [[ "$check" == *'"ok":true'* ]]; then break; fi
  [ "$i" = 60 ] && fail "снимок так и не сошёлся: $check"
  sleep 2
done
echo "   $check"
[[ "$check" == *'"writerAlive":false'* ]] || fail "автор снимка должен быть удалён"
[[ "$check" != *"\"checkedBy\":\"$writer\""* ]] || fail "проверять должен другой под"

echo "✅ поды пересозданы (включая базу), данные на месте"
