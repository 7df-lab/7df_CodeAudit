#!/usr/bin/env bash
# deploy.sh 行为锁定关卡——PATH 桩 docker/pct/ssh/curl
# 记录实参到 LOGFILE、可配置退出码，断言脚本侧命令形态（不模拟远端语义，
# live 验证归 U5 人工授权窗口）。
#
# 锁定：stop 失败不回落 up / 非 pct REMOTE 零 pct 调用 /
#       token 按行前缀剥离（含 base64 padding）+ env 缺失 check SKIPPED。
# 用法：bash tests/deploy_cases.sh
set -u
root="$(cd "$(dirname "$0")/.." && pwd)"
dsh="$root/deploy/deploy.sh"
FAILED=0
fail() { echo "FAIL: $1" >&2; FAILED=1; }
pass() { echo "ok: $1"; }

bash -n "$dsh" || fail "bash -n deploy.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/log"

stub() { cat > "$tmp/bin/$1" && chmod +x "$tmp/bin/$1"; }
stub docker <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "${LOGFILE:?}"
exit "${DOCKER_RC:-0}"
EOF
stub pct <<'EOF'
#!/usr/bin/env bash
# 消费 stdin（缺省 REMOTE 下 sync_all 的 tar 管道右侧经 pct exec 进入），
# 否则上游 tar 收 SIGPIPE → pipefail
cat > /dev/null
printf 'pct %s\n' "$*" >> "${LOGFILE:?}"
exit "${PCT_RC:-0}"
EOF
stub ssh <<'EOF'
#!/usr/bin/env bash
# 消费 stdin（sync_all 的 tar 管道右侧），否则上游 tar 收 SIGPIPE → pipefail
cat > /dev/null
printf 'ssh %s\n' "$*" >> "${LOGFILE:?}"
exit "${SSH_RC:-0}"
EOF
stub curl <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
export PATH="$tmp/bin:$PATH"

# 极小 SRC 树（tar 输入只需存在）：树比对走 --mtime 归一，与内容体积无关
src="$tmp/src"
mkdir -p "$src/openshell_manager" "$src/libs/OpenShell/python"
: > "$src/config.json"

# --- C2-1：stop 且 compose 失败 → 非零退出且绝不回落 compose up -----------
export LOGFILE="$tmp/log/c1"; : > "$LOGFILE"
DOCKER_RC=1 REMOTE="" DEPLOY_DIR="$tmp/d1" \
    HEALTH_URL="http://127.0.0.1:1/healthz" SRC="$root" \
    "$dsh" stop >/dev/null 2>&1
rc=$?
[ "$rc" -ne 0 ] || fail "C2-1: stop with failing compose must exit nonzero (got 0)"
grep -q ' stop' "$LOGFILE" || fail "C2-1: compose stop was not even invoked: $(cat "$LOGFILE")"
if grep -q ' up ' "$LOGFILE"; then
    fail "C2-1: stop fell back to compose up (语义反转): $(cat "$LOGFILE")"
else
    pass "C2-1: stop 失败不回落 compose up（rc=$rc，stop 确已下发）"
fi

# --- C2-2：非 pct 形态 REMOTE → 零 pct 调用 + 显式跳过提示 ---------------
export LOGFILE="$tmp/log/c2"; : > "$LOGFILE"
dep2="$tmp/d2"
mkdir -p "$dep2"
: > "$dep2/.env"   # 非 pct 形态由操作者自备 .env（脚本会 chmod 600 它）
REMOTE="ssh fakehost --" DEPLOY_DIR="$dep2" SRC="$src" \
    HEALTH_URL="http://127.0.0.1:1/healthz" \
    "$dsh" deploy >"$tmp/log/c2.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] || fail "C2-2: deploy(ssh REMOTE) should succeed (rc=$rc): $(tail -3 "$tmp/log/c2.out")"
if grep -q '^pct ' "$LOGFILE"; then
    fail "C2-2: pct invoked under non-pct REMOTE: $(grep '^pct ' "$LOGFILE")"
else
    pass "C2-2: 非 pct REMOTE 零 pct 调用"
fi
grep -q "跳过 env 推送" "$tmp/log/c2.out" \
    || fail "C2-2: 缺显式跳过提示: $(tail -3 "$tmp/log/c2.out")"

# --- C2-3a：token 按行前缀剥离（多行 env + base64 padding `=` 完整保留）--
td3="$tmp/d3"
mkdir -p "$td3"
cp "$dsh" "$td3/deploy.sh"
printf 'OPENSHELL_MANAGER_TOKEN=abc=de==f\n# comment\nOPENSHELL_GATEWAY_ENDPOINT=gw:8080\n' \
    > "$td3/env"
got="$(cd "$td3" && eval "$(sed -n '/^manager_token_of()/,/^}/p' "$dsh")" \
    && manager_token_of)"
[ "$got" = "abc=de==f" ] \
    || fail "C2-3: token_of='$got' (want abc=de==f——多行 env 不得串行，padding 不得截断)"
[ "$got" = "abc=de==f" ] && pass "C2-3: token 按行前缀剥离且 padding 完整"

# --- C2-3b：env 缺失 → check 输出 SKIPPED 且 exit 0（不假失败）----------
export LOGFILE="$tmp/log/c4"; : > "$LOGFILE"
td4="$tmp/d4"; dep4="$tmp/d4-dep"
mkdir -p "$td4" "$dep4"
cp "$dsh" "$td4/deploy.sh"
cp "$root/deploy/Dockerfile.manager" "$root/deploy/docker-compose.yml" \
   "$root/deploy/env.template" "$td4/" 2>/dev/null
: > "$dep4/.env"
REMOTE="" DEPLOY_DIR="$dep4" SRC="$src" \
    HEALTH_URL="http://127.0.0.1:1/healthz" \
    "$td4/deploy.sh" deploy >"$tmp/log/c4.out" 2>&1 \
    || fail "C2-3: setup deploy failed: $(tail -3 "$tmp/log/c4.out")"
out="$(REMOTE="" DEPLOY_DIR="$dep4" SRC="$src" \
    HEALTH_URL="http://127.0.0.1:1/healthz" "$td4/deploy.sh" check 2>&1)"
rc=$?
[ "$rc" -eq 0 ] || fail "C2-3: check with missing env must exit 0 (got $rc): $out"
echo "$out" | grep -q "SKIPPED" \
    || fail "C2-3: check must report SKIPPED for missing env: $out"
[ "$rc" -eq 0 ] && echo "$out" | grep -q "SKIPPED" \
    && pass "C2-3: env 缺失 check 显式 SKIPPED 且 in sync"

# --- C2-2b：缺省 REMOTE（pct 形态）→ pct push 照常执行（锁 case 守卫不回潮
# --- 成"永远 skip"；审查 F5：该分支此前零覆盖）---------------------------
export LOGFILE="$tmp/log/c5"; : > "$LOGFILE"
dep5="$tmp/d5"
mkdir -p "$dep5"
: > "$dep5/.env"
unset REMOTE   # 不设 → 脚本内缺省 `pct exec 107 --`
DEPLOY_DIR="$dep5" SRC="$src" HEALTH_URL="http://127.0.0.1:1/healthz" \
    "$dsh" deploy >"$tmp/log/c5.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] || fail "C2-2b: deploy(default REMOTE) should succeed (rc=$rc): $(tail -3 "$tmp/log/c5.out")"
grep -q '^pct push ' "$LOGFILE" \
    || fail "C2-2b: default pct REMOTE must push env: $(grep '^pct ' "$LOGFILE" | head -3)"
[ "$rc" -eq 0 ] && grep -q '^pct push ' "$LOGFILE" \
    && pass "C2-2b: 缺省 pct REMOTE 照常推送 env"

if [ "$FAILED" -ne 0 ]; then
    echo "deploy_cases: FAILED"
    exit 1
fi
echo "deploy_cases: ALL GREEN"
