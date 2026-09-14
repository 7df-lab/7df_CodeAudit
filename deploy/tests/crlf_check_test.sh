#!/usr/bin/env bash
# deploy/tests/crlf_check_test.sh — crlf_check.sh 哨兵/修复行为合成仓测试
# （2026-09-13 审计 B2/B10 落地批：修复守卫内化 + 纯行尾判别）
#
# 用法: bash deploy/tests/crlf_check_test.sh   —— 纯本机 git（需 git≥2.16），无需 Windows
#
# 退出码契约（crlf_check.sh）: 0 干净/修复成功 | 1 哨兵命中 | 2 用法错误 | 3 真实内容脏拒修
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
CHECK="$HERE/../windows/crlf_check.sh"
[ -f "$CHECK" ] || { echo "crlf_check.sh 不存在: $CHECK" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "git 不可用" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
PASS=0; FAIL=0
ok()  { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad() { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
expect() { # expect <说明> <期望码> <实际码>
  if [ "$2" = "$3" ]; then ok "$1（exit=$3）"; else bad "$1：期望 exit=$2 实际 exit=$3"; fi
}
GIT="git -c user.email=t@t -c user.name=t -c protocol.file.allow=always"

# 合成仓：根仓含 a.sh + Dockerfile，子仓 sub 含 s.sh（全 LF 基线）
make_repo() { # make_repo <父目录>
  local p="$1"; mkdir -p "$p/mod"
  ( cd "$p/mod" && git init -q . && printf '#!/bin/sh\necho sub\n' > s.sh \
      && $GIT add s.sh && $GIT commit -qm init )
  ( cd "$p" && git init -q . && printf '#!/bin/sh\necho top\n' > a.sh \
      && printf 'FROM alpine\n' > Dockerfile \
      && $GIT add a.sh Dockerfile && $GIT commit -qm init \
      && $GIT submodule add -q ./mod sub && $GIT commit -qm sub )
}
crlfize() { sed -i 's/$/\r/' "$1"; }   # LF→CRLF 落盘（GNU sed \r=CR）
idx_blob() { ( cd "$2" && git cat-file blob ":$1" ); }  # index 里的 $1 内容

echo "== S0 用法错误 =="
sh "$CHECK" >/dev/null 2>&1; expect "S0 缺参退出 2" 2 $?

echo "== S1 干净树：哨兵零命中零输出 =="
R="$WORK/s1"; make_repo "$R"
out=$(sh "$CHECK" "$R"); expect "S1 哨兵干净退出 0" 0 $?
[ -z "$out" ] && ok "S1 输出为空" || bad "S1 应无输出：$out"

echo "== S2 根仓纯行尾脏（re-smudge 形态）：哨兵命中→修复归一不丢内容 =="
R="$WORK/s2"; make_repo "$R"; crlfize "$R/a.sh"
sh "$CHECK" "$R" >/dev/null 2>&1; expect "S2 哨兵命中退出 1" 1 $?
sh "$CHECK" --repair "$R" >/dev/null 2>&1; expect "S2 修复退出 0" 0 $?
idx_blob a.sh "$R" | cmp -s - "$R/a.sh" && ok "S2 修复后 a.sh=索引（LF）" || bad "S2 修复后 a.sh 与索引不一致"
sh "$CHECK" "$R" >/dev/null 2>&1; expect "S2 复查干净退出 0" 0 $?

echo "== S3 根仓真实内容脏：拒修 exit 3 且文件未被覆盖 =="
R="$WORK/s3"; make_repo "$R"; printf '#!/bin/sh\necho changed-top\n' > "$R/a.sh"
sh "$CHECK" --repair "$R" >/dev/null 2>&1; expect "S3 修复拒绝退出 3" 3 $?
grep -q changed-top "$R/a.sh" && ok "S3 用户改动未被覆盖" || bad "S3 用户改动被 checkout-index 抹掉"

echo "== S3b 暂存区真实脏（git add 后）：同样拒修 =="
R="$WORK/s3b"; make_repo "$R"; printf '#x\n' >> "$R/a.sh"; ( cd "$R" && $GIT add a.sh )
sh "$CHECK" --repair "$R" >/dev/null 2>&1; expect "S3b 暂存脏拒修退出 3" 3 $?

echo "== S4 子仓纯行尾脏：哨兵命中（[sub] 清单）→修复归一 =="
R="$WORK/s4"; make_repo "$R"; crlfize "$R/sub/s.sh"
out=$(sh "$CHECK" "$R"); expect "S4 哨兵命中退出 1" 1 $?
echo "$out" | grep -q '\[sub\]' && ok "S4 清单标注子仓来源" || bad "S4 清单缺 [sub] 标注：$out"
sh "$CHECK" --repair "$R" >/dev/null 2>&1; expect "S4 修复退出 0" 0 $?
idx_blob s.sh "$R/sub" | cmp -s - "$R/sub/s.sh" && ok "S4 子仓 s.sh=索引（LF）" || bad "S4 子仓 s.sh 与索引不一致"

echo "== S5 子仓真实内容脏：拒修 exit 3 =="
R="$WORK/s5"; make_repo "$R"; printf '#!/bin/sh\necho changed-sub\n' > "$R/sub/s.sh"
sh "$CHECK" --repair "$R" >/dev/null 2>&1; expect "S5 拒修退出 3" 3 $?
grep -q changed-sub "$R/sub/s.sh" && ok "S5 子仓改动未被覆盖" || bad "S5 子仓改动被抹掉"

echo "== S6 全链 re-smudge：autocrlf=true 克隆→命中→修复归一 =="
R="$WORK/s6"; make_repo "$R"
( cd "$WORK" && git -c protocol.file.allow=always -c core.autocrlf=true clone -q "$R" s6c )
file_has_cr() { grep -q $'\r' "$1"; }
if file_has_cr "$WORK/s6c/a.sh"; then ok "S6 re-smudge 复现（a.sh 含 CR）"; else bad "S6 克隆未复现 smudge（a.sh 无 CR）——场景不成立"; fi
sh "$CHECK" "$WORK/s6c" >/dev/null 2>&1; expect "S6 哨兵命中退出 1" 1 $?
sh "$CHECK" --repair "$WORK/s6c" >/dev/null 2>&1; expect "S6 修复退出 0" 0 $?
sh "$CHECK" "$WORK/s6c" >/dev/null 2>&1; expect "S6 复查干净退出 0" 0 $?
file_has_cr "$WORK/s6c/a.sh" && bad "S6 修复后仍含 CR" || ok "S6 修复后 a.sh 纯 LF"

echo ""
echo "================ crlf_check_test 结果 ================"
echo "通过=$PASS 失败=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
