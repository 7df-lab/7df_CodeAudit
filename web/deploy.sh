#!/usr/bin/env bash
# web (CodeAudit Console) 部署 —— 前端容器（nginx SPA + /v1 反代）→ 部署主机 docker。
#
# 背景：生产 web 旧口径为「由 CD 单独发布」（时名 console）；CD 退役后该职责
# 由本脚本接续，纳入主仓库统一部署链（deploy/internal/sandbox-deploy.toml 第 5 项）。
#
# 布局：部署主机 内 DEPLOY_DIR = web 源码树；compose 以仓库自带 docker-compose.yml
# 单文件运行（项目名由 compose 顶层 name 钉死为 codeaudit-web，2026-09-15 起；
# 8088→80；upstream = 宿主发布网关 8090，经 host-gateway 别名）。
#
# 统一契约：deploy | check | status | start | stop | restart | logs [N]
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL
set -euo pipefail

cd "$(dirname "$0")"

# REMOTE 空串=本机执行契约（`-` 非 `:-`，空串不得回填 远程执行——四仓 deploy.sh 家族同构）
REMOTE="${REMOTE-ssh <deploy-host> --}"
VMID="${VMID:-}"
# 2026-09-19 修订（gateway R11 同批扩面，）：缺省自 os-deploy 死路径
# 改钉部署树 web 源码树（本脚本语义 DEPLOY_DIR=远端源码树；与主仓库
# sandbox-deploy.toml remote 同值）。
DEPLOY_DIR="${DEPLOY_DIR:-/opt/codeaudit/web}"
SRC="${SRC:-$(pwd)}"
HEALTH_URL="${HEALTH_URL:-http://192.0.2.122:8088/}"
# 宿主发布网关（deploy/prod 的 CODEAUDIT_HOST_GATEWAY=8090；容器经 host-gateway 可达）
GW_UPSTREAM="${GW_UPSTREAM:-host.docker.internal:8090}"
WEB_PORT="${WEB_PORT:-8088}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

# 镜像溯源：构建参数 GIT_REV 取本地源码树 git HEAD（远端树无 .git，须本地算好
# 注入远端构建进程）；gitless 树回落 .buildrev 的 BUILDREV_WEB（prod-sync 戳，
# 与 engine tag 同次序 git>.buildrev）——无戳才 unknown（versions 对账失据的根）
REV="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || true)"
if [ -z "$REV" ] && [ -f "$SRC/.buildrev" ]; then
    REV="$(grep -E '^BUILDREV_WEB=' "$SRC/.buildrev" | cut -d= -f2- || true)"
fi
REV="${REV:-unknown}"

# WEB_IMAGE_TAG：缺省=树 sha（git>.buildrev，同 REV 推导
# 次序）——构建直出 sha tag（生产/S 级预建比对不再依赖 :latest 同源间接）；显式 env
# 尊重（promote/回滚=引用既有 tag）。latest 仅"无 sha 可用"兜底。
WEB_IMAGE_TAG="${WEB_IMAGE_TAG:-$REV}"
# 无 sha 可用（REV=unknown 字面量）→ latest 兜底（deployment.md 口径；原 [ -n "$REV" ]
# 判断为死代码——上行 REV 缺省已填 unknown，恒非空，gitless 树曾得 :unknown tag）
[ "$WEB_IMAGE_TAG" = "unknown" ] && WEB_IMAGE_TAG="latest"

compose() {
    # ENV_FILE 显式设置 → compose 直读该 env 文件（一键链 production.env 口径；
    # 缺省回落 project-directory 自动发现的 .env——本地态由 sync_all 写入）
    local ef=()
    [ -n "${ENV_FILE:-}" ] && ef=(--env-file "$ENV_FILE")
    run_remote env GIT_REV="$REV" WEB_IMAGE_TAG="$WEB_IMAGE_TAG" docker compose --project-directory "$DEPLOY_DIR" \
        "${ef[@]}" -f "$DEPLOY_DIR/docker-compose.yml" "$@"
}

# 一次性迁移：项目名自目录 basename "web"
# 钉为 compose 顶层 name=codeaudit-web。旧项目持有钉名容器 codeaudit-web 与
# 钉名网络 codeaudit-web-net 的属主 label，不先 down 则新项目 up 撞名直接失败；
# web 无状态，down→up 秒级窗口可接受。幂等可重入：迁移完成后本守卫 no-op。
migrate_legacy_project() {
    if run_remote docker ps -aq --filter "label=com.docker.compose.project=web" | grep -q . \
       || [ "$(run_remote docker network inspect codeaudit-web-net \
              --format '{{index .Labels "com.docker.compose.project"}}' 2>/dev/null || true)" = "web" ]; then
        echo "legacy: 检出旧项目名 web（目录 basename 时代）—— down 旧项目完成迁移（一次性）"
        compose -p web down --remove-orphans
    fi
    # console→web 改名批：旧钉名容器 codeaudit-console 与新名同抢 8088，
    # 不移除则新容器 up 必撞端口。web 无状态，rm -f 秒级窗口；迁移后本守卫 no-op。
    if run_remote docker ps -aq --filter "name=^codeaudit-console$" | grep -q .; then
        echo "legacy: 移除旧名容器 codeaudit-console（console→web 改名批，一次性）"
        run_remote docker rm -f codeaudit-console
    fi
}

EXCLUDES=(--exclude=.git --exclude=node_modules --exclude=dist --exclude='.agent' --exclude='*.log' --exclude=.buildrev --exclude='*.tsbuildinfo') # B5: .agent 过程文档禁上行（同 2026-09-08 GitLab 退订口径）；.buildrev=prod-sync 部署戳（2026-09-19 修复：gitless 树构建溯源戳非源码，check 对比双侧剔除——否则 prod-sync 收敛后 check 必假红）；*.tsbuildinfo=tsc 增量构建缓存（gitignored 不入 archive 而工作树恒有，不剔则本地一构建 check 永假红，同日实测）

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null || echo 000; }
health_ok() { [ "$(http_code "$HEALTH_URL")" = "200" ]; }

wait_health() {
    local deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT:-240} ))
    echo "waiting for web at $HEALTH_URL ..."
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if health_ok; then echo "web healthy ($HEALTH_URL)"; return 0; fi
        sleep 3
    done
    echo "ERROR: web not healthy within timeout; try: $0 logs 100" >&2
    return 1
}

# 确定性内容哈希（源码树；远端侧剔除部署态 .env 后同参对比；不吞 stderr——
# tar 报错须可见，manager B1-14 同口径）
dtar() {
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$SRC" "${EXCLUDES[@]}" -cf - . | md5sum | awk '{print $1}'
}
rdtar() {
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR" --exclude=.git --exclude=node_modules --exclude=dist \
        --exclude='*.log' --exclude=.env --exclude=.buildrev --exclude='*.tsbuildinfo' \
        -cf - . | md5sum | awk '{print $1}'
}

sync_all() {
    # 本地态守卫：部署目标即源码树（一键链 REMOTE="" DEPLOY_DIR=<检出本身>）——
    # 收敛式清空会毁掉源码树，跳过同步（.env 由 ENV_FILE 直通或操作者自备）
    if [ "$(cd "$DEPLOY_DIR" 2>/dev/null && pwd)" = "$(cd "$SRC" && pwd)" ]; then
        echo "note: DEPLOY_DIR=SRC（本地源码树即部署目标）——跳过同步"
        return 0
    fi
    # 收敛式同步：清空旧树再解包（防止上游已删文件残留使 check 永远 drift）
    run_remote mkdir -p "$DEPLOY_DIR"
    run_remote find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
    tar -C "$SRC" "${EXCLUDES[@]}" -cf - . | run_remote tar -xf - -C "$DEPLOY_DIR"
    run_remote bash -c "printf 'CODEAUDIT_WEB_PORT=$WEB_PORT\nCODEAUDIT_GATEWAY_UPSTREAM=$GW_UPSTREAM\n' > '$DEPLOY_DIR/.env'"
    echo "synced: $SRC → $VMID:$DEPLOY_DIR"
}

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        sync_all
        migrate_legacy_project
        # BUILD=0 → up --no-build（S 级预建镜像直接消费；缺省 1=构建）
        up_flags=(-d --build)
        [ "${BUILD:-1}" = "0" ] && up_flags=(-d --no-build)
        compose up "${up_flags[@]}"
        wait_health
        # 反代连通性：/v1 必须透传到网关（未认证 → 401，而非 404/502）
        code=$(http_code "$HEALTH_URL/v1/projects")
        [ "$code" = "401" ] || { echo "ERROR: web /v1 反代异常（HTTP $code，期望 401）" >&2; exit 1; }
        echo "web /v1 反代 OK（401 透传）"
        ;;
    check)
        if [ "$(dtar)" != "$(rdtar)" ]; then
            echo "drift: 源码树与本仓不一致，重新 deploy 收敛"; exit 1
        fi
        echo "in sync"
        ;;
    status)
        compose ps || true
        if health_ok; then echo "web: OK ($HEALTH_URL)"; else echo "web: DOWN"; exit 1; fi
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
