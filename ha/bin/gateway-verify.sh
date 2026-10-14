#!/bin/bash
# /work/SPK-OS/soft/basic/new-api/ha/bin/gateway-verify.sh
# LLM Gateway 高可用部署验证脚本
# 用法: ./gateway-verify.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
NGINX_CONF="$HA_DIR/nginx/upstream.conf"
PG_PASSWORD_FILE="/work/SPK-OS/env/secrets/infra-pg-prod-password"
REDIS_PASSWORD_FILE="/work/SPK-OS/env/secrets/infra-redis-prod-password"

for secret_file in "$PG_PASSWORD_FILE" "$REDIS_PASSWORD_FILE"; do
    if [[ ! -r "$secret_file" ]]; then
        echo "required secret is unreadable: $secret_file" >&2
        exit 1
    fi
done

PG_PASSWORD="$(tr -d '\r\n' < "$PG_PASSWORD_FILE")"
REDIS_PASSWORD="$(tr -d '\r\n' < "$REDIS_PASSWORD_FILE")"

echo "═══════════════════════════════════════"
echo "  LLM Gateway HA 验证"
echo "═══════════════════════════════════════"
echo ""

# 1. Nginx 端口可达
echo -n "1. Nginx (:3500) 端口可达: "
if curl -sf http://localhost:3500/api/status 2>/dev/null | grep -q '"success":.*true'; then
    echo "✓"
else
    echo "✗"
fi

# 2. 活跃节点直连
ACTIVE=$(grep -oP 'server new-api-\K(blue|green)(?=:\d+;)' "$NGINX_CONF" | head -1)
if [[ "$ACTIVE" == "blue" ]]; then
    ACTIVE_PORT=3501
else
    ACTIVE_PORT=3502
fi
echo -n "2. 活跃节点 $ACTIVE (:$ACTIVE_PORT) 直连: "
if curl -sf "http://localhost:$ACTIVE_PORT/api/status" 2>/dev/null | grep -q '"success":.*true'; then
    echo "✓"
else
    echo "✗"
fi

# 3. 备用节点直连
if [[ "$ACTIVE" == "blue" ]]; then
    STANDBY="green"
    STANDBY_PORT=3502
else
    STANDBY="blue"
    STANDBY_PORT=3501
fi
echo -n "3. 备用节点 $STANDBY (:$STANDBY_PORT) 直连: "
if curl -sf "http://localhost:$STANDBY_PORT/api/status" 2>/dev/null | grep -q '"success":.*true'; then
    echo "✓"
else
    echo "✗ (未启动或不可达)"
fi

# 4. 数据库连接
echo -n "4. PostgreSQL 连接: "
if docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
    pg_isready -h 127.0.0.1 -U spk -d new-api 2>/dev/null | grep -q "accepting"; then
    echo "✓"
else
    echo "✗"
fi

# 5. Redis 连接
echo -n "5. Redis 连接: "
if docker exec spk-redis-prod redis-cli -a "$REDIS_PASSWORD" --no-auth-warning ping 2>/dev/null | grep -q PONG; then
    echo "✓"
else
    echo "✗"
fi

# 6. 数据完整性
echo -n "6. 渠道数量 (PG): "
CH_COUNT=$(docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
    psql -h 127.0.0.1 -U spk -d new-api -t -c "SELECT COUNT(*) FROM channels;" 2>/dev/null | xargs)
echo "$CH_COUNT"

echo -n "7. Token 数量 (PG): "
TK_COUNT=$(docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
    psql -h 127.0.0.1 -U spk -d new-api -t -c "SELECT COUNT(*) FROM tokens;" 2>/dev/null | xargs)
echo "$TK_COUNT"

echo -n "8. 用户数量 (PG): "
USR_COUNT=$(docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
    psql -h 127.0.0.1 -U spk -d new-api -t -c "SELECT COUNT(*) FROM users;" 2>/dev/null | xargs)
echo "$USR_COUNT"

# 9. API 功能测试
echo -n "9. API /v1/models 可用: "
TEST_TOKEN="${NEW_API_TEST_TOKEN:-}"
if [[ -z "$TEST_TOKEN" ]]; then
    TEST_TOKEN="$(docker exec -e PGPASSWORD="$PG_PASSWORD" spk-pg-prod \
        psql -h 127.0.0.1 -U spk -d new-api -Atqc \
        "SELECT key FROM tokens WHERE status=1 AND deleted_at IS NULL ORDER BY id LIMIT 1" 2>/dev/null)"
    [[ -n "$TEST_TOKEN" && "$TEST_TOKEN" != sk-* ]] && TEST_TOKEN="sk-$TEST_TOKEN"
fi
if [[ -n "$TEST_TOKEN" ]] && \
    curl -sf http://localhost:3500/v1/models -H "Authorization: Bearer $TEST_TOKEN" 2>/dev/null | grep -q '"data"'; then
    echo "✓"
else
    echo "✗ (需验证 Token)"
fi

# 10. Redis 亲和绑定（共享 prod Redis）
echo -n "10. Redis 亲和绑定: "
AFF_COUNT=$(docker exec spk-redis-prod redis-cli -a "$REDIS_PASSWORD" --no-auth-warning \
    keys "gateway:affinity:*" 2>/dev/null | wc -l)
echo "$AFF_COUNT 条"

echo ""
echo "═══════════════════════════════════════"
echo "  验证完成"
echo "═══════════════════════════════════════"
