#!/usr/bin/env bash
# 变异检验（docs/regression-guard.md 第 3 道防线）：对每类历史 bug 的根因代码注入等价变异，
# 跑对应测试，证明**会红**（锁存活）。变异存活 = 测试被删/弱化/假绿 → 脚本退出非零。
# 用法：npm run mutation-check（或 bash scripts/mutation-check.sh）。
# 前提：工作区必须干净（变异靠 git checkout 还原）。全量约 2-4 分钟，不必每次交付跑。
# 演进纪律：新增变异必须对应 docs/regression-guard.md §3 缺陷模式档案的一行（同 commit）。
set -u
cd "$(dirname "$0")/.."

if [[ -n "$(git status --porcelain)" ]]; then
  echo "ABORT: 工作区有未提交改动——变异检验临时改源码、靠 git 还原，拒绝脏区运行。"
  exit 2
fi

M_FILE=(); M_ANCHOR=(); M_MUTANT=(); M_TARGET=(); M_DESC=()
add() { # <源文件> <锚字符串(恰好1次)> <变异体> <目标测试文件> <说明>
  M_FILE+=("$1"); M_ANCHOR+=("$2"); M_MUTANT+=("$3"); M_TARGET+=("$4"); M_DESC+=("$5")
}

# ── 变异清单（与 docs/regression-guard.md §3 档案行一一对应）──
add src/api/client.ts \
  "typeof value === 'object' ? JSON.stringify(value) : String(value)" \
  "String(value)" \
  src/__tests__/clientParams.test.ts \
  "M1 P-01 查询参数 JSON 序列化退化为标量（ADR-155 翻页失效）"

add src/api/client.ts \
  "refreshInFlight = refreshInFlight ?? requestRefresh();" \
  "refreshInFlight = requestRefresh();" \
  src/__tests__/client.test.ts \
  "M2 P-04 401 单飞刷新退化（并发刷新风暴）"

add src/tasks/stateMachine.ts \
  "TASK_STATUS_RUNNING: ['pause', 'cancel']" \
  "TASK_STATUS_RUNNING: ['cancel']" \
  src/__tests__/stateMachine.test.ts \
  "M3 P-15 状态机镜像漂移（RUNNING 丢暂停，ADR-200）"

add src/pages/tasks/TaskDetailPage.tsx \
  "ws?.close();" \
  "void 0;" \
  src/__tests__/TaskDetailPage.test.tsx \
  "M4 P-05 WS 卸载不关闭（连接泄漏，2026-09-06 修复）"

add src/pages/tasks/TaskDetailPage.tsx \
  "(d.logs?.logs ?? []).filter((l) => !seenLogIdsRef.current.has(l.log_id))" \
  "(d.logs?.logs ?? [])" \
  src/__tests__/TaskDetailSnapshot.test.tsx \
  "M5 P-06 快照吸收去重失效（轮询/WS 交叠重复行）"

add src/pages/findings/FindingsPage.tsx \
  "setVerdictFilter(v ?? '')" \
  "setVerdictFilter('')" \
  src/__tests__/FindingsPage.test.tsx \
  "M6 P-07 结论筛选死链路（onChange 恒清空，2026-09-06 修复）"

add src/dict/index.ts \
  "return ext[format ?? 0] ?? 'json';" \
  "return 'json';" \
  src/__tests__/dict.test.ts \
  "M7 P-13 下载扩展名恒 json（.bin 回归）"

add src/components/AIInteractionLogPanel.tsx \
  "const box = modalOpen ? modalBoxRef.current : inlineBoxRef.current;" \
  "const box = modalBoxRef.current;" \
  src/__tests__/AIInteractionLogPanel.test.tsx \
  "M8 P-11 滚底 ref 被 Modal 抢占（2026-09-06 修复）"

add src/auth/session.tsx \
  "{ access_token: getAccessToken() }" \
  "{}" \
  src/__tests__/session.test.tsx \
  "M9 P-14 logout 空 body（后端恒 400 被掩盖）"

add src/auth/session.tsx \
  "invite_code: inviteCode || undefined," \
  "invite_code: inviteCode," \
  src/__tests__/session.test.tsx \
  "M10 P-14 register 空邀请码误传（不剔除）"

add src/api/client.ts \
  "if (status === 503 && cfg && (cfg._retry503 ?? 0) < 3) {" \
  "if (false && cfg && (cfg._retry503 ?? 0) < 3) {" \
  src/__tests__/clientInterceptors.test.ts \
  "M11 E-44 503 自动重试被移除"

add src/pages/tasks/TaskDetailPage.tsx \
  "void qc.refetchQueries({ queryKey: ['task-snapshot', taskId] });" \
  ";" \
  src/__tests__/TaskDetailPage.test.tsx \
  "M12 P-20 WS 非收束断线不立即补拉快照（116af13 修复，gw-f6a3523 实证）"

# ── 执行 ──
total=${#M_FILE[@]}
killed=0
survived=()
misconf=()

for i in "${!M_FILE[@]}"; do
  f="${M_FILE[$i]}"; a="${M_ANCHOR[$i]}"; m="${M_MUTANT[$i]}"; t="${M_TARGET[$i]}"; d="${M_DESC[$i]}"
  cnt=$(grep -cF -- "$a" "$f" || true)
  if [[ "$cnt" != "1" ]]; then
    echo "MISCONF [$d] 锚点命中 $cnt 次（要求恰好 1）——源码漂移，先修锚点"
    misconf+=("$d")
    continue
  fi
  A="$a" M2="$m" perl -0pi -e 's/\Q$ENV{A}\E/$ENV{M2}/' "$f"
  if npx vitest run "$t" >/tmp/mutation_vitest_$i.log 2>&1; then
    echo "SURVIVED [$d] —— 目标测试未变红：锁失效（被删/弱化/假绿）"
    survived+=("$d")
  else
    echo "killed    [$d]"
    killed=$((killed+1))
  fi
  git checkout -- "$f"
done

echo
if [[ -n "$(git status --porcelain)" ]]; then
  echo "FATAL: 还原后工作区仍脏——立即检查 git status，勿提交！"
  exit 3
fi
if [[ ${#misconf[@]} -gt 0 || ${#survived[@]} -gt 0 ]]; then
  echo "mutation-check: ${killed}/${total} 被杀；变异存活 ${#survived[@]}，锚点失配 ${#misconf[@]}。"
  printf '  存活/失配: %s\n' "${survived[@]}" "${misconf[@]}"
  exit 1
fi
echo "mutation-check: ${killed}/${total} 全部被杀——三道防线中的用例锁全部存活。"
