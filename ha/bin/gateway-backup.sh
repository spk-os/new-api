#!/bin/bash
# /work/spk-os/soft/basic/new-api/ha/bin/gateway-backup.sh
# LLM Gateway PG 备份脚本
# 用法: ./gateway-backup.sh [full|quick]
#   full  （默认）全库 dump，含 logs 表
#   quick 排除 logs 表（占 95% 体积），渠道/配置变更前快照用；
#         logs 全量由每日 10:00 spk-backup.sh both 覆盖（backups/prod/postgresql）
# 功能: 预校验 → PG dump（gzip 压缩）→ 后校验 → 保留策略（最多3份，至少1份>=1天前）

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HA_DIR="$(dirname "$SCRIPT_DIR")"
BAK_DIR="$HA_DIR/bak"
PG_CONTAINER="spk-pg-prod"
PG_USER="spk"
PG_DB="new-api"
MODE="${1:-full}"
MAX_BACKUPS=3
MIN_AGE_HOURS=24

case "$MODE" in
    full)  DUMP_ARGS=() ;;
    quick) DUMP_ARGS=(--exclude-table=logs) ;;
    *) echo "用法: $0 [full|quick]" >&2; exit 2 ;;
esac

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
# Phase 2: PG dump（gzip -9 压缩；裸 SQL 已验证 2.0G → 317M）
# ============================================================
log "Phase 2: 执行 PG dump（mode=$MODE）..."
TIMESTAMP=$(date +%Y%m%d%H%M%S)
[[ "$MODE" == quick ]] && TAG="_quick" || TAG=""
DUMP_FILE="$BAK_DIR/pg_dump${TAG}_${TIMESTAMP}.sql.gz"

docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" "${DUMP_ARGS[@]}" "$PG_DB" | gzip -9 > "$DUMP_FILE" \
    || die "pg_dump 失败"

log "  dump 完成: $DUMP_FILE ($(du -h "$DUMP_FILE" | cut -f1))"

# ============================================================
# Phase 3: 后校验
# ============================================================
log "Phase 3: 后校验..."

# 3.1 文件非空
FILE_SIZE=$(stat -c%s "$DUMP_FILE")
[[ "$FILE_SIZE" -gt 1000 ]] || die "dump 文件过小（$FILE_SIZE bytes），可能为空或损坏"

# 3.2 gzip 完整性 + SQL 头部校验（压缩流是整体，坏了恢复时才炸，必须当场验）
# 注意脚本开了 pipefail：grep -m1 提前退出会让 zcat 收 SIGPIPE 返回 141，
# 因此校验结果先落变量再判，不能依赖管道整体退出码。
gzip -t "$DUMP_FILE" || die "gzip 完整性校验失败，文件可能损坏"
DUMP_HEAD=$(zcat "$DUMP_FILE" | head -30 || true)
[[ "$DUMP_HEAD" == *"PostgreSQL database dump"* ]] \
    || die "dump 文件头部不是有效的 SQL dump"
log "  dump 有效（$FILE_SIZE bytes，gzip 校验通过）✓"

# ============================================================
# Phase 4: 保留策略
# 最多保留 MAX_BACKUPS 份，其中至少 1 份 >= MIN_AGE_HOURS 小时前
# ============================================================
log "Phase 4: 保留策略（最多 ${MAX_BACKUPS} 份，至少 1 份 >= ${MIN_AGE_HOURS}h）..."

# images_*.txt 只保留最新 MAX_BACKUPS 份（升级时顺带存的镜像列表，价值低、随时可重列）
mapfile -t ALL_IMAGES < <(ls -t "$BAK_DIR"/images_*.txt 2>/dev/null || true)
if [[ ${#ALL_IMAGES[@]} -gt $MAX_BACKUPS ]]; then
    for f in "${ALL_IMAGES[@]:$MAX_BACKUPS}"; do
        rm -f "$f"
        log "  删除镜像列表: $(basename "$f")"
    done
fi

# 列出所有备份文件，按 mtime 排序（新→旧）。.sql 和 .sql.gz 统一参与保留策略，
# 否则 .gz 会无限累积（docker exec pg_dump | gzip 产生的快照）。
# quick（排除 logs）与 full/全量快照分开计数：quick 体积可忽略（~340K）但高频，
# 全量含 logs 一份 300M+，混在一起会把同日的全量挤掉（曾把 model_cleanup 挤丢）。
mapfile -t ALL_QUICK < <(ls -t "$BAK_DIR"/pg_dump_quick_*.sql "$BAK_DIR"/pg_dump_quick_*.sql.gz 2>/dev/null || true)
if [[ ${#ALL_QUICK[@]} -gt $MAX_BACKUPS ]]; then
    for f in "${ALL_QUICK[@]:$MAX_BACKUPS}"; do
        rm -f "$f"
        log "  删除: $(basename "$f")"
    done
fi

mapfile -t ALL_BAKS < <(ls -t "$BAK_DIR"/pg_dump_*.sql "$BAK_DIR"/pg_dump_*.sql.gz 2>/dev/null \
    | grep -v '/pg_dump_quick_' || true)

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
