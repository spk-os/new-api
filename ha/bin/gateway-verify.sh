#!/bin/bash
# /work/SPK-OS/soft/basic/new-api/ha/bin/gateway-verify.sh
# LLM Gateway 高可用部署验证脚本
# 用法: ./gateway-verify.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
NGINX_CONF="$HA_DIR/nginx/upstream.conf"

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
ACTIVE=$(grep -oP 'server new-api-\K(blue|green)' "$NGINX_CONF" | head -1)
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
if docker exec docker-db_postgres-1 pg_isready -U postgres -d new-api 2>/dev/null | grep -q "accepting"; then
    echo "✓"
else
    echo "✗"
fi

# 5. Redis 连接
echo -n "5. Redis 连接: "
if docker exec new-api-redis redis-cli -a redis_secure_2026 ping 2>/dev/null | grep -q PONG; then
    echo "✓"
else
    echo "✗"
fi

# 6. 数据完整性
echo -n "6. 渠道数量 (PG): "
CH_COUNT=$(docker exec docker-db_postgres-1 psql -U postgres -d new-api -t -c "SELECT COUNT(*) FROM channels;" 2>/dev/null | xargs)
echo "$CH_COUNT (预期: 10)"

echo -n "7. Token 数量 (PG): "
TK_COUNT=$(docker exec docker-db_postgres-1 psql -U postgres -d new-api -t -c "SELECT COUNT(*) FROM tokens;" 2>/dev/null | xargs)
echo "$TK_COUNT (预期: 2)"

echo -n "8. 用户数量 (PG): "
USR_COUNT=$(docker exec docker-db_postgres-1 psql -U postgres -d new-api -t -c "SELECT COUNT(*) FROM users;" 2>/dev/null | xargs)
echo "$USR_COUNT (预期: 1)"

# 9. API 功能测试
echo -n "9. API /v1/models 可用: "
if curl -sf http://localhost:3500/v1/models -H "Authorization: Bearer sk-test" 2>/dev/null | grep -q "data"; then
    echo "✓"
else
    echo "✗ (需验证 Token)"
fi

# 10. Redis 亲和绑定
echo -n "10. Redis 亲和绑定: "
AFF_COUNT=$(docker exec new-api-redis redis-cli -a redis_secure_2026 keys "gateway:affinity:*" 2>/dev/null | wc -l)
echo "$AFF_COUNT 条"

echo ""
echo "═══════════════════════════════════════"
echo "  验证完成"
echo "═══════════════════════════════════════"
