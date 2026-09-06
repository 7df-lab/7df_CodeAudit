#!/usr/bin/env bash
# sim-sync.sh — pb-A 模拟栈源码供给：本地 git 已提交树 → docker 宿主的 sim 检出。
#
# 背景：栈操作须在有 docker 的宿主上执行（pb-A；本机无 docker → LXC 107），
# 宿主侧检出台（默认 /root/codeaudit-sim-check）是 engine + deploy 的源码拷贝
# （无 .git）。本脚本把"打 tar → pct push → 远端解包 → sim.sh up"的手工管线
# 固化为可重复入口（2026-09-06 人类指令"用脚本走安装流程，而不是手动安装"）：
#   - 同步源 = git archive，仅已提交内容——脏树/本机杂物不可泄漏（pb-D 同纪律）
#   - 覆盖式解包（overlay，不删远端既有文件）：env.sim 等本地 gitignored 的
#     真实值只存于远端，不受同步影响
#   - rebuild 即调 deploy/sim.sh up（幂等 up -d --build）；Go 全量构建耗时以
#     分钟计，调用方不得套短超时
#
# 用法：
#   deploy/sim-sync.sh push [VMID]     # 仅同步源码
#   deploy/sim-sync.sh rebuild [VMID]  # push + 远端 sim.sh up（等 gateway 健康）
#   deploy/sim-sync.sh test [VMID]     # 远端跑 deploy/tests/run.sh（九用例 e2e）
#
# 环境：VMID（默认 107）、SIM_DIR（默认 /root/codeaudit-sim-check）。
set -euo pipefail
cd "$(dirname "$0")/.."              # 伞仓根

VMID="${2:-${VMID:-107}}"
SIM_DIR="${SIM_DIR:-/root/codeaudit-sim-check}"
REMOTE="pct exec $VMID --"
run_remote() { $REMOTE "$@"; }       # intentional word splitting (command prefix)

cmd="${1:-push}"

sync_tree() {
    # 注意：work 不能 local——EXIT trap 在函数作用域外触发，set -u 下会报未绑定
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    # engine 取子仓 HEAD（子仓内容不入伞仓 archive）；deploy 取伞仓 HEAD
    git -C engine archive --prefix=engine/ -o "$work/engine.tgz" HEAD
    git archive -o "$work/deploy.tgz" HEAD deploy
    pct push "$VMID" "$work/engine.tgz" /tmp/sim-sync-engine.tgz
    pct push "$VMID" "$work/deploy.tgz" /tmp/sim-sync-deploy.tgz
    run_remote bash -c "mkdir -p '$SIM_DIR' && cd '$SIM_DIR' && \
        tar -xzf /tmp/sim-sync-engine.tgz && tar -xzf /tmp/sim-sync-deploy.tgz && \
        rm -f /tmp/sim-sync-engine.tgz /tmp/sim-sync-deploy.tgz && \
        echo 'sim-sync: source tree updated at $SIM_DIR'"
}

case "$cmd" in
    push)
        sync_tree
        ;;
    rebuild)
        sync_tree
        run_remote bash -c "cd '$SIM_DIR' && \
            PLATFORM_DIR='$SIM_DIR/engine' bash deploy/sim.sh up"
        ;;
    test)
        run_remote bash -c "cd '$SIM_DIR' && bash deploy/tests/run.sh"
        ;;
    *)
        echo "unknown command: $cmd (push|rebuild|test)" >&2
        exit 2
        ;;
esac
