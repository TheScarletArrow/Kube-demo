#!/usr/bin/env bash
# Проверяем, что rolling update проходит без ошибок у клиентов:
# в фоне непрерывно шлём запросы, параллельно обновляем Deployment.
#   ./scripts/zero-downtime-test.sh [http://localhost:30080]
set -euo pipefail

URL="${1:-http://localhost:30080}"
NS="${NS:-kube-demo}"

log=$(mktemp)
trap 'rm -f "$log"' EXIT

(
  while true; do
    # при сетевой ошибке curl сам напечатает 000 через -w
    curl -s -o /dev/null --max-time 3 -w '%{http_code}\n' "$URL/api/hello" >>"$log" || true
    sleep 0.05
  done
) &
loader=$!

sleep 2
# rollout restart всегда запускает новый rollout (в отличие от set env с тем же значением).
echo "→ rolling update: kubectl rollout restart"
kubectl -n "$NS" rollout restart deployment/kube-demo
kubectl -n "$NS" rollout status deployment/kube-demo --timeout=180s
sleep 3
kill "$loader"
wait "$loader" 2>/dev/null || true

total=$(wc -l <"$log")
errors=$(grep -vc '^200$' "$log" || true)
echo "запросов: $total, ошибок: $errors"
sort "$log" | uniq -c

[ "$errors" -eq 0 ] || { echo "❌ во время обновления были ошибки"; exit 1; }
echo "✅ обновление прошло без единой ошибки"
