#!/usr/bin/env bash
# gateway-recompress-zstd.sh — 存量 .log.gz 内容日志归档 → .log.zst 重压缩
#
# 背景: 2026-10-10 前归档用 gzip -6, 内容日志跨条目重复大, gzip 32KB 窗口
# 只能压到 ~26%; zstd --long 窗口可压到 <2%。本脚本把存量 .log.gz 转为 .log.zst。
#
# 安全策略（用户要求：不删除任何数据，防止误删）:
#   1. 只创建新文件 .log.zst, 绝不删除/修改源 .log.gz
#   2. 每个文件转压后做 sha256 回读校验 (gzip -dc vs zstd -dc), 不一致则
#      删除刚生成的失败 .zst 并跳过 (只清自己产生的半成品, 不碰原始数据)
#   3. 已存在同名 .log.zst 时跳过, 不覆盖 (重跑幂等)
#   4. .log.gz 删除与否由用户校验后自行决定, 脚本末尾给出建议命令但不执行
#
# 用法: gateway-recompress-zstd.sh [日志目录]
#   默认目录: /root/share/ops/logs/new-api
set -uo pipefail

LOG_DIR="${1:-/root/share/ops/logs/new-api}"
ZSTD_LEVEL_ARGS="--ultra -22 --long=27 -T0"   # 与 SpeedBestCompression 同档, 实测 <2%
REPORT="${LOG_DIR}/recompress-zstd-report-$(date +%Y%m%d%H%M%S).txt"

if ! command -v zstd >/dev/null || ! command -v gzip >/dev/null; then
    echo "ERROR: 需要 zstd 与 gzip 命令" >&2
    exit 1
fi
if [[ ! -d "$LOG_DIR" ]]; then
    echo "ERROR: 目录不存在: $LOG_DIR" >&2
    exit 1
fi

ok=0; skip=0; fail=0
src_total=0; dst_total=0
zip_ok=0; zip_skip=0; zip_fail=0
zip_src_total=0; zip_dst_total=0
{
    echo "=== .log.gz / legacy .zip → zstd 重压缩报告 $(date '+%F %T') ==="
    echo "源文件未删除; 删除需用户校验后自行执行"
    printf '%-45s %12s %12s %8s\n' "文件" "源(MB)" "zst(MB)" "占比"
} > "$REPORT"

for src in "$LOG_DIR"/*.log.gz; do
    [[ -e "$src" ]] || continue
    dst="${src%.gz}.zst"
    name=$(basename "$src")

    if [[ -e "$dst" ]]; then
        skip=$((skip + 1))
        echo "SKIP (已存在): $name" >> "$REPORT"
        continue
    fi

    # 先写临时文件, 校验通过后再 rename, 避免半成品被误认为有效归档
    tmp="$dst.partial.$$"
    if ! gzip -dc -- "$src" | zstd $ZSTD_LEVEL_ARGS -q -o "$tmp" -; then
        fail=$((fail + 1))
        rm -f -- "$tmp"
        echo "FAIL (压缩失败, 源文件保留): $name" >> "$REPORT"
        continue
    fi

    # 回读校验: 解压内容必须与源字节一致
    src_sha=$(gzip -dc -- "$src" | sha256sum | awk '{print $1}')
    dst_sha=$(zstd -dc -- "$tmp" | sha256sum | awk '{print $1}')
    if [[ "$src_sha" != "$dst_sha" ]]; then
        fail=$((fail + 1))
        rm -f -- "$tmp"
        echo "FAIL (sha256 校验不一致, 源文件保留): $name" >> "$REPORT"
        continue
    fi

    mv -- "$tmp" "$dst"
    ok=$((ok + 1))
    sz_src=$(stat -c %s -- "$src")
    sz_dst=$(stat -c %s -- "$dst")
    src_total=$((src_total + sz_src))
    dst_total=$((dst_total + sz_dst))
    pct=$(( sz_dst * 100 / (sz_src > 0 ? sz_src : 1) ))
    printf '%-45s %12s %12s %7s%%\n' "$name" \
        "$((sz_src / 1024 / 1024))" "$((sz_dst / 1024 / 1024))" "$pct" >> "$REPORT"
done

# Legacy zip 归档（content_*.log.zip，旧格式轮转产物，不在查询窗口内）:
# 内部通常是单成员日志, 解包流压成 .zip.zst; 同样零删除 + sha256 校验。
for src in "$LOG_DIR"/*.zip; do
    [[ -e "$src" ]] || continue
    dst="${src}.zst"
    name=$(basename "$src")

    if [[ -e "$dst" ]]; then
        zip_skip=$((zip_skip + 1))
        echo "SKIP (已存在): $name" >> "$REPORT"
        continue
    fi

    tmp="$dst.partial.$$"
    if ! unzip -p -- "$src" | zstd $ZSTD_LEVEL_ARGS -q -o "$tmp" -; then
        zip_fail=$((zip_fail + 1))
        rm -f -- "$tmp"
        echo "FAIL (压缩失败, 源文件保留): $name" >> "$REPORT"
        continue
    fi

    # 回读校验: zip 解包内容必须与 zst 解压内容一致
    src_sha=$(unzip -p -- "$src" | sha256sum | awk '{print $1}')
    dst_sha=$(zstd -dc -- "$tmp" | sha256sum | awk '{print $1}')
    if [[ "$src_sha" != "$dst_sha" ]]; then
        zip_fail=$((zip_fail + 1))
        rm -f -- "$tmp"
        echo "FAIL (sha256 校验不一致, 源文件保留): $name" >> "$REPORT"
        continue
    fi

    mv -- "$tmp" "$dst"
    zip_ok=$((zip_ok + 1))
    sz_src=$(stat -c %s -- "$src")
    sz_dst=$(stat -c %s -- "$dst")
    zip_src_total=$((zip_src_total + sz_src))
    zip_dst_total=$((zip_dst_total + sz_dst))
    pct=$(( sz_dst * 100 / (sz_src > 0 ? sz_src : 1) ))
    printf '%-45s %12s %12s %7s%%\n' "$name" \
        "$((sz_src / 1024 / 1024))" "$((sz_dst / 1024 / 1024))" "$pct" >> "$REPORT"
done

src_mb=$((src_total / 1024 / 1024))
dst_mb=$((dst_total / 1024 / 1024))
zip_src_mb=$((zip_src_total / 1024 / 1024))
zip_dst_mb=$((zip_dst_total / 1024 / 1024))
{
    echo "=== 汇总: gz 成功 $ok / 跳过 $skip / 失败 $fail; zip 成功 $zip_ok / 跳过 $zip_skip / 失败 $zip_fail ==="
    if (( ok > 0 )); then
        printf 'gz:  已转换源大小: %d MB → 压缩后: %d MB (占比 %d%%)\n' \
            "$src_mb" "$dst_mb" "$(( dst_total * 100 / (src_total > 0 ? src_total : 1) ))"
    fi
    if (( zip_ok > 0 )); then
        printf 'zip: 已转换源大小: %d MB → 压缩后: %d MB (占比 %d%%)\n' \
            "$zip_src_mb" "$zip_dst_mb" "$(( zip_dst_total * 100 / (zip_src_total > 0 ? zip_src_total : 1) ))"
    fi
    cat <<'EOF'

后续释放空间（脚本不执行，由用户确认后手工运行）:
  校验 zst 归档可正常查询/解压后, 手工删除对应源文件:
    # 先抽查 (应能解出内容):
    zstd -dc <某文件>.log.zst | head -c 200
    zstd -dc <某文件>.zip.zst | head -c 200
    # 确认无误后 (rm -i 逐个确认, 防误删):
    find <日志目录> -name '*.log.zst' | sed 's/\.zst$/.gz/' | xargs -I{} rm -i {}
    find <日志目录> -name '*.zip.zst' | sed 's/\.zst$//' | xargs -I{} rm -i {}
EOF
} >> "$REPORT"

cat "$REPORT" | tail -20
echo "完整报告: $REPORT"
