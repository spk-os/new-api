#!/bin/bash
# /work/SPK-OS/soft/basic/new-api/ha/bin/gateway-switch.sh
# LLM Gateway 蓝绿切换脚本
# 用法: ./gateway-switch.sh [blue|green|status|rollback]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
NGINX_CONF="$HA_DIR/nginx/upstream.conf"
LOG_FILE="$HA_DIR/logs/switch.log"

mkdir -p "$HA_DIR/logs"

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOG_FILE"; }

get_active() {
    grep -oP 'server new-api-\K(blue|green)' "$NGINX_CONF" | head -1
}

get_standby() {
    local active=$(get_active)
    [[ "$active" == "blue" ]] && echo "green" || echo "blue"
}

cmd_status() {
    local active=$(get_active)
    local standby=$(get_standby)
    local active_port standby_port
    if [[ "$active" == "blue" ]]; then
        active_port=3501
        standby_port=3502
    else
        active_port=3502
        standby_port=3501
    fi
    echo "═══════════════════════════════════════"
    echo "  LLM Gateway 蓝绿部署状态"
    echo "═══════════════════════════════════════"
    echo "  活跃节点: $active (:$active_port)"
    echo "  备用节点: $standby (:$standby_port)"
    echo "───────────────────────────────────────"
    docker ps --filter "name=new-api-" --format "  {{.Names}}\t{{.Status}}"
    echo "───────────────────────────────────────"
    for node in blue green; do
        local port
        if [[ "$node" == "blue" ]]; then
            port=3501
        else
            port=3502
        fi
        local status="✗"
        curl -sf "http://localhost:$port/api/status" 2>/dev/null | grep -q '"success":.*true' && status="✓"
        echo "  $node (:$port): $status"
    done
    echo "  nginx (:3500): $(curl -sf http://localhost:3500/api/status 2>/dev/null | grep -q '"success":.*true' && echo '✓' || echo '✗')"
    echo "═══════════════════════════════════════"
}

cmd_switch() {
    local target=$1
    local current=$(get_active)

    if [[ "$target" == "$current" ]]; then
        log "WARN: $target 已经是活跃节点，无需切换"
        return 0
    fi

    local target_port
    if [[ "$target" == "blue" ]]; then
        target_port=3501
    else
        target_port=3502
    fi

    # Step 1: 健康检查目标节点
    log "INFO: 检查 $target (:$target_port) 健康状态..."
    local retries=0
    while [[ $retries -lt 10 ]]; do
        if curl -sf "http://localhost:$target_port/api/status" 2>/dev/null | grep -q '"success":.*true'; then
            log "INFO: $target 健康检查通过 ✓"
            break
        fi
        retries=$((retries + 1))
        if [[ $retries -eq 10 ]]; then
            log "ERROR: $target 健康检查失败，中止切换"
            return 1
        fi
        sleep 3
    done

    # Step 2: 发送测试请求
    log "INFO: 发送测试请求到 $target..."
    local test_resp=$(curl -sf "http://localhost:$target_port/api/status" 2>/dev/null)
    if [[ -z "$test_resp" ]]; then
        log "ERROR: 测试请求失败，中止切换"
        return 1
    fi
    log "INFO: 测试请求通过 ✓"

    # Step 3: 备份当前配置
    cp "$NGINX_CONF" "${NGINX_CONF}.bak.$(date +%Y%m%d%H%M%S)"
    log "INFO: 已备份 upstream.conf"

    # Step 4: 切换 upstream (在容器内修改，避免 sed -i 破坏 Docker bind mount)
    if [[ "$target" == "blue" ]]; then
        docker exec llm-gateway-nginx sed -i 's/server new-api-green:3000;/server new-api-green:3000 backup;/' /etc/nginx/conf.d/upstream.conf
        docker exec llm-gateway-nginx sed -i 's/server new-api-blue:3000 backup;/server new-api-blue:3000;/' /etc/nginx/conf.d/upstream.conf
    else
        docker exec llm-gateway-nginx sed -i 's/server new-api-blue:3000;/server new-api-blue:3000 backup;/' /etc/nginx/conf.d/upstream.conf
        docker exec llm-gateway-nginx sed -i 's/server new-api-green:3000 backup;/server new-api-green:3000;/' /etc/nginx/conf.d/upstream.conf
    fi
    # 同步 host 文件 (用 cp 从容器复制回来，保持 bind mount inode 一致)
    docker cp llm-gateway-nginx:/etc/nginx/conf.d/upstream.conf "$NGINX_CONF"
    log "INFO: 已修改 upstream.conf -> $target"

    # Step 5: Reload Nginx
    docker exec llm-gateway-nginx nginx -t 2>/dev/null
    if [[ $? -ne 0 ]]; then
        log "ERROR: Nginx 配置检测失败，回滚..."
        cp "${NGINX_CONF}.bak."* "$NGINX_CONF" 2>/dev/null
        docker exec llm-gateway-nginx nginx -s reload
        return 1
    fi
    docker exec llm-gateway-nginx nginx -s reload
    log "INFO: Nginx reload 完成 ✓"

    # Step 6: 验证切换
    sleep 2
    local via_nginx=$(curl -sf "http://localhost:3500/api/status" 2>/dev/null)
    if [[ -n "$via_nginx" ]]; then
        log "INFO: 切换成功！流量已通过 :3500 -> $target ✓"
    else
        log "WARN: :3500 验证失败，请手动检查"
    fi

    log "INFO: 切换完成: $current -> $target"
}

cmd_rollback() {
    local standby=$(get_standby)
    log "INFO: 回滚到 $standby..."
    cmd_switch "$standby"
}

# ============================================================
# 主入口
# ============================================================
case "${1:-status}" in
    status)   cmd_status ;;
    blue)     cmd_switch blue ;;
    green)    cmd_switch green ;;
    rollback) cmd_rollback ;;
    *)
        echo "用法: $0 [blue|green|status|rollback]"
        echo "  status   - 查看当前状态"
        echo "  blue     - 切换到 blue 节点"
        echo "  green    - 切换到 green 节点"
        echo "  rollback - 回滚到备用节点"
        exit 1
        ;;
esac
