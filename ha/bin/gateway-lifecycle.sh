#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="/work/SPK-OS/soft/basic/new-api/ha"
PG_PASSWORD_FILE="/work/SPK-OS/env/secrets/infra-pg-prod-password"
REDIS_PASSWORD_FILE="/work/SPK-OS/env/secrets/infra-redis-prod-password"
COMPOSE_FILE="$BASE_DIR/docker-compose.ha.yml"

action="${1:-}"
case "$action" in
  start|stop|restart) ;;
  *) echo "usage: $0 {start|stop|restart}" >&2; exit 2 ;;
esac

for secret_file in "$PG_PASSWORD_FILE" "$REDIS_PASSWORD_FILE"; do
  if [[ ! -r "$secret_file" ]]; then
    echo "required secret is unreadable: $secret_file" >&2
    exit 1
  fi
done

export PG_PASSWORD REDIS_PASSWORD
PG_PASSWORD="$(tr -d '\r\n' < "$PG_PASSWORD_FILE")"
REDIS_PASSWORD="$(tr -d '\r\n' < "$REDIS_PASSWORD_FILE")"

compose=(docker compose -f "$COMPOSE_FILE")

preflight() {
  local container state
  for container in spk-pg-prod spk-redis-prod; do
    state="$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container" 2>/dev/null || true)"
    if [[ "$state" != "running healthy" ]]; then
      echo "new-api prerequisite is not healthy: $container ($state)" >&2
      return 1
    fi
  done
  docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
    psql -h 127.0.0.1 -U spk -d new-api -Atqc 'select 1' | grep -qx 1
  docker exec spk-redis-prod redis-cli -a "$REDIS_PASSWORD" --no-auth-warning ping | grep -qx PONG
}

infra_network_stale() {
  local expected_network_id actual_network_id container
  expected_network_id="$(docker network inspect -f '{{.Id}}' spk-infra-net 2>/dev/null || true)"
  if [[ -z "$expected_network_id" ]]; then
    echo "new-api prerequisite network is missing: spk-infra-net" >&2
    return 0
  fi

  for container in new-api-blue new-api-green; do
    if ! docker container inspect "$container" >/dev/null 2>&1; then
      continue
    fi
    actual_network_id="$(docker inspect -f '{{with index .NetworkSettings.Networks "spk-infra-net"}}{{.NetworkID}}{{end}}' "$container" 2>/dev/null || true)"
    if [[ "$actual_network_id" != "$expected_network_id" ]]; then
      echo "new-api stale shared network attachment: $container (${actual_network_id:-missing} != $expected_network_id)" >&2
      return 0
    fi
  done
  return 1
}

start_gateway() {
  # PostgreSQL/Redis 由 spk-infra 统一提供，preflight 已验证其健康和认证。
  # 本 Compose 只管理无状态网关节点与入口 Nginx。
  if infra_network_stale; then
    # 只重建无状态网关容器，使其绑定当前 spk-infra-net；不删卷和数据容器。
    "${compose[@]}" up -d --force-recreate blue green nginx
  else
    "${compose[@]}" up -d blue green nginx
  fi
}

cd "$BASE_DIR"
case "$action" in
  start)
    preflight
    start_gateway
    ;;
  stop)
    "${compose[@]}" stop nginx green blue
    ;;
  restart)
    "${compose[@]}" stop nginx green blue
    preflight
    start_gateway
    ;;
esac
