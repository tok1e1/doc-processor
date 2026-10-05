#!/usr/bin/env bash
# End-to-end check against a running stack (docker compose up).
set -euo pipefail

BASE_URL=${BASE_URL:-http://localhost:8080}
DIR=$(cd "$(dirname "$0")/.." && pwd)

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "waiting for $BASE_URL/readyz"
for _ in $(seq 1 60); do
  curl -fsS "$BASE_URL/readyz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$BASE_URL/readyz" >/dev/null || fail "service is not ready"

key="smoke-$(date +%s)-$RANDOM"
for template in invoice offer_letter; do
  resp=$(curl -sS -w '\n%{http_code}' -X POST "$BASE_URL/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Idempotency-Key: $key-$template" \
    --data @"$DIR/examples/$template.json")
  code=$(tail -n1 <<<"$resp"); body=$(sed '$d' <<<"$resp")
  [ "$code" = "202" ] || fail "$template: expected 202, got $code: $body"
  id=$(jq -r .id <<<"$body")

  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/v1/jobs" \
    -H 'Content-Type: application/json' -H "Idempotency-Key: $key-$template" \
    --data @"$DIR/examples/$template.json")
  [ "$code" = "200" ] || fail "$template: repeated request expected 200, got $code"

  status=""
  for _ in $(seq 1 60); do
    status=$(curl -fsS "$BASE_URL/api/v1/jobs/$id" | jq -r .status)
    [ "$status" = "done" ] || [ "$status" = "failed" ] && break
    sleep 1
  done
  [ "$status" = "done" ] || fail "$template: job $id finished with status '$status'"

  out=$(mktemp)
  curl -fsS "$BASE_URL/api/v1/jobs/$id/document" -o "$out"
  [ "$(head -c 5 "$out")" = "%PDF-" ] || fail "$template: document is not a PDF"
  echo "ok: $template -> $id ($(wc -c <"$out") bytes)"
  rm -f "$out"
done

code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/v1/jobs" \
  -H 'Content-Type: application/json' --data '{"template":"invoice","data":{"number":""}}')
[ "$code" = "422" ] || fail "invalid payload: expected 422, got $code"
echo "ok: validation"

echo "smoke test passed"
