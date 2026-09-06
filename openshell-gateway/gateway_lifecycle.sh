#!/usr/bin/env bash
# openshell-gateway lifecycle (deploy-side, no HTTP surface).
#
# Lives in the umbrella submodule openshell-gateway/, which is the
# SOURCE OF TRUTH for the gateway deployment files. This script operates on
# the runtime copy inside the target LXC; run ./deploy.sh first to sync the
# CD copies out. The manager service (openshell_manager) stays a pure
# transport pipe — it holds no lifecycle logic and exposes none over HTTP.
#
# What this script does: edits the gateway's TOML config inside the target
# LXC (enforcing the service routing domain) and drives the gateway's
# docker-compose service through start/stop/restart.
#
# Current deployment (discovered 2026-08-31):
#   LXC 107 (PVE guest "docker", gateway.internal = gateway.internal)
#     /root/os-deploy/deploy/docker/{docker-compose.yml,gateway.toml}
#     container: docker-gateway-1  (image ghcr.io/nvidia/openshell/gateway)
#     ports: 8080 gRPC, 8081 health  -> published on the LXC
#
# Usage:
#   gateway_lifecycle.sh ensure     # enforce routing domain, restart if changed, verify
#   gateway_lifecycle.sh verify     # check config + liveness only (no changes)
#   gateway_lifecycle.sh status     # container state + configured routing domain
#   gateway_lifecycle.sh start|stop|restart
#   gateway_lifecycle.sh recreate   # compose up -d — applies compose file changes
#   gateway_lifecycle.sh logs [N]   # last N gateway log lines (default 50)
#
# Environment:
#   REMOTE           command prefix reaching the LXC   (default: "pct exec 107 --")
#   DEPLOY_DIR       compose dir inside the LXC        (default: /root/os-deploy/deploy/docker)
#   SERVICE          compose service name              (default: gateway)
#   ROUTING_DOMAIN   enforced service routing domain   (default: openshell.internal)
#   LIVENESS_HOST/_PORT  TCP liveness probe target     (default: 127.0.0.1 / 8080)
#   LIVENESS_TIMEOUT_SECS                                (default: 60)
#
# WARNING on `recreate`: the compose file currently sets `command: []` while the
# running container still carries the image default CMD (--bind-address 0.0.0.0
# --port 8080). `up -d` applies the file, so TOML's bind_address (127.0.0.1:8080)
# takes over — verify external reachability (published ports) afterwards. The
# daily operations (`ensure`/`restart`) intentionally use `compose restart`,
# which keeps the running container spec and only re-reads the TOML.
set -euo pipefail

# `-` 而非 `:-`：REMOTE=""（空串）是显式的"本机执行"契约（伞仓生产态部署链
# production-deploy.sh 依赖），不得在空串时触发 pct 缺省（2026-09-05 dind 实测）。
REMOTE="${REMOTE-pct exec 107 --}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/os-deploy/deploy/docker}"
SERVICE="${SERVICE:-gateway}"
ROUTING_DOMAIN="${ROUTING_DOMAIN:-openshell.internal}"
LIVENESS_HOST="${LIVENESS_HOST:-127.0.0.1}"
LIVENESS_PORT="${LIVENESS_PORT:-8080}"
LIVENESS_TIMEOUT_SECS="${LIVENESS_TIMEOUT_SECS:-60}"
# JWT 签名密钥目录（gateway.toml gateway_jwt 段指向的同一路径）；镜像与
# compose 保持同源（IMAGE_TAG 变更时两处同步）。
JWT_DIR="${JWT_DIR:-/var/lib/openshell/tls/jwt}"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/nvidia/openshell/gateway:latest}"

# intentional word splitting: REMOTE is a command prefix ("pct exec 107 --")
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
    run_remote sed -n -E \
        's/^server_sans[[:space:]]*=[[:space:]]*(.+)$/\1/p' \
        "$(toml_path)" | head -1 | tr -d '\r'
}

patch_server_sans() {
    # Idempotently pin `server_sans = ["*.<ROUTING_DOMAIN>]` under
    # [openshell.gateway]. Runs remotely so the edit and the file never leave
    # the LXC. Timestamped backup written next to the file.
    run_remote bash -s "$ROUTING_DOMAIN" "$(toml_path)" <<'PATCH'
set -euo pipefail
domain="$1"; toml="$2"
line="server_sans = [\"*.$domain\"]"
[ -f "$toml" ] || { echo "missing $toml" >&2; exit 1; }
backup="$toml.bak.$(date +%Y%m%d%H%M%S)"
if grep -Eq '^server_sans[[:space:]]*=' "$toml"; then
    sed -i -E "s|^server_sans[[:space:]]*=.*|$line|" "$toml"
    echo "rewrote server_sans -> $line"
elif grep -q '^\[openshell\.gateway\]' "$toml"; then
    sed -i "/^\[openshell\.gateway\]/a $line" "$toml"
    echo "inserted $line under [openshell.gateway]"
else
    echo "no [openshell.gateway] table in $toml" >&2; exit 1
fi
# keep exactly one server_sans line
awk '/^server_sans[[:space:]]*=/{n++; if (n > 1) next} {print}' \
    "$toml" > "$toml.tmp" && mv "$toml.tmp" "$toml"
cp "$toml" "$backup" 2>/dev/null || true
echo "backup: $backup"
PATCH
}

# -- liveness ----------------------------------------------------------------

tcp_ok() {
    run_remote bash -c \
        "exec 3<>/dev/tcp/$LIVENESS_HOST/$LIVENESS_PORT" \
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
    if run_remote test -f "$JWT_DIR/signing.pem"; then return 0; fi
    echo "JWT signing keys absent at $JWT_DIR — one-shot generate-certs ..."
    run_remote docker run --rm --user 0 \
        -v /var/lib/openshell:/var/lib/openshell \
        "$GATEWAY_IMAGE" generate-certs \
        --output-dir /var/lib/openshell/tls \
        --server-san host.openshell.internal
}

configured_supervisor_image() {
    # stdout: gateway.toml 的 supervisor_image 值（空=用镜像默认）
    run_remote sed -n -E \
        's/^[[:space:]]*supervisor_image[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' \
        "$(toml_path)" | head -1 | tr -d '\r'
}

ensure_supervisor_image() {
    # 全新宿主没有 supervisor 镜像，网关启动即拉取失败退出。TOML 钉的常是
    # 本地构建约定 tag（:local，上游不存在）——拉不到就取同仓 :latest retag
    # 成配置名（与 gateway :latest 同批次发布，版本对齐；幂等，已有不重拉）。
    local ref latest
    ref=$(configured_supervisor_image)
    [ -n "$ref" ] || return 0
    run_remote docker image inspect "$ref" >/dev/null 2>&1 && return 0
    echo "supervisor image absent: $ref — pulling ..."
    if run_remote docker pull --quiet "$ref" >/dev/null 2>&1; then return 0; fi
    latest="${ref%%@*}"; latest="${latest%:*}:latest"
    echo "  $ref not upstream — pulling $latest and retagging ..."
    run_remote docker pull --quiet "$latest" \
        || die "supervisor image unavailable (${ref} and ${latest} both failed)"
    run_remote docker tag "$latest" "$ref"
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

case "${1:-}" in
    ensure)  cmd_ensure ;;
    verify)  cmd_verify ;;
    status)  cmd_status ;;
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    restart) cmd_restart ;;
    recreate) cmd_recreate ;;
    logs)    shift; cmd_logs "$@" ;;
    *) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
