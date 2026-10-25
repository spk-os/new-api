#!/bin/bash
# /work/spk-os/soft/basic/new-api/ha/bin/gateway-upgrade.sh
# LLM Gateway 蓝绿升级脚本
# 用法: ./gateway-upgrade.sh <new-image-tag>
# 示例: ./gateway-upgrade.sh new-api:gateway-v2.1.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
NEW_IMAGE="${1:?用法: $0 <new-image-tag>}"

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }

# Step 1: 确认当前状态
ACTIVE=$("$SCRIPT_DIR/gateway-switch.sh" status 2>/dev/null | grep "活跃节点" | grep -oP '(blue|green)')
STANDBY=$([[ "$ACTIVE" == "blue" ]] && echo "green" || echo "blue")
STANDBY_PORT=$([[ "$STANDBY" == "blue" ]] && echo 3501 || echo 3502)

log "═══════════════════════════════════════"
log "  LLM Gateway 蓝绿升级"
log "  当前活跃: $ACTIVE -> 升级目标: $STANDBY"
log "  新镜像: $NEW_IMAGE"
log "═══════════════════════════════════════"

# Step 2: 备份数据库（预校验 + dump + 后校验 + 保留策略）
log "Phase 1: 备份数据库..."
"$SCRIPT_DIR/gateway-backup.sh" || { log "ERROR: 备份失败，升级中止"; exit 1; }

# Step 3: 拉取/构建新镜像
log "Phase 2: 准备新镜像..."
if docker image inspect "$NEW_IMAGE" &>/dev/null; then
    log "  镜像 $NEW_IMAGE 已存在"
else
    log "  拉取镜像 $NEW_IMAGE..."
    docker pull "$NEW_IMAGE" || { log "ERROR: 镜像拉取失败"; exit 1; }
fi

# Step 4: 停止并更新 standby 容器
log "Phase 3: 更新 $STANDBY 容器..."
cd "$HA_DIR"
docker stop "new-api-$STANDBY" 2>/dev/null || true
docker rm "new-api-$STANDBY" 2>/dev/null || true

# 使用新镜像启动 standby
# docker-compose.ha.yml 的 SQL_DSN/REDIS_CONN_STRING 要求 PG_PASSWORD/REDIS_PASSWORD
# 从 secrets 导出（同 gateway-lifecycle.sh），否则 compose 插值直接失败。
PG_PASSWORD_FILE="/work/spk-os/env/secrets/infra-pg-prod-password"
REDIS_PASSWORD_FILE="/work/spk-os/env/secrets/infra-redis-prod-password"
export PG_PASSWORD="$(tr -d '\r\n' < "$PG_PASSWORD_FILE")"
export REDIS_PASSWORD="$(tr -d '\r\n' < "$REDIS_PASSWORD_FILE")"
GATEWAY_IMAGE="$NEW_IMAGE" docker compose -f docker-compose.ha.yml up -d "$STANDBY"
log "  $STANDBY 容器已启动 ✓"

# Step 5: 等待健康检查
log "Phase 4: 等待 $STANDBY 就绪..."
RETRIES=0
MAX_RETRIES=20
while [[ $RETRIES -lt $MAX_RETRIES ]]; do
    if curl -sf "http://localhost:$STANDBY_PORT/api/status" 2>/dev/null | grep -q '"success":.*true'; then
        log "  $STANDBY 健康检查通过 ✓"
        break
    fi
    RETRIES=$((RETRIES + 1))
    sleep 5
    log "  等待中... ($RETRIES/$MAX_RETRIES)"
done

if [[ $RETRIES -eq $MAX_RETRIES ]]; then
    log "ERROR: $STANDBY 启动超时，升级中止"
    log "  请检查日志: docker logs new-api-$STANDBY"
    exit 1
fi

# Step 6: 功能验证
log "Phase 5: 功能验证..."
TEST_RESP=$(curl -sf "http://localhost:$STANDBY_PORT/api/status" 2>/dev/null)
if [[ -z "$TEST_RESP" ]]; then
    log "ERROR: API 验证失败"
    exit 1
fi
log "  API 验证通过 ✓"

# Step 7: 切换流量
log "Phase 6: 切换流量到 $STANDBY..."
"$SCRIPT_DIR/gateway-switch.sh" "$STANDBY"

# Step 8: 观察期
log "Phase 7: 观察期（60秒）..."
sleep 60

# 检查错误率（grep -c 无匹配时输出 0 且退出码 1，不能再用 || echo 0，会拼出两行）
ERROR_COUNT=$(docker logs --since 60s "new-api-$STANDBY" 2>&1 | grep -ci "error") || ERROR_COUNT=0
if [[ $ERROR_COUNT -gt 10 ]]; then
    log "WARN: 观察期内发现 $ERROR_COUNT 条错误日志"
    log "  如需回滚: $SCRIPT_DIR/gateway-switch.sh rollback"
else
    log "  观察期通过 ✓ (错误数: $ERROR_COUNT)"
fi

# Step 9: 停止旧容器
log "Phase 8: 停止旧容器 $ACTIVE（保留用于回滚）..."
docker stop "new-api-$ACTIVE"
log "  $ACTIVE 已停止 ✓"

# Step 10: 回写默认镜像标签 new-api:gateway -> 本次新镜像
# 使 .env.ha 的 GATEWAY_IMAGE=new-api:gateway 默认启动路径始终指向最新已部署镜像，
# 避免后续不带 GATEWAY_IMAGE 覆盖的 compose up / --force-recreate / 容器重建
# 用旧标签重建容器、把升级冲回（2026-08-03 踩坑：标签停留在 Jul 23 旧构建）。
log "Phase 9: 回写默认标签 new-api:gateway -> $NEW_IMAGE..."
docker tag "$NEW_IMAGE" new-api:gateway
DEPLOYED_ID=$(docker inspect "$NEW_IMAGE" --format '{{.Id}}')
DEFAULT_ID=$(docker inspect new-api:gateway --format '{{.Id}}')
if [[ "$DEPLOYED_ID" != "$DEFAULT_ID" ]]; then
    log "ERROR: 标签回写校验失败 (deployed=$DEPLOYED_ID default=$DEFAULT_ID)"
    log "  请手动: docker tag $NEW_IMAGE new-api:gateway"
else
    log "  new-api:gateway -> $NEW_IMAGE ✓"
fi

log "═══════════════════════════════════════"
log "  升级完成！"
log "  活跃节点: $STANDBY (镜像: $NEW_IMAGE)"
log "  回滚命令: $SCRIPT_DIR/gateway-switch.sh $ACTIVE"
log "═══════════════════════════════════════"
