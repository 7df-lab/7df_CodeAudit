#!/usr/bin/env bash
# baton-provision.sh — baton CLI 二进制供给（本地构建 → docker 宿主 bin 目录）。
#
# 背景（2026-09-19 批固化；同日 io-audit 批次1 挂进入口链，
# docs/designs/io-audit-fix-plan-2026-09-19.md）：runtime 的 baton 投影钩子
# （BATON_BIN 指到挂载点内二进制）是计量归集（usage_collector 读
# .tasklog/events.jsonl）与换手种子投影的唯一通路；二进制是宿主级运行态资产
# （不入任何树——prod-sync/sim-sync 均不管理），供给固化为本脚本。
#
# 事实源 = 主仓库 baton/ 检出（go build 自当前树）。
# 目标目录：
#   生产面 /opt/codeaudit/bin  （engine/deploy overlay BATON 宿主挂载缺省）
#   模拟面 /opt/codeaudit-sim/bin（sim overlay BATON_HOST_BIN_DIR 缺省；--sim 一并刷新）
#
# REMOTE 契约（与 openshell-gateway/deploy.sh 同语义，`-` 而非 `:-`）：
#   未设/设 VMID   → 远程执行 push <VMID>（须显式传入）
#   REMOTE="" 空串 → 本机直投（production-deploy.sh「目标宿主本机 daemon」形态）
#
# 入口链：production-deploy.sh（deploy_engine 步前置）+ build.sh（构建批末尾）+
#   手工（sim 面刷新带 --sim）。旁路键 BATON_PROVISION=0 在两个入口调用方判定。
# 退出码：0=投放完成；3=供给前置缺失（无 go / 无 baton 检出台）——调用方按
#   runtime 钩子「缺二进制=诚实禁用」同构语义警告+跳过不阻塞；1=其他失败
#   （fail-loud，可 BATON_PROVISION=0 旁路重跑）。
#
# 用法：deploy/internal/baton-provision.sh [--sim] [VMID]
#   --sim   同时刷新模拟面目录（默认只投生产面）
set -euo pipefail
cd "$(dirname "$0")/../.."              # 主仓库根

SIM=0
REMOTE="${REMOTE-107}"
for a in "$@"; do
    case "$a" in
        --sim) SIM=1 ;;
        *) REMOTE="$a" ;;
    esac
done

# go 定位（与 engine verify 同痛点：PATH 优先，回落 /usr/local/go；.toolchain 1.22
# 过老不适用——baton go.mod 地板以本机新工具链为准）
GO_BIN="$(command -v go || true)"
if [ -z "$GO_BIN" ] && [ -x /usr/local/go/bin/go ]; then GO_BIN=/usr/local/go/bin/go; fi
if [ -z "${GO_BIN:-}" ]; then
    echo "[baton-provision] 无 go 工具链（PATH 与 /usr/local/go/bin/go 均无）——前置缺失 rc=3（钩子诚实禁用语义，调用方警告跳过）" >&2
    exit 3
fi
# 发布树（prod-sync/sim-sync 的 git archive）不含 baton 子仓——同样属前置缺失
if [ ! -f baton/go.mod ]; then
    echo "[baton-provision] 无 baton/ 检出台（gitless 发布树未含子仓）——前置缺失 rc=3（同上；完整 clone 须 --recurse-submodules）" >&2
    exit 3
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "[baton-provision] 本地构建（baton 检出 HEAD）..."
( cd baton && "$GO_BIN" build -o "$work/baton" ./cmd/baton )
sha=$(sha256sum "$work/baton" | awk '{print $1}')
echo "[baton-provision] baton sha256=${sha:0:16} size=$(stat -c %s "$work/baton")"

install_one() { # install_one <宿主目录>
    local dir="$1" tmp
    if [ -n "$REMOTE" ]; then
        pct push "$REMOTE" "$work/baton" "/tmp/baton-provision.bin"
        ssh "$REMOTE" -- sh -c "mkdir -p '$dir' && mv /tmp/baton-provision.bin '$dir/baton' && chmod 755 '$dir/baton'"
        echo "[baton-provision] pushed → ${REMOTE}:${dir}/baton"
    else
        mkdir -p "$dir"
        tmp="$dir/.baton-provision.$$"
        cp "$work/baton" "$tmp"
        chmod 755 "$tmp"
        mv -f "$tmp" "$dir/baton"   # 原子换名：容器正执行旧档不受扰（mv 替代原地写）
        echo "[baton-provision] 本机直投 → ${dir}/baton"
    fi
}

install_one /opt/codeaudit/bin
[ "$SIM" = "1" ] && install_one /opt/codeaudit-sim/bin
echo "[baton-provision] 完成（钩子重启 runtime 后生效；缺失=钩子禁用诚实降级）"
