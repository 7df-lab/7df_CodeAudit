#!/usr/bin/env bash
# 分发脚本 usage/未知子命令行为锁定（胶水收敛批 2026-09-16）——五脚本此前
# 仅 bash -n 级覆盖，分发行为（未知命令拒绝、usage 输出）零护栏。
# PATH 桩 docker/pct 防意外触真；只测分发面不测部署语义。
# 用法：bash deploy/tests/dispatch_cases.sh（伞仓根执行）
set -u
root="$(cd "$(dirname "$0")/../../.." && pwd)"
FAILED=0
fail() { echo "FAIL: $1" >&2; FAILED=1; }
pass() { echo "ok: $1"; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
stub() { cat > "$tmp/bin/$1" && chmod +x "$tmp/bin/$1"; }
for c in docker pct ssh curl openssl; do
    stub "$c" <<'EOF'
#!/usr/bin/env bash
cat > /dev/null 2>&1
exit 0
EOF
done
export PATH="$tmp/bin:$PATH"

# check <name> <script...> —— 未知子命令：非零退出 + stderr 有 usage 提示
check_reject() {
    local name="$1"; shift
    local out rc
    out=$("$@" 2>&1); rc=$?
    if [ "$rc" -ne 0 ] && echo "$out" | grep -qiE 'usage|用法|未知|invalid|not a|unknown'; then
        pass "D: $name 未知子命令拒绝（exit $rc + usage）"
    else
        fail "D: $name 未知子命令应非零+usage (rc=$rc): $(echo "$out" | head -2)"
    fi
}

check_reject "sandbox-deploy" bash "$root/deploy/internal/sandbox-deploy.sh" definitely-not-a-cmd
check_reject "sim"           bash "$root/deploy/internal/sim.sh" definitely-not-a-cmd
check_reject "sim-sync"      bash "$root/deploy/internal/sim-sync.sh" definitely-not-a-cmd
check_reject "prod-sync"     bash "$root/deploy/internal/prod-sync.sh" definitely-not-a-cmd
check_reject "engine-deploy" bash "$root/engine/deploy/deploy.sh" definitely-not-a-cmd
check_reject "web-deploy"    bash "$root/web/deploy.sh" definitely-not-a-cmd

# 分发器只读面在位：plan 子命令不触部署动作（PATH 桩兜底，应快速返回 0）
if bash "$root/deploy/internal/sandbox-deploy.sh" plan >"$tmp/plan.out" 2>&1; then
    grep -q 'codeaudit' "$tmp/plan.out" \
        && grep -q 'dir *= *"engine/deploy"' "$root/deploy/internal/sandbox-deploy.toml" \
        && pass "D: plan 成功且 toml codeaudit 条目已重指 engine/deploy" \
        || fail "D: plan 失败或 toml 未重指: $(tail -3 "$tmp/plan.out")"
else
    fail "D: sandbox-deploy.sh plan 应只读成功: $(tail -3 "$tmp/plan.out")"
fi

if [ "$FAILED" -ne 0 ]; then
    echo "dispatch_cases: FAILED"
    exit 1
fi
echo "dispatch_cases: ALL GREEN"
