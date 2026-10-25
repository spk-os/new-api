#!/bin/bash
# gateway-build.sh — 网关标准构建入口
#
# 核心保证只有一条：patch 模式每次都先在容器内重建前端（bun 官方镜像，
# 不依赖宿主 node），再编译 Go —— dist 永远是刚构建的，不存在陈旧前端
# 被 go:embed 静默编进二进制的问题。
#
# 用法:
#   gateway-build.sh patch [源码目录]    # 补丁构建（后端小改动）
#   gateway-build.sh full  [context目录] # 全量构建（前端在 Dockerfile 内现构建）
#   gateway-build.sh release <image>     # 部署验证通过后，占用浮动 tag new-api:gateway
#   gateway-build.sh clean               # 清理过期无用镜像
#
# 镜像清理规则：new-api 网关镜像超过 3 个时，删除 2 天前未被任何容器
# （含停止的回滚锚点）使用、也不是浮动 tag 指向的 tag。

set -euo pipefail

SRC_DEFAULT=/work/spk-os/soft/basic/new-api
FLOATING_TAG=new-api:gateway
GO_BUILDER=golang:1.26.1-alpine
GO_PROXY=https://goproxy.cn,direct
GO_CACHE=/tmp/go-build-cache
IMG_KEEP=3
IMG_MAX_AGE_DAYS=2

MODE="${1:-}"
SRC="${2:-$SRC_DEFAULT}"
TS=$(date +%Y%m%d%H%M%S)

die() { echo "✗ $*" >&2; exit 1; }
info() { echo "  $*"; }

# 镜像清理：网关镜像 tag 超过 IMG_KEEP 个时，删除 IMG_MAX_AGE_DAYS 天前
# 且未被任何容器（含停止，回滚锚点）使用、也不是浮动 tag 指向的 tag。
cleanup_images() {
    local total floating_id removed=0
    mapfile -t TAGS < <(docker images --format '{{.Repository}}:{{.Tag}}' \
        | grep -E '^new-api:gateway-[0-9]{14}$' | sort)
    total=${#TAGS[@]}
    (( total > IMG_KEEP )) || { info "镜像清理：$total 个 ≤ $IMG_KEEP，跳过"; return 0; }
    floating_id=$(docker images -q "$FLOATING_TAG" 2>/dev/null | head -1)
    local cutoff=$(( $(date +%s) - IMG_MAX_AGE_DAYS * 86400 ))
    for t in "${TAGS[@]}"; do
        local id created_ts
        id=$(docker images -q "$t" | head -1)
        [[ "$id" == "$floating_id" ]] && continue
        [[ -n "$(docker ps -aq --filter "ancestor=$id" 2>/dev/null)" ]] && continue
        created_ts=$(docker inspect -f '{{.Created}}' "$id" 2>/dev/null | xargs -I{} date -d "{}" +%s 2>/dev/null || echo 0)
        (( created_ts > 0 && created_ts < cutoff )) || continue
        docker rmi "$t" >/dev/null 2>&1 || continue
        removed=$((removed + 1))
        info "清理过期无用镜像: $t"
    done
    info "镜像清理：$total -> $((total - removed)) 个"
}

case "$MODE" in

patch)
    cd "$SRC"
    [[ -f Dockerfile ]] || die "$SRC 不是源码目录"
    echo "═══ patch 构建 @ $SRC ═══"
    echo "1/3 容器内重建前端（保证 dist 最新）…"
    docker run --rm \
        -v "$SRC:/app" -w /app \
        oven/bun:1@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 \
        sh -c 'cd web && bun install --frozen-lockfile && DISABLE_ESLINT_PLUGIN=true VITE_REACT_APP_VERSION="$(cat ../VERSION)" bun run build'

    echo "2/3 容器内编译 Go（embed 刚构建的 dist）…"
    OUT_DIR=$(mktemp -d /tmp/gw-build.XXXXXX)
    docker run --rm \
        -v "$SRC:/app" -v "$OUT_DIR:/output" \
        -v "$GO_CACHE:/tmp/go-cache" -w /app \
        -e GOPROXY="$GO_PROXY" -e GOCACHE=/tmp/go-cache \
        "$GO_BUILDER" \
        go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o /output/new-api .

    echo "3/3 构建 patch 镜像…"
    NEW_IMAGE="new-api:gateway-$TS"
    {
        echo "FROM $FLOATING_TAG"
        echo "COPY new-api /new-api"
    } > "$OUT_DIR/Dockerfile"
    docker build -q -t "$NEW_IMAGE" "$OUT_DIR" >/dev/null
    echo "完成: $NEW_IMAGE"
    echo "  浮动 tag 未动。部署验证通过后: $0 release $NEW_IMAGE"
    cleanup_images
    ;;

full)
    CONTEXT="${2:-$SRC_DEFAULT}"
    cd "$CONTEXT"
    [[ -f Dockerfile ]] || die "$CONTEXT 不是源码目录"
    echo "═══ full 构建 @ $CONTEXT ═══"
    NEW_IMAGE="new-api:gateway-$TS"
    DOCKER_BUILDKIT=1 docker build -f Dockerfile . -t "$NEW_IMAGE" -q
    echo "完成: $NEW_IMAGE"
    echo "  浮动 tag 未动。部署验证通过后: $0 release $NEW_IMAGE"
    cleanup_images
    ;;

release)
    IMAGE="${2:-}"
    [[ -n "$IMAGE" ]] || die "用法: $0 release <image>"
    docker image inspect "$IMAGE" >/dev/null 2>&1 || die "镜像不存在: $IMAGE"
    docker tag "$IMAGE" "$FLOATING_TAG"
    echo "✓ 浮动 tag 已重指: $FLOATING_TAG -> $(docker images -q "$IMAGE" | head -1)"
    echo "  之后 recreate/重启都会用该镜像，请跑 ./gateway-verify.sh"
    ;;

clean)
    cleanup_images
    ;;

*)
    grep '^#' "$0" | head -16
    exit 1
    ;;
esac
