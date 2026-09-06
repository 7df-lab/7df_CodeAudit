#!/usr/bin/env bash
# codeaudit (CodeAudit) 部署 —— deploy/prod（部署态事实源）→ LXC 107 docker compose。
#
# 布局：LXC 内 DEPLOY_DIR = codeaudit 源码树 + 本目录的
# docker-compose.deploy.yml + .env；compose 以「仓库 base + 本 overlay」
# 双文件运行，构建上下文 = 源码树根。
#
# 密钥接线：远端 .env 由本脚本生成 —— JWT 随机生成（已存在则保留），
# OPENSHELL_MANAGER_TOKEN 单一事实源取自伞仓 manager/deploy/env。
#
# 统一契约：deploy | check | status | start | stop | restart | logs [N]
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL
set -euo pipefail

# 仓根必须在 cd 进本目录之前用 $0 定位一次——下方第 13 行会先切目录，
# 之后相对 $0 的二次定位会叠加路径而失败（相对路径调用必炸的潜伏缺陷，ADR-208 批次③实测暴露）
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$(dirname "$0")"

REMOTE="${REMOTE:-pct exec 107 --}"
VMID="${VMID:-107}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/os-deploy/deploy/codeaudit}"
SRC="${SRC:-$REPO_ROOT/engine}"
HEALTH_URL="${HEALTH_URL:-http://gateway.internal:8090/health}"
MANAGER_ENV="${MANAGER_ENV:-$REPO_ROOT/manager/deploy/env}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

compose() { run_remote docker compose --project-directory "$DEPLOY_DIR" \
    -f "$DEPLOY_DIR/docker-compose.yml" \
    -f "$DEPLOY_DIR/docker-compose.deploy.yml" "$@"; }

EXCLUDES=(--exclude=.git --exclude='node_modules' --exclude='.toolchain'
          --exclude='archive' --exclude='*.log')

health_ok() { curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; }

wait_health() {
    # 上限只兜异常（正常几十秒内 healthy 即提前返回）；暖启动 240s 足够，
    # 冷启动（首次拉镜像+九服务构建）用 HEALTH_TIMEOUT 环境变量临时放大。
    local deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT:-240} ))
    echo "waiting for CodeAudit gateway at $HEALTH_URL ..."
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if health_ok; then echo "codeaudit gateway healthy ($HEALTH_URL)"; return 0; fi
        sleep 5
    done
    echo "ERROR: codeaudit gateway not healthy within timeout; try: $0 logs 100" >&2
    return 1
}

sync_all() {
    run_remote mkdir -p "$DEPLOY_DIR"
    # 收敛式同步：解包前清空旧树（仅保留 .env，gen_remote_env 沿用其 JWT）。
    # 纯 overlay 不删远端已删文件——上游删除后 check 永远 drift（叠加同步膨胀）。
    run_remote find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 '!' -name .env -exec rm -rf {} +
    tar -C "$SRC" "${EXCLUDES[@]}" -cf - . | run_remote tar -xf - -C "$DEPLOY_DIR"
    tar -C . -cf - docker-compose.deploy.yml | run_remote tar -xf - -C "$DEPLOY_DIR"
    gen_remote_env
    echo "synced: $SRC + overlay + .env → $VMID:$DEPLOY_DIR"
}

gen_remote_env() {
    # JWT：远端已有 .env 则沿用其 JWT 与端口覆盖，避免每次 deploy 轮换密钥；
    # manager token 永远跟随 manager/deploy/env（单一事实源）。
    local tmp; tmp=$(mktemp)
    local jwt gw manager_url
    jwt=$(openssl rand -hex 24)
    gw=8090
    manager_url="http://gateway.internal:18800"
    if run_remote test -f "$DEPLOY_DIR/.env"; then
        jwt=$(run_remote grep -E '^CODEAUDIT_JWT_SECRET=' "$DEPLOY_DIR/.env" | cut -d= -f2 || true)
        gw=$(run_remote grep -E '^CODEAUDIT_HOST_GATEWAY=' "$DEPLOY_DIR/.env" | cut -d= -f2 || true)
        [ -n "$gw" ] || gw=8090
    fi
    {
        echo "CODEAUDIT_JWT_SECRET=$jwt"
        echo "CODEAUDIT_HOST_GATEWAY=$gw"
        echo "OPENSHELL_MANAGER_URL=$manager_url"
        echo "OPENSHELL_MANAGER_TOKEN=$(cut -d= -f2 "$MANAGER_ENV")"
        echo "CODEAUDIT_MIMO_API_KEY="
        echo "CODEAUDIT_MIMO_ENDPOINT="
    } > "$tmp"
    pct push "$VMID" "$tmp" "$DEPLOY_DIR/.env"
    run_remote chmod 600 "$DEPLOY_DIR/.env"
    rm -f "$tmp"
    echo "HEALTH_URL effective: http://gateway.internal:${gw}/health"
}

# 只建空库，不建表：各服务启动时自带 CREATE TABLE IF NOT EXISTS 自迁移
# （scripts/init-db.sql 的表结构已落后于服务迁移，例如 findings.verdict，
#  执行它反而会让服务自迁移失败——故弃用，见 README）。
init_dbs() {
    run_remote docker exec -i codeaudit-postgres psql -U postgres -tAc \
        "SELECT 'CREATE DATABASE ' || d || ' OWNER postgres;' FROM (VALUES ('codeaudit_project'),('codeaudit_task'),('codeaudit_result')) AS v(d) WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = v.d)" \
        | run_remote docker exec -i codeaudit-postgres psql -U postgres
}

# 确定性内容哈希（源码树，排除不参与部署的重目录）
dtar() {
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$SRC" "${EXCLUDES[@]}" -cf - . 2>/dev/null | md5sum | awk '{print $1}'
}
rdtar() {
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR" --exclude=.git --exclude=node_modules --exclude=.toolchain \
        --exclude=archive --exclude='*.log' \
        --exclude=docker-compose.deploy.yml --exclude=.env \
        -cf - . 2>/dev/null | md5sum | awk '{print $1}'
}

pg_ready() {
    [ "$(run_remote docker inspect -f '{{.State.Health.Status}}' codeaudit-postgres 2>/dev/null)" = "healthy" ]
}

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        sync_all
        compose up -d --build
        pg_ready || { echo "waiting for postgres ..."; }
        until pg_ready; do sleep 3; done
        init_dbs
        compose ps
        wait_health
        ;;
    check)
        # 源码树内容哈希（远端侧剔除部署态文件后同参对比）
        if [ "$(dtar)" != "$(rdtar)" ]; then
            echo "drift: 源码树与本仓不一致，重新 deploy 收敛"; exit 1
        fi
        if [ "$(md5sum docker-compose.deploy.yml | awk '{print $1}')" != \
             "$(run_remote md5sum "$DEPLOY_DIR/docker-compose.deploy.yml" 2>/dev/null | awk '{print $1}')" ]; then
            echo "drift: docker-compose.deploy.yml"; exit 1
        fi
        echo "in sync"
        ;;
    status)
        compose ps || true
        if health_ok; then echo "codeaudit gateway: OK ($HEALTH_URL)"; else echo "codeaudit gateway: DOWN"; exit 1; fi
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
        echo "usage: $0 [deploy|check|status|start|stop|restart|logs [N] [service...]]" >&2
        exit 2
        ;;
esac
