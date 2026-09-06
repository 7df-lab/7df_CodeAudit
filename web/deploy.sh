#!/usr/bin/env bash
# web (CodeAudit Console) 部署 —— 前端容器（nginx SPA + /v1 反代）→ LXC 107 docker。
#
# 背景：生产 console 旧口径为「由 CD 单独发布」；CD 退役（2026-09-05）后该职责
# 由本脚本接续，纳入伞仓统一部署链（deploy/sandbox-deploy.toml 第 5 项）。
#
# 布局：LXC 内 DEPLOY_DIR = web 源码树；compose 以仓库自带 docker-compose.yml
# 单文件运行（8088→80；upstream = 宿主发布网关 8090，经 host-gateway 别名）。
#
# 统一契约：deploy | check | status | start | stop | restart | logs [N]
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL
set -euo pipefail

cd "$(dirname "$0")"

REMOTE="${REMOTE:-pct exec 107 --}"
VMID="${VMID:-107}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/os-deploy/deploy/web}"
SRC="${SRC:-$(pwd)}"
HEALTH_URL="${HEALTH_URL:-http://gateway.internal:8088/}"
# 宿主发布网关（deploy/prod 的 CODEAUDIT_HOST_GATEWAY=8090；容器经 host-gateway 可达）
GW_UPSTREAM="${GW_UPSTREAM:-host.docker.internal:8090}"
CONSOLE_PORT="${CONSOLE_PORT:-8088}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

compose() { run_remote docker compose --project-directory "$DEPLOY_DIR" \
    -f "$DEPLOY_DIR/docker-compose.yml" "$@"; }

EXCLUDES=(--exclude=.git --exclude=node_modules --exclude=dist --exclude='*.log')

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null || echo 000; }
health_ok() { [ "$(http_code "$HEALTH_URL")" = "200" ]; }

wait_health() {
    local deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT:-240} ))
    echo "waiting for console at $HEALTH_URL ..."
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if health_ok; then echo "console healthy ($HEALTH_URL)"; return 0; fi
        sleep 3
    done
    echo "ERROR: console not healthy within timeout; try: $0 logs 100" >&2
    return 1
}

# 确定性内容哈希（源码树；远端侧剔除部署态 .env 后同参对比）
dtar() {
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$SRC" "${EXCLUDES[@]}" -cf - . 2>/dev/null | md5sum | awk '{print $1}'
}
rdtar() {
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR" --exclude=.git --exclude=node_modules --exclude=dist \
        --exclude='*.log' --exclude=.env \
        -cf - . 2>/dev/null | md5sum | awk '{print $1}'
}

sync_all() {
    # 收敛式同步：清空旧树再解包（防止上游已删文件残留使 check 永远 drift）
    run_remote mkdir -p "$DEPLOY_DIR"
    run_remote find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
    tar -C "$SRC" "${EXCLUDES[@]}" -cf - . | run_remote tar -xf - -C "$DEPLOY_DIR"
    run_remote bash -c "printf 'CODEAUDIT_CONSOLE_PORT=$CONSOLE_PORT\nCODEAUDIT_GATEWAY_UPSTREAM=$GW_UPSTREAM\n' > '$DEPLOY_DIR/.env'"
    echo "synced: $SRC → $VMID:$DEPLOY_DIR"
}

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        sync_all
        compose up -d --build
        wait_health
        # 反代连通性：/v1 必须透传到网关（未认证 → 401，而非 404/502）
        code=$(http_code "$HEALTH_URL/v1/projects")
        [ "$code" = "401" ] || { echo "ERROR: console /v1 反代异常（HTTP $code，期望 401）" >&2; exit 1; }
        echo "console /v1 反代 OK（401 透传）"
        ;;
    check)
        if [ "$(dtar)" != "$(rdtar)" ]; then
            echo "drift: 源码树与本仓不一致，重新 deploy 收敛"; exit 1
        fi
        echo "in sync"
        ;;
    status)
        compose ps || true
        if health_ok; then echo "console: OK ($HEALTH_URL)"; else echo "console: DOWN"; exit 1; fi
        ;;
    start|stop|restart)
        compose "$cmd"
        [ "$cmd" = "stop" ] || wait_health
        ;;
    logs)
        shift || true
        compose logs --tail="${1:-50}" "$@"
        ;;
    *)
        echo "usage: $0 [deploy|check|status|start|stop|restart|logs [N]]" >&2
        exit 2
        ;;
esac
