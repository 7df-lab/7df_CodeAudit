#!/usr/bin/env bash
# sanitize.sh — 敏感信息清除与提交前门禁
#
# 用法:
#   bash sanitize.sh fix    # 把工作区里的真实内网地址/域名替换为占位符（幂等）
#   bash sanitize.sh check  # 提交前门禁：硬违例(退出码1) + 建议人工确认项
#   bash sanitize.sh map    # 本地查看映射表解码结果（勿截图/勿粘贴到外部）
#
# push 前的标准流程:  bash sanitize.sh fix && bash sanitize.sh check
# pre-commit 钩子已自动执行 fix→重新暂存→check（新克隆先: make hooks）。
#
# 安全设计: 映射键（真实值）以十六进制存储——本脚本会推送到公开仓库，
# 明文写入等于泄露。新增映射:  printf '%s' '真实值' | xxd -p | tr -d '\n'
# 然后向 HEXMAP 加一行 "<hex>=<占位符>"，注释写占位符含义（勿写真实值）。
# 注意: 本脚本所有扫描必须排除自身，否则 fix 会替换自己的映射表。

set -euo pipefail
cd "$(dirname "$0")"

SELF="sanitize.sh"

# ---- 映射表: hex(真实值)=占位符 ----
# gitlab.local      内网 GitLab
# gateway.internal  LXC 107 沙箱/生产宿主机（= gateway.internal 域名）
# pve.internal      PVE 宿主机
# proxy.internal    上网代理（GitHub/MCR 出口）
# *.internal        公司域名子域（xwpt 的占位域）
HEXMAP=(
  "3139322e3136382e302e323130=gitlab.local"
  "3139322e3136382e302e313232=gateway.internal"
  "3139322e3136382e302e323030=pve.internal"
  "3139322e3136382e302e313133=proxy.internal"
  "787770742e636f6d=internal"
)

# ---- 不允许入库的路径模式（连同 gitignore 双保险）----
FORBIDDEN_PATHS='(^deploy/env\.sim$|^deploy/production\.env$|^\.agent/(sessions|evidence)/)'

load_map() {
  declare -gA MAP=()
  local entry hex key
  for entry in "${HEXMAP[@]}"; do
    hex="${entry%%=*}"
    key="$(printf '%s' "$hex" | xxd -r -p)"
    MAP["$key"]="${entry#*=}"
  done
}

map_keys_regex() {
  local IFS='|'
  printf '%s' "${!MAP[*]}"
}

mode_map() {
  load_map
  local k
  for k in "${!MAP[@]}"; do printf '%s -> %s\n' "$k" "${MAP[$k]}"; done
}

mode_fix() {
  load_map
  local regex; regex=$(map_keys_regex)
  local files
  files=$(grep -rlIE "$regex" --exclude-dir=.git --exclude="$SELF" . 2>/dev/null || true)
  if [ -z "$files" ]; then
    echo "[fix] 工作区干净，无需替换"
    return 0
  fi
  local sed_expr="" k
  for k in "${!MAP[@]}"; do sed_expr+="; s/${k//./\\.}/${MAP[$k]}/g"; done
  # NUL 分隔直接管道给 xargs（经 $() 捕获会丢 NUL 字节，勿改回）
  grep -rlIZ -E "$regex" --exclude-dir=.git --exclude="$SELF" . 2>/dev/null \
    | xargs -0 -r sed -i "${sed_expr#;}"
  echo "[fix] 已替换以下文件中的敏感值:"
  printf '%s\n' "$files" | sed 's/^/       /'
  echo "[fix] 如这些文件已暂存，请重新 git add（pre-commit 钩子会自动做）"
}

mode_check() {
  load_map
  local rc=0 regex; regex=$(map_keys_regex)
  regex=${regex//./\\.}   # . → 字面点号

  echo "== 硬检查 1: 暂存区/工作区敏感值 =="
  local index_hits tree_hits
  index_hits=$(git grep --cached -nE "$regex" -- ':!'"$SELF" 2>/dev/null || true)
  tree_hits=$(grep -rnIE "$regex" --exclude-dir=.git --exclude="$SELF" . 2>/dev/null || true)
  if [ -n "$index_hits" ]; then
    echo "!! 暂存区发现敏感值（这些内容会进提交）:"
    printf '%s\n' "$index_hits" | head -50 | sed 's/^/     /'
    rc=1
  fi
  if [ -n "$tree_hits" ]; then
    echo "!! 工作区发现敏感值（先跑 bash sanitize.sh fix）:"
    printf '%s\n' "$tree_hits" | head -50 | sed 's/^/     /'
    rc=1
  fi
  if [ $rc -eq 0 ]; then echo "   通过"; fi

  echo "== 硬检查 2: 禁止入库的文件路径 =="
  local bad_paths
  bad_paths=$(git diff --cached --name-only 2>/dev/null | grep -E "$FORBIDDEN_PATHS" || true)
  if [ -n "$bad_paths" ]; then
    echo "!! 暂存区含禁止文件:"
    printf '%s\n' "$bad_paths" | sed 's/^/     /'
    rc=1
  else
    echo "   通过"
  fi

  echo "== 人工确认项（不阻断，逐条过目即可）=="
  local advisory
  advisory=$(grep -rnIE \
    -e '192\.168\.[0-9]+\.[0-9]+' \
    -e '[A-Za-z0-9._%+-]+@(qq|163|126|gmail|hotmail|outlook|foxmail)\.(com|net)' \
    -e 'BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY' \
    -e '\bsk-[A-Za-z0-9]{16,}' \
    --exclude-dir=.git \
    --exclude="$SELF" \
    . 2>/dev/null \
    | grep -vE '192\.168\.0\.0/16' || true)
  if [ -n "$advisory" ]; then
    printf '%s\n' "$advisory" | head -40 | sed 's/^/   ? /'
    echo "   （说明: 192.168.0.0/16 为 Docker 默认地址池的通用文档表述，已自动豁免；"
    echo "    测试夹具里的通用私网 IP 如 192.168.1.1 属正常，确认非真实基建即可）"
  else
    echo "   无待确认项"
  fi

  if [ $rc -ne 0 ]; then
    echo "sanitize: check 未通过，禁止提交" >&2
  else
    echo "sanitize: check 通过"
  fi
  exit $rc
}

case "${1:-}" in
  fix)   mode_fix ;;
  check) mode_check ;;
  map)   mode_map ;;
  *)     sed -n '2,15p' "$0"; exit 2 ;;
esac
