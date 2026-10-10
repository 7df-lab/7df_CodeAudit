#!/usr/bin/env bash
# openshell-gateway lifecycle (deploy-side, no HTTP surface).
#
# Lives in the umbrella submodule openshell-gateway/, which is the
# SOURCE OF TRUTH for the gateway deployment files. This script operates on
# the runtime copy inside the target 部署主机; run ./deploy.sh first to sync the
# CD copies out. The manager service (openshell_manager) stays a pure
# transport pipe — it holds no lifecycle logic and exposes none over HTTP.
#
# What this script does: edits the gateway's TOML config inside the target
# 部署主机 (enforcing the service routing domain) and drives the gateway's
# docker-compose service through start/stop/restart.
#
# Current deployment :
#     /opt/codeaudit/openshell-gateway/{docker-compose.yml,gateway.toml}
#     （<host-id> 生产部署树，prod-sync 管理；自运行容器 compose working_dir label 实证）
#     container: openshell-gateway-gateway-1  (image ghcr.io/nvidia/openshell/gateway)
#     ports: 8080 gRPC, 8081 health  -> published on the 部署主机
#
# Usage:
#   gateway_lifecycle.sh ensure     # enforce routing domain, restart if changed, verify
#   gateway_lifecycle.sh verify     # check config + liveness only (no changes)
#   gateway_lifecycle.sh status     # container state + configured routing domain
#   gateway_lifecycle.sh start|stop|restart
#   gateway_lifecycle.sh recreate   # compose up -d — applies compose file changes
#   gateway_lifecycle.sh image       # pinned digest ↔ running container reconcile (read-only)
#   gateway_lifecycle.sh logs [N]   # last N gateway log lines (default 50)
#
# Environment:
#   REMOTE           command prefix reaching the 部署主机   (default: "ssh <deploy-host> --")
#   DEPLOY_DIR       compose dir inside the 部署主机        (default: /opt/codeaudit/openshell-gateway)
#   SERVICE          compose service name              (default: gateway)
#   LIVENESS_HOST/_PORT  TCP liveness probe target     (default: 127.0.0.1 / 8080)
#   LIVENESS_TIMEOUT_SECS                                (default: 60)
#   ROUTING_DOMAIN   enforced service routing domain   (default: sandbox.codeaudit.internal)
#
# WARNING on `recreate`: the compose file currently sets `command: []` while the
# running container still carries the image default CMD (--bind-address 0.0.0.0
# --port 8080). `up -d` applies the file, so TOML's bind_address (127.0.0.1:8080)
# takes over — verify external reachability (published ports) afterwards. The
# daily operations (`ensure`/`restart`) intentionally use `compose restart`,
# which keeps the running container spec and only re-reads the TOML.
set -euo pipefail

# `-` 而非 `:-`：REMOTE=""（空串）是显式的"本机执行"契约（主仓库生产态部署链
# production-deploy.sh 依赖），不得在空串时触发 远程执行 缺省。
REMOTE="${REMOTE-ssh <deploy-host> --}"
# 2026-09-18 修订：缺省自 os-deploy 死路径
# 改钉部署树现役目录（<host-id> compose working_dir 实证；<legacy-path> 在 <host-id> 已整个不存在）。
DEPLOY_DIR="${DEPLOY_DIR:-/opt/codeaudit/openshell-gateway}"
SERVICE="${SERVICE:-gateway}"
ROUTING_DOMAIN="${ROUTING_DOMAIN:-sandbox.codeaudit.internal}"
LIVENESS_HOST="${LIVENESS_HOST:-127.0.0.1}"
LIVENESS_PORT="${LIVENESS_PORT:-8080}"
LIVENESS_TIMEOUT_SECS="${LIVENESS_TIMEOUT_SECS:-60}"
# JWT 签名密钥目录（gateway.toml gateway_jwt 段指向的同一路径）；镜像与
# compose 保持同源（GATEWAY_IMAGE 变更时两处同步，T-D1 守卫）。
JWT_DIR="${JWT_DIR:-/var/lib/openshell/tls/jwt}"
# digest 钉扎（R9，LESSONS #18）：缺省锚定 2026-09-17 现网 digest（自 <host-id> 运行
# 容器 openshell-gateway-gateway-1 的镜像 RepoDigests 实取）；升级=人类裁决后
# 与 compose 缺省两处同改。
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/nvidia/openshell/gateway@sha256:05cf77bbb022a739aed6f22daa0e7e164415f4ab273b5f84319e46d91eb8f645}"

# intentional word splitting: REMOTE is a command prefix ("ssh <deploy-host> --")
run_remote() { $REMOTE "$@"; }
compose() { run_remote docker compose --project-directory "$DEPLOY_DIR" \
    -f "$DEPLOY_DIR/docker-compose.yml" "$@"; }
toml_path() { echo "$DEPLOY_DIR/gateway.toml"; }
enforced_value() { echo "[\"*.$ROUTING_DOMAIN\"]"; }

die() { echo "ERROR: $*" >&2; exit 1; }

# -- config: routing domain ------------------------------------------------

configured_server_sans() {
    # stdout: the TOML value of the first `server_sans = ...` line, or "" when
    # absent (gateway then falls back to the default openshell.localhost).
    # 首行用 `sed -n '1p'` 而非 `head -1`（B1-4 审计加固）：head 命中即退出，
    # 双行场景上游 sed 收 SIGPIPE（pipefail 下 141），sed -n '1p' 读全输入无此险。
    run_remote sed -n -E \
        's/^server_sans[[:space:]]*=[[:space:]]*(.+)$/\1/p' \
        "$(toml_path)" | sed -n '1p' | tr -d '\r'
}

patch_server_sans() {
    # Idempotently pin `server_sans = ["*.<ROUTING_DOMAIN>]` under
    # [openshell.gateway]. Runs remotely so the edit and the file never leave
    # the 部署主机. Timestamped backup written next to the file.
    run_remote bash -s "$ROUTING_DOMAIN" "$(toml_path)" <<'PATCH'
set -euo pipefail
domain="$1"; toml="$2"
line="server_sans = [\"*.$domain\"]"
# sed 替换文本转义（§14 加固）：$line 要内插进 s||| 替换文本与 a 追加文本——
# 域名含 | \ & / 时未转义会破坏 sed 脚本（| 提前闭段/& 展开全匹配/\ 吞字符），
# 损坏 gateway.toml。esc_sed 后拼，写盘的是字面域。
esc_sed() { printf '%s' "$1" | sed -e 's/[\\|&/]/\\&/g'; }
esc_line="$(esc_sed "$line")"
[ -f "$toml" ] || { echo "missing $toml" >&2; exit 1; }
backup="$toml.bak.$(date +%Y%m%d%H%M%S)"
# 备份必须先于改写：cp 放在 sed/awk 之后备份的
# 是改写后的新文件，.bak 的回滚语义尽失。失败不阻断钉域（|| true），但告警
# 到 stderr，不得静默。
cp "$toml" "$backup" 2>/dev/null \
    || echo "WARNING: backup cp failed: $toml -> $backup" >&2
if grep -Eq '^server_sans[[:space:]]*=' "$toml"; then
    sed -i -E "s|^server_sans[[:space:]]*=.*|$esc_line|" "$toml"
    echo "rewrote server_sans -> $line"
elif grep -q '^\[openshell\.gateway\]' "$toml"; then
    sed -i "/^\[openshell\.gateway\]/a $esc_line" "$toml"
    echo "inserted $line under [openshell.gateway]"
else
    echo "no [openshell.gateway] table in $toml" >&2; exit 1
fi
# keep exactly one server_sans line
awk '/^server_sans[[:space:]]*=/{n++; if (n > 1) next} {print}' \
    "$toml" > "$toml.tmp" && mv "$toml.tmp" "$toml"
echo "backup: $backup"
PATCH
}

# -- liveness ----------------------------------------------------------------

tcp_ok() {
    # timeout 5（B1-4 审计加固）：/dev/tcp 无内建超时，对 DROP/半开目标会挂死
    # 到 LIVENESS_TIMEOUT_SECS 整窗，拖垮 wait_liveness 轮询与 status/verify。
    # 单次探测上限 5s；远程执行 路径下 timeout 在 部署主机 内执行（coreutils 标配）。
    # 位置参数传递（§14 加固）：host/port 经 "$1"/"$2" 进脚本文本零插值——
    # 含空格/元字符的值只作为独立 argv，不构成 probe shell 注入面。
    run_remote timeout 5 bash -c \
        'exec 3<>/dev/tcp/$1/$2' _ "$LIVENESS_HOST" "$LIVENESS_PORT" \
        >/dev/null 2>&1
}

wait_liveness() {
    local deadline=$(( $(date +%s) + LIVENESS_TIMEOUT_SECS ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if tcp_ok; then echo "gateway liveness OK ($LIVENESS_HOST:$LIVENESS_PORT)"; return 0; fi
        sleep 2
    done
    die "gateway not accepting connections on $LIVENESS_HOST:$LIVENESS_PORT within ${LIVENESS_TIMEOUT_SECS}s; check: $0 logs"
}

# -- subcommands ---------------------------------------------------------------

cmd_status() {
    echo "== compose service =="
    compose ps "$SERVICE" || true
    echo "== routing domain =="
    local sans; sans=$(configured_server_sans)
    if [ -n "$sans" ]; then
        echo "server_sans = $sans"
    else
        echo "(server_sans unset -> gateway default: openshell.localhost)"
    fi
    echo "== liveness =="
    if tcp_ok; then echo "OK ($LIVENESS_HOST:$LIVENESS_PORT)"; else echo "DOWN"; exit 1; fi
}

cmd_verify() {
    local sans; sans=$(configured_server_sans)
    [ "$sans" = "$(enforced_value)" ] || die "server_sans is '${sans:-<unset>}', expected '$(enforced_value)'"
    tcp_ok || die "gateway not reachable on $LIVENESS_HOST:$LIVENESS_PORT"
    echo "verify OK: server_sans=$sans, liveness $LIVENESS_HOST:$LIVENESS_PORT"
}

ensure_jwt_keys() {
    # 全新宿主（首次部署/全量清理后）没有 JWT 签名密钥，网关启动即崩
    # （签名密钥不会自动生成，上游口径=一次性 generate-certs 预置）。
    # 与 compose 同款 bind（同路径宿主目录），产物正好落在网关读取的位置。
    # 存在性检查经同款挂载的 busybox test：宿主侧
    # test -f 在 Docker Desktop（Windows/macOS）下永远看不到 VM 内的 bind 目标
    # → 每轮 ensure 误判无钥重复 generate（既有 JWT 全部作废）；网关镜像自身是
    # distroless（无 test/ls，实测 exec not found），检查载体用 alpine:3.19
    # （部署链 pull-images 预拉清单必在位；Linux 宿主 bind 同一目录语义不变）。
    jwt_parent="$(dirname "$JWT_DIR")"
    # MSYS_NO_PATHCONV：Git Bash 会把 -v 的 VM 绝对
    # 路径（/var/lib/...）改写为宿主 Git 安装根下的伪路径；compose 侧同路径不走
    # argv 转换（Docker Desktop 解析为 VM 文件系统）——两侧不一致会让密钥写错位、
    # 网关无钥 crash loop。显式关掉转换保持与 compose 同语义；非 MSYS 宿主零影响。
    if MSYS_NO_PATHCONV=1 run_remote docker run --rm \
            -v "$jwt_parent":"$jwt_parent" \
            alpine:3.19 test -f "$JWT_DIR/signing.pem" >/dev/null 2>&1; then
        return 0
    fi
    echo "JWT signing keys absent at $JWT_DIR — one-shot generate-certs ..."
    # B2-3：mount/output 由 $JWT_DIR 推导——原硬编码 /var/lib/openshell，
    # JWT_DIR 覆盖后密钥落不到检查路径 → 每轮 ensure 重复 generate 且网关依旧无钥。
    # B2-3c：generate-certs 把 --output-dir 当 PKI 根、
    # 在其下自建 jwt/server/client + ca.*（产物=根/jwt/signing.pem）——B2-3 传 JWT_DIR
    # 本身曾嵌套成 jwt/jwt/signing.pem，检查与网关读的都是 JWT_DIR/signing.pem → 全新
    # 宿主网关 crash loop。<host-id> 现役布局（tls/{ca.*,client,jwt,server}）即 PKI 根=
    # dirname(JWT_DIR) 的实证；B2-3 的"随 JWT_DIR 推导"意图保留，只是根=父目录。
    MSYS_NO_PATHCONV=1 run_remote docker run --rm --user 0 \
        -v "$jwt_parent":"$jwt_parent" \
        "$GATEWAY_IMAGE" generate-certs \
        --output-dir "$jwt_parent" \
        --server-san host.openshell.internal
    # 2026-10-09 122 实录：半套 PKI（public.pem 在而 signing.pem 缺）时 generate-certs 只报
    # "already exist, skipping" 并非零退、不补私钥——整目录轮换（旧套留档 .stale-<ts>，挂 PARENT
    # 移子目录避开挂载根不可移）后重生成完整套。JWT 随之轮换=全新部署可接受口径。
    if ! MSYS_NO_PATHCONV=1 run_remote docker run --rm \
            -v "$jwt_parent":"$jwt_parent" alpine:3.19 test -f "$JWT_DIR/signing.pem" >/dev/null 2>&1; then
        echo "  半套 PKI（signing.pem 缺）——轮换 ${jwt_parent} 后重生成（旧套 .stale 留档）"
        local base; base="$(dirname "$jwt_parent")"
        MSYS_NO_PATHCONV=1 run_remote docker run --rm --user 0 -v "$base":"$base" alpine:3.19 \
            sh -c "mv '$jwt_parent' '${jwt_parent}.stale-'$(date +%s) 2>/dev/null; mkdir -p '$jwt_parent'" \
            || die "PKI 轮换失败（$jwt_parent）"
        MSYS_NO_PATHCONV=1 run_remote docker run --rm --user 0 \
            -v "$jwt_parent":"$jwt_parent" \
            "$GATEWAY_IMAGE" generate-certs \
            --output-dir "$jwt_parent" \
            --server-san host.openshell.internal \
            || die "PKI 轮换后 generate-certs 仍失败"
        MSYS_NO_PATHCONV=1 run_remote docker run --rm \
            -v "$jwt_parent":"$jwt_parent" alpine:3.19 test -f "$JWT_DIR/signing.pem" >/dev/null 2>&1 \
            || die "PKI 轮换后 signing.pem 仍缺失"
        echo "  PKI 轮换完成（旧套见 ${jwt_parent}.stale-*）"
    fi
}

configured_supervisor_image() {
    # stdout: gateway.toml 的 supervisor_image 值（空=用镜像默认）
    # 同 configured_server_sans：`sed -n '1p'` 取首行，避免 head 的 SIGPIPE。
    run_remote sed -n -E \
        's/^[[:space:]]*supervisor_image[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' \
        "$(toml_path)" | sed -n '1p' | tr -d '\r'
}

ensure_supervisor_image() {
    # 全新宿主没有 supervisor 镜像，网关启动即拉取失败退出。TOML 钉的常是
    # 本地构建约定 tag（:local，上游不存在）。
    # 2026-10-09 双轮断代考古终版（真配对 digest 钉版）：钉版 gateway
    # 的真搭档=2026-08-28 构建（:latest 当期）——根提取位原生+老 CLI 契约+携带 sandbox
    # principal 出示能力。上游 :latest换新契约且旧版无 tag 存档→digest 钉
    # c8c42aef（不可变永可拉）。4 月版本线 0.0.3x 虽老契约但无 principal 出示→log-push 恒
    # 403+exec internal error（122 实测 denied 行>0；换真配对后 denied=0+exec 哨兵 OK）。
    # 配对升级：gateway digest-pin 与本 digest 同改（LESSONS #18 四处同源口径）。
    local ref era_tag era_digest
    ref=$(configured_supervisor_image)
    [ -n "$ref" ] || return 0
    era_tag='ghcr.io/nvidia/openshell/supervisor:era'
    era_digest='ghcr.io/nvidia/openshell/supervisor@sha256:c8c42aef16c200063e32cbf72e553e4ead027085427b555efafd95063ecead42'
    if ! run_remote docker image inspect "$ref" >/dev/null 2>&1; then
        echo "supervisor image absent: $ref — 真配对 era 自举（2026-08-28 digest 钉版）..."
        if ! run_remote docker image inspect "$era_tag" >/dev/null 2>&1; then
            run_remote docker pull --quiet "$era_digest" \
                || die "supervisor era 镜像拉取失败（$era_digest）"
            run_remote docker tag "$era_digest" "$era_tag" 2>/dev/null || true
        fi
        run_remote docker tag "$era_tag" "$ref"
    fi
    ensure_supervisor_compat "$ref" "$era_tag"
}

ensure_supervisor_compat() {  # era 一致性守门（2026-10-09 真配对版：比对源=根提取位，不一致即 retag 不构建）
    # 以 era 镜像为底，scratch 同内容 + 把 /usr/local/bin/openshell-sandbox 复制到
    # 根提取位 /openshell-sandbox，覆写目标 tag。该镜像仅用于网关二进制提取，
    # ENV/ENTRYPOINT 元数据不参与提取链。幂等判据=ref 根路径二进制与 era 二进制
    # 字节一致（保护用户自建 ref 不被覆写）；一致即跳过，否则重建（层缓存秒级）。
    local ref="$1" era_tag="$2" tmp ca cb cid_a cid_b img_id ref_id
    run_remote docker image inspect "$era_tag" >/dev/null 2>&1 \
        || { echo "WARN: era 底座不在位（$era_tag）——跳过兼容层（先经 ensure_supervisor_image 拉取）"; return 0; }
    tmp=$(mktemp -d); ca="$tmp/a"; cb="$tmp/b"
    cid_a=$(run_remote docker create "$ref" 2>/dev/null) || true   # 镜像存疑时 create 失败——守卫住, 空 cid 走字节比对必败→era 重建自愈
    if [ -n "$cid_a" ]; then
        run_remote docker export "$cid_a" 2>/dev/null | tar -xO openshell-sandbox > "$ca" 2>/dev/null || : > "$ca"
        run_remote docker rm "$cid_a" >/dev/null 2>&1
    else : > "$ca"; fi
    cid_b=$(run_remote docker create "$era_tag" 2>/dev/null) || true
    if [ -n "$cid_b" ]; then
        run_remote docker export "$cid_b" 2>/dev/null | tar -xO openshell-sandbox > "$cb" 2>/dev/null || : > "$cb"
        run_remote docker rm "$cid_b" >/dev/null 2>&1
    else rm -rf "$tmp"; return 0; fi
    if [ -s "$ca" ] && cmp -s "$ca" "$cb"; then rm -rf "$tmp"; return 0; fi
    echo "  supervisor 根路径二进制与 era 不一致（缺失/漂移）— retag $era_tag → $ref ..."
    rm -rf "$tmp"
    run_remote docker tag "$era_tag" "$ref"         || die "supervisor era retag 失败（$era_tag → $ref）"
    echo "  era retag ready: $ref ← $era_tag（2026-08-28 真配对；比对源=根提取位，旧 /usr/local/bin 构建路径随真配对退役）"
        # 容器重建：restart 沿用旧镜像 ID、不重解 tag——crash-loop 中的网关容器必须
    # force-recreate 才会拿到 compat 镜像（无容器/已同镜像则跳过，幂等）
    cid_a=$(compose ps -q "$SERVICE" 2>/dev/null)
    if [ -n "$cid_a" ]; then
        img_id=$(run_remote docker inspect "$cid_a" --format '{{.Image}}' 2>/dev/null)
        ref_id=$(run_remote docker image inspect "$ref" --format '{{.Id}}' 2>/dev/null)
        if [ -n "$img_id" ] && [ -n "$ref_id" ] && [ "$img_id" != "$ref_id" ]; then
            echo "  recreating gateway container to pick up era compat supervisor image ..."
            compose up -d --force-recreate "$SERVICE" >/dev/null
        fi
    fi
}

cmd_ensure() {
    local before after
    ensure_jwt_keys
    ensure_supervisor_image
    # ensure 必须自足：容器缺失/未运行时先 up -d（全新宿主、清理后重部署的
    # 唯一起路径；up 对配置未变的已停容器只做启动，不会无谓 recreate）
    if [ -z "$(compose ps -q "$SERVICE" 2>/dev/null)" ]; then
        echo "gateway container absent/not running — compose up -d"
        compose up -d "$SERVICE"
    fi
    before=$(configured_server_sans)
    if [ "$before" != "$(enforced_value)" ]; then
        echo "enforcing routing domain '$ROUTING_DOMAIN' (was: '${before:-<unset>}')"
        patch_server_sans
        echo "restarting gateway to re-read config ..."
        compose restart "$SERVICE"
    else
        echo "routing domain already enforced: server_sans=$before"
    fi
    wait_liveness
    cmd_verify
}

cmd_start()   { compose start "$SERVICE"; wait_liveness; }
cmd_stop()    { compose stop "$SERVICE"; }
cmd_restart() { compose restart "$SERVICE"; wait_liveness; }
cmd_recreate() {
    echo "WARNING: 'up -d' applies the compose file. Pending change vs the" >&2
    echo "running container: command: [] -> TOML bind_address 127.0.0.1:8080" >&2
    echo "takes over. Verify published-port reachability afterwards." >&2
    compose up -d "$SERVICE"
    wait_liveness
}
cmd_logs() { compose logs --tail="${1:-50}" "$SERVICE"; }

cmd_image() {
    # 钉扎对账（只读）：GATEWAY_IMAGE（digest 锚）↔ 运行容器实际镜像。
    # 比镜像 ID 而非 ref 字符串——容器 Config.Image 停留在创建时的 ref 写法
    # （如 :latest），内容一致即同步；:latest 重拉换版 / 手工换镜像才会漂移。
    # 注意：compose 文件改 digest 后仅 restart 不生效（保留容器规格），漂移
    # 消除走 recreate（up -d 应用 compose）——生产动作，须。
    local pinned_id run_id cid created_ref
    pinned_id=$(run_remote docker image inspect "$GATEWAY_IMAGE" --format '{{.Id}}' 2>/dev/null) \
        || die "pinned image not present on host: $GATEWAY_IMAGE (pull it, or check GATEWAY_IMAGE)"
    cid=$(compose ps -q "$SERVICE" 2>/dev/null)
    [ -n "$cid" ] || die "gateway container not running (cannot reconcile image)"
    run_id=$(run_remote docker inspect "$cid" --format '{{.Image}}')
    created_ref=$(run_remote docker inspect "$cid" --format '{{.Config.Image}}')
    echo "pinned:   $GATEWAY_IMAGE"
    echo "pinned image id:    $pinned_id"
    echo "running container: $cid (created with ref: $created_ref)"
    echo "running image id:  $run_id"
    if [ "$pinned_id" = "$run_id" ]; then
        echo "image reconcile OK: running container matches pinned digest"
    else
        echo "IMAGE DRIFT: running container does not match $GATEWAY_IMAGE" >&2
        echo "  fix (production, human-approved only): deploy.sh deploy && $0 recreate" >&2
        return 1
    fi
}

case "${1:-}" in
    ensure)  cmd_ensure ;;
    verify)  cmd_verify ;;
    status)  cmd_status ;;
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    restart) cmd_restart ;;
    recreate) cmd_recreate ;;
    image)    cmd_image ;;
    logs)     shift; cmd_logs "$@" ;;
    *) sed -n '2,41p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
