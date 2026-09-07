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
#   deploy     默认。交互确认关键参数后幂等全量部署（可重复跑收敛）
#   configure  只做交互式参数确认/生成 production.env，不部署
#   status     各项目容器/健康一览
#   check      部署前置只读预检（不构建不启动）
#   stop       按反拓扑序停栈（保卷保配置）
#   down       compose down（保卷）；down -v 连卷清除（全量重置）
#
# 交互口径：deploy/configure 在终端里运行时会与部署人员确认个性化关键信息
#   （访问入口 IP、端口冲突改配、网段重叠建议），并汇总确认后才开工；
#   --yes（或 PROD_DEPLOY_ASSUME_YES=1）或 stdin 非终端（CI/管道）自动跳过问答按现值执行。
#   LLM provider 不在部署期配置——部署成功后按完成横幅指引自行注册管理。
#
# 参数：deploy/production.env（gitignored；缺失时自动按 production.env.template 生成，
#       密钥随机 + 宿主 IP 自动探测）。改参数后重跑 deploy 即收敛。
#       联动键（manager 地址/网关端点/沙箱拨号/console 反代）每次运行按端口与
#       访问 IP 自动重算回写，手改无效——改 OPENSHELL_PORT 即全链联动。
#
# 前置（check 会逐项核验）：
#   - docker + compose 插件；bash/curl/openssl/python3
#   - 子仓就位（engine/web/manager/openshell-gateway/dsh-runtime/dsh-pentest-sse，
#     clone 须带 --recurse-submodules 或先 make update）
#   - engine/services/sast-adapter-service/tools/opengrep（gitignored 大件；
#     缺失时本脚本按 PROVENANCE.md 的官方 release 自动拉取并 sha256 复核，
#     无 GitHub 出口则给出手工 vendor 指引后终止）
#   - 端口 8080/8081/8090/8088/18800 及中间件口空闲（交互中可改配；18800 本版固定）
#   - 网段 10.10.110.0/24（可 env 改）与 10.10.109.0/24 可用（重叠时交互给建议）
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

PYTHON="$(command -v python3 || command -v python || echo python3)"  # Git Bash 常缺 python3 别名
ENV_FILE="deploy/production.env"
TEMPLATE="deploy/production.env.template"
LOG_TEE=""
ASSUME_YES="${PROD_DEPLOY_ASSUME_YES:-0}"

# ---- 交互底座 ----------------------------------------------------------------

interactive() { [ "${ASSUME_YES}" = "1" ] && return 1; [ -t 0 ]; }

ask() {  # ask <提示> -> 经 stdout 打印（随 tee 落证据），stdin 读答案
    printf "[prod-deploy] ? %s" "$1"
}

is_wsl() { grep -qi microsoft /proc/version 2>/dev/null; }
is_msys() { uname -s 2>/dev/null | grep -qiE 'MINGW|MSYS'; }  # Git Bash 壳

ms_ips() {  # Windows ipconfig 解析（Git Bash 无 ip/hostname -I 时的宿主地址面）
    ipconfig 2>/dev/null | grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' \
        | grep -vE '^(127\.|255\.|0\.|169\.254\.)' | sort -u
}

access_ip() {  # 访问面缺省地址——仅显示用（横幅/汇总），不参与任何接线；WSL2 下 Windows 侧一律 localhost
    if is_wsl; then echo "localhost"; return 0; fi
    local ip
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oE 'src [0-9.]+' | awk '{print $2}' | head -1)
    [ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}')
    if [ -z "$ip" ] && command -v ipconfig >/dev/null 2>&1; then
        ip=$(ms_ips | head -1)
    fi
    echo "${ip:-}"
}

host_ip_candidates() {  # 宿主全局地址（剔除 docker 网桥/veth 类），每行一个
    if command -v ip >/dev/null 2>&1; then
        ip -4 -o addr show scope global 2>/dev/null \
            | awk '$2 !~ /^(docker|br-|veth|virbr|lo|tun|tap)/ {split($4,a,"/"); print a[1]}' \
            | sort -u
    else
        ms_ips   # Git Bash 壳：ipconfig 面（含虚拟网卡，交互时可人工甄别）
    fi
}

port_listening() {  # ss(Git Bash 无) → netstat 回退；都无=按空闲处理（compose 绑定失败 fail-loud 兜底）
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE ":$1\$"
    elif command -v netstat >/dev/null 2>&1; then
        netstat -an 2>/dev/null | grep -Eq "[:.]$1[[:space:]]+.*[[:space:]](LISTEN|LISTENING)"
    else
        return 1
    fi
}

port_is_ours() {  # 端口是否由本栈既有容器发布（复用而非冲突）
    docker ps --format '{{.Ports}}' 2>/dev/null | grep -qE ":$1->"
}

pick_free_port() {  # pick_free_port <被替换端口> —— 从 +1 起找空闲口，避开本栈计划口
    local repl="$1" p="$1" spec key rest def planned=""
    for spec in "${PORT_KEYS[@]}"; do
        key="${spec%%:*}"; rest="${spec#*:}"; def="${rest%%:*}"
        planned="$planned|${!key:-$def}"
    done
    planned="${planned}|18800"
    while :; do
        p=$((p + 1))
        port_listening "$p" && continue
        case "|$planned|" in *"|$p|"*) ;; *) echo "$p"; return 0 ;; esac
    done
}

env_access_ip() {  # 已确认的访问入口地址（仅显示用）；未确认过则回落探测
    local ip
    ip=$(grep -E '^CODEAUDIT_ACCESS_IP=' "$ENV_FILE" 2>/dev/null | head -1 | cut -d= -f2-)
    [ -n "$ip" ] && { echo "$ip"; return 0; }
    access_ip
}

fp() { printf '%s' "$1" | sha256sum | cut -c1-8; }  # 密钥指纹（不打明文）

set_env_kv() {  # set_env_kv <KEY> <value> —— 原地更新 $ENV_FILE（cat> 保 inode/权限 600）
    local key="$1" val="$2" tmp
    tmp=$(mktemp)
    if grep -qE "^${key}=" "$ENV_FILE"; then
        sed "s|^${key}=.*|${key}=${val}|" "$ENV_FILE" > "$tmp"
    else
        cp "$ENV_FILE" "$tmp"
        printf '%s=%s\n' "$key" "$val" >> "$tmp"
    fi
    cat "$tmp" > "$ENV_FILE"
    rm -f "$tmp"
}

# 可交互改配的宿主端口：KEY:缺省:说明（18800 固定，不在此列）
PORT_KEYS=(
    "OPENSHELL_PORT:8080:openshell-gateway gRPC"
    "OPENSHELL_HEALTH_PORT:8081:gateway health"
    "CODEAUDIT_HOST_GATEWAY:8090:引擎网关 API"
    "CODEAUDIT_CONSOLE_PORT:8088:web 控制台"
    "CODEAUDIT_HOST_PG:5432:PostgreSQL"
    "CODEAUDIT_HOST_REDIS:6379:Redis"
    "CODEAUDIT_HOST_MINIO_API:9000:MinIO API"
    "CODEAUDIT_HOST_MINIO_CONSOLE:9001:MinIO 控制台"
    "CODEAUDIT_HOST_KAFKA:9092:Kafka"
)

source_env() { set -a; # shellcheck disable=SC1090
    . "$ENV_FILE"; set +a; }

converge_env() {  # 联动键按「两个网关发布口」重算回写（模板已注明手改无效）：
    #   manager→网关端点与 dsh-runtime 沙箱拨号随 OPENSHELL_PORT；
    #   console /v1 反代随引擎网关发布口（dind 实测：指错到 openshell 网关 8080 会 404）。
    #   manager 是内部面非交互面：OPENSHELL_MANAGER_URL 恒为内部常量（经 hosts 别名解析，
    #   零 DNS、零用户输入）——用户确认的访问 IP 只存 CODEAUDIT_ACCESS_IP 供横幅/汇总
    #   显示，不参与任何内部接线（2026-09-08，原"manager 地址随访问 IP"的耦合已拆除）。
    local gw eng
    gw="${OPENSHELL_PORT:-8080}"
    eng="${CODEAUDIT_HOST_GATEWAY:-8090}"
    set_env_kv "OPENSHELL_MANAGER_URL" "host.docker.internal:18800"
    set_env_kv "OPENSHELL_GATEWAY_ENDPOINT" "host.docker.internal:${gw}"
    set_env_kv "CODEAUDIT_GATEWAY_DIAL_ADDR" "host.docker.internal:${gw}"
    set_env_kv "CODEAUDIT_GATEWAY_UPSTREAM" "host.docker.internal:${eng}"
    source_env
}

print_summary() {
    local ip
    ip=$(env_access_ip)
    say "──────── 部署参数确认 ────────"
    say "访问入口(显示) : ${ip:-<自动>} ——仅用于下方 URL 呈现，内部接线不依赖此值"
    say "控制台         : http://${ip:-<IP>}:${CODEAUDIT_CONSOLE_PORT:-8088}  (初始账号 admin/admin，登录后请立即改密)"
    say "网关 API       : http://${ip:-<IP>}:${CODEAUDIT_HOST_GATEWAY:-8090}/v1  (JWT Bearer)"
    say "manager(内部面) : host.docker.internal:18800（容器互访；宿主机排障经 http://127.0.0.1:18800；token 指纹 ${OPENSHELL_MANAGER_TOKEN:+$(fp "$OPENSHELL_MANAGER_TOKEN")})"
    say "openshell 网关 : 发布 ${OPENSHELL_PORT:-8080}(gRPC) / ${OPENSHELL_HEALTH_PORT:-8081}(health)    沙箱路由域: ${ROUTING_DOMAIN:-sandbox.codeaudit.internal}(纯路由键,不解析)"
    say "中间件宿主口   : PG ${CODEAUDIT_HOST_PG:-5432} / Redis ${CODEAUDIT_HOST_REDIS:-6379} / MinIO ${CODEAUDIT_HOST_MINIO_API:-9000},${CODEAUDIT_HOST_MINIO_CONSOLE:-9001} / Kafka ${CODEAUDIT_HOST_KAFKA:-9092}"
    say "Kafka 广播     : ${CODEAUDIT_KAFKA_ADVERTISED:-kafka}    引擎网段: ${CODEAUDIT_ENGINE_SUBNET:-10.10.110.0/24}"
    say "沙箱镜像       : ${DSH_IMAGE:-dsh-pentest-sse:latest}    JWT 指纹: ${CODEAUDIT_JWT_SECRET:+$(fp "$CODEAUDIT_JWT_SECRET")}"
    say "LLM provider   : 本次不配置——部署成功后按完成横幅指引自行注册管理"
    is_wsl && say "WSL2 提示      : Windows 本机浏览器经 http://localhost:<端口> 访问；局域网设备需 portproxy/镜像网络（deploy/windows/）"
    say "参数落盘       : $ENV_FILE（改后重跑 deploy 即收敛；联动键自动重算）"
    say "──────────────────────────────"
}

# ---- 交互确认（deploy/configure 共用；任何一步改动都落盘 production.env）------

interact_config() {
    local choice ip cands n spec key rest def cur desc ans newp used cfg_prefix sugg
    # 1) 访问入口地址——仅显示用（完成横幅/汇总里的 console 与网关 URL）；
    #    内部接线全部走 host.docker.internal 别名/服务名/固定常量，与此值无关
    ip=$(env_access_ip)
    cands=$(host_ip_candidates)
    n=$(printf '%s\n' "$cands" | grep -c . || true)
    if [ "$n" -gt 1 ]; then
        say "检测到多个宿主地址（已剔除 docker 网桥）："
        printf '%s\n' "$cands" | nl -ba | sed 's/^/    /'
        ask "选择访问入口地址（序号/直接输 IP，回车=默认 ${ip}）: "
        IFS= read -r choice
        case "$choice" in
            '') ;;
            *[!0-9]*) ip="$choice" ;;
            *) ip=$(printf '%s\n' "$cands" | sed -n "${choice}p"); [ -z "$ip" ] && ip=$(access_ip) ;;
        esac
    else
        ask "访问入口地址用 ${ip:-<探测失败，请手工输入>}？(回车=确认 / 输入其它 IP): "
        IFS= read -r choice
        [ -n "$choice" ] && ip="$choice"
    fi
    [ -n "$ip" ] && set_env_kv "CODEAUDIT_ACCESS_IP" "$ip"
    # 2) 沙箱服务路由域——纯字符串路由键（全程无 DNS 解析，零 DNS 依赖不变量），
    #    决定沙箱服务 URL 形态（default--<沙箱>--<服务>.<域>）与网关证书 SAN；
    #    gateway server_sans 与 sse 冒烟断言经本键同源联动
    ask "沙箱服务路由域（回车=缺省 ${ROUTING_DOMAIN:-sandbox.codeaudit.internal} / 输入自定义域名）: "
    IFS= read -r choice
    [ -n "$choice" ] && set_env_kv "ROUTING_DOMAIN" "$choice"
    # 3) 端口冲突：异己占用才问（本栈旧容器占用=复用）；18800 固定口单列
    for spec in "${PORT_KEYS[@]}"; do
        key="${spec%%:*}"; rest="${spec#*:}"; def="${rest%%:*}"; desc="${rest#*:}"
        cur="${!key:-$def}"
        port_listening "$cur" || continue
        if port_is_ours "$cur"; then
            say "△ 端口 ${cur}(${desc}) 由本栈既有容器占用 —— 沿用复用"
            continue
        fi
        newp=$(pick_free_port "$cur")
        ask "端口 ${cur}(${desc}) 已被其他程序监听：回车=改用 ${newp} / 输入其它端口 / s=停止部署: "
        IFS= read -r ans
        case "$ans" in
            s|S) return 1 ;;
            '') set_env_kv "$key" "$newp"; say "  ${key} → ${newp}" ;;
            *)  set_env_kv "$key" "$ans";  say "  ${key} → ${ans}" ;;
        esac
    done
    if port_listening 18800 && ! port_is_ours 18800; then
        ask "manager 固定发布 18800 且被其他程序占用：回车=停止部署(去腾口) / c=强行继续(将失败): "
        IFS= read -r ans
        case "$ans" in c|C) say "  继续部署（18800 冲突将在 manager 健康门失败）" ;; *) return 1 ;; esac
    fi
    # 4) 引擎网段与宿主/常驻网段重叠检测（重叠 compose 建网会失败；给跳位建议）
    cfg_prefix="${CODEAUDIT_ENGINE_SUBNET:-10.10.110.0/24}"; cfg_prefix="${cfg_prefix%%/*}"; cfg_prefix="${cfg_prefix%.*}"
    used="|$(host_ip_candidates | sed 's/\.[0-9]*$//' | tr '\n' '|')"
    for ans in 0 1 2 3 4 5 38 39 40 41 42 43 44 105 106 107 108 109 110 210; do used="${used}10.10.${ans}|"; done
    case "$used" in
        *"|$cfg_prefix|"*)
            sugg=""
            for ans in $(seq 111 254) $(seq 6 37) $(seq 45 104); do
                case "$used" in *"|10.10.${ans}|"*) ;; *) sugg="10.10.${ans}.0/24"; break ;; esac
            done
            if interactive && [ -n "$sugg" ]; then
                ask "引擎网段 ${CODEAUDIT_ENGINE_SUBNET} 与宿主/常驻网段重叠：回车=改用 ${sugg} / k=坚持现值: "
                IFS= read -r ans
                case "$ans" in
                    k|K) say "  保留 ${CODEAUDIT_ENGINE_SUBNET}（若 compose 建网失败请改 CODEAUDIT_ENGINE_SUBNET）" ;;
                    '')  set_env_kv "CODEAUDIT_ENGINE_SUBNET" "$sugg"; say "  CODEAUDIT_ENGINE_SUBNET → ${sugg}" ;;
                esac
            else
                say "△ 引擎网段 ${CODEAUDIT_ENGINE_SUBNET:-10.10.110.0/24} 与宿主/常驻网段重叠 —— 非交互模式不改动，deploy 可能建网失败"
            fi
            ;;
    esac
    return 0
}

# ---- 输出：全部落盘 .agent/evidence/（U8：结论只认命令原始输出）------------
evidence_dir() { mkdir -p .agent/evidence; echo ".agent/evidence/prod-deploy-$(date +%Y%m%d-%H%M%S).log"; }

say() { echo "[prod-deploy] $*"; }
die() { echo "[prod-deploy] ERROR: $*" >&2; exit 1; }

# ---- 子命令 ----------------------------------------------------------------

cmd="${1:-deploy}"
[ $# -gt 0 ] && shift
for _a in "$@"; do  # -y/--yes（或 PROD_DEPLOY_ASSUME_YES=1）跳过交互问答
    case "$_a" in -y|--yes) ASSUME_YES=1 ;; esac
done

# ---- 公共：读 env（缺则生成）------------------------------------------------

load_env() {
    if [ ! -f "$ENV_FILE" ]; then
        say "生成 $ENV_FILE（按 $TEMPLATE，密钥随机 + 宿主 IP 自动探测）..."
        local ip jwt tok
        ip=$(access_ip)
        [ -n "$ip" ] || die "无法探测宿主 IP（ip/hostname 均失败），请手工创建 $ENV_FILE"
        jwt=$(openssl rand -hex 24) || die "openssl 不可用"
        tok=$(openssl rand -hex 24)
        sed -e "s|<auto: openssl rand -hex 24>|$jwt|" \
            -e "s|<auto: openssl rand -hex 24，manager 与 engine 同值由本文件保证>|$tok|" \
            -e "s|<auto: <宿主IP>>|${ip}|" \
            "$TEMPLATE" > "$ENV_FILE"
        chmod 600 "$ENV_FILE"
        say "已生成（密钥已随机，宿主 IP=${ip}）；可编辑后重跑。"
    fi
    source_env
}

# ---- check：只读预检 --------------------------------------------------------

check_ports() {
    local port occupied=""
    for port in "${OPENSHELL_PORT:-8080}" "${OPENSHELL_HEALTH_PORT:-8081}" \
                "${CODEAUDIT_HOST_GATEWAY:-8090}" "${CODEAUDIT_CONSOLE_PORT:-8088}" 18800 \
                "${CODEAUDIT_HOST_PG:-5432}" "${CODEAUDIT_HOST_REDIS:-6379}" \
                "${CODEAUDIT_HOST_MINIO_API:-9000}" "${CODEAUDIT_HOST_KAFKA:-9092}"; do
        if port_listening "$port"; then
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
    if bash dsh-pentest-sse/sandbox-artifacts/fetch.sh --verify >/dev/null 2>&1 \
        && bash dsh-pentest-sse/sandbox-artifacts/fetch-agent-tools.sh --verify >/dev/null 2>&1; then
        say "✓ 沙箱构建素材在位（sha 复核通过）"
    else
        say "△ 沙箱构建素材缺失/漂移（gitignored）——deploy 时自动拉取（需网络出口）"
    fi
    check_ports
    # 结构化配置 parse（U6/LESSONS #8）
    ensure_pyyaml
    "$PYTHON" deploy/check-yaml-dups.py engine/docker-compose.yml deploy/prod/docker-compose.deploy.yml \
        manager/deploy/docker-compose.yml web/docker-compose.yml openshell-gateway/docker-compose.yml \
        || fail=1
    "$PYTHON" deploy/check-wiring.py engine || fail=1
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
    ensure_pyyaml
    "$PYTHON" deploy/check-yaml-dups.py engine/docker-compose.yml deploy/prod/docker-compose.deploy.yml \
        manager/deploy/docker-compose.yml web/docker-compose.yml openshell-gateway/docker-compose.yml \
        || fail=1
    [ "$fail" = "0" ] && say "预检通过" || die "预检未通过（见上）"
}

check_tool() { command -v "$1" >/dev/null 2>&1 && { say "✓ $1"; return 0; } || { say "✗ 缺工具: $1"; return 1; } }

ensure_pyyaml() {  # 配置审计两脚本依赖 PyYAML；全新机器常缺——pip 用户级自装，失败给发行版指引
    "$PYTHON" -c 'import yaml' >/dev/null 2>&1 && return 0
    say "△ Python 缺 PyYAML —— 尝试用户级 pip 安装..."
    "$PYTHON" -m pip install -q --user pyyaml >/dev/null 2>&1 \
        || "$PYTHON" -m pip install -q --user --break-system-packages pyyaml >/dev/null 2>&1 || true
    if "$PYTHON" -c 'import yaml' >/dev/null 2>&1; then
        say "✓ PyYAML 就绪（用户级）"; return 0
    fi
    die "缺 PyYAML：Debian/Ubuntu=apt install python3-yaml；Alpine=apk add py3-yaml；或 pip3 install --user pyyaml 后重试"
}

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

ensure_sandbox_artifacts() {  # 沙箱镜像构建素材（pdtools/nuclei-templates/agent-tools，gitignored）：
    # 先 --verify 离线复核，在位即零下载（sbom sha256 逐项）；缺失/漂移才全量拉取
    for f in fetch.sh fetch-agent-tools.sh; do
        if bash "dsh-pentest-sse/sandbox-artifacts/$f" --verify >/dev/null 2>&1; then
            say "$f: 素材在位（sha 复核通过，零下载）"
        else
            say "$f: 素材缺失/漂移 —— 全量拉取（需网络出口；版本/sha 事实源=仓内 sbom）..."
            bash "dsh-pentest-sse/sandbox-artifacts/$f" \
                || die "$f 素材拉取失败（离线主机按 dsh-pentest-sse/sandbox-artifacts/README 手工补件）"
        fi
    done
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
        ROUTING_DOMAIN="${ROUTING_DOMAIN:-sandbox.codeaudit.internal}" \
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
        # openshell_manager/config.py 头注的规范最小档生成（运行态由 env 覆盖：
        # stage/.env 会带 OPENSHELL_GATEWAY_ENDPOINT=host.docker.internal:<OPENSHELL_PORT>）
        cat > "$stage/config.json" <<'JSON'
{
  "url": "http://127.0.0.1:18800",
  "bind": "127.0.0.1",
  "port": 18800,
  "tokenFile": ".token",
  "gatewayEndpoint": "host.docker.internal:8080",
  "libPath": "libs/OpenShell/python"
}
JSON
        say "manager/config.json 缺失 —— 已生成规范最小档"
    fi
    cp manager/deploy/Dockerfile.manager manager/deploy/docker-compose.yml manager/deploy/env.template "$stage"/
    # compose 插值读取 stage/.env（${OPENSHELL_MANAGER_TOKEN:?} 的满足点 + 网关端点联动值）
    grep -E '^(OPENSHELL_MANAGER_TOKEN|OPENSHELL_GATEWAY_ENDPOINT)=' "$ENV_FILE" > "$stage/.env"
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
        EXPOSE_DOMAIN="${ROUTING_DOMAIN:-sandbox.codeaudit.internal}" \
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
    converge_env
    if interactive; then
        interact_config || die "已按部署人员要求停止"
        converge_env   # 吸收问答改动（端口/IP 变了，联动键跟随）
    fi
    print_summary
    if interactive; then
        ask "按以上参数开始部署？(回车=开始 / n=中止): "
        local _confirm; IFS= read -r _confirm
        case "$_confirm" in n*|N*) die "用户中止" ;; esac
    fi
    cmd_check_deploy
    ensure_opengrep
    ensure_sandbox_artifacts
    prepull_images
    deploy_gateway
    deploy_manager
    deploy_engine
    deploy_sandbox_image
    deploy_web
    local ip
    ip=$(env_access_ip)
    cat <<EOF

============================================================
部署完成。访问入口（IP+端口直访，无需任何 DNS）：
  控制台   http://${ip:-<宿主IP>}:${CODEAUDIT_CONSOLE_PORT:-8088}   （admin / admin，登录后请立即改密）
  网关 API http://${ip:-<宿主IP>}:${CODEAUDIT_HOST_GATEWAY:-8090}/v1  （JWT Bearer）
  manager（内部面，无需对用户暴露）容器互访 host.docker.internal:18800；宿主机排障 http://127.0.0.1:18800（Bearer token 见 $ENV_FILE）
运维：bash deploy/production-deploy.sh status|stop|down
      （WSL2 部署时 Windows 本机用 localhost 访问；局域网设备经 deploy/windows/expose-lan.ps1）
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

cmd_configure() {  # 只确认参数不部署：生成/核对 production.env 后退出
    local ev; ev=$(evidence_dir)
    exec > >(tee -a "$ev") 2>&1
    load_env
    converge_env
    if interactive; then
        interact_config || die "已按部署人员要求停止"
        converge_env
    fi
    print_summary
    say "参数已确认并落盘 $ENV_FILE；执行 bash deploy/production-deploy.sh deploy 开始部署。"
}

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
    deploy)    cmd_deploy ;;
    configure) cmd_configure ;;
    check)  load_env; cmd_check ;;
    status) cmd_status ;;
    stop)   cmd_stop ;;
    down)   cmd_down "$@" ;;
    *) sed -n '2,44p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
