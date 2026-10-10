#!/usr/bin/env bash
# codeaudit (CodeAudit) 部署 —— engine/deploy（本仓部署态事实源）→ 目标 docker compose。
#
# 归属：部署知识归离服务最近的
# 层——engine 栈的部署事实源自主仓库 deploy/prod/ 迁入本目录（git 历史在主仓库可
# 追溯）；主仓库只做拓扑编排（sandbox-deploy.toml 清单 / production-deploy 一键链）。
# 单仓检出可独立部署：REMOTE 空串=本机执行契约；MANAGER_ENV/UMBRELLA_ROOT/
# ENV_FILE 可覆写（缺省按主仓库兄弟布局解析，见下）。
#
# 布局：目标 DEPLOY_DIR = engine compose base + configs/ + 本目录的
# docker-compose.deploy.yml + .env；compose 以「仓库 base + 本 overlay」
# 双文件运行（promote 模式下构建上下文不再随行——见下）。
#
# 密钥接线：远端 .env 由本脚本生成 —— JWT 随机生成（已存在则保留），
# OPENSHELL_MANAGER_TOKEN 单一事实源取 manager 仓 deploy/env（经 MANAGER_ENV
# 指向；主仓库布局缺省解析，单仓检出需显式提供）。ENV_FILE 显式设置时直接
# 采用该文件为 .env 源（production-deploy 的 production.env 链走此口）。
#
# 统一契约：deploy | check | status | start | stop | restart | logs [N]
#
# 两种 deploy 模式：
#   promote（缺省）：只同步 compose base + configs/ + overlay + .env 四件，
#     预检 codeaudit-<svc>:<ENGINE_IMAGE_TAG> 七镜像已在目标 daemon 在位
#     （构建职责在 sim 构建验证级——sim-sync.sh rebuild 同 sha 已建），
#     `up -d --no-build` 纯引用——部署=秒级，回滚=promote 上一 tag。
#   BUILD=1（恢复锚点）：旧全量路径——tar 同步整棵 engine 源码树 + `up --build`
#     远端重建。sim 链不可用/镜像丢失时的兜底，语义与 2026-09-16 前完全一致。
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL / BUILD /
#              ENGINE_IMAGE_TAG / MANAGER_ENV / UMBRELLA_ROOT / ENV_FILE
set -euo pipefail

# 路径定位须在 cd 进本目录之前用 $0 算一次（ADR-208 教训：cd 后相对 $0 的
# 二次定位会叠加路径而失败）。
ENGINE_ROOT="$(cd "$(dirname "$0")/.." && pwd)"          # = engine 仓根
UMBRELLA_ROOT="${UMBRELLA_ROOT:-$(cd "$ENGINE_ROOT/.." && pwd)}"   # 主仓库兄弟布局（可覆写）
cd "$(dirname "$0")"

# `-` 而非 `:-`：REMOTE=""（空串）是显式"本机执行"契约（deploy.sh 家族同构；
# production-deploy 以 REMOTE="" 调用本脚本做本机 docker 部署）。
REMOTE="${REMOTE-ssh <deploy-host> --}"
VMID="${VMID:-}"
# 2026-09-19 修订（gateway R11 同批扩面，授权收口）：缺省自 os-deploy
# 死路径改钉树外扁平落点（<legacy-path> 在 <host-id> 已整个不存在）。落点不可入
# 部署树：本脚本 promote 会 find 清空 DEPLOY_DIR（除 .env），prod-sync 又整树
# 清空重建——互斥。与主仓库 sandbox-deploy.toml remote 同值（check-wiring A15 守卫）。
DEPLOY_DIR="${DEPLOY_DIR:-/opt/codeaudit/engine}"
SRC="${SRC:-$ENGINE_ROOT}"
HEALTH_URL="${HEALTH_URL:-http://192.0.2.122:8090/health}"
MANAGER_ENV="${MANAGER_ENV:-$UMBRELLA_ROOT/manager/deploy/env}"
# 镜像钉版（ADR-233，三链同源）：engine 7 服务镜像 tag=engine 树 short sha，
# sim.sh / production-deploy.sh 同口径；可经 env 覆盖（钉制发布/回滚特定版本）
ENGINE_IMAGE_TAG="${ENGINE_IMAGE_TAG:-$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo latest)}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

compose() {
    # ENV_FILE 显式设置 → 直读该 env 文件（一键链 production.env 口径；
    # 缺省 compose 在 project-directory 自动发现 .env——<host-id> 链 gen_remote_env 生成）
    local ef=()
    [ -n "${ENV_FILE:-}" ] && ef=(--env-file "$ENV_FILE")
    # overlay 定位：<host-id> 链在 $DEPLOY_DIR（sync_min/sync_all 已复制）；本地态
    # （REMOTE=""，DEPLOY_DIR=SRC 跳同步）下 overlay 在源码树 deploy/ 子目录——
    # 就地取用。⚠ 回退判定必须仅限本机模式：[ -f ] 是本地文件系统测试，远端链
    # 恒假→回退会把本地绝对路径喂给远端 compose（<host-id> 链 promote 自 ADR-235 迁移
    # 后首次真跑即炸，2026-09-19 批抓出）。
    local ovf="$DEPLOY_DIR/docker-compose.deploy.yml"
    if [ -z "${REMOTE:-}" ] && [ ! -f "$ovf" ]; then
        ovf="$SRC/deploy/docker-compose.deploy.yml"
    fi
    run_remote docker compose --project-directory "$DEPLOY_DIR" \
        "${ef[@]}" \
        -f "$DEPLOY_DIR/docker-compose.yml" \
        -f "$ovf" "$@"
}

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
    # 本地态守卫：部署目标即源码树（一键链 REMOTE="" DEPLOY_DIR=<检出本身>）——
    # 收敛式清空会毁掉源码树，跳过同步（overlay 同目录天然在位；env 经 ENV_FILE
    # 直通或远端既有 .env 沿用）
    if [ "$(cd "$DEPLOY_DIR" 2>/dev/null && pwd)" = "$(cd "$SRC" && pwd)" ]; then
        echo "note: DEPLOY_DIR=SRC（本地源码树即部署目标）——跳过同步"
        gen_remote_env_if_needed
        return 0
    fi
    run_remote mkdir -p "$DEPLOY_DIR"
    # 收敛式同步：解包前清空旧树（仅保留 .env，gen_remote_env 沿用其 JWT）。
    # 纯 overlay 不删远端已删文件——上游删除后 check 永远 drift（叠加同步膨胀）。
    run_remote find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 '!' -name .env -exec rm -rf {} +
    tar -C "$SRC" "${EXCLUDES[@]}" -cf - . | run_remote tar -xf - -C "$DEPLOY_DIR"
    tar -C . -cf - docker-compose.deploy.yml | run_remote tar -xf - -C "$DEPLOY_DIR"
    gen_remote_env
    echo "synced: $SRC + overlay + .env → ${VMID:-本机}:$DEPLOY_DIR"
}

# ---- promote 模式（缺省）：源码树不随行，只同步 compose 运行四件套 ----
# compose 运行需要的文件全部在此清单：base compose / configs（bind-mount 硬依赖，
# engine 7 服务 volumes 挂 ./configs/codeaudit.yaml）/ overlay / .env。
# build: 段中的 context 路径只在构建时被解析——up 不带 --build 时不需要源码树。
sync_min() {
    if [ "$(cd "$DEPLOY_DIR" 2>/dev/null && pwd)" = "$(cd "$SRC" && pwd)" ]; then
        echo "note: DEPLOY_DIR=SRC（本地源码树即部署目标）——跳过同步"
        gen_remote_env_if_needed
        return 0
    fi
    run_remote mkdir -p "$DEPLOY_DIR"
    run_remote find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 '!' -name .env -exec rm -rf {} +
    tar -C "$SRC" -cf - docker-compose.yml configs | run_remote tar -xf - -C "$DEPLOY_DIR"
    tar -C . -cf - docker-compose.deploy.yml | run_remote tar -xf - -C "$DEPLOY_DIR"
    gen_remote_env
    echo "synced(promote): engine compose + configs + overlay + .env → ${VMID:-本机}:$DEPLOY_DIR（tag=$ENGINE_IMAGE_TAG）"
}

# 镜像在位预检：promote 只引用不构建，七镜像缺一即 fail-loud（给出恢复出口）。
# 参数 $1=要核的 tag（缺省本地 ENGINE_IMAGE_TAG；check 场景应传远端 .env 的 tag——
# 本地领先时远端旧版本仍是合法运行态，不应误报）。
check_images_present() {
    local tag="${1:-$ENGINE_IMAGE_TAG}" svc missing=""
    for svc in gateway project task storage result sast-adapter runtime; do
        run_remote docker image inspect "codeaudit-$svc:$tag" >/dev/null 2>&1 \
            || missing="$missing codeaudit-$svc:$tag"
    done
    [ -z "$missing" ] && { echo "images: codeaudit-<svc>:$tag ×7 在位"; return 0; }
    echo "ERROR: 目标 daemon 缺镜像:$missing" >&2
    echo "  promote 模式不构建——先在构建验证级产出同 tag 镜像：" >&2
    echo "    bash deploy/internal/sim-sync.sh rebuild   # sim 链按 engine 树 sha 构建七镜像" >&2
    echo "  或走全量构建兜底：BUILD=1 $0 deploy" >&2
    return 1
}

push_env() {  # push_env <localfile>：pct 形态推送 / 本机与 ssh 形态拷贝
    case "$REMOTE" in
        "remote exec"*) pct push "$VMID" "$1" "$DEPLOY_DIR/.env" ;;
        *) cp "$1" "$DEPLOY_DIR/.env" ;;
    esac
}

gen_remote_env_if_needed() {
    # ENV_FILE 直通模式：compose 已带 --env-file，无远端 .env 生成/推送诉求
    if [ -n "${ENV_FILE:-}" ] && [ -f "$ENV_FILE" ]; then
        echo "env: 采用 ENV_FILE=$ENV_FILE（compose 直读，不生成远端 .env）"
        return 0
    fi
    gen_remote_env
}

gen_remote_env() {
    # JWT：远端已有 .env 则沿用其 JWT 与端口覆盖，避免每次 deploy 轮换密钥；
    # manager token 永远跟随 manager/deploy/env（单一事实源）。
    # （ENV_FILE 直通由 gen_remote_env_if_needed 前置分流，此处不再处理）
    local tmp; tmp=$(mktemp)
    local jwt gw manager_url
    jwt=$(openssl rand -hex 24)
    gw=8090
    manager_url="http://192.0.2.122:18800"
    if run_remote test -f "$DEPLOY_DIR/.env"; then
        jwt=$(run_remote grep -E '^CODEAUDIT_JWT_SECRET=' "$DEPLOY_DIR/.env" | cut -d= -f2- || true)
        gw=$(run_remote grep -E '^CODEAUDIT_HOST_GATEWAY=' "$DEPLOY_DIR/.env" | cut -d= -f2- || true)
        [ -n "$gw" ] || gw=8090
    fi
    {
        echo "CODEAUDIT_JWT_SECRET=$jwt"
        echo "CODEAUDIT_HOST_GATEWAY=$gw"
        # 镜像钉版（ADR-233）：每次部署随 SRC 树 sha 刷新（不沿用旧值）——
        # codeaudit-<svc>:<sha> 跨链同名同源，docker images 可查所部版本
        echo "ENGINE_IMAGE_TAG=$ENGINE_IMAGE_TAG"
        echo "OPENSHELL_MANAGER_URL=$manager_url"
        echo "OPENSHELL_MANAGER_TOKEN=$(cut -d= -f2- "$MANAGER_ENV")"
        echo "CODEAUDIT_MIMO_API_KEY="
        echo "CODEAUDIT_MIMO_ENDPOINT="
    } > "$tmp"
    push_env "$tmp"
    run_remote chmod 600 "$DEPLOY_DIR/.env"
    rm -f "$tmp"
    echo "HEALTH_URL effective: http://192.0.2.122:${gw}/health"
}

# 只建空库，不建表：各服务启动时自带 CREATE TABLE IF NOT EXISTS 自迁移
# （scripts/init-db.sql 的表结构已落后于服务迁移，例如 findings.verdict，
#  执行它反而会让服务自迁移失败——故弃用，见 README）。
init_dbs() {
    run_remote docker exec -i codeaudit-postgres psql -U postgres -tAc \
        "SELECT 'CREATE DATABASE ' || d || ' OWNER postgres;' FROM (VALUES ('codeaudit_project'),('codeaudit_task'),('codeaudit_result')) AS v(d) WHERE NOT EXISTS (SELECT from pg_database WHERE datname = v.d)" \
        | run_remote docker exec -i codeaudit-postgres psql -U postgres
}

# 确定性内容哈希（源码树，排除不参与部署的重目录；不吞 stderr——tar 报错须
# 可见，manager B1-14 / web 同口径）
dtar() {
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$SRC" "${EXCLUDES[@]}" -cf - . | md5sum | awk '{print $1}'
}
rdtar() {
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR" --exclude=.git --exclude=node_modules --exclude=.toolchain \
        --exclude=archive --exclude='*.log' \
        --exclude=docker-compose.deploy.yml --exclude=.env \
        -cf - . | md5sum | awk '{print $1}'
}

pg_ready() {
    [ "$(run_remote docker inspect -f '{{.State.Health.Status}}' codeaudit-postgres 2>/dev/null)" = "healthy" ]
}

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        if [ "${BUILD:-0}" = "1" ]; then
            echo "[mode] BUILD=1 全量路径（源码树随行 + 远端重建）"
            sync_all
            compose up -d --build
        else
            echo "[mode] promote（引用 $ENGINE_IMAGE_TAG 镜像，不构建；BUILD=1 切旧全量路径）"
            sync_min
            check_images_present
            compose up -d --no-build
        fi
        pg_deadline=$(( $(date +%s) + ${PG_TIMEOUT:-300} ))
        pg_note=0
        until pg_ready; do
            [ "$(date +%s)" -lt "$pg_deadline" ] || {
                echo "ERROR: postgres 未在 ${PG_TIMEOUT:-300}s 内 healthy; try: $0 logs 100 codeaudit-postgres" >&2
                exit 1
            }
            [ "$pg_note" = "0" ] && { echo "waiting for postgres ..."; pg_note=1; }
            sleep 3
        done
        init_dbs
        # 空卷首启时序自愈：task/result 于 compose
        # 拉起时即连业务库，而业务库由 init_dbs 建；两服务 restart=no，空卷首启
        # （全新安装/换项目卷）必崩躺尸——库就绪后回拉，已跑者 no-op 幂等。
        compose up -d --no-deps task result
        compose ps
        wait_health
        ;;
    check)
        # overlay 一致性两模式共检
        if [ "$(md5sum docker-compose.deploy.yml | awk '{print $1}')" != \
             "$(run_remote md5sum "$DEPLOY_DIR/docker-compose.deploy.yml" 2>/dev/null | awk '{print $1}')" ]; then
            echo "drift: docker-compose.deploy.yml"; exit 1
        fi
        if [ "${BUILD:-0}" = "1" ]; then
            # 源码树内容哈希（远端侧剔除部署态文件后同参对比）
            if [ "$(dtar)" != "$(rdtar)" ]; then
                echo "drift: 源码树与本仓不一致，重新 deploy 收敛"; exit 1
            fi
            echo "in sync（BUILD 模式：源码树+overlay）"
            exit 0
        fi
        # promote 模式：对账 configs/镜像在位/运行容器引用的 tag
        if [ "$(md5sum "$SRC/configs/codeaudit.yaml" 2>/dev/null | awk '{print $1}')" != \
             "$(run_remote md5sum "$DEPLOY_DIR/configs/codeaudit.yaml" 2>/dev/null | awk '{print $1}')" ]; then
            echo "drift: configs/codeaudit.yaml（本地与远端不一致，重新 deploy 收敛）"; exit 1
        fi
        local_tag="$ENGINE_IMAGE_TAG"
        remote_tag=$(run_remote grep -E '^ENGINE_IMAGE_TAG=' "$DEPLOY_DIR/.env" 2>/dev/null | cut -d= -f2-)
        [ -n "$remote_tag" ] || { echo "远端 .env 无 ENGINE_IMAGE_TAG（未部署过？先 deploy）"; exit 1; }
        check_images_present "$remote_tag" || exit 1
        running_mismatch=""
        for svc in gateway project task storage result sast-adapter runtime; do
            rimg=$(run_remote docker inspect -f '{{.Config.Image}}' "codeaudit-$svc" 2>/dev/null) || rimg=""
            [ -n "$rimg" ] || { running_mismatch="$running_mismatch codeaudit-$svc(未运行)"; continue; }
            [ "$rimg" = "codeaudit-$svc:$remote_tag" ] || running_mismatch="$running_mismatch $svc:$rimg"
        done
        if [ -n "$running_mismatch" ]; then
            echo "drift: 运行容器引用 ≠ .env tag($remote_tag):$running_mismatch —— 重新 deploy 收敛"; exit 1
        fi
        [ "$local_tag" = "$remote_tag" ] \
            && echo "in sync（promote：镜像 $remote_tag ×7 在位且运行中，overlay/configs 一致）" \
            || echo "behind: 远端跑 $remote_tag，本地树为 $local_tag —— promote 部署即收敛"
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
        echo "  deploy/check 缺省 promote 模式（镜像引用不构建）；BUILD=1 切全量源码同步+远端重建" >&2
        exit 2
        ;;
esac
