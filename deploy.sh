#!/bin/bash
# new-api 网关 启停管理脚本
# Usage: ./deploy.sh {start|stop|restart|logs|rebuild|status}

NAME="new-api"
PORT="3500"
IMAGE="new-api:gateway"
DATA_DIR="/data"

usage() {
    echo "Usage: $0 {start|stop|restart|logs|rebuild|status}"
    echo ""
    echo "  start    启动容器"
    echo "  stop     停止容器"
    echo "  restart  重启容器"
    echo "  logs     查看日志 (加 -f 实时跟踪)"
    echo "  rebuild  重新构建镜像并启动"
    echo "  status   查看运行状态"
    exit 1
}

case "${1:-}" in
    start)
        echo "[*] Starting $NAME..."
        docker start "$NAME" 2>/dev/null || \
        docker run -d --name "$NAME" \
            -p "$PORT:3000" \
            -v "$DATA_DIR:/data" \
            -e TZ=Asia/Shanghai \
            "$IMAGE"
        echo "[✓] $NAME started on port $PORT"
        ;;
    stop)
        echo "[*] Stopping $NAME..."
        docker stop "$NAME" 2>/dev/null && echo "[✓] Stopped" || echo "[-] Not running"
        ;;
    restart)
        $0 stop
        $0 start
        ;;
    logs)
        shift
        docker logs "$@" "$NAME"
        ;;
    rebuild)
        echo "[*] Rebuilding image..."
        docker stop "$NAME" 2>/dev/null
        docker rm "$NAME" 2>/dev/null
        cd "$(dirname "$0")" && docker build -t "$IMAGE" . && \
        docker run -d --name "$NAME" \
            -p "$PORT:3000" \
            -v "$DATA_DIR:/data" \
            -e TZ=Asia/Shanghai \
            "$IMAGE" && \
        echo "[✓] Rebuild & deploy complete on port $PORT"
        ;;
    status)
        echo "=== Container ==="
        docker ps -a --filter "name=$NAME" --format "table {{.ID}}\t{{.Names}}\t{{.Status}}\t{{.Ports}}"
        echo ""
        echo "=== Image ==="
        docker images "$IMAGE" --format "table {{.Repository}}:{{.Tag}}\t{{.Size}}\t{{.CreatedAt}}"
        echo ""
        echo "=== Data Directory ==="
        ls -lh "$DATA_DIR/" 2>/dev/null
        echo ""
        echo "=== Recent Logs (last 10 lines) ==="
        docker logs --tail 10 "$NAME" 2>/dev/null || echo "[-] Container not running"
        ;;
    *)
        usage
        ;;
esac
