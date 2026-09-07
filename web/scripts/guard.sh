#!/usr/bin/env bash
# 静态守卫（docs/regression-guard.md 第 2 道防线）：历史"禁区模式"grep 拦截 + 关键锚点存在性自检。
# 用法：npm run guard（或 bash scripts/guard.sh）。秒级，每次交付提交前必跑。
# 规则演进纪律：新增条目必须同步 docs/regression-guard.md 缺陷模式档案（同 commit）。
set -u
cd "$(dirname "$0")/.."

fail=0
ok=0

# 禁止出现的模式：check_block <ID> <说明> <ERE> <路径...>
check_block() {
  local id="$1" desc="$2" pattern="$3"; shift 3
  local hits
  hits=$(grep -rnE "$pattern" "$@" --include='*.ts' --include='*.tsx' 2>/dev/null || true)
  if [[ -n "$hits" ]]; then
    echo "FAIL $id $desc"
    echo "$hits" | head -8 | sed 's/^/     /'
    fail=1
  else
    echo "PASS $id $desc"; ok=$((ok+1))
  fi
}

# 必须存在的锚点：check_anchor <ID> <说明> <ERE> <路径...>
check_anchor() {
  local id="$1" desc="$2" pattern="$3"; shift 3
  if grep -rnqE "$pattern" "$@" --include='*.ts' --include='*.tsx' 2>/dev/null; then
    echo "PASS $id $desc"; ok=$((ok+1))
  else
    echo "FAIL $id $desc（锚点丢失——防御要地被改动，对照 docs/external-interfaces.md §2）"
    fail=1
  fi
}

echo "== G-01 测试纪律：禁止整模块 mock api/client（P-02 假绿根源，一律走 fakeGateway 传输层）=="
check_block G-01 "vi.mock api/client 禁止" \
  "vi\.mock\(\s*['\"][^'\"]*api/client" src/__tests__

echo "== G-02 死契约禁区（P-03/P-10：复活即红；警示注释属许可提及，须含标记词）=="
g02() { # <ID> <说明> <ERE> <路径...> —— 命中行含许可标记词（已死/勿复活/禁复活/遗物/no-op）则豁免
  local id="$1" desc="$2" pattern="$3"; shift 3
  local hits
  hits=$(grep -rnE "$pattern" "$@" --include='*.ts' --include='*.tsx' 2>/dev/null | grep -vE '已死|勿复活|禁复活|遗物|no-op' || true)
  if [[ -n "$hits" ]]; then
    echo "FAIL $id $desc"; echo "$hits" | head -8 | sed 's/^/     /'; fail=1
  else
    echo "PASS $id $desc"; ok=$((ok+1))
  fi
}
g02 G-02a "res.dir 死链路（ADR-200 前解包目录契约已死）" "res\.dir\b" src/pages src/components
g02 G-02b "['tasks-infinite'] 死缓存键（无限滚动时代遗物，invalidate 是 no-op）" "tasks-infinite" src

echo "== G-03 client.ts 防御要地锚点（P-01/P-04：缺失即说明拦截器/序列化被改）=="
check_anchor G-03a "查询参数 JSON 风格序列化（E-00a）" "paramsSerializer" src/api/client.ts
check_anchor G-03b "401 单飞刷新（E-41）" "refreshInFlight \?\? requestRefresh" src/api/client.ts
check_anchor G-03c "503 重试上限计数（E-44）" "_retry503" src/api/client.ts
check_anchor G-03d "403/501/503 全局错误事件派发（E-45）" "API_ERROR_EVENT, \{ detail: status \}" src/api/client.ts
check_anchor G-03e "refresh_token 键名（E-00c 本地存储契约）" "codeaudit\.refresh_token" src/api/client.ts
check_anchor G-03f "上传 multipart + 120s 超时（E-11）" "multipart/form-data" src/api/client.ts
check_anchor G-03g "上传 120s 超时（E-11）" "120_000" src/api/client.ts

echo "== G-04 泄漏与 ref 抢占修复锚点（P-05/P-11：2026-09-06 六缺陷修复）=="
check_anchor G-04a "WS 卸载必须 close" "ws\?\.close\(\)" src/pages/tasks/TaskDetailPage.tsx
check_anchor G-04b "AI 面板内联/Modal 双 ref（禁共享）" "modalBoxRef" src/components/AIInteractionLogPanel.tsx
check_anchor G-04c "测试台未建模路由响亮失败（禁静默空成功）" "未建模路由" src/testsupport/fakeGateway.ts

echo "== G-05 客户端零直连（14号 P1：HTTP 调用只允许同源相对路径）=="
check_block G-05 "api/fetch 直连绝对 URL 禁止" \
  "(api\.(get|post|put|delete|request)|fetch)\(\s*['\`\"]https?://" src

echo
if [[ $fail -ne 0 ]]; then
  echo "guard: 存在 FAIL（$ok 项通过）——禁区模式复活或防御锚点丢失，交付被拦截。"
  echo "  如属有意变更：先改对应契约测试与 docs/*.md（同 commit），再调整本守卫条目。"
  exit 1
fi
echo "guard: 全部通过（$ok 项）。"
