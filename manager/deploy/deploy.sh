#!/usr/bin/env bash
# openshell-manager 部署 —— 本目录（部署事实源）→ LXC 107 docker compose。
#
# 同步内容：服务源码（openshell_manager/ libs/ config.json，来自 SRC）+
# 本目录部署产物（Dockerfile.manager docker-compose.yml env.template）+
# 密钥 env → 远端 .env（600）。config.json 保持本地开发态原样（bind
# 127.0.0.1）；容器内由 .env 覆盖（env > config.json，见服务 config.py）。
#
# 统一契约：deploy | check | status | start | stop | restart | logs [N]
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL
set -euo pipefail

cd "$(dirname "$0")"

# `-` 而非 `:-`：REMOTE=""（空串）是显式"本机执行"契约（与 openshell-gateway
# 两脚本同语义；`:-` 会在空串时静默触发 pct 缺省、打错目标宿主——B1-11 审计
# 修复，2026-09-11 对齐伞仓 REMOTE 契约家族）。
REMOTE="${REMOTE-pct exec 107 --}"
VMID="${VMID:-107}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/os-deploy/deploy/openshell-manager}"
SRC="${SRC:-$(cd .. && pwd)}"   # 默认 = 本仓根（脚本已 cd 到自身目录）
HEALTH_URL="${HEALTH_URL:-http://gateway.internal:18800/healthz}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

compose() { run_remote docker compose --project-directory "$DEPLOY_DIR" \
    -f "$DEPLOY_DIR/docker-compose.yml" "$@"; }

# 只同步 SDK 的 python 子树：libs/OpenShell 整树 7.2G（Rust target 构建产物），
# 运行只需要 libs/OpenShell/python（<1M）。
SYNC_TAR="openshell_manager libs/OpenShell/python config.json"     # 服务侧
DEPLOY_TAR="Dockerfile.manager docker-compose.yml env.template"     # 部署侧（同名文件）

health_ok() { curl -fsS --max-time 4 "$HEALTH_URL" >/dev/null 2>&1; }

wait_health() {
    local deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT:-90} ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if health_ok; then echo "manager healthz OK ($HEALTH_URL)"; return 0; fi
        sleep 3
    done
    echo "ERROR: manager not healthy within timeout; try: $0 logs" >&2
    return 1
}

sync_all() {
    run_remote mkdir -p "$DEPLOY_DIR"
    # 先清同步目标再解包（叠加式同步会残留旧树：libs 曾含 7.2G Rust target/）。
    run_remote rm -rf "$DEPLOY_DIR/openshell_manager" "$DEPLOY_DIR/libs"
    # pct exec 管道直灌，不落中间文件；tar -xf 保留 mtime，check 的确定性
    # 哈希依赖这一点（远端树 mtime == 本地树 mtime）。
    tar -C "$SRC" --exclude='__pycache__' --exclude='.token' --exclude='manager.log' \
        -cf - $SYNC_TAR | run_remote tar -xf - -C "$DEPLOY_DIR"
    tar -C . -cf - $DEPLOY_TAR | run_remote tar -xf - -C "$DEPLOY_DIR"
    pct push "$VMID" env "$DEPLOY_DIR/.env"
    run_remote chmod 600 "$DEPLOY_DIR/.env"
    echo "synced: $SRC + 部署产物 + .env → $VMID:$DEPLOY_DIR"
}

# 确定性内容哈希：排序 + 归一 owner/mtime 的 tar 流，本地与远端同参可比。
# 排除项必须与 sync_all 一致，否则本地含 pyc/log、远端没有 → 假漂移。
# 注意 --mtime 用 @epoch（无空格），经 run_remote 分词安全。
# 2>/dev/null 已去（B1-14 审计修复）：tar 出错（路径缺失等）曾被静默吞掉、
# 只剩哈希失配的间接症状——错误必须可见。
dtar() {  # dtar <base> <paths...>
    local base="$1"; shift
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        --exclude='__pycache__' --exclude='.token' --exclude='manager.log' \
        -C "$base" -cf - "$@"
}
rdtar() {  # 同参数的远端版
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR" -cf - "$@"
}
hashof() { md5sum | awk '{print $1}'; }

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        sync_all
        compose up -d --build
        wait_health
        echo "== gateway 可达性（经 manager，Bearer token）=="
        # -f2-（B1-14 审计修复）：token 常见 base64 padding '='，-f2 从值内
        # 第一个 = 截断 → Authorization 头残缺
        if curl -fsS --max-time 8 -H "Authorization: Bearer $(cut -d= -f2- env)" \
            "${HEALTH_URL%/healthz}/api/v1/gateway/health"; then
            echo
        else
                echo "WARN: gateway/health 未通（网关未起？../../openshell-gateway/deploy.sh start）"
        fi
        echo "hint: 宿主机引擎接入：export OPENSHELL_MANAGER_URL=http://gateway.internal:18800 OPENSHELL_MANAGER_TOKEN=\$(cut -d= -f2- deploy/env)"
        ;;
    check)
        d=0
        if [ "$(dtar "$SRC" $SYNC_TAR | hashof)" != "$(rdtar $SYNC_TAR | hashof)" ]; then
            echo "drift: 服务源码"; d=1
        fi
        if [ "$(dtar . $DEPLOY_TAR | hashof)" != "$(rdtar $DEPLOY_TAR | hashof)" ]; then
            echo "drift: 部署产物"; d=1
        fi
        if [ "$(md5sum env | awk '{print $1}')" != "$(run_remote md5sum "$DEPLOY_DIR/.env" 2>/dev/null | awk '{print $1}')" ]; then
            echo "drift: .env"; d=1
        fi
        if [ "$d" = "0" ]; then echo "in sync"; else echo "^ 与本仓不一致，重新 deploy 收敛"; exit 1; fi
        ;;
    status)
        compose ps || true
        if health_ok; then echo "healthz: OK ($HEALTH_URL)"; else echo "healthz: DOWN"; exit 1; fi
        ;;
    start|stop|restart)
        compose "$cmd" || compose up -d
        [ "$cmd" = "stop" ] || wait_health
        ;;
    logs)
        shift || true
        compose logs --tail="${1:-50}" manager
        ;;
    *)
        echo "usage: $0 [deploy|check|status|start|stop|restart|logs [N]]" >&2
        exit 2
        ;;
esac
