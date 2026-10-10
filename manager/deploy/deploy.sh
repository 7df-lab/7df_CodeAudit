#!/usr/bin/env bash
# openshell-manager 部署 —— 本目录（部署事实源）→ 部署主机 docker compose。
#
# 同步内容：服务源码（openshell_manager/ openshell/ config.json
# .dockerignore，来自 SRC 仓根）+ 本目录部署产物（落远端 deploy/ 子目录：
# Dockerfile.manager docker-compose.yml env.template）+ 密钥 env → 远端
# .env（600）。config.json 保持本地开发态原样（bind 127.0.0.1）；容器内由
# .env 覆盖（env > config.json，见服务 config.py）。
#
# 远端布局与仓内同构（compose build context=.. = DEPLOY_DIR，构建输入与
# 仓根零差异）——2026-09-15 解绑 vendored SDK 起不再 staging 摊平（R31）。
#
# 统一契约：deploy | ensure-config | check | status | start | stop | restart | logs [N]
#
# Environment: REMOTE / VMID / DEPLOY_DIR / SRC / HEALTH_URL
set -euo pipefail

cd "$(dirname "$0")"

# `-` 而非 `:-`：REMOTE=""（空串）是显式"本机执行"契约（与 openshell-gateway
# 两脚本同语义；`:-` 会在空串时静默触发 远程执行 缺省、打错目标宿主——B1-11 审计
# 修复，2026-09-11 对齐主仓库 REMOTE 契约家族）。
REMOTE="${REMOTE-ssh <deploy-host> --}"
VMID="${VMID:-}"
# 2026-09-19 修订（gateway R11 同批扩面，）：缺省自 os-deploy 死路径
# 改钉部署树现役目录（<legacy-path> 在 <host-id> 已整个不存在；现役 compose
# working_dir=/opt/codeaudit/manager/deploy = 本值+/deploy；与主仓库
# sandbox-deploy.toml remote 同值）。
DEPLOY_DIR="${DEPLOY_DIR:-/opt/codeaudit/manager}"
SRC="${SRC:-$(cd .. && pwd)}"   # 默认 = 本仓根（脚本已 cd 到自身目录）
HEALTH_URL="${HEALTH_URL:-http://192.0.2.122:18800/healthz}"

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

# manager_tag_default — 镜像 tag 缺省统一推导（Phase 2 2b，2026-09-21 code-review
# 收口：原 compose() 缺省字面 2.0.0 与 deploy 分支树 sha 两套并存，restart 失败
# 回落 up 时按 2.0.0 重建偏离 sha 部署）。次序 git>.buildrev>2.0.0；显式 env 恒
# 尊重（promote/回滚=引用既有 tag）。
manager_tag_default() {
    local t
    t="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || true)"
    if [ -z "$t" ] && [ -f "$SRC/.buildrev" ]; then
        t="$(grep -E '^BUILDREV_MANAGER=' "$SRC/.buildrev" | cut -d= -f2- || true)"
    fi
    echo "${t:-2.0.0}"
}

compose() {
    # ENV_FILE 显式设置 → 直读该 env 文件（一键链 production.env 口径）；
    # 缺省 $DEPLOY_DIR/.env（<host-id> 链 sync_all 推送/本机自备）
    local ef=(--env-file "$DEPLOY_DIR/.env")
    [ -n "${ENV_FILE:-}" ] && ef=(--env-file "$ENV_FILE")
    run_remote env MANAGER_IMAGE_TAG="${MANAGER_IMAGE_TAG:-$(manager_tag_default)}" docker compose --project-directory "$DEPLOY_DIR/deploy" \
        "${ef[@]}" \
        -f "$DEPLOY_DIR/deploy/docker-compose.yml" "$@"
}
# project-directory 必须钉 compose 文件所在目录（DEPLOY_DIR/deploy，与本地
# "从 manager/deploy 目录制"同构）：compose 把 build.context 的相对路径按
# **project directory** 解析——钉 DEPLOY_DIR 时 context ".." 会错位到
# os-deploy 父层、dockerfile 解析成 <父层>/deploy/...（R31 deferred 的 docker
# 级实证 2026-09-16 首跑即炸，lstat <legacy-path>/deploy/deploy）。项目名
# 由 compose 顶层 name: openshell-manager 钉死，不随目录变。.env 在 DEPLOY_DIR
# 根而 compose 只在 project directory 找默认 .env——必须显式 --env-file
# （up/down/ps 全走本包装=同 project+env-file，LESSONS #16 口径）。

# openshell/ SDK 已并入仓根，按普通包随服务源码同步；
# .dockerignore 必须随行——构建上下文=DEPLOY_DIR，它挡密钥/过程文件入镜像。
# requirements.txt 是 Dockerfile.manager 的 COPY 输入——漏同步则远端构建必败
# 。
SYNC_TAR="openshell_manager openshell requirements.txt config.json .dockerignore"     # 服务侧（SRC=仓根）
DEPLOY_TAR="Dockerfile.manager docker-compose.yml env.template"     # 部署侧（本目录 → 远端 deploy/）

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
    # 本地态守卫：部署目标即源码树（一键链 REMOTE="" DEPLOY_DIR=<检出本身>）——
    # 收敛式清空会毁掉源码树，跳过同步（.env 由 ENV_FILE 直通或操作者自备）
    if [ "$(cd "$DEPLOY_DIR" 2>/dev/null && pwd)" = "$(cd "$SRC" && pwd)" ]; then
        echo "note: DEPLOY_DIR=SRC（本地源码树即部署目标）——跳过同步"
        return 0
    fi
    run_remote mkdir -p "$DEPLOY_DIR"
    # 先清同步目标再解包（叠加式同步会残留旧树/旧布局残留文件）。
    run_remote rm -rf "$DEPLOY_DIR/openshell_manager" "$DEPLOY_DIR/openshell" \
        "$DEPLOY_DIR/requirements.txt" "$DEPLOY_DIR/deploy"
    run_remote mkdir -p "$DEPLOY_DIR/deploy"
    # remote exec 管道直灌，不落中间文件；tar -xf 保留 mtime，check 的确定性
    # 哈希依赖这一点（远端树 mtime == 本地树 mtime）。
    tar -C "$SRC" --exclude='__pycache__' --exclude='.token' --exclude='manager.log' \
        -cf - $SYNC_TAR | run_remote tar -xf - -C "$DEPLOY_DIR"
    tar -C . -cf - $DEPLOY_TAR | run_remote tar -xf - -C "$DEPLOY_DIR/deploy"
    # C2-2：远程执行 push 只在 REMOTE 为 远程执行 形态时可用——
    # 远程执行 硬编码绕过 REMOTE 契约（REMOTE=""=本机执行时 部署主机 内无 远程执行，set -e
    # 必炸；REMOTE=ssh 形态时环境变量被打到错误宿主）。对齐 openshell-gateway
    # B1-5 同族修复：非 远程执行 形态跳过并提示操作者自行确保 .env 在位。
    case "$REMOTE" in
        "remote exec"*) pct push "$VMID" env "$DEPLOY_DIR/.env" ;;
        *)
            if [ -z "$REMOTE" ] && [ -f env ] && [ ! -f "$DEPLOY_DIR/.env" ]; then
                # 本机模式：从 deploy/env 引导一份（幂等——不覆盖既有 .env）
                cp env "$DEPLOY_DIR/.env"
                echo "note: 本机执行——已从 deploy/env 引导 $DEPLOY_DIR/.env"
            else
                echo "note: REMOTE='$REMOTE' 非 pct 形态——跳过 env 推送，请自行确保 $DEPLOY_DIR/.env 在位"
            fi
            ;;
    esac
    run_remote chmod 600 "$DEPLOY_DIR/.env"
    echo "synced: $SRC + 部署产物(deploy/) + .env → $VMID:$DEPLOY_DIR"
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
rdtar_sub() {  # rdtar_sub <subdir> <paths...>：远端 DEPLOY_DIR/<subdir> 版（部署侧）
    local sub="$1"; shift
    run_remote tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@1767225600 \
        -C "$DEPLOY_DIR/$sub" -cf - "$@"
}
hashof() { md5sum | awk '{print $1}'; }

# C2-3：token 按行前缀剥离——B1-14 的 `cut -d= -f2-`
# 只修了行内 padding 截断，没修"作用于整个文件"：env 按 env.template 补全成
# 多行后，cut 会把其他行也卷进 Authorization 头（网关可达性检查静默 WARN
# 且报障指向网关）。2>/dev/null 仅静默"文件缺失"，配合 check 分支的显式
# SKIPPED（env 缺失不假失败）。函数体多行展开（tests/deploy_cases.sh 以
# /^manager_token_of()/,/^}/ 行范围提取本函数做剥离断言）。
manager_token_of() {
    # token 来源两档：<host-id> 链=本目录 env；一键链（production.env 派生）=部署目标 .env
    grep -m1 '^OPENSHELL_MANAGER_TOKEN=' env 2>/dev/null | cut -d= -f2- \
        || grep -m1 '^OPENSHELL_MANAGER_TOKEN=' "$DEPLOY_DIR/.env" 2>/dev/null | cut -d= -f2- \
        || true
}

# config.json 是 gitignored 构建输入（Dockerfile COPY；运行态 env 覆盖）。
# 单仓检出/fresh clone 自足出口：规范最小档模板
# 入库本目录，缺失时由此生成——此前唯一生成出口在主仓库两处内联副本（已随
# 本批收敛为调用此处）。5 键=服务 config.py 头注规范最小档。
ensure_config() {
    if [ -f "$SRC/config.json" ]; then
        echo "config.json: 在位"
        return 0
    fi
    cp config.template.json "$SRC/config.json"
    echo "config.json 缺失 —— 已从 deploy/config.template.json 生成规范最小档（运行态 env 覆盖）"
}

cmd="${1:-deploy}"
case "$cmd" in
    deploy)
        ensure_config
        sync_all
        # Phase 2 2b：按树 sha 刷 tag（消灭字面 2.0.0 运行 tag——
        # versions.sh manager 行可判同步）；显式 env 尊重。推导=manager_tag_default
        # 单点
        export MANAGER_IMAGE_TAG="${MANAGER_IMAGE_TAG:-$(manager_tag_default)}"
        # 预建快路径：BUILD=0 且目标镜像在位时 up 不带 --build——伞仓
        # production-deploy 预拉发布镜像（RELEASE_REGISTRY 通道）后零编译消费。
        # 与 engine/web 的 BUILD 开关同语义；缺省 BUILD=1 = 原地构建行为不变。
        if [ "${BUILD:-1}" = "0" ] && \
            docker image inspect "openshell-manager:$MANAGER_IMAGE_TAG" >/dev/null 2>&1; then
            echo "openshell-manager:$MANAGER_IMAGE_TAG 在位——跳过构建（BUILD=1 强制重建）"
            compose up -d
        else
            compose up -d --build
        fi
        wait_health
        echo "== gateway 可达性（经 manager，Bearer token）=="
        # C2-3：manager_token_of 按行前缀剥离（-f2- 保留 padding，B1-14 口径）
        if curl -fsS --max-time 8 -H "Authorization: Bearer $(manager_token_of)" \
            "${HEALTH_URL%/healthz}/api/v1/gateway/health"; then
            echo
        else
            echo "WARN: gateway/health 未通（网关未起？../../openshell-gateway/deploy.sh start）"
        fi
        echo "hint: 宿主机引擎接入：export OPENSHELL_MANAGER_URL=http://192.0.2.122:18800 OPENSHELL_MANAGER_TOKEN=\$(grep -m1 '^OPENSHELL_MANAGER_TOKEN=' deploy/env | cut -d= -f2-)"
        ;;
    ensure-config)
        ensure_config
        ;;
    check)
        d=0
        if [ "$(dtar "$SRC" $SYNC_TAR | hashof)" != "$(rdtar $SYNC_TAR | hashof)" ]; then
            echo "drift: 服务源码"; d=1
        fi
    if [ "$(dtar . $DEPLOY_TAR | hashof)" != "$(rdtar_sub deploy $DEPLOY_TAR | hashof)" ]; then
        echo "drift: 部署产物"; d=1
    fi
        if [ -f env ]; then
            if [ "$(md5sum env | awk '{print $1}')" != "$(run_remote md5sum "$DEPLOY_DIR/.env" 2>/dev/null | awk '{print $1}')" ]; then
                echo "drift: .env"; d=1
            fi
        else
            # C2-3：env 缺失显式 SKIPPED——树比对照常，.env 对比不假失败
            echo "check: env 缺失 → .env 对比 SKIPPED（先补 deploy/env 再 deploy）"
        fi
        if [ "$d" = "0" ]; then echo "in sync"; else echo "^ 与本仓不一致，重新 deploy 收敛"; exit 1; fi
        ;;
    status)
        compose ps || true
        if health_ok; then echo "healthz: OK ($HEALTH_URL)"; else echo "healthz: DOWN"; exit 1; fi
        ;;
    start)
        compose start || compose up -d
        wait_health
        ;;
    stop)
        # C2-1：stop 失败绝不回落 up——"停不下来反而拉起"
        # 是语义反转（原 `|| compose up -d` 对 stop 同样生效）；失败随 set -e
        # 退出如实报错。
        compose stop
        ;;
    restart)
        compose restart || compose up -d
        wait_health
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
