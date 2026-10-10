#!/usr/bin/env bash
# deploy/deploy.sh —— deploy/ 唯一用户入口。
#
# 本目录顶层只此一个 sh：第三方自部署与开发者日常都从这里进；
# 机制实现在 internal/（被本启动器与 Makefile 调用，不直接手敲）。
#
# 用法：
#   bash deploy/deploy.sh                  # = install：一键部署到本机 docker（第三方入口）
#   bash deploy/deploy.sh install          # 同上；可带 production-deploy 全部参数（configure/check/status/stop/down）
#   bash deploy/deploy.sh sim up|down|test|logs|destroy|status   # 模拟栈（集成测试，pb-A）
#   bash deploy/deploy.sh build [ARGS]     # 一键构建 12 发布镜像（--plan 只读预演）
#   bash deploy/deploy.sh versions         # 版本对账（只读观测）
#   bash deploy/deploy.sh plan | check     # <host-id> 链只读预检（部署动作走 pb-D，须）
#   bash deploy/deploy.sh sync-sim         # 源码供给：同步树到 <host-id> sim 检出台
#   bash deploy/deploy.sh sync-prod        # 源码供给：同步发布树到 <host-id> 生产检出台
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

cmd="${1:-install}"
shift || true
case "$cmd" in
    install)    exec bash deploy/internal/production-deploy.sh "$@" ;;
    sim)        sub="${1:-}"; [ -n "$sub" ] || { echo "usage: $0 sim up|down|test|logs|destroy|status" >&2; exit 2; }
                shift
                case "$sub" in
                    up|down|destroy|logs|status|chain|wait|seed) exec bash deploy/internal/sim.sh "$sub" "$@" ;;
                    test) exec bash deploy/internal/tests/run.sh "$@" ;;
                    *) echo "unknown sim action: $sub" >&2; exit 2 ;;
                esac ;;
    build)      exec bash deploy/internal/build.sh "$@" ;;
    versions)   exec bash deploy/internal/versions.sh "$@" ;;
    plan)       exec bash deploy/internal/sandbox-deploy.sh plan "$@" ;;
    check)      exec bash deploy/internal/sandbox-deploy.sh check "$@" ;;
    sync-sim)   exec bash deploy/internal/sim-sync.sh "$@" ;;
    sync-prod)  exec bash deploy/internal/prod-sync.sh "$@" ;;
    help|-h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//' ;;
    *) echo "unknown command: $cmd（入口契约见 deploy/README.md §0）" >&2; exit 2 ;;
esac
