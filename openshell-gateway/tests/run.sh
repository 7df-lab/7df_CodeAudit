#!/usr/bin/env bash
# =============================================================================
# run.sh — openshell-gateway 离线测试套
#
# 本仓是部署事实源（配方+两脚本），无网关源码；测试分四层：
#   A 静态解析  —— TOML/compose parse、全键断言（U6/LESSONS#8 门禁，--fast 含）
#   B 契约      —— REMOTE 空串本机契约（回归 R1）、lifecycle 子命令行为
#   C/D/E 行为级 —— stub docker/pct + 真 sed/grep/awk 走通 ensure/patch/push
#                   全链路（无 docker、无 LXC、无网络）；跨文件一致性断言
#   G 运行时    —— --with-runtime 才跑：真 REMOTE 打 LXC 的 check/verify/status
#
# 用法:
#   bash tests/run.sh                 # 全量离线（默认）
#   bash tests/run.sh --fast          # 静态层（秒级，pre-commit 钩子用）
#   bash tests/run.sh --selfcheck     # 变异自检：对 10 个历史缺陷逐一注入
#                                     #   临时副本，证明测试套必然拦截
#   bash tests/run.sh --with-runtime  # 追加 G 段（需 pct/107 可达，只读动作）
# 退出码: 0=全过；非0=有失败（明细见输出）
# =============================================================================
set -u
ROOT="${GATEWAY_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
STUBS="$ROOT/tests/stubs"
COMPOSE="$ROOT/docker-compose.yml"
TOML="$ROOT/gateway.toml"
DEPLOY_SH="$ROOT/deploy.sh"
LIFE_SH="$ROOT/gateway_lifecycle.sh"

PASS=0; FAIL=0; FAILED=()

ok()  { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad() { echo "  ✗ $1${2:+ — $2}"; FAIL=$((FAIL+1)); FAILED+=("$1"); }
t() { local name="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name"; fi; }

WORK="$(mktemp -d)"
# trap 里禁用 kill 0（未设 LISTENER_PID 时会杀整个进程组，连父 shell 一起带走）
trap 'rm -rf "$WORK"; [ -n "${LISTENER_PID:-}" ] && kill "$LISTENER_PID" 2>/dev/null' EXIT
STUB_LOG="$WORK/stub.log"; : > "$STUB_LOG"
STUB_CTRL="$WORK/stub.ctrl"; : > "$STUB_CTRL"
export STUB_LOG STUB_CTRL

# ---- 公共工具 ---------------------------------------------------------------

toml_py() { # toml_py <python表达式，d=解析后的TOML对象> → 打印求值结果
  python3 -c '
import tomllib,sys
with open(sys.argv[1],"rb") as f: d=tomllib.load(f)
r=eval(sys.argv[2])
print(r if r is not None else "")
' "$TOML" "$1"
}

start_listener() { # 真 TCP 监听器：供 wait_liveness 的 /dev/tcp 探测连
  python3 - "$WORK/lport" <<'PY' &
import socket, sys, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 0)); s.listen(64)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
deadline = time.time() + 300
while time.time() < deadline:
    try:
        c, _ = s.accept(); c.close()
    except OSError:
        break
PY
  LISTENER_PID=$!
  for _ in $(seq 1 50); do [ -s "$WORK/lport" ] && break; sleep 0.1; done
  LIVENESS_PORT="$(cat "$WORK/lport")"
}

new_case() { # new_case <name> —— 每个行为级用例：新 fixture 目录 + 清桩日志
  CASE_DIR="$WORK/$1"; rm -rf "$CASE_DIR"; mkdir -p "$CASE_DIR/deploy" "$CASE_DIR/jwt"
  : > "$STUB_LOG"; : > "$STUB_CTRL"
}

# =============================================================================
# A 静态解析层（--fast 含）
# =============================================================================

sec_a() {
  echo "[A] 静态解析（U6：结构化配置 parse + 全键口径）"

  t "T-A1a gateway.toml tomllib parse 通过（重键即失败）" \
    python3 -c 'import tomllib,sys; tomllib.load(open(sys.argv[1],"rb"))' "$TOML"

  python3 - "$TOML" <<'PY'
import tomllib, sys, re
with open(sys.argv[1], "rb") as f: d = tomllib.load(f)
text = open(sys.argv[1]).read()
fails = []
def t(name, cond, detail=""):
    print(f"  {'✓' if cond else '✗'} {name}"+(f" — {detail}" if not cond else ""))
    if not cond: fails.append(name)
g = d["openshell"]["gateway"]; dr = d["openshell"]["drivers"]["docker"]
jwt = g["gateway_jwt"]
t("T-A1b version = 1", d["openshell"]["version"] == 1)
t("T-A1c server_sans = [\"*.sandbox.codeaudit.internal\"]（路由域口径）",
  g.get("server_sans") == ["*.sandbox.codeaudit.internal"], repr(g.get("server_sans")))
t("T-A1d bind_address = 127.0.0.1:8080", g.get("bind_address") == "127.0.0.1:8080",
  repr(g.get("bind_address")))
t("T-A1e health_bind_address = 127.0.0.1:8081", g.get("health_bind_address") == "127.0.0.1:8081")
t("T-A1f compute_drivers = [docker]", g.get("compute_drivers") == ["docker"])
t("T-A1g disable_tls = true", g.get("disable_tls") is True)
t("T-A1h grpc_endpoint 同号回调契约",
  dr.get("grpc_endpoint") == "http://host.openshell.internal:8080", repr(dr.get("grpc_endpoint")))
t("T-A1i allow_unauthenticated_users = true（部署链未配鉴权）",
  g.get("auth", {}).get("allow_unauthenticated_users") is True)
t("T-A1j supervisor_image 带 :local tag 无 digest（retag 契约前提）",
  dr.get("supervisor_image") == "ghcr.io/nvidia/openshell/supervisor:local",
  repr(dr.get("supervisor_image")))
t("T-A1k default_image = base:latest",
  dr.get("default_image") == "ghcr.io/nvidia/openshell-community/sandboxes/base:latest")
t("T-A1l sandbox_namespace = openshell", dr.get("sandbox_namespace") == "openshell")
t("T-A1m image_pull_policy = IfNotPresent", dr.get("image_pull_policy") == "IfNotPresent")
t("T-A1n jwt 三路径同目录且在 /var/lib/openshell bind 内",
  jwt.get("signing_key_path") == "/var/lib/openshell/tls/jwt/signing.pem"
  and jwt.get("public_key_path") == "/var/lib/openshell/tls/jwt/public.pem"
  and jwt.get("kid_path") == "/var/lib/openshell/tls/jwt/kid")
t("T-A1o jwt ttl = 3600", jwt.get("ttl_secs") == 3600)
t("T-A1p TOML 不含 DB_URL（env-only 禁令，防密钥入库）",
  "db_url" not in text.lower())
t("T-A2  server_sans 恰好一行（多行=ensure 只改首行的暗雷）",
  len(re.findall(r"^server_sans\s*=", text, re.M)) == 1)
t("T-A1q 首行 provenance 标记在位（edit-here 纪律）",
  text.splitlines()[0].startswith("# managed by codeaudit-umbrella/openshell-gateway"))
sys.exit(1 if fails else 0)
PY
  if [ $? -eq 0 ]; then ok "T-A1/A2 TOML 电池"; else bad "T-A1/A2 TOML 电池（明细见上）"; fi

  python3 - "$COMPOSE" <<'PY'
import sys, yaml
class NoDup(yaml.SafeLoader): pass
def no_dup(loader, node, deep=False):
    seen = set()
    for k, _ in loader.construct_pairs(node, deep=True):
        if k in seen: raise ValueError(f"duplicate key: {k}")
        seen.add(k)
    return dict(loader.construct_pairs(node, deep=True))
NoDup.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, no_dup)
d = yaml.load(open(sys.argv[1]), Loader=NoDup)
fails = []
def t(name, cond, detail=""):
    print(f"  {'✓' if cond else '✗'} {name}"+(f" — {detail}" if not cond else ""))
    if not cond: fails.append(name)
s = d["services"]["gateway"]
t("T-A3a compose parse + 重键检测通过", True)
t("T-A3b command = []（清默认 CMD 让 TOML 接管，R2 核心）", s.get("command") == [],
  repr(s.get("command")))
t("T-A3c user = 0（distroless 无 passwd，写 /var/lib/openshell + docker.sock）",
  str(s.get("user")) == "0")
t("T-A3d image = gateway 上游镜像", str(s.get("image","")).startswith("ghcr.io/nvidia/openshell/gateway:"))
t("T-A3e restart = unless-stopped", s.get("restart") == "unless-stopped")
ports = [str(p) for p in s.get("ports", [])]
t("T-A3f 8080 宿主同号发布（沙箱回调路由的根）",
  "0.0.0.0:${OPENSHELL_PORT:-8080}:8080" in ports, repr(ports))
t("T-A3g 8081 health 映射在位", "0.0.0.0:${OPENSHELL_HEALTH_PORT:-8081}:8081" in ports)
vols = s.get("volumes", [])
def v_short(src, tgt):
    return any(isinstance(v, str) and v.split(":")[0] == src and v.split(":")[1] == tgt
               for v in vols)
def v_long(src, tgt):
    return any(isinstance(v, dict) and v.get("type") == "bind" and v.get("source") == src
               and v.get("target") == tgt for v in vols)
t("T-A3h 挂 docker.sock（DooD）", v_short("/var/run/docker.sock", "/var/run/docker.sock"))
t("T-A3i /var/lib/openshell 同路径 bind（宿主 daemon 解析 bind source 的前提）",
  v_long("/var/lib/openshell", "/var/lib/openshell"))
create_ok = any(isinstance(v, dict) and v.get("source") == "/var/lib/openshell"
                and v.get("bind", {}).get("create_host_path") is True for v in vols)
t("T-A3j create_host_path = true", create_ok)
toml_mount = [v for v in vols if isinstance(v, dict)
              and v.get("target") == "/etc/openshell/gateway.toml"]
t("T-A3k ./gateway.toml → /etc/openshell/gateway.toml 且只读",
  len(toml_mount) == 1 and toml_mount[0].get("source") == "./gateway.toml"
  and toml_mount[0].get("read_only") is True)
env = s.get("environment", {})
env = {k.split("=")[0]: k.split("=", 1)[1] for k in env} if isinstance(env, list) else env
t("T-A3l OPENSHELL_GATEWAY_CONFIG ↔ 挂载目标一致",
  env.get("OPENSHELL_GATEWAY_CONFIG") == "/etc/openshell/gateway.toml")
t("T-A3m DB_URL 走 env 且为 sqlite（TOML env-only 禁令的对偶）",
  str(env.get("OPENSHELL_DB_URL", "")).startswith("sqlite:/var/lib/openshell/gateway.db"))
t("T-A3n XDG_DATA_HOME = HOME = /var/lib/openshell（supervisor 缓存落 bind）",
  env.get("XDG_DATA_HOME") == "/var/lib/openshell" and env.get("HOME") == "/var/lib/openshell")
hosts = dict(h.split(":") for h in s.get("extra_hosts", []))
need = ["host.docker.internal", "host.openshell.internal"]
t("T-A3o extra_hosts 中性双别名全在且 → host-gateway（回调+宿主解析；lab 实验域已清, 2026-09-08）",
  all(hosts.get(h) == "host-gateway" for h in need), repr(sorted(hosts)))
net = (d.get("networks") or {}).get("default", {})
t("T-A3p 显式网络名 codeaudit-sandbox-gateway-net（伞仓前缀口径）",
  net.get("name") == "codeaudit-sandbox-gateway-net", repr(net))
sys.exit(1 if fails else 0)
PY
  if [ $? -eq 0 ]; then ok "T-A3 compose 电池"; else bad "T-A3 compose 电池（明细见上）"; fi

  t "T-A4  bash -n deploy.sh"              bash -n "$DEPLOY_SH"
  t "T-A5  bash -n gateway_lifecycle.sh"   bash -n "$LIFE_SH"
  if command -v shellcheck >/dev/null 2>&1; then
    t "T-A6  shellcheck 两脚本（error 级）" shellcheck -S error "$DEPLOY_SH" "$LIFE_SH"
  else
    echo "  ⊙ T-A6 shellcheck 未安装，跳过（不判失败）"
  fi
  for f in docker-compose.yml gateway.toml Dockerfile.gateway Dockerfile.supervisor; do
    t "T-A7  FILES 成员存在: $f" test -f "$ROOT/$f"
  done
}

# =============================================================================
# B 契约层：REMOTE 空串本机契约 + lifecycle 子命令行为
# =============================================================================

sec_b() {
  echo "[B] REMOTE 契约（回归 R1：空串=本机执行，`-` 非 `:-`）与子命令行为"

  t "T-B1a lifecycle 用减号缺省（空串不回落 pct）" \
    grep -qF 'REMOTE="${REMOTE-pct exec 107 --}"' "$LIFE_SH"
  t "T-B1b deploy.sh 用减号缺省（与 lifecycle 同语义）" \
    grep -qF 'REMOTE="${REMOTE-pct exec 107 --}"' "$DEPLOY_SH"
  t "T-B1c 两脚本均无 :- 形式的 REMOTE 缺省" \
    bash -c "! grep -qF 'REMOTE:-pct' '$DEPLOY_SH' '$LIFE_SH'"

  new_case b2
  cp "$TOML" "$CASE_DIR/deploy/gateway.toml"
  touch "$CASE_DIR/jwt/signing.pem"
  echo 'container_id=stub123' > "$STUB_CTRL"
  start_listener

  # REMOTE="" → 全程本机：桩 pct 绝不被调，桩 docker 被 compose 调
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" status ) >"$WORK/out" 2>&1
  RC=$?
  t "T-B2a REMOTE='' status 本机执行成功"             test $RC -eq 0
  t "T-B2b REMOTE='' 时桩 pct 未被调用"               bash -c "! grep -q '^pct ' '$STUB_LOG'"
  t "T-B2c compose 经桩 docker 以 DEPLOY_DIR 调用" \
    grep -qF "docker compose --project-directory $CASE_DIR/deploy" "$STUB_LOG"
  t "T-B2d status 打印路由域现值" \
    grep -qF 'server_sans = ["*.sandbox.codeaudit.internal"]' "$WORK/out"

  # REMOTE=pct 前缀（桩）→ 命令经桩透传真执行
  : > "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='pct exec 107 --' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" status ) >/dev/null 2>&1
  RC=$?
  t "T-B3a REMOTE=pct 前缀 status 成功"               test $RC -eq 0
  t "T-B3b 命令经桩 pct 透传（exec 107 -- sed …）"    grep -q '^pct exec 107 -- sed' "$STUB_LOG"

  # REMOTE 缺省（未设）→ 同 pct 前缀
  : > "$STUB_LOG"
  ( cd "$ROOT" && env -u REMOTE DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" status ) >/dev/null 2>&1
  RC=$?
  t "T-B4a REMOTE 缺省 status 成功"                   test $RC -eq 0
  t "T-B4b 缺省走 pct 前缀"                           grep -q '^pct exec 107 -- ' "$STUB_LOG"

  # verify：相符/不符两态
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" verify ) >/dev/null 2>&1
  RC=$?
  t "T-B5a verify 相符 → exit 0"                      test $RC -eq 0
  sed -i 's/sandbox\.codeaudit\.internal/old.example/' "$CASE_DIR/deploy/gateway.toml"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" verify ) >/dev/null 2>"$WORK/err"
  RC=$?
  t "T-B5b verify 路由域不符 → exit 1"                test $RC -eq 1
  t "T-B5c 错误指认现值并给出期望值" \
    grep -qF "expected '[\"*.sandbox.codeaudit.internal\"]'" "$WORK/err"

  # DOWN 探测（连不上的端口）
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=1 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" status ) >/dev/null 2>&1
  RC=$?
  t "T-B6  探测目标不通 → DOWN + exit 1"              test $RC -eq 1

  # logs / recreate / usage
  : > "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" logs 7 ) >/dev/null 2>&1
  t "T-B7  logs N → compose logs --tail=N"            grep -qF 'logs --tail=7 gateway' "$STUB_LOG"
  : > "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" recreate ) >"$WORK/out" 2>&1
  RC=$?
  t "T-B8a recreate 打 WARNING 且 exit 0"             test $RC -eq 0
  t "T-B8b recreate 警告在输出中"                     grep -q WARNING "$WORK/out"
  t "T-B8c recreate 走 up -d"                         grep -qF 'up -d gateway' "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='' bash "$LIFE_SH" nonsense ) >/dev/null 2>&1
  RC=$?
  t "T-B9  未知子命令 → usage exit 2"                 test $RC -eq 2
}

# =============================================================================
# C 行为级：ensure 全链路（自足三件套 + 钉域幂等）——stub docker + 真 patch
# =============================================================================

mk_fixture_toml() { # mk_fixture_toml <变体: ok|wrong|dup|notable>
  cp "$TOML" "$CASE_DIR/deploy/gateway.toml"
  case "$1" in
    wrong) sed -i 's/\["\*\.sandbox\.codeaudit\.internal"\]/["*.old.example"]/' \
             "$CASE_DIR/deploy/gateway.toml" ;;
    dup)   sed -i 's/\["\*\.sandbox\.codeaudit\.internal"\]/["*.old.example"]/' \
             "$CASE_DIR/deploy/gateway.toml"
           echo 'server_sans = ["*.older.example"]' >> "$CASE_DIR/deploy/gateway.toml" ;;
    notable) cat > "$CASE_DIR/deploy/gateway.toml" <<'EOF'
[openshell]
version = 1

[openshell.drivers.docker]
image_pull_policy = "IfNotPresent"
EOF
  esac
}

sec_c() {
  echo "[C] ensure 行为级全链路（stub docker/pct + 真 sed/grep/awk patch）"

  # ---- C1/C2 错域纠正 + 幂等钉住 ----
  new_case c1; mk_fixture_toml wrong; touch "$CASE_DIR/jwt/signing.pem"
  echo 'container_id=stub123' > "$STUB_CTRL"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >"$WORK/out" 2>&1
  RC=$?
  t "T-C1a ensure 纠正错域 → exit 0"                  test $RC -eq 0
  t "T-C1b TOML 改写为强制域" \
    grep -qF 'server_sans = ["*.sandbox.codeaudit.internal"]' "$CASE_DIR/deploy/gateway.toml"
  t "T-C1c 全文件恰好一行 server_sans" \
    test "$(grep -cE '^server_sans[[:space:]]*=' "$CASE_DIR/deploy/gateway.toml")" = 1
  t "T-C1d 改写留时间戳备份" \
    bash -c "compgen -G '$CASE_DIR/deploy/gateway.toml.bak.*' >/dev/null"
  t "T-C1e 钉域后 compose restart（保留规格口径）"    grep -qF 'restart gateway' "$STUB_LOG"
  t "T-C1f 容器在位时绝不 up -d"                      bash -c "! grep -qF 'up -d gateway' '$STUB_LOG'"
  local md5_before; md5_before="$(md5sum "$CASE_DIR/deploy/gateway.toml" | cut -d' ' -f1)"
  : > "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >"$WORK/out" 2>&1
  RC=$?
  t "T-C2a 二次 ensure 幂等 exit 0"                   test $RC -eq 0
  t "T-C2b 报 already enforced 不再 restart" \
    grep -q 'already enforced' "$WORK/out" && bash -c "! grep -qF 'restart gateway' '$STUB_LOG'"
  t "T-C2c 幂等：文件不再被改" \
    test "$md5_before" = "$(md5sum "$CASE_DIR/deploy/gateway.toml" | cut -d' ' -f1)"

  # ---- C3 双 server_sans 暗雷去重 ----
  new_case c3; mk_fixture_toml dup; touch "$CASE_DIR/jwt/signing.pem"
  echo 'container_id=stub123' > "$STUB_CTRL"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >"$WORK/out" 2>&1
  RC=$?
  t "T-C3a 双行输入 ensure 收敛 exit 0"               test $RC -eq 0
  t "T-C3b 去重后恰好一行且为强制域" \
    test "$(grep -cE '^server_sans[[:space:]]*=' "$CASE_DIR/deploy/gateway.toml")" = 1 \
    -a "$(grep -cF 'server_sans = ["*.sandbox.codeaudit.internal"]' "$CASE_DIR/deploy/gateway.toml")" = 1

  # ---- C4 无 [openshell.gateway] 表 → 报错拒钉 ----
  new_case c4; mk_fixture_toml notable; touch "$CASE_DIR/jwt/signing.pem"
  echo 'container_id=stub123' > "$STUB_CTRL"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>"$WORK/err"
  RC=$?
  t "T-C4a 无表 TOML ensure 失败退出"                 test $RC -ne 0
  t "T-C4b 报 no [openshell.gateway] table"           grep -qF 'no [openshell.gateway] table' "$WORK/err"

  # ---- C5 JWT 密钥自举（R3-2）----
  new_case c5; mk_fixture_toml ok
  echo 'container_id=stub123' > "$STUB_CTRL"          # JWT_DIR 故意无 signing.pem
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>&1
  RC=$?
  t "T-C5a JWT 缺失时 ensure 仍走通（自举）"          test $RC -eq 0
  t "T-C5b 触发一次性 generate-certs"                 grep -qF 'generate-certs' "$STUB_LOG"
  t "T-C5c generate-certs 参数指向 bind 内目录+正确 SAN" \
    grep -qF -- '--output-dir /var/lib/openshell/tls' "$STUB_LOG" \
    -a grep -qF -- '--server-san host.openshell.internal' "$STUB_LOG"
  : > "$STUB_LOG"
  touch "$CASE_DIR/jwt/signing.pem"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>&1
  t "T-C5d 密钥已在 → 不再 generate-certs（幂等）"    bash -c "! grep -qF generate-certs '$STUB_LOG'"

  # ---- C6 supervisor 镜像自举（R3-3）----
  new_case c6; mk_fixture_toml ok; touch "$CASE_DIR/jwt/signing.pem"
  { echo 'container_id=stub123'; echo 'supervisor_present=0'; echo 'pull_local_fails=1'; } \
    > "$STUB_CTRL"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>&1
  RC=$?
  t "T-C6a :local 缺失+上游404 → ensure 走通"         test $RC -eq 0
  t "T-C6b 拉同批 :latest 并 retag 成配置名" \
    grep -qF 'docker tag ghcr.io/nvidia/openshell/supervisor:latest ghcr.io/nvidia/openshell/supervisor:local' "$STUB_LOG"
  sed -i 's/^supervisor_present=.*/supervisor_present=1/' "$STUB_CTRL"  # retag 后镜像已在
  : > "$STUB_LOG"
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>&1
  t "T-C6c 镜像已在 → 不再 pull/tag（幂等）"          bash -c "! grep -qE 'docker (pull|tag)' '$STUB_LOG'"

  # ---- C7 容器缺失自举（R3-1）----
  new_case c7; mk_fixture_toml ok; touch "$CASE_DIR/jwt/signing.pem"
  : > "$STUB_CTRL"                                    # container_id 空 = 容器缺失
  ( cd "$ROOT" && REMOTE='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$LIFE_SH" ensure ) >/dev/null 2>&1
  RC=$?
  t "T-C7a 容器缺失 ensure 先 up -d 走通"             test $RC -eq 0
  t "T-C7b up -d 紧随容器探测、先于 liveness（时序契约）" \
    bash -c "grep -A1 -F 'ps -q gateway' '$STUB_LOG' | tail -1 | grep -qF 'up -d gateway'"
}

# =============================================================================
# D 跨文件一致性（内部接口 I4/I5/I6/I7 的静态守卫）
# =============================================================================

sec_d() {
  echo "[D] 跨文件一致性（镜像/JWT/端口三角/缺省矩阵/FILES）"
  local life_img comp_img life_dd deploy_dd jwt_dir toml_jwt sup_ref
  life_img="$(sed -n 's/^GATEWAY_IMAGE="${GATEWAY_IMAGE:-\(.*\)}"$/\1/p' "$LIFE_SH")"
  comp_img="$(sed -n 's/^ *image: \(.*\)$/\1/p' "$COMPOSE" | head -1 | tr -d '"')"
  comp_img="${comp_img/\$\{IMAGE_TAG:-latest\}/latest}"
  t "T-D1  GATEWAY_IMAGE 缺省 ↔ compose image 同源"   test "$life_img" = "$comp_img"

  t "T-D2  端口同号三角：TOML bind ↔ compose 宿主缺省 ↔ grpc_endpoint" \
    grep -qF 'bind_address        = "127.0.0.1:8080"' "$TOML" \
    -a grep -qF '"0.0.0.0:${OPENSHELL_PORT:-8080}:8080"' "$COMPOSE" \
    -a grep -qF 'grpc_endpoint     = "http://host.openshell.internal:8080"' "$TOML"

  jwt_dir="$(sed -n 's/^JWT_DIR="${JWT_DIR:-\(.*\)}"$/\1/p' "$LIFE_SH")"
  toml_jwt="$(sed -n 's|^signing_key_path = "\(.*\)/signing.pem"$|\1|p' "$TOML")"
  t "T-D3  JWT_DIR 缺省 ↔ TOML 密钥路径同目录且在 bind 内" \
    test "$jwt_dir" = "$toml_jwt" -a "$jwt_dir" = "/var/lib/openshell/tls/jwt"

  local life_remote deploy_remote
  life_remote="$(grep -cF 'REMOTE="${REMOTE-pct exec 107 --}"' "$LIFE_SH")"
  deploy_remote="$(grep -cF 'REMOTE="${REMOTE-pct exec 107 --}"' "$DEPLOY_SH")"
  life_dd="$(sed -n 's/^DEPLOY_DIR="${DEPLOY_DIR:-\(.*\)}"$/\1/p' "$LIFE_SH")"
  deploy_dd="$(sed -n 's/^DEPLOY_DIR="${DEPLOY_DIR:-\(.*\)}"$/\1/p' "$DEPLOY_SH")"
  t "T-D4  两脚本 REMOTE 契约与 DEPLOY_DIR 缺省逐字一致（I7）" \
    test "$life_remote" = 1 -a "$deploy_remote" = 1 -a "$life_dd" = "$deploy_dd"

  t "T-D5  deploy.sh FILES 清单恰为 4 部署文件（I6）" \
    grep -qF 'FILES=(docker-compose.yml gateway.toml Dockerfile.gateway Dockerfile.supervisor)' "$DEPLOY_SH"

  sup_ref="$(toml_py 'd["openshell"]["drivers"]["docker"]["supervisor_image"]')"
  t "T-D6  supervisor retag 推导可产 :latest（I4 前提）" \
    test "${sup_ref%:*}:latest" = "ghcr.io/nvidia/openshell/supervisor:latest"
}

# =============================================================================
# E deploy.sh 行为级（stub pct push + 差量→bak→push→ensure 全时序）
# =============================================================================

sec_e() {
  echo "[E] deploy.sh 行为级（REMOTE='' 本机契约 + 差量下发时序）"

  new_case e
  for f in docker-compose.yml gateway.toml Dockerfile.gateway Dockerfile.supervisor; do
    cp "$ROOT/$f" "$CASE_DIR/deploy/"
  done
  touch "$CASE_DIR/jwt/signing.pem"
  echo 'container_id=stub123' > "$STUB_CTRL"

  ( cd "$ROOT" && REMOTE='' VMID='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$DEPLOY_SH" ) >"$WORK/out" 2>&1
  RC=$?
  t "T-E1a 同步态 deploy → in sync + ensure 收尾 exit 0" \
    test $RC -eq 0 -a "$(grep -cF 'in sync, ensure only' "$WORK/out")" = 1
  t "T-E1b 无文件被推"                                bash -c "! grep -q pushed '$WORK/out'"

  # 漂移：远端副本（fixture）被改 → check 报告不改 → deploy 留 bak→push→ensure 自愈
  sed -i 's/sandbox\.codeaudit\.internal/old.example/' "$CASE_DIR/deploy/gateway.toml"
  ( cd "$ROOT" && REMOTE='' VMID='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    PATH="$STUBS:$PATH" bash "$DEPLOY_SH" check ) >"$WORK/out" 2>&1
  RC=$?
  t "T-E2a check 报 drift 且 exit 0（漂移≠失败）" \
    test $RC -eq 0 -a "$(grep -cF 'drift: gateway.toml' "$WORK/out")" = 1
  ( cd "$ROOT" && REMOTE='' VMID='' DEPLOY_DIR="$CASE_DIR/deploy" JWT_DIR="$CASE_DIR/jwt" \
    LIVENESS_HOST=127.0.0.1 LIVENESS_PORT=$LIVENESS_PORT LIVENESS_TIMEOUT_SECS=10 \
    PATH="$STUBS:$PATH" bash "$DEPLOY_SH" deploy ) >"$WORK/out" 2>&1
  RC=$?
  t "T-E2b deploy 推送漂移文件 exit 0"                test $RC -eq 0
  t "T-E2c 推送前远端留 .bak 时间戳备份" \
    bash -c "compgen -G '$CASE_DIR/deploy/gateway.toml.bak.*' >/dev/null"
  t "T-E2d 文件经桩 pct push 下发"                    grep -qF 'pushed: gateway.toml' "$WORK/out"
  t "T-E2e 下发后远端副本与仓内一致（自愈）" \
    bash -c "cmp -s '$ROOT/gateway.toml' '$CASE_DIR/deploy/gateway.toml'"
  t "T-E2f 未漂移文件不重推"                          test "$(grep -cF 'pushed:' "$WORK/out")" = 1

  ( cd "$ROOT" && REMOTE='' bash "$DEPLOY_SH" nonsense ) >/dev/null 2>&1
  RC=$?
  t "T-E3  未知子命令 → usage exit 2"                 test $RC -eq 2
}

# =============================================================================
# G 运行时门禁（--with-runtime；真 REMOTE，只读动作）
# =============================================================================

sec_g() {
  echo "[G] 运行时门禁（真 LXC；check/verify/status 均只读）"
  if ! command -v pct >/dev/null 2>&1; then
    echo "  ⊙ SKIP：本机无 pct（未在 PVE 宿主），运行时门禁未验——如实标注，不冒充通过"
    return 0
  fi
  t "T-G1  deploy.sh check（漂移报告）" \
    env REMOTE="${REMOTE:-pct exec 107 --}" LIVENESS_TIMEOUT_SECS="${LIVENESS_TIMEOUT_SECS:-60}" \
    bash "$DEPLOY_SH" check
  t "T-G2  gateway_lifecycle.sh verify" \
    env REMOTE="${REMOTE:-pct exec 107 --}" LIVENESS_TIMEOUT_SECS="${LIVENESS_TIMEOUT_SECS:-60}" \
    bash "$LIFE_SH" verify
  t "T-G3  gateway_lifecycle.sh status" \
    env REMOTE="${REMOTE:-pct exec 107 --}" LIVENESS_TIMEOUT_SECS="${LIVENESS_TIMEOUT_SECS:-60}" \
    bash "$LIFE_SH" status
}

# =============================================================================
# selfcheck：变异注入自检（防回归机制的证明层）
# =============================================================================

selfcheck() {
  echo "[S] 变异自检：向临时副本注入历史缺陷，测试套必须拦截"
  local rc=0
  inject() { # inject <id> <文件> <sed表达式> <描述>
    local mut="$WORK/mut-$1"; rm -rf "$mut"; mkdir -p "$mut"
    cp -a "$ROOT/." "$mut/"; rm -rf "$mut/.git" "$mut/tests"
    sed -i "$3" "$mut/$2"
    if GATEWAY_ROOT="$mut" bash "$0" --fast >/dev/null 2>&1; then
      echo "  ✗ M-$1 守卫失效：$4（注入后 --fast 仍通过！）"
      FAIL=$((FAIL+1)); FAILED+=("M-$1"); rc=1
    else
      echo "  ✓ M-$1 $4 → 按预期拦截"; PASS=$((PASS+1))
    fi
  }
  inject 1 docker-compose.yml 's/^    command: \[\]$/    command: ["--bind-address","0.0.0.0"]/' \
    "R2 注入：删 command:[] 让 CLI flags 压过 TOML（T-A3b 拦截）"
  inject 2 gateway.toml 's/\["\*\.sandbox\.codeaudit\.internal"\]/["*.wrong.example"]/' \
    "R4 注入：路由域漂移（T-A1c 拦截）"
  inject 3 gateway.toml '$a\server_sans = ["*.shadow.example"]' \
    "R4 注入：server_sans 双行暗雷（T-A2 拦截）"
  inject 4 gateway_lifecycle.sh 's/REMOTE="${REMOTE-pct/REMOTE="${REMOTE:-pct/' \
    "R1 注入：lifecycle 减号缺省退化（T-B1a 拦截）"
  inject 5 deploy.sh 's/REMOTE="${REMOTE-pct/REMOTE="${REMOTE:-pct/' \
    "R1 注入：deploy.sh 空串契约回归（T-B1b 拦截）"
  inject 6 docker-compose.yml 's/^    restart: unless-stopped$/    restart: unless-stopped\n    restart: unless-stopped/' \
    "R5 注入：compose 重键（T-A3a dup-loader 拦截）"
  inject 7 deploy.sh '$a\fi' \
    "R5 注入：bash 语法损坏（T-A4 拦截）"
  inject 8 docker-compose.yml 's/0\.0\.0\.0:\${OPENSHELL_PORT:-8080}:8080/0.0.0.0:\${OPENSHELL_PORT:-9090}:8080/' \
    "R6 注入：8080 宿主同号发布被改（T-A3f 拦截）"
  inject 9 gateway.toml 's|/var/lib/openshell/tls/jwt/signing.pem|/tmp/jwt/signing.pem|' \
    "R3 注入：JWT 密钥路径漂出 bind（T-A1n 拦截）"
  inject 10 docker-compose.yml 's/^        read_only: true$/        read_only: false/' \
    "I5 注入：TOML 挂载丢失只读（T-A3k 拦截）"
  echo "  （10 个变异全部被拦截 = 测试套自证明有效）"
  return $rc
}

# =============================================================================

MODE="${1:-all}"
case "$MODE" in
  --fast)
    sec_a; sec_d
    echo "[B-s] REMOTE 契约静态断言"
    t "T-B1a lifecycle 用减号缺省"  grep -qF 'REMOTE="${REMOTE-pct exec 107 --}"' "$LIFE_SH"
    t "T-B1b deploy.sh 用减号缺省"  grep -qF 'REMOTE="${REMOTE-pct exec 107 --}"' "$DEPLOY_SH"
    ;;
  --selfcheck)
    selfcheck
    ;;
  --with-runtime)
    sec_a; start_listener; sec_b; sec_c; sec_d; sec_e; sec_g
    ;;
  all)
    sec_a; start_listener; sec_b; sec_c; sec_d; sec_e
    echo "  ⊙ 运行时门禁（G 段）默认不跑：加 --with-runtime，或手动 ./deploy.sh --check && ./gateway_lifecycle.sh verify"
    ;;
  *)
    sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac

echo "———————————————————————————————"
echo "通过 $PASS / $((PASS+FAIL))"
if [ "$FAIL" -gt 0 ]; then
  echo "失败 ${FAIL} 项："; printf '  - %s\n' "${FAILED[@]}"
  exit 1
fi
exit 0
