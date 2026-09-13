#!/usr/bin/env bash
# sim-sync.sh — pb-A 模拟栈源码供给：本地 git 已提交树 → docker 宿主的 sim 检出。
#
# 背景：栈操作须在有 docker 的宿主上执行（pb-A；本机无 docker → LXC 107），
# 宿主侧检出台（默认 /root/codeaudit-sim-check）是 engine + deploy 的源码拷贝
# （无 .git）。本脚本把"打 tar → pct push → 远端解包 → sim.sh up"的手工管线
# 固化为可重复入口：
#   - 同步源 = git archive，仅已提交内容——脏树/本机杂物不可泄漏（pb-D 同纪律）
#   - 收敛式同步（对齐 prod/deploy.sh 与 web/deploy.sh 同型）：
#     解包前清空远端 engine/ web/ 与 deploy/ 旧树，白名单保留 deploy/env.sim——
#     本地 gitignored 的真实值只存于远端（sim.sh 靠它注入沙箱接线）；不清空则
#     上游已删/改名文件残留进构建上下文（LESSONS #7 叠加同步膨胀，旧实现可静默
#     复活进构建产物）
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
    # engine/web 取子仓 HEAD（子仓内容不入伞仓 archive）；deploy 取伞仓 HEAD。
    # web 必须随批同步（2026-09-07 实证）：console 容器 build context=../web，
    # 只同步 engine 时 console 永远用残留旧树重建——前端修复不进部署产物，
    # "修过的缺陷在 GUI 又出现"即此缺口。
    git -C engine archive --prefix=engine/ -o "$work/engine.tgz" HEAD
    git -C web archive --prefix=web/ -o "$work/web.tgz" HEAD
    git archive -o "$work/deploy.tgz" HEAD deploy
    pct push "$VMID" "$work/engine.tgz" /tmp/sim-sync-engine.tgz
    pct push "$VMID" "$work/web.tgz" /tmp/sim-sync-web.tgz
    pct push "$VMID" "$work/deploy.tgz" /tmp/sim-sync-deploy.tgz
    run_remote bash -c "mkdir -p '$SIM_DIR/deploy' && cd '$SIM_DIR' && \
        { [ -f engine/services/sast-adapter-service/tools/opengrep ] && rm -f /tmp/sim-sync-tools.keep && cp -a engine/services/sast-adapter-service/tools/opengrep /tmp/sim-sync-tools.keep || true; } && \\
        rm -rf engine web && \\
        find deploy -mindepth 1 -maxdepth 1 '!' -name env.sim -exec rm -rf {} + && \\
        tar -xzf /tmp/sim-sync-engine.tgz && tar -xzf /tmp/sim-sync-web.tgz && tar -xzf /tmp/sim-sync-deploy.tgz && \\
        rm -f /tmp/sim-sync-engine.tgz /tmp/sim-sync-web.tgz /tmp/sim-sync-deploy.tgz && \\
        mkdir -p engine/services/sast-adapter-service/tools && \\
        { [ -f /tmp/sim-sync-tools.keep ] && cp -a /tmp/sim-sync-tools.keep engine/services/sast-adapter-service/tools/opengrep || true; } && \\
        rm -f /tmp/sim-sync-tools.keep && \\
        echo 'sim-sync: source tree converged at $SIM_DIR (deploy/env.sim + sast-adapter tools/opengrep preserved)'"
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
