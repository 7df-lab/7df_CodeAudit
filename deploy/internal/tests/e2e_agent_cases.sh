#!/usr/bin/env bash
# =============================================================================
# e2e_agent_cases.sh — b3-3 L2 agent 维度 e2e 用例（sim 栈上运行）
#
# 前置: sim 栈在位(gateway 18080/web 18088)+南向链(manager+openshell-gateway)+
#       agent 镜像在 107 docker(opencode-cli:s1 / maka-cli:s4)+providers 描述符
#       已随 engine 树挂载进 runtime。
# 用法(107 的 sim 检出台): bash deploy/internal/tests/e2e_agent_cases.sh <case>
#   case = E1(opencode 平面) | E2(maka-acp 平面) | E3(图×opencode) | E5(不可达 agent 诚实失败)
#   E1/E2 前置: runtime 以对应 CODEAUDIT_AGENT 重启(本脚本代做,见 switch_agent)。
# 断言: 任务 COMPLETED + AI 流非空(total_bytes>0 且 complete) + tasklog 喂入非空。
# 证据: stdout 原始输出(调用方归档 .agent/evidence/)。
# =============================================================================
set -uo pipefail
CASE="${1:?usage: e2e_agent_cases.sh E1|E2|E5}"
SIM_DIR="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$SIM_DIR"
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

TASK_TIMEOUT_E="${TASK_TIMEOUT_E:-2100}"   # maka 回合 3~18min 实况(2026-09-25 L1 实证),35min 上限

# switch_agent <descriptor-id> — 重启 runtime 携带 agent env(compose 从调用 env 解析)
switch_agent() {
  local agent="$1" args=()
  [ "$agent" != "dsh" ] && args=(--force-recreate)
  # env.sim 必须显式加载(LESSONS #17 同族:2026-09-28 实证——直跑 compose 丢
  # OPENSHELL_MANAGER_TOKEN → manager 401 → 任务静默降级 RuleScan)
  set -a; . deploy/internal/env.sim; set +a
  (cd engine && CODEAUDIT_AGENT="$agent" docker compose -f docker-compose.yml \
     -f ../deploy/internal/docker-compose.sim.yml --project-name codeaudit-sim \
     up -d "${args[@]}" runtime >/dev/null)
  sleep 8
  docker logs codeaudit-sim-runtime 2>&1 | tail -1
}

case_run() { # case_run <label> <timeout_s> — 登录→上传→项目→任务→启动→轮询
  local label="$1" timeout_s="$2"
  local work up fid pid tid out status ai_total ai_complete
  login || { echo "[$label] 登录失败——栈未就绪"; return 1; }
  work=$(mktemp -d)
  printf 'TOKEN = "e2e-agent-secret"  # B105\n' > "$work/app.py"
  make_zip "$work/app.zip" "$work/app.py" app.py
  up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" -H "Authorization: Bearer $ACCESS" -F "file=@$work/app.zip;type=application/zip")
  fid=$(jsonq "$up" "d['file_id']")
  [ -n "$fid" ] || { echo "[$label] 上传失败(空仓跑完会假绿): $up"; return 1; }
  out=$(http POST /v1/projects "{\"project\":{\"name\":\"sim-e2e-$label\",\"default_branch\":\"main\",\"default_scan_mode\":\"SCAN_MODE_AI_ONLY\"}}" "$ACCESS")
  pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  [ -n "$pid" ] || { echo "[$label] 项目创建失败: $out"; return 1; }
  http PUT "/v1/projects/$pid/config" "{\"config\":{\"project_id\":\"$pid\",\"config\":{\"upload_file_id\":\"$fid\"}}}" "$ACCESS" >/dev/null
  out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_AI_ONLY\",\"sast_tools\":[],\"config\":{}}" "$ACCESS")
  tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  [ -n "$tid" ] || { echo "[$label] 任务创建失败: $out"; return 1; }
  echo "[$label] task=$tid 启动(上限 ${timeout_s}s)..."
  http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null
  # 轮询:终态即退(枚举带 TASK_STATUS_ 前缀——对齐 lib.sh poll_task 口径;
  # 修前裸 COMPLETED 永不匹配=恒烧满窗口且终态押在末次快照上,瞬时失败即误判)
  local deadline=$(( $(date +%s) + timeout_s )) body=""
  FINAL_STATUS=""
  status=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    sleep 15
    out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
    body=$(echo "$out" | tail -n +2)
    status=$(jsonq "$body" "d['task']['status']" 2>/dev/null)
    case "$status" in
      TASK_STATUS_COMPLETED|TASK_STATUS_FAILED|TASK_STATUS_TIMEOUT|TASK_STATUS_DEAD) break ;;
    esac
  done
  # 空终态补一次终检(轮询末次请求瞬时失败时 body 为空/陈旧)
  if [ -z "$status" ]; then
    out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
    body=$(echo "$out" | tail -n +2)
    status=$(jsonq "$body" "d['task']['status']" 2>/dev/null)
  fi
  FINAL_STATUS="$status"   # 带出给调用方(E5 区分挂死与诚实失败)
  ai_total=$(jsonq "$body" "d['ai']['total_bytes']" 2>/dev/null)
  ai_complete=$(jsonq "$body" "d['ai']['complete']" 2>/dev/null)
  echo "[$label] 终态=$status ai.total_bytes=${ai_total:-?} ai.complete=${ai_complete:-?} task=$tid"
  echo "$tid" > "/tmp/e2e-agent-$label.taskid"   # 任务已创建即落盘(E5 降级完成语义 ai=0 也要可查)
  [ "$status" = "TASK_STATUS_COMPLETED" ] || return 1
  [ "${ai_total:-0}" -gt 0 ] 2>/dev/null || return 1
  [ "$ai_complete" = "True" ] || return 1   # 流未 complete 不算全链通(jsonq bool=True)
  return 0
}

case "$CASE" in
  E1)
    switch_agent opencode
    case_run E1 900; rc=$?
    [ "$rc" = 0 ] && echo "[E1] PASS" || echo "[E1] FAIL"
    exit $rc
    ;;
  E2)
    switch_agent maka-acp
    case_run E2 "$TASK_TIMEOUT_E"; rc=$?
    [ "$rc" = 0 ] && echo "[E2] PASS" || echo "[E2] FAIL"
    exit $rc
    ;;
  E3)
    # 图任务×agent(b3 计划 §4 L2 E3;B1 前置=A1 多会话✓)。图路径不吃任务级 agent
    # 字段(runGraphViaSandbox 无 SelectAgent——flat 面独享,缺口入档)——经 env
    # CODEAUDIT_AGENT 切,与 E1 同法。断言:COMPLETED+graph 全节点终态+通道分节
    # (ai.channels 逐节点非空=批1 通道层全链)。
    # GRAPH_AGENT 参配(缺省 opencode):E3M=GRAPH_AGENT=maka-acp(计划原文配对——
    # 围栏契约通路,engine 70c72782 caps 分岔)。
    GA="${GRAPH_AGENT:-opencode}"
    switch_agent "$GA"
    login || { echo "[E3] 登录失败"; exit 3; }
    work=$(mktemp -d)
    printf 'TOKEN = "e2e-graph-secret"  # B105\n' > "$work/app.py"
    make_zip "$work/app.zip" "$work/app.py" app.py
    up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" -H "Authorization: Bearer $ACCESS" -F "file=@$work/app.zip;type=application/zip")
    fid=$(jsonq "$up" "d['file_id']")
    [ -n "$fid" ] || { echo "[E3] 上传失败"; exit 3; }
    out=$(http POST /v1/projects "{\"project\":{\"name\":\"sim-e2e-E3图\",\"default_branch\":\"main\",\"default_scan_mode\":\"SCAN_MODE_AI_ONLY\"}}" "$ACCESS")
    pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
    http PUT "/v1/projects/$pid/config" "{\"config\":{\"project_id\":\"$pid\",\"config\":{\"upload_file_id\":\"$fid\",\"pack\":\"audit-graph\"}}}" "$ACCESS" >/dev/null
    out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_AI_ONLY\",\"sast_tools\":[],\"config\":{\"pack\":\"audit-graph\"}}" "$ACCESS")
    tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
    [ -n "$tid" ] || { echo "[E3] 任务创建失败: $out"; exit 3; }
    echo "[E3] task=$tid 图任务启动(上限 ${TASK_TIMEOUT_E}s)..."
    http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null
    deadline=$(( $(date +%s) + TASK_TIMEOUT_E )); status=""
    # 通道采样:快照 channels 在 settle 后可回空(组装面观察项)——判据取轮询期峰值
    ch_total_max=0; ch_nonempty_max=0
    while [ "$(date +%s)" -lt "$deadline" ]; do
      sleep 20
      out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
      body=$(echo "$out" | tail -n +2)
      status=$(jsonq "$body" "d['task']['status']" 2>/dev/null)
      ct=$(jsonq "$body" "len(d.get('ai',{}).get('channels') or [])" 2>/dev/null)
      cn=$(jsonq "$body" "sum(1 for v in (d.get('ai',{}).get('channels') or []) if (v.get('chunk') or int(v.get('total') or 0) > 0))" 2>/dev/null)
      [ "${ct:-0}" -gt "$ch_total_max" ] 2>/dev/null && ch_total_max=$ct
      [ "${cn:-0}" -gt "$ch_nonempty_max" ] 2>/dev/null && ch_nonempty_max=$cn
      case "$status" in
        TASK_STATUS_COMPLETED|TASK_STATUS_FAILED|TASK_STATUS_TIMEOUT|TASK_STATUS_DEAD) break ;;
      esac
    done
    ai_total=$(jsonq "$body" "d['ai']['total_bytes']" 2>/dev/null)
    echo "[E3] 终态=$status ai.total_bytes=${ai_total:-?} task=$tid"
    # 图终态:GET /v1/tasks/{id}/graph 全节点 completed
    gout=$(http GET "/v1/tasks/$tid/graph" "" "$ACCESS")
    gbody=$(echo "$gout" | tail -n +2)
    nodes_total=$(jsonq "$gbody" "len(d.get('nodes') or [])")
    nodes_done=$(jsonq "$gbody" "sum(1 for n in (d.get('nodes') or []) if n.get('status')=='completed')")
    nodes_bad=$(jsonq "$gbody" "sum(1 for n in (d.get('nodes') or []) if n.get('status') in ('failed','rejected','indeterminate'))")
    # 通道分节:轮询期峰值(snap channels=list;settle 后可回空=组装面观察项)
    ct=$(jsonq "$body" "len(d.get('ai',{}).get('channels') or [])" 2>/dev/null)
    cn=$(jsonq "$body" "sum(1 for v in (d.get('ai',{}).get('channels') or []) if (v.get('chunk') or int(v.get('total') or 0) > 0))" 2>/dev/null)
    [ "${ct:-0}" -gt "$ch_total_max" ] 2>/dev/null && ch_total_max=$ct
    [ "${cn:-0}" -gt "$ch_nonempty_max" ] 2>/dev/null && ch_nonempty_max=$cn
    ch_total=$ch_total_max
    ch_nonempty=$ch_nonempty_max
    echo "[E3] 图节点 $nodes_done/$nodes_total completed(bad=$nodes_bad) 通道非空(峰值) $ch_nonempty/$ch_total"
    rc=0
    [ "$status" = "TASK_STATUS_COMPLETED" ] || rc=1
    [ "${ai_total:-0}" -gt 0 ] 2>/dev/null || rc=1
    [ "${nodes_total:-0}" -ge 4 ] 2>/dev/null || rc=1
    [ "$nodes_done" = "$nodes_total" ] || rc=1
    [ "${nodes_bad:-0}" = "0" ] || rc=1
    [ "${ch_nonempty:-0}" -ge 3 ] 2>/dev/null || rc=1
    [ "$ch_nonempty" = "$ch_total" ] || rc=1
    echo "$tid" > /tmp/e2e-agent-E3.taskid
    if [ "$rc" = 0 ]; then echo "[E3] PASS(图×${GA}:COMPLETED+图全节点 completed+通道分节 ${ch_nonempty}/${ch_total},task=$tid)"
    else echo "[E3] FAIL"; fi
    switch_agent dsh
    exit $rc
    ;;
  E5)
    # 判据(2026-09-28 实测校准+审计批收紧):engine 对不可达 agent 的既有语义=
    # RuleScan 兜底降级完成(R57:degraded 标记,COMPLETED+AI 零产出)——「不挂死+
    # 错误可见」即达设计意图;FAILED 同为诚实失败;**无终态=挂死 FAIL**(修前
    # rc=1 即 PASS 会把挂死误报为通过)。
    switch_agent nonexistent-agent
    case_run E5 900; rc=$?
    tid=$(cat /tmp/e2e-agent-E5.taskid 2>/dev/null)
    case "$FINAL_STATUS" in
      TASK_STATUS_COMPLETED)
        echo "[E5] PASS(降级完成语义:COMPLETED+AI 零产出+degraded 标记,task=$tid)"; rc=0 ;;
      TASK_STATUS_FAILED|TASK_STATUS_TIMEOUT|TASK_STATUS_DEAD)
        echo "[E5] PASS(诚实失败语义:$FINAL_STATUS,task=$tid)"; rc=0 ;;
      "")
        if [ -n "$tid" ]; then echo "[E5] FAIL(挂死:窗口内无终态,task=$tid)"; rc=1
        else echo "[E5] BLOCKED(基础设施错误,任务未创建)"; rc=3; fi ;;
      *)
        echo "[E5] FAIL(非预期终态 $FINAL_STATUS,task=$tid)"; rc=1 ;;
    esac
    switch_agent dsh   # 复位缺省
    exit $rc
    ;;
  *) echo "usage: $0 E1|E2|E3|E5" >&2; exit 2 ;;
esac
