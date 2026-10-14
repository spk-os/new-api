#!/usr/bin/env bash
set -euo pipefail

HA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIFECYCLE="$HA_DIR/bin/gateway-lifecycle.sh"
VERIFY="$HA_DIR/bin/gateway-verify.sh"
ENV_FILE="$HA_DIR/.env.ha"
COMPOSE_FILE="$HA_DIR/docker-compose.ha.yml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

bash -n "$LIFECYCLE"
bash -n "$VERIFY"

services="$({
  PG_PASSWORD=test-only REDIS_PASSWORD=test-only \
    docker compose -f "$COMPOSE_FILE" config --services
} 2>/dev/null)"

for expected in nginx blue green; do
  grep -qx "$expected" <<<"$services" \
    || fail "compose service is missing: $expected"
done

if grep -qx redis <<<"$services"; then
  fail "dedicated Redis must not return to the new-api compose stack"
fi

if grep -Eq '(^|[[:space:]])(up -d|stop)[^#\n]*[[:space:]]redis([[:space:]]|$)' "$LIFECYCLE"; then
  fail "lifecycle references removed compose service: redis"
fi

grep -Fq 'spk-pg-prod spk-redis-prod' "$LIFECYCLE" \
  || fail "shared infrastructure health gate is missing"

grep -Fq "(?=:\\d+;)" "$VERIFY" \
  || fail "verification must ignore backup upstreams when detecting the active node"

if grep -Eq 'new-api-redis|docker-db_postgres-1|redis_secure_2026' "$LIFECYCLE" "$VERIFY" "$ENV_FILE"; then
  fail "lifecycle verification still references retired infrastructure"
fi

for shared_container in spk-pg-prod spk-redis-prod; do
  grep -Fq "$shared_container" "$VERIFY" \
    || fail "verification does not check shared container: $shared_container"
done

echo "PASS: new-api lifecycle matches shared spk-infra compose topology"
