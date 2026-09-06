#!/usr/bin/env bash
# Sync this directory (source of truth) -> gateway runtime dir in the LXC,
# then make it take effect via gateway_lifecycle.sh ensure.
#
# Discipline: gateway.toml / docker-compose.yml are edited HERE, never on the
# LXC; the LXC copy is a deployment target. deploy.sh overwrites it on every
# run (a timestamped backup of the previous runtime copy is kept remotely).
#
# 统一契约（供伞仓 ../deploy/sandbox-deploy.sh 分发，清单 deploy/sandbox-deploy.toml）：
#   deploy   下发变更并生效（默认；配置无变化则不重启）
#   check    漂移检查，不改任何东西（= --check）
#   status|start|stop|restart|logs [N]   委托 gateway_lifecycle.sh
#
# Usage:
#   deploy.sh              # push files + ensure (restart only if config changed)
#   deploy.sh deploy       # 同上
#   deploy.sh check        # show drift between CD and LXC copies, change nothing
#
# Environment: REMOTE / DEPLOY_DIR / SERVICE / ROUTING_DOMAIN / LIVENESS_*
#   (same variables as gateway_lifecycle.sh, which this script calls).
set -euo pipefail

cd "$(dirname "$0")"

REMOTE="${REMOTE:-pct exec 107 --}"
VMID="${VMID:-107}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/os-deploy/deploy/docker}"

FILES=(docker-compose.yml gateway.toml Dockerfile.gateway Dockerfile.supervisor)

run_remote() { $REMOTE "$@"; }  # intentional word splitting (command prefix)

md5_local() { md5sum "$1" | awk '{print $1}'; }
md5_remote() { run_remote md5sum "$DEPLOY_DIR/$1" 2>/dev/null | awk '{print $1}'; }

drift=0
for f in "${FILES[@]}"; do
    [ -f "$f" ] || { echo "missing in CD: $f" >&2; exit 1; }
    if [ "$(md5_local "$f")" != "$(md5_remote "$f")" ]; then
        echo "drift: $f"
        drift=1
    fi
done

cmd="${1:-deploy}"
case "$cmd" in
    check|--check)
        [ "$drift" = "0" ] && echo "in sync" || echo "^ CD differs from LXC runtime; run deploy.sh to apply"
        exit 0
        ;;
    deploy)
        if [ "$drift" = "0" ]; then echo "openshell-gateway: in sync, ensure only"; fi
        run_remote mkdir -p "$DEPLOY_DIR"
        for f in "${FILES[@]}"; do
            if [ "$(md5_local "$f")" = "$(md5_remote "$f")" ]; then
                echo "unchanged: $f"
                continue
            fi
            run_remote cp "$DEPLOY_DIR/$f" "$DEPLOY_DIR/$f.bak.$(date +%Y%m%d%H%M%S)" 2>/dev/null || true
            pct push "$VMID" "$f" "$DEPLOY_DIR/$f"
            echo "pushed: $f"
        done
        ./gateway_lifecycle.sh ensure
        ;;
    status|start|stop|restart)
        exec ./gateway_lifecycle.sh "$cmd"
        ;;
    logs)
        shift
        exec ./gateway_lifecycle.sh logs "$@"
        ;;
    *)
        echo "usage: $0 [deploy|check|status|start|stop|restart|logs [N]]" >&2
        exit 2
        ;;
esac
