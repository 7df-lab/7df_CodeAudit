#!/usr/bin/env bash
# production-deploy 拓扑化行为锁定（胶水收敛批 2026-09-16）——验证伞仓编排层
# 「只派生跨仓键+调子仓脚本」，不再内联服务部署细节。
#
# 手法：sed 提取 deploy_manager/deploy_web/deploy_engine 函数体 eval 执行；
# 子仓 deploy.sh 以记录桩替换（写实参+关键 env 到 LOG）；docker/pct/curl PATH 桩。
# 锁定：P1 ensure-config 调用 / P2 manager .env 派生+子仓 deploy 调用 /
#       P3 web 子仓调用（BUILD/ENV_FILE/REMOTE 传递）/ P4 engine 子仓调用（ENV_FILE 直通）/
#       P5 结构断言（伞仓无内联 migrate/config JSON/deploy-prod 残留）/
#       P6 load_env 首跑 token 收养（共享 manager 在跑→沿用；无实例/含元字符→随机）。
# 用法：bash deploy/tests/production_deploy_cases.sh（伞仓根执行）
set -u
root="$(cd "$(dirname "$0")/../../.." && pwd)"
pd="$root/deploy/internal/production-deploy.sh"
FAILED=0
fail() { echo "FAIL: $1" >&2; FAILED=1; }
pass() { echo "ok: $1"; }

bash -n "$pd" || fail "bash -n production-deploy.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/log"

stub() { cat > "$tmp/bin/$1" && chmod +x "$tmp/bin/$1"; }
stub docker <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "${LOGFILE:-/dev/null}"
case "$1" in
    image) [ "$2" = inspect ] && echo img-id ;;
    inspect)
        # P6 收养桩：ADOPT_STUB 置值且查询 openshell-manager 时回报在跑 token
        if [ -n "${ADOPT_STUB:-}" ] && [ "$2" = openshell-manager ]; then
            printf 'PATH=/usr/bin\nOPENSHELL_MANAGER_TOKEN=%s\n' "$ADOPT_STUB"
        fi ;;
    *) exit 0 ;;
esac
EOF
stub pct <<'EOF'
#!/usr/bin/env bash
cat > /dev/null; printf 'pct %s\n' "$*" >> "${LOGFILE:?}"; exit 0
EOF
stub curl <<'CSTUB'
#!/usr/bin/env bash
for a in "$@"; do case "$a" in */v1/*) echo 401; exit 0 ;; esac; done
prev=""
for a in "$@"; do
    [ "$prev" = "-w" ] && { echo 200; exit 0; }
    prev="$a"
done
echo '<html><body><div id="root"></div><script src="/assets/index-abc123.js"></script></body></html>'
CSTUB
export PATH="$tmp/bin:$PATH"

# 记录桩：三个子仓 deploy.sh（写"动作 + 关键 env"到 RECORDER）
recorder() { cat > "$1" <<RSTUB
#!/usr/bin/env bash
printf '%s %s BUILD=%s REMOTE=[%s] ENV_FILE=%s DEPLOY_DIR=%s\n' \\
    "\$(basename "\${SRC:-?}")" "\${1:-deploy}" "\${BUILD:-}" "\${REMOTE-<unset>}" "\${ENV_FILE:-}" "\${DEPLOY_DIR:-}" >> "\$RECORDER"
exit 0
RSTUB
chmod +x "$1"; }
fake="$tmp/root"
mkdir -p "$fake/engine/deploy" "$fake/web" "$fake/manager/deploy" "$fake/deploy"
recorder "$fake/engine/deploy/deploy.sh"
recorder "$fake/web/deploy.sh"
recorder "$fake/manager/deploy/deploy.sh"
git -C "$tmp" init -q 2>/dev/null; (cd "$fake/engine" && git init -q && git commit -q --allow-empty -m x) 2>/dev/null
: > "$fake/engine/docker-compose.yml"
export RECORDER="$tmp/log/recorder"; : > "$RECORDER"
printf 'OPENSHELL_MANAGER_TOKEN=tok=pad==\nOPENSHELL_GATEWAY_ENDPOINT=host.docker.internal:8080\nCODEAUDIT_HOST_GATEWAY=8090\n' > "$fake/prod.env"
export ENV_FILE="prod.env"   # production-deploy 内部以 $ROOT/$ENV_FILE 拼接——须相对

# 公共桩：编排层健康门/通知链（本关卡不测其内部）
say() { echo "[say] $*"; }
die() { echo "[die] $*" >&2; exit 1; }
wait_http() { return 0; }
gate_service_plane() { return 0; }
ensure_notification_chain() { return 0; }
engine_images_present() { return 0; }   # 走 BUILD 判定稳定分支
ROOT="$fake"
export PYTHON=python3

# --- P1+P2：deploy_manager —— ensure-config 调用 + .env 派生 + 子仓 deploy ----
: > "$RECORDER"
( cd "$fake" && eval "$(sed -n '/^deploy_manager()/,/^}$/p' "$pd")" && deploy_manager ) >"$tmp/log/p2.out" 2>&1
rc=$?
grep -q 'manager ensure-config' "$RECORDER" \
    || fail "P1: 未调子仓 ensure-config: $(cat "$RECORDER")"
grep -Eq 'manager deploy BUILD= REMOTE=\[\]' "$RECORDER" \
    && pass "P2: manager 经子仓 deploy（REMOTE 空串）" \
    || fail "P2: manager 子仓调用形态异常: $(cat "$RECORDER")"
grep -q '^OPENSHELL_MANAGER_TOKEN=tok=pad==$' "$fake/manager/.env" 2>/dev/null \
    && pass "P2b: manager/.env 从 ENV_FILE 派生（padding 完整）" \
    || fail "P2b: manager/.env 派生异常: $(cat "$fake/manager/.env" 2>/dev/null)"
[ "$rc" -eq 0 ] || fail "P2: deploy_manager rc=$rc: $(tail -3 "$tmp/log/p2.out")"

# --- P3：deploy_web —— 子仓调用 + BUILD/ENV_FILE 传递 + 无内联 migrate -------
: > "$RECORDER"
( cd "$fake" && eval "$(sed -n '/^deploy_web()/,/^}$/p' "$pd")" && deploy_web ) >"$tmp/log/p3.out" 2>&1
rc=$?
grep -Eq 'web deploy BUILD=[01] REMOTE=\[\]' "$RECORDER" \
    && pass "P3: web 经子仓 deploy（BUILD 传递+REMOTE 空串）" \
    || fail "P3: web 子仓调用形态异常: $(cat "$RECORDER")"
grep -q "ENV_FILE=$fake/prod.env" "$RECORDER" \
    && pass "P3b: ENV_FILE 直通 web 子仓" \
    || fail "P3b: ENV_FILE 未传递: $(cat "$RECORDER")"
[ "$rc" -eq 0 ] || fail "P3: deploy_web rc=$rc: $(tail -3 "$tmp/log/p3.out")"

# --- P4：deploy_engine —— 子仓调用 + ENV_FILE 直通 + promote 判定 ------------
: > "$RECORDER"
( cd "$fake" && eval "$(sed -n '/^deploy_engine()/,/^}$/p' "$pd")" && BUILD=1 deploy_engine ) >"$tmp/log/p4.out" 2>&1
rc=$?
grep -q "engine deploy.*ENV_FILE=$fake/prod.env" "$RECORDER" \
    && pass "P4: engine 经子仓 deploy（ENV_FILE 直通）" \
    || fail "P4: engine 子仓调用形态异常: $(cat "$RECORDER")"
[ "$rc" -eq 0 ] || fail "P4: deploy_engine rc=$rc: $(tail -3 "$tmp/log/p4.out")"

# --- P5：结构断言 —— 伞仓不再内联服务部署细节 --------------------------------
grep -q 'label=com.docker.compose.project=web' "$pd" \
    && fail "P5: 伞仓仍内联 web 迁移守卫（应归 web/deploy.sh）" \
    || pass "P5a: 伞仓无内联 web migrate"
grep -q '"tokenFile"' "$pd" \
    && fail "P5: 伞仓仍内联 manager config JSON" \
    || pass "P5b: 伞仓无内联 config JSON"
grep -q 'deploy/prod/' "$pd" \
    && fail "P5: production-deploy 仍引用 deploy/prod/（ADR-235 残留）" \
    || pass "P5c: 无 deploy/prod/ 残留引用（production.env 为不同路径）"
grep -q '"tokenFile"' "$root/deploy/internal/build.sh" \
    && fail "P5: build.sh 仍内联 config JSON" \
    || pass "P5d: build.sh 无内联 config JSON"

# --- P6：load_env 首跑 token 收养（2026-09-17：一键链不得轮换共享 manager 凭据）---
# 桩：docker inspect 经 PATH 桩 ADOPT_STUB 模拟在跑 openshell-manager；fp/source_env
# 内联重定义（单行函数，sed 行提取会吞后续函数体，不走 eval 提取）。
mkdir -p "$tmp/env"
(
    eval "$(sed -n '/^adopt_manager_token()/,/^}$/p' "$pd")"
    eval "$(sed -n '/^load_env()/,/^}$/p' "$pd")"
    fp() { printf '%s' "$1" | sha256sum | cut -c1-8; }
    source_env() { set -a; . "$ENV_FILE"; set +a; }
    access_ip() { echo 192.0.2.10; }
    export TEMPLATE="$root/deploy/internal/production.env.template"

    export ADOPT_STUB="adopted-tok-123" ENV_FILE="$tmp/env/adopt.env"
    load_env >"$tmp/log/p6a.out" 2>&1
    grep -q '^OPENSHELL_MANAGER_TOKEN=adopted-tok-123$' "$ENV_FILE" 2>/dev/null \
        && pass "P6: 宿主在跑 manager → 首跑收养其 token" \
        || fail "P6: 收养失败: $(grep OPENSHELL_MANAGER_TOKEN "$ENV_FILE" 2>/dev/null)"
    grep -Eq '^CODEAUDIT_JWT_SECRET=[0-9a-f]{48}$' "$ENV_FILE" 2>/dev/null \
        || fail "P6a: 收养档 JWT 应仍随机 48hex"

    unset ADOPT_STUB
    export ENV_FILE="$tmp/env/fresh.env"
    load_env >"$tmp/log/p6b.out" 2>&1
    grep -Eq '^OPENSHELL_MANAGER_TOKEN=[0-9a-f]{48}$' "$ENV_FILE" 2>/dev/null \
        && pass "P6b: 无在跑 manager → 随机 48hex" \
        || fail "P6b: 随机档异常: $(grep OPENSHELL_MANAGER_TOKEN "$ENV_FILE" 2>/dev/null)"

    export ADOPT_STUB='bad|tok' ENV_FILE="$tmp/env/meta.env"
    load_env >"$tmp/log/p6c.out" 2>&1
    grep -Eq '^OPENSHELL_MANAGER_TOKEN=[0-9a-f]{48}$' "$ENV_FILE" 2>/dev/null \
        && pass "P6c: 收养值含 sed 元字符 → 弃收养走随机" \
        || fail "P6c: 元字符防护异常: $(grep OPENSHELL_MANAGER_TOKEN "$ENV_FILE" 2>/dev/null)"
)

# --- P7：provision_baton —— baton 供给接线（2026-09-19 io-audit 批次1）--------
# 供给脚本以记录桩替换：断言 REMOTE 空串本机直投契约 / BATON_PROVISION=0 旁路 /
# rc=3 前置缺失警告跳过不阻塞 / rc=1 fail-loud / cmd_deploy 顺序 engine 前置 /
# build.sh 构建批末尾同款接线。
mkdir -p "$fake/deploy/internal"
cat > "$fake/deploy/internal/baton-provision.sh" <<'BPSTUB'
#!/usr/bin/env bash
printf 'baton-provision REMOTE=[%s] args=[%s]\n' "${REMOTE-<unset>}" "$*" >> "$RECORDER"
[ -n "${BP_STUB_RC:-}" ] && exit "$BP_STUB_RC"
exit 0
BPSTUB
chmod +x "$fake/deploy/internal/baton-provision.sh"

pb_body="$(sed -n '/^provision_baton()/,/^}$/p' "$pd")"
[ -n "$pb_body" ] || fail "P7: production-deploy.sh 缺 provision_baton 函数"

: > "$RECORDER"
( cd "$fake" && eval "$pb_body" && provision_baton ) >"$tmp/log/p7.out" 2>&1
rc=$?
grep -q 'baton-provision REMOTE=\[\] args=\[\]' "$RECORDER" \
    && pass "P7: engine 前置供给（REMOTE 空串本机直投契约）" \
    || fail "P7: 供给调用形态异常: $(cat "$RECORDER")"
[ "$rc" -eq 0 ] || fail "P7: provision_baton rc=$rc: $(tail -3 "$tmp/log/p7.out")"

: > "$RECORDER"
( cd "$fake" && eval "$pb_body" && BATON_PROVISION=0 provision_baton ) >"$tmp/log/p7b.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && [ ! -s "$RECORDER" ] \
    && pass "P7b: BATON_PROVISION=0 旁路（不调供给脚本）" \
    || fail "P7b: 旁路异常 rc=$rc rec=$(cat "$RECORDER")"

( cd "$fake" && eval "$pb_body" && BP_STUB_RC=3 provision_baton ) >"$tmp/log/p7c.out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && grep -q '跳过' "$tmp/log/p7c.out" \
    && pass "P7c: 前置缺失（rc=3）→ 警告跳过不阻塞（诚实降级）" \
    || fail "P7c: 降级语义异常 rc=$rc: $(tail -3 "$tmp/log/p7c.out")"

( cd "$fake" && eval "$pb_body" && BP_STUB_RC=1 provision_baton ) >"$tmp/log/p7d.out" 2>&1
rc=$?
[ "$rc" -ne 0 ] \
    && pass "P7d: 供给真失败（rc=1）→ fail-loud" \
    || fail "P7d: 失败被吞（rc=0）"

cmd_body="$(sed -n '/^cmd_deploy()/,/^}$/p' "$pd")"
lm=$(printf '%s\n' "$cmd_body" | grep -n '^    deploy_manager$' | cut -d: -f1)
lp=$(printf '%s\n' "$cmd_body" | grep -n '^    provision_baton$' | cut -d: -f1)
le=$(printf '%s\n' "$cmd_body" | grep -n '^    deploy_engine$' | cut -d: -f1)
[ -n "$lp" ] && [ -n "$lm" ] && [ -n "$le" ] && [ "$lp" -gt "$lm" ] && [ "$lp" -lt "$le" ] \
    && pass "P7e: cmd_deploy 顺序 manager → baton 供给 → engine" \
    || fail "P7e: cmd_deploy 接线顺序异常 lm=$lm lp=$lp le=$le"

grep -q 'deploy/internal/baton-provision.sh' "$root/deploy/internal/build.sh" \
    && grep -q 'BATON_PROVISION' "$root/deploy/internal/build.sh" \
    && pass "P7f: build.sh 构建批末尾同款接线（供给+旁路键）" \
    || fail "P7f: build.sh 缺 baton 供给接线/旁路键"

if [ "$FAILED" -ne 0 ]; then
    echo "production_deploy_cases: FAILED"
    exit 1
fi
echo "production_deploy_cases: ALL GREEN"
