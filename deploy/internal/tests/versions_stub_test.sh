#!/usr/bin/env bash
# =============================================================================
# versions_stub_test.sh — deploy/versions.sh 的 stub 自测（2026-09-16）
#
# 原理：versions.sh 的远端面全部经 `$REMOTE docker ...`（REMOTE 可覆写）——
# 本测试用假远端脚本喂确定性数据，断言表格行/结论/退出码；不触 107、不触
# docker（本机无 docker 也能跑）。覆盖三条路径：
#   ① 全对齐（engine 行"同步"）＋单镜像缺失（sast-adapter → 缺镜像结论）
#   ② web :latest+revision label 显示路径（Phase 2 前的生产列形态）
#   ③ daemon 不可达韧性（表格仍完整打出、exit 0）
# =============================================================================
set -u
cd "$(dirname "$0")/../../.."

fail=0
t_ok=0; t_no=0
assert_contains() {  # <haystack> <needle> <label>
    if grep -qF -- "$2" <<<"$1"; then t_ok=$((t_ok+1)); else
        t_no=$((t_no+1)); fail=1; echo "FAIL: $3（未命中: $2）"; fi
}
assert_eq() {  # <a> <b> <label>
    if [ "$1" = "$2" ]; then t_ok=$((t_ok+1)); else
        t_no=$((t_no+1)); fail=1; echo "FAIL: $3（'$1' != '$2'）"; fi
}

ESHA="$(git -C engine rev-parse --short HEAD)"
WSHA="$(git -C web rev-parse --short HEAD)"
export VST_ESHA="$ESHA" VST_WSHA="$WSHA"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ---- 假远端：`$REMOTE docker <sub...>` 的确定性应答 ---------------------------
cat > "$work/fake_remote.sh" <<'EOF'
#!/usr/bin/env bash
# 调用形态（versions.sh 侧）：docker version | docker image inspect [-f FMT] IMG | docker inspect -f FMT CTR
[ "$1" = docker ] || exit 1
sub="$2"; shift 2
if [ "$sub" = version ]; then exit 0; fi
# `image inspect ...` 形态多一层字面 inspect 子命令，先剥掉再统一解析
[ "${1:-}" = inspect ] && shift
fmt=""; ref=""
if [ "${1:-}" = "-f" ]; then fmt="$2"; ref="$3"; else ref="${1:-}"; fi

if [ "$sub" = image ]; then
    if [ -z "$fmt" ]; then  # 在位性探测
        case "$ref" in
            codeaudit-sast-adapter:*) exit 1;;  # 用例①：单镜像缺失
            *) exit 0;;
        esac
    fi
    case "$fmt" in
        '{{.Created}}') echo "2026-09-16T10:00:00Z";;
        *)  # label 探测
            case "$ref" in
                codeaudit-web:latest) echo "$VST_WSHA";;   # 用例②：revision label
                *) echo "feedface0000";;                        # inputs.digest
            esac;;
    esac
    exit 0
fi
if [ "$sub" = inspect ]; then  # 容器 -> Config.Image
    case "$ref" in
        codeaudit-sim-web) echo "codeaudit-web:$VST_WSHA";;  # 桥落地后 sim 引用 sha tag
        codeaudit-sim-*) svc="${ref#codeaudit-sim-}"; echo "codeaudit-${svc}:$VST_ESHA";;
        codeaudit-web) echo "codeaudit-web:latest";;   # 用例②：生产仍 :latest
        codeaudit-*)      svc="${ref#codeaudit-}";      echo "codeaudit-${svc}:$VST_ESHA";;
        openshell-manager) echo "openshell-manager:2.0.0";;
        openshell-gateway-gateway-1) echo "ghcr.io/nvidia/openshell/gateway:latest";;
        *) exit 1;;  # 无容器
    esac
    exit 0
fi
exit 1
EOF

# ---- 用例①②：确定性数据下的表格与结论 ----------------------------------------
out="$(REMOTE="bash $work/fake_remote.sh" bash deploy/internal/versions.sh)"; rc=$?
assert_eq "$rc" "0" "versions.sh 退出码恒 0（观测工具）"
assert_contains "$out" "engine/gateway" "engine 行在表"
assert_contains "$out" "engine/sast-adapter" "sast 行在表"
assert_contains "$out" "$ESHA ✓09-16" "镜像列=tag+构建日期"
assert_contains "$out" " web " "web 行在表"
assert_contains "$out" "2.0.0" "manager tag 显示（Phase 2 2b 后=tag 直显）"
assert_contains "$out" "字面/旧tag" "manager 非 sha tag 的同步判定注记（2b）"
assert_contains "$out" "缺镜像(sim-sync rebuild)" "缺失镜像结论"
assert_contains "$out" "同步" "对齐行结论=同步"
assert_contains "$out" "latest(rev=$WSHA" "web :latest 经 revision label 显示版本"
assert_contains "$out" "生产未升(latest(rev=$WSHA" "web 生产落后结论含 rev 形态"
assert_contains "$out" "inputs=feedface0000" "沙箱 inputs.digest 显示"
assert_contains "$out" "ghcr.io/nvidia/openshell/gateway:latest" "gateway 上游镜像显示"
sync_n="$(grep -c '同步' <<<"$out" || true)"
[ "$sync_n" -ge 6 ] && { t_ok=$((t_ok+1)); } || { t_no=$((t_no+1)); fail=1; echo "FAIL: 同步行数($sync_n)<6"; }

# ---- 用例③：daemon 不可达韧性 --------------------------------------------------
printf '#!/usr/bin/env bash\nexit 1\n' > "$work/dead_remote.sh"
out2="$(REMOTE="bash $work/dead_remote.sh" bash deploy/internal/versions.sh)"; rc2=$?
assert_eq "$rc2" "0" "daemon 不可达时退出码仍 0"
assert_contains "$out2" "不可达" "不可达警示在表"
assert_contains "$out2" "engine/gateway" "不可达时表格仍完整"

echo "----"
echo "versions stub test: $t_ok 过 / $t_no 失败"
exit "$fail"
