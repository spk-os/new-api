#!/bin/bash
# /work/SPK-OS/soft/basic/new-api/ha/bin/gateway-backup.sh
# LLM Gateway PG 备份脚本
# 用法: ./gateway-backup.sh
# 功能: 预校验 → PG dump → 后校验 → 保留策略（最多3份，至少1份>=1天前）

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
BAK_DIR="$HA_DIR/bak"
PG_CONTAINER="docker-db_postgres-1"
PG_USER="postgres"
PG_DB="new-api"
MAX_BACKUPS=3
MIN_AGE_HOURS=24

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }
die() { log "ERROR: $*"; exit 1; }

mkdir -p "$BAK_DIR"

# ============================================================
# Phase 1: 预校验
# ============================================================
log "Phase 1: 预校验..."

# 1.1 PG 容器存活
docker inspect --format='{{.State.Status}}' "$PG_CONTAINER" 2>/dev/null | grep -q "running" \
    || die "PG 容器 $PG_CONTAINER 未运行"
log "  PG 容器运行中 ✓"

# 1.2 PG 连接 + 数据库存在
ROW_COUNT=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -t -c \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';" 2>/dev/null | xargs) \
    || die "无法连接 PG 或数据库 $PG_DB 不存在"
[[ "$ROW_COUNT" -gt 0 ]] || die "数据库 $PG_DB 无表，可能未初始化"
log "  PG 连接正常，$PG_DB 有 $ROW_COUNT 张表 ✓"

# 1.3 磁盘空间（至少 1GB 可用）
AVAIL_KB=$(df -P "$BAK_DIR" | awk 'NR==2 {print $4}')
[[ "$AVAIL_KB" -gt 1048576 ]] || die "磁盘空间不足（需要 >=1GB，当前 $((AVAIL_KB/1024))MB）"
log "  磁盘空间充足（$((AVAIL_KB/1024))MB 可用）✓"

# ============================================================
# Phase 2: PG dump
# ============================================================
log "Phase 2: 执行 PG dump..."
TIMESTAMP=$(date +%Y%m%d%H%M%S)
DUMP_FILE="$BAK_DIR/pg_dump_${TIMESTAMP}.sql"

docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" "$PG_DB" > "$DUMP_FILE" \
    || die "pg_dump 失败"

log "  dump 完成: $DUMP_FILE ($(du -h "$DUMP_FILE" | cut -f1))"

# ============================================================
# Phase 3: 后校验
# ============================================================
log "Phase 3: 后校验..."

# 3.1 文件非空
FILE_SIZE=$(stat -c%s "$DUMP_FILE")
[[ "$FILE_SIZE" -gt 1000 ]] || die "dump 文件过小（$FILE_SIZE bytes），可能为空或损坏"

# 3.2 SQL 头部校验
head -5 "$DUMP_FILE" | grep -qi "postgresql database dump\|SET\|CREATE\|COPY\|INSERT" \
    || die "dump 文件头部不是有效的 SQL dump"
log "  dump 文件有效（$FILE_SIZE bytes）✓"

# ============================================================
# Phase 4: 保留策略
# 最多保留 MAX_BACKUPS 份，其中至少 1 份 >= MIN_AGE_HOURS 小时前
# ============================================================
log "Phase 4: 保留策略（最多 ${MAX_BACKUPS} 份，至少 1 份 >= ${MIN_AGE_HOURS}h）..."

# 列出所有备份文件，按 mtime 排序（新→旧）
mapfile -t ALL_BAKS < <(ls -t "$BAK_DIR"/pg_dump_*.sql 2>/dev/null || true)

if [[ ${#ALL_BAKS[@]} -le $MAX_BACKUPS ]]; then
    log "  当前仅 ${#ALL_BAKS[@]} 份备份，无需清理"
    log "备份完成 ✓"
    exit 0
fi

NOW_EPOCH=$(date +%s)
CUTOFF_EPOCH=$((NOW_EPOCH - MIN_AGE_HOURS * 3600))

# 找到最新的 >= 1天前 的备份
OLDEST_KEEP=""
for f in "${ALL_BAKS[@]}"; do
    MTIME=$(stat -c%Y "$f")
    if [[ "$MTIME" -le "$CUTOFF_EPOCH" ]]; then
        OLDEST_KEEP="$f"
        break
    fi
done

# 构建保留集合
declare -A KEEP
# 保留最新的 (MAX_BACKUPS - 1) 份（或全部如果不够）
KEEP_COUNT=0
for f in "${ALL_BAKS[@]}"; do
    if [[ $KEEP_COUNT -lt $((MAX_BACKUPS - 1)) ]]; then
        KEEP["$f"]=1
        KEEP_COUNT=$((KEEP_COUNT + 1))
    fi
done

# 如果有 >= 1天前 的备份，保留它
if [[ -n "$OLDEST_KEEP" ]]; then
    KEEP["$OLDEST_KEEP"]=1
    log "  保留 >= 1天前 备份: $(basename "$OLDEST_KEEP")"
else
    log "  WARN: 未找到 >= 1天前 的备份，仅保留最新 $MAX_BACKUPS 份"
fi

# 删除不在保留集合中的文件
DELETED=0
for f in "${ALL_BAKS[@]}"; do
    if [[ -z "${KEEP[$f]:-}" ]]; then
        rm -f "$f"
        log "  删除: $(basename "$f")"
        DELETED=$((DELETED + 1))
    fi
done

log "  保留 ${#KEEP[@]} 份，删除 $DELETED 份"

# 列出保留的文件
for f in "${!KEEP[@]}"; do
    SIZE=$(du -h "$f" | cut -f1)
    MTIME=$(date -d "@$(stat -c%Y "$f")" '+%Y-%m-%d %H:%M:%S')
    log "  → $(basename "$f")  ($SIZE, $MTIME)"
done

log "备份完成 ✓"
