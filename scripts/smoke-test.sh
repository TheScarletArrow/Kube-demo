#!/usr/bin/env bash
# Smoke-тест развёрнутого приложения: отвечает, балансирует, пишет в Postgres.
#   ./scripts/smoke-test.sh [http://localhost:30080]
set -euo pipefail

URL="${1:-http://localhost:30080}"
REQUESTS="${REQUESTS:-30}"

fail() { echo "❌ $*" >&2; exit 1; }

echo "→ жду, пока $URL начнёт отвечать..."
for i in $(seq 1 60); do
  curl -sf --max-time 2 "$URL/api/hello" >/dev/null && break
  [ "$i" = 60 ] && fail "приложение не ответило за 60 с"
  sleep 1
done

echo "→ $REQUESTS запросов к сервису"
pods=()
for _ in $(seq 1 "$REQUESTS"); do
  body=$(curl -sf --max-time 2 "$URL/api/hello") || fail "запрос завершился ошибкой"
  pods+=("$(grep -o '"pod":"[^"]*"' <<<"$body" | cut -d'"' -f4)")
  storage=$(grep -o '"storage":"[^"]*"' <<<"$body" | cut -d'"' -f4)
done
unique=$(printf '%s\n' "${pods[@]}" | sort | uniq -c)
echo "$unique"

[ "$storage" = "postgres" ] || fail "ожидался storage=postgres, получили $storage"
[ "$(wc -l <<<"$unique")" -ge 2 ] || fail "все запросы ушли в один под — балансировка не работает"

echo "→ гостевая книга"
marker="smoke-$(date +%s)"
curl -sf --max-time 2 -X POST "$URL/api/messages" -H 'Content-Type: application/json' \
  -d "{\"author\":\"smoke-test\",\"text\":\"$marker\"}" >/dev/null || fail "не удалось записать сообщение"
curl -sf --max-time 2 "$URL/api/messages" | grep -q "$marker" || fail "сообщение не читается (другой под не видит данные?)"

echo "✅ smoke-тест пройден"
