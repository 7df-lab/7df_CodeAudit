#!/usr/bin/env bash
# CodeAudit 生产态一键部署 —— 用户 clone 本伞仓后，在自己的 docker 服务器上运行：
#
#   git clone --recurse-submodules <umbrella>   # 或 clone 后 make update
#   cd codeaudit-umbrella
#   bash deploy/production-deploy.sh            # 一键：预检→密钥→镜像→5项目→健康门
#
# 与 sandbox-deploy.sh（开发测试态：经 pct 下发到 LXC 107）同构的 5 项目拓扑，
# 但目标=本机 docker daemon：gateway → manager → codeaudit(engine) → dsh-pentest-sse
# (沙箱镜像) → web(console)。全部服务定义/overlay 与沙箱部署共用同一事实源，
# 差异只在接线地址（本机=经发布端口 + host-gateway 别名，见 production.env.template）。
#
# 命令：
#   deploy   默认。幂等全量部署（可重复跑收敛）
#   status   各项目容器/健康一览
#   check    部署前置只读预检（不构建不启动）
#   stop     按反拓扑序停栈（保卷保配置）
#   down     compose down（保卷）；down -v 连卷清除（全量重置）
#
# 参数：deploy/production.env（gitignored；缺失时自动按 production.env.template 生成，
#       密钥随机 + 宿主 IP 自动探测）。改参数后重跑 deploy 即收敛。
#
# 前置（check 会逐项核验）：
#   - docker + compose 插件；bash/curl/openssl/python3
#   - 子仓就位（engine/web/manager/openshell-gateway/dsh-runtime/dsh-pentest-sse，
#     clone 须带 --recurse-submodules 或先 make update）
#   - engine/services/sast-adapter-service/tools/opengrep（gitignored 大件；
#     缺失时本脚本按 PROVENANCE.md 的官方 release 自动拉取并 sha256 复核，
#     无 GitHub 出口则给出手工 vendor 指引后终止）
#   - 端口 8080/8081/8090/8088/18800 及中间件口空闲（可在 production.env 改）
#   - 网段 10.10.110.0/24（可 env 改）与 10.10.109.0/24 可用
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ENV_FILE="deploy/production.env"
TEMPLATE="deploy/production.env.template"
LOG_TEE=""

# ---- 输出：全部落盘 .agent/evidence/（U8：结论只认命令原始输出）------------
evidence_dir() { mkdir -p .agent/evidence; echo ".agent/evidence/prod-deploy-$(date +%Y%m%d-%H%M%S).log"; }

say() { echo "[prod-deploy] $*"; }
die() { echo "[prod-deploy] ERROR: $*" >&2; exit 1; }

# ---- 子命令 ----------------------------------------------------------------

cmd="${1:-deploy}"
[ $# -gt 0 ] && shift

# ---- 公共：读 env（缺则生成）------------------------------------------------

load_env() {
    if [ ! -f "$ENV_FILE" ]; then
        say "生成 $ENV_FILE（按 $TEMPLATE，密钥随机 + 宿主 IP 自动探测）..."
        local ip jwt tok
        ip=$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oE 'src [0-9.]+' | awk '{print $2}' | head -1)
        [ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}')
        [ -n "$ip" ] || die "无法探测宿主 IP（ip/hostname 均失败），请手工创建 $ENV_FILE"
        jwt=$(openssl rand -hex 24) || die "openssl 不可用"
        tok=$(openssl rand -hex 24)
        sed -e "s|<auto: openssl rand -hex 24>|$jwt|" \
            -e "s|<auto: openssl rand -hex 24，manager 与 engine 同值由本文件保证>|$tok|" \
            -e "s|<auto: http://<宿主IP>:18800>|http://${ip}:18800|" \
            "$TEMPLATE" > "$ENV_FILE"
        chmod 600 "$ENV_FILE"
        say "已生成（密钥已随机，宿主 IP=${ip}）；可编辑后重跑。"
    fi
    # 旧版生成的 env 文件补新键（幂等升级；console 反代目标缺省=引擎网关发布口）
    if ! grep -qE '^CODEAUDIT_GATEWAY_UPSTREAM=' "$ENV_FILE"; then
        echo "CODEAUDIT_GATEWAY_UPSTREAM=host.docker.internal:${CODEAUDIT_HOST_GATEWAY:-8090}" >> "$ENV_FILE"
    fi
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
}

# ---- check：只读预检 --------------------------------------------------------

check_ports() {
    local port occupied=""
    for port in "${OPENSHELL_PORT:-8080}" "${OPENSHELL_HEALTH_PORT:-8081}" \
                "${CODEAUDIT_HOST_GATEWAY:-8090}" "${CODEAUDIT_CONSOLE_PORT:-8088}" 18800 \
                "${CODEAUDIT_HOST_PG:-5432}" "${CODEAUDIT_HOST_REDIS:-6379}" \
                "${CODEAUDIT_HOST_MINIO_API:-9000}" "${CODEAUDIT_HOST_KAFKA:-9092}"; do
        if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE ":${port}\$"; then
            occupied="$occupied $port"
        fi
    done
    [ -z "$occupied" ] && { say "✓ 端口无冲突"; return 0; }
    say "△ 端口已被占用:$occupied —— 若属本栈旧容器则 compose up 会原样复用，否则改 $ENV_FILE 或腾出"
}

cmd_check() {
    local fail=0
    say "== 预检 =="
    check_tool docker || fail=1
    docker compose version >/dev/null 2>&1 || { say "✗ docker compose 插件不可用"; fail=1; }
    for t in bash curl openssl python3; do check_tool "$t" || fail=1; done
    for d in engine web manager openshell-gateway dsh-runtime dsh-pentest-sse; do
        [ -e "$d/.git" ] || { say "✗ 子仓缺失: $d/（clone 加 --recurse-submodules 或 make update）"; fail=1; }
    done
    [ -f "$ENV_FILE" ] && say "✓ $ENV_FILE 在位" || say "△ $ENV_FILE 不存在（deploy 时自动生成）"
    if [ -f engine/services/sast-adapter-service/tools/opengrep ]; then
        (cd engine && sha256sum -c services/sast-adapter-service/tools/opengrep.sha256 >/dev/null 2>&1 \
            && say "✓ opengrep 在位（sha256 复核通过）") || { say "✗ opengrep sha256 漂移"; fail=1; }
    else
        say "△ opengrep 缺失（gitignored）——deploy 时将自动从官方 release 拉取（需 GitHub 出口）"
    fi
    check_ports
    # 结构化配置 parse（U6/LESSONS #8）
    python3 deploy/check-yaml-dups.py engine/docker-compose.yml deploy/prod/docker-compose.deploy.yml \
        manager/deploy/docker-compose.yml web/docker-compose.yml openshell-gateway/docker-compose.yml \
        || fail=1
    python3 deploy/check-wiring.py engine || fail=1
    [ "$fail" = "0" ] && say "预检通过" || die "预检未通过（见上）"
}

# 幂等重跑口径：deploy 不做端口检查——本栈旧容器占用的口是"复用"而非"冲突"；
# 真正的异己占用由 compose up 的绑定失败兜底（fail-loud）。独立 check 命令才跑全量端口表。
cmd_check_deploy() {
    local fail=0
    say "== 预检（deploy 口径，不含端口表）=="
    for t in docker bash curl openssl python3; do check_tool "$t" || fail=1; done
    docker compose version >/dev/null 2>&1 || { say "✗ docker compose 插件不可用"; fail=1; }
    for d in engine web manager openshell-gateway dsh-runtime dsh-pentest-sse; do
        [ -e "$d/.git" ] || { say "✗ 子仓缺失: $d/（clone 加 --recurse-submodules 或 make update）"; fail=1; }
    done
    python3 deploy/check-yaml-dups.py engine/docker-compose.yml deploy/prod/docker-compose.deploy.yml \
        manager/deploy/docker-compose.yml web/docker-compose.yml openshell-gateway/docker-compose.yml \
        || fail=1
    [ "$fail" = "0" ] && say "预检通过" || die "预检未通过（见上）"
}

check_tool() { command -v "$1" >/dev/null 2>&1 && { say "✓ $1"; return 0; } || { say "✗ 缺工具: $1"; return 1; } }

# ---- deploy：全量幂等 -------------------------------------------------------

ensure_opengrep() {
    local t="engine/services/sast-adapter-service/tools"
    [ -f "$t/opengrep" ] && { (cd engine && sha256sum -c services/sast-adapter-service/tools/opengrep.sha256 >/dev/null 2>&1) \
        && { say "opengrep: 在位（sha256 OK）"; return 0; } \
        || die "opengrep sha256 漂移：按 $t/PROVENANCE.md 重新 vendor"; }
    say "opengrep 缺失 —— 从官方 release 拉取（v1.29.0 manylinux x86，需 GitHub 出口）..."
    mkdir -p "$t"
    curl -fL --retry 3 --max-time 600 \
        -o "$t/opengrep" \
        "https://github.com/opengrep/opengrep/releases/download/v1.29.0/opengrep_manylinux_x86" \
        || die "opengrep 下载失败（无 GitHub 出口？）。手工步骤见 $t/PROVENANCE.md：宿主机下载 opengrep_manylinux_x86 覆盖 $t/opengrep"
    (cd engine && sha256sum -c services/sast-adapter-service/tools/opengrep.sha256 >/dev/null 2>&1) \
        || die "opengrep sha256 不符（下载不完整或版本漂移），删除 $t/opengrep 后重试或手工 vendor"
    say "opengrep: 已拉取并复核"
}

prepull_images() {
    say "== 上游镜像预拉（多源兜底）=="
    # docker 服务刚起时其网络栈未必就绪（bridge/DNS 初始化），首轮 pull 会
    # 全数快败——等 daemon 真正可用再开拉（dind 实测沉淀）。
    local n=0
    until docker info >/dev/null 2>&1; do
        n=$((n + 1)); [ "$n" -ge 30 ] && die "docker daemon 未就绪（60s）"
        sleep 2
    done
    REMOTE="" bash deploy/pull-images.sh \
"ghcr.io/nvidia/openshell/gateway:latest,ghcr.io/nvidia/openshell/supervisor:latest,python:3.12-slim,postgres:16-alpine,redis:7-alpine,minio/minio:latest,bitnami/kafka:3.7,golang:1.22-alpine,alpine:3.19,node:20-alpine,nginx:1.27-alpine"
}

deploy_gateway() {
    say "== [1/5] openshell-gateway（本机 compose + ensure 自足：JWT 密钥/supervisor 镜像自举）=="
    (cd openshell-gateway && REMOTE="" VMID="" DEPLOY_DIR="$ROOT/openshell-gateway" \
        ./gateway_lifecycle.sh ensure)
}

deploy_manager() {
    say "== [2/5] openshell-manager（装配 staging → 构建+健康门）=="
    # manager 的 compose 构建上下文=「源码+配方同居一目录」的部署布局（与远端
    # DEPLOY_DIR 同构）；仓库检出内 openshell_manager/ 在仓根而 compose 在 deploy/，
    # 直接 up 会 COPY 失败——故 staging 装配（幂等：每次清空重拷）。
    local stage="$ROOT/deploy/.manager-stage"
    rm -rf "$stage"; mkdir -p "$stage"
    cp -R manager/openshell_manager "$stage"/
    mkdir -p "$stage/libs/OpenShell"
    cp -R manager/libs/OpenShell/python "$stage/libs/OpenShell"/
    if [ -f manager/config.json ]; then cp manager/config.json "$stage"/; else
        # config.json 是 gitignored 构建输入（Dockerfile COPY）；缺失时按
        # openshell_manager/config.py 头注的规范最小档生成（运行态由 env 覆盖）
        cat > "$stage/config.json" <<'JSON'
{
  "url": "http://127.0.0.1:18800",
  "bind": "127.0.0.1",
  "port": 18800,
  "tokenFile": ".token",
  "gatewayEndpoint": "gateway.internal:8080",
  "libPath": "libs/OpenShell/python"
}
JSON
        say "manager/config.json 缺失 —— 已生成规范最小档"
    fi
    cp manager/deploy/Dockerfile.manager manager/deploy/docker-compose.yml manager/deploy/env.template "$stage"/
    # compose 插值读取 stage/.env（${OPENSHELL_MANAGER_TOKEN:?} 的满足点）
    grep -E '^OPENSHELL_MANAGER_TOKEN=' "$ENV_FILE" > "$stage/.env"
    chmod 600 "$stage/.env"
    (cd "$stage" && docker compose --project-directory "$stage" -f "$stage/docker-compose.yml" up -d --build)
    wait_http "manager healthz" "http://127.0.0.1:18800/healthz" 180 'ok'
}

deploy_engine() {
    say "== [3/5] codeaudit engine（7 服务+4 中间件构建）=="
    (cd engine && docker compose --env-file "$ROOT/$ENV_FILE" \
        -f docker-compose.yml -f "$ROOT/deploy/prod/docker-compose.deploy.yml" up -d --build)
    say "等待 postgres healthy → 建 3 空库（服务自迁移，不建表）..."
    local deadline=$(( $(date +%s) + 300 ))
    until [ "$(docker inspect -f '{{.State.Health.Status}}' codeaudit-postgres 2>/dev/null)" = "healthy" ]; do
        [ "$(date +%s)" -lt "$deadline" ] || die "postgres 未在 300s 内 healthy（docker logs codeaudit-postgres）"
        sleep 3
    done
    docker exec -i codeaudit-postgres psql -U postgres -tAc \
        "SELECT 'CREATE DATABASE ' || d || ' OWNER postgres;' FROM (VALUES ('codeaudit_project'),('codeaudit_task'),('codeaudit_result')) AS v(d) WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = v.d)" \
        | docker exec -i codeaudit-postgres psql -U postgres >/dev/null
    wait_http "engine gateway" "http://127.0.0.1:${CODEAUDIT_HOST_GATEWAY:-8090}/health" 420 'ok'
}

deploy_sandbox_image() {
    say "== [4/5] dsh-pentest-sse 沙箱镜像（staging 组装→本机 docker build→manager 冒烟）=="
    (cd dsh-pentest-sse && DOCKER_CMD="docker" CONTEXT="$ROOT" \
        MANAGER_ENV="$ROOT/$ENV_FILE" MANAGER_BASE="http://127.0.0.1:18800" \
        IMAGE="${DSH_IMAGE:-dsh-pentest-sse:latest}" ./deploy.sh deploy)
}

deploy_web() {
    say "== [5/5] web console（nginx SPA + /v1 反代）=="
    (cd web && docker compose --project-directory "$ROOT/web" -f "$ROOT/web/docker-compose.yml" \
        --env-file "$ROOT/$ENV_FILE" up -d --build)
    local port="${CODEAUDIT_CONSOLE_PORT:-8088}"
    wait_http "console" "http://127.0.0.1:${port}/" 240 ''
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${port}/v1/projects" || echo 000)
    [ "$code" = "401" ] || die "console /v1 反代异常（HTTP $code，期望 401 透传）"
    say "console /v1 反代 OK（401 透传）"
}

wait_http() {  # wait_http <名称> <url> <timeout_s> <须含子串，空=只看 2xx>
    local name="$1" url="$2" timeout="$3" want="$4" deadline resp
    deadline=$(( $(date +%s) + timeout ))
    say "等待 $name at $url（上限 ${timeout}s）..."
    while [ "$(date +%s)" -lt "$deadline" ]; do
        resp=$(curl -fsS --max-time 5 "$url" 2>/dev/null || true)
        if [ -n "$resp" ] && { [ -z "$want" ] || echo "$resp" | grep -q "$want"; }; then
            say "$name OK"; return 0
        fi
        sleep 3
    done
    die "$name 未在 ${timeout}s 内就绪（compose ps / docker logs 定位）"
}

cmd_deploy() {
    local ev; ev=$(evidence_dir)
    say "全量部署开始（原始输出 → $ev）"
    exec > >(tee -a "$ev") 2>&1
    load_env
    cmd_check_deploy
    ensure_opengrep
    prepull_images
    deploy_gateway
    deploy_manager
    deploy_engine
    deploy_sandbox_image
    deploy_web
    local ip
    ip=$(grep -oE 'http://[0-9.]+:18800' "$ENV_FILE" | head -1 | sed 's|http://||;s|:18800||')
    cat <<EOF

============================================================
部署完成。访问入口：
  控制台   http://${ip:-<宿主IP>}:${CODEAUDIT_CONSOLE_PORT:-8088}   （admin / admin，登录后请立即改密）
  网关 API http://${ip:-<宿主IP>}:${CODEAUDIT_HOST_GATEWAY:-8090}/v1  （JWT Bearer）
  manager  http://${ip:-<宿主IP>}:18800  （Bearer token 见 $ENV_FILE）
运维：bash deploy/production-deploy.sh status|stop|down
说明：AI 全链需在网关注册推理 provider（LLM key，一次性步骤，配法见
      docs/manual-test-guide.md「推理 provider 配置方法」——含智谱 /v1/../
      绕过写法；provider 存网关容器 /var/lib/openshell/gateway.db，清空即丢）；
      参数调整改 $ENV_FILE 后重跑 deploy 即收敛。
============================================================
EOF
}

# ---- status / stop / down ---------------------------------------------------

compose_engine() { docker compose --env-file "$ROOT/$ENV_FILE" -f engine/docker-compose.yml -f deploy/prod/docker-compose.deploy.yml "$@"; }
compose_manager() { docker compose --env-file "$ROOT/$ENV_FILE" --project-directory manager/deploy -f manager/deploy/docker-compose.yml "$@"; }
compose_web()     { docker compose --env-file "$ROOT/$ENV_FILE" --project-directory web -f web/docker-compose.yml "$@"; }
compose_gateway() { docker compose --project-directory openshell-gateway -f openshell-gateway/docker-compose.yml "$@"; }

cmd_status() {
    load_env
    say "== gateway =="; compose_gateway ps 2>/dev/null || true
    say "== manager =="; compose_manager ps 2>/dev/null || true
    say "== engine ==";  compose_engine ps 2>/dev/null || true
    say "== web ==";     compose_web ps 2>/dev/null || true
    curl -fsS --max-time 4 "http://127.0.0.1:${CODEAUDIT_HOST_GATEWAY:-8090}/health" >/dev/null 2>&1 && say "engine gateway: OK" || say "engine gateway: DOWN"
    curl -fsS --max-time 4 "http://127.0.0.1:18800/healthz" >/dev/null 2>&1 && say "manager: OK" || say "manager: DOWN"
    [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 "http://127.0.0.1:${CODEAUDIT_CONSOLE_PORT:-8088}/")" = "200" ] && say "console: OK" || say "console: DOWN"
    docker image inspect "${DSH_IMAGE:-dsh-pentest-sse:latest}" >/dev/null 2>&1 && say "sandbox image: OK" || say "sandbox image: MISSING"
}

cmd_stop() {
    load_env
    compose_web stop; compose_engine stop; compose_manager stop; compose_gateway stop
    say "已停栈（卷与配置保留；down 则释放，down -v 全量重置）"
}

cmd_down() {
    load_env
    local v=""; [ "${2:-}" = "-v" ] && v="-v"
    compose_web down $v; compose_engine down $v; compose_manager down $v; compose_gateway down $v
    say "down ${v:-（卷保留）} 完成"
}

case "$cmd" in
    deploy) cmd_deploy ;;
    check)  load_env; cmd_check ;;
    status) cmd_status ;;
    stop)   cmd_stop ;;
    down)   cmd_down "$@" ;;
    *) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
