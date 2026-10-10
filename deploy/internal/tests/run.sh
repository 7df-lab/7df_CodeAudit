#!/usr/bin/env bash
# =============================================================================
# run.sh — CodeAudit 生产模拟环境功能测试套
#
# 对"生产模拟栈"（deploy/sim.sh up 拉起）做黑盒功能验证：只经 gateway/web
# 的 HTTP 面驱动真实链路（上传→storage→task 拉包解包→SAST→result→报告/通知）。
#
# 用法:  bash deploy/tests/run.sh [用例编号，如 04]
# 退出码: 0=全部通过；非零=存在失败（失败明细见输出）
# =============================================================================
set -u
cd "$(dirname "$0")" && . ./lib.sh
# env.sim 锚定（c12 token 读取不可依赖调用时 CWD——sim-sync 以相对路径调用时
# $(dirname $0) 相对串在上方 cd 后失锚，2026-09-21 实证 c12 八连锁假红）。
SCRIPT_DIR="$(pwd)"

# ---------- 01 健康与就绪 ----------
c01_health() {
  echo "[01] 健康与就绪"
  local out; out=$(http GET /health)
  check "gateway /health → 200" eq "$(echo "$out" | head -1)" "200"
  check "健康响应含 status=ok" contains "$(echo "$out" | tail -n +2)" '"ok"'
  out=$(http GET /v1/tools)
  check "未认证 /v1/tools 被拒（401）" eq "$(echo "$out" | head -1)" "401"
}

# ---------- 02 认证 ----------
c02_auth() {
  echo "[02] 认证"
  check "登录 admin 成功并取得 access_token" login
  local out; out=$(http POST /v1/auth/login '{"username":"admin","password":"wrong-password"}')
  check "错误口令被拒（非200）" test "$(echo "$out" | head -1)" != "200"
  if [ -n "${REFRESH:-}" ]; then
    out=$(http POST /v1/auth/refresh "{\"refresh_token\":\"$REFRESH\"}")
    check "refresh_token 换发新 access（200）" eq "$(echo "$out" | head -1)" "200"
    check "换发响应含新 access_token" contains "$(echo "$out" | tail -n +2)" access_token
  else
    echo "  -（网关未返回 refresh_token，跳过续签用例）"
  fi
}

# ---------- 03 项目管理 ----------
c03_projects() {
  echo "[03] 项目管理"
  local out id
  out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-项目","repo_url":"https://demo.example/sim.git","default_branch":"main","default_scan_mode":"SCAN_MODE_SAST_ONLY"}}' "$ACCESS")
  check "创建项目（200/201）" test "$(echo "$out" | head -1)" = "200" -o "$(echo "$out" | head -1)" = "201"
  id=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "响应含 project_id" nonempty "$id"
  SIM_PID="$id"
  out=$(http GET /v1/projects "" "$ACCESS")
  check "项目出现在列表" contains "$(echo "$out" | tail -n +2)" "sim-e2e-项目"
  out=$(http PUT "/v1/projects/$id/config" '{"config":{"project_id":"'"$id"'","config":{"sim_marker":"e2e"}}}' "$ACCESS")
  check "写入项目 config（200）" eq "$(echo "$out" | head -1)" "200"
  out=$(http GET "/v1/projects/$id/config" "" "$ACCESS")
  check "读回项目 config 含 sim_marker" contains "$(echo "$out" | tail -n +2)" "e2e"
}

# ---------- 04 上传→SAST 任务全链（核心资金流）----------
# 上传含真实漏洞样本的 zip（storage 通道）→ SAST_ONLY(bandit) 任务 →
# task 从 storage 拉包解包 → bandit 扫描 → result 落库 → 报告生成
c04_sast_fullchain() {
  echo "[04] 上传→SAST 任务全链"
  local work; work=$(mktemp -d)
  cat > "$work/app.py" <<'PY'
import sqlite3
def get_user(uid):
    conn = sqlite3.connect("app.db")
    cur = conn.cursor()
    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)  # SQL 注入（SQLITE_INJECTION）
    return cur.fetchone()
API_TOKEN = "hunter2-hardcoded-secret"  # 硬编码凭据（B105）
PY
  make_zip "$work/sample.zip" "$work/app.py" app.py
  local up; up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" \
    -H "Authorization: Bearer $ACCESS" -F "file=@$work/sample.zip;type=application/zip")
  local fid; fid=$(jsonq "$up" "d['file_id']")
  check "上传压缩包取得 file_id" nonempty "$fid"
  local pid="${SIM_PID:-}"; [ -z "$pid" ] && {
    local out; out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-孤例","repo_url":"https://demo.example/s.git","default_branch":"main","default_scan_mode":"SCAN_MODE_SAST_ONLY"}}' "$ACCESS")
    pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  }
  local out; out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_SAST_ONLY\",\"sast_tools\":[\"bandit\"],\"config\":{\"upload_file_id\":\"$fid\"}}" "$ACCESS")
  local tid; tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  check "创建 SAST 任务取得 task_id" nonempty "$tid"
  out=$(http POST "/v1/tasks/$tid/start" "" "$ACCESS")
  check "任务启动（200）" eq "$(echo "$out" | head -1)" "200"

  local res; res=$(poll_task "$tid")
  local status="${res%%|*}" emsg="${res#*|}"
  check "任务到达 COMPLETED（实际=$status ${emsg:+错误=$emsg}）" eq "$status" "TASK_STATUS_COMPLETED"

  out=$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS")
  local fcount; fcount=$(jsonq "$(echo "$out" | tail -n +2)" "len(d.get('findings',[]))")
  check "发现数 ≥1（bandit 扫出样本漏洞，实际=$fcount）" test "${fcount:-0}" -ge 1
  # pagination.total 契约（2026-10-10 GUI 遍历判例：total 恒 0 → web「共 0 条」与实体
  # 同屏矛盾；protojson 的 0 非 nullish 绕过前端回退）——锁 total ≥ 实体数
  local ptotal; ptotal=$(jsonq "$(echo "$out" | tail -n +2)" "int((d.get('pagination') or {}).get('total',-1))")
  check "pagination.total 与实体一致（实际 total=$ptotal 实体=$fcount）" test "$ptotal" -ge "$fcount"
  check "发现标注 source_tool=bandit" contains "$(echo "$out" | tail -n +2)" '"bandit"'
  # v0.1.2 持久化补齐契约（2026-10-10 GUI 判例：title/cwe_id 表无列落库即丢）：bandit
  # 上游 CWEID/Title 双有值（bandit_adapter.go），SAST 路径锁 cwe_id 非空
  check "CWE 持久化（bandit 首条 cwe_id 非空）" test -n "$(jsonq "$(echo "$out" | tail -n +2)" "str((d.get('findings') or [{}])[0].get('cwe_id',''))")"

  # 源码全文端点冒烟（gw-f6a3523 实证防回归：ADR-200 拉包流布局迁移后 source-file
  # 解析链四流全死 → 发现详情"源码全文不可用/Sink 链路不可用"。上传流任务的源根
  # 必须 经 ①b uploads-<task_id>/unpacked 流解析命中）。
  out=$(http GET "/v1/tasks/$tid/source-file?path=app.py" "" "$ACCESS")
  check "source-file 对上传流任务返回 200（源根解析链存活）" eq "$(echo "$out" | head -1)" "200"
  check "source-file 内容即样本源码" contains "$(echo "$out" | tail -n +2)" "SQLITE_INJECTION"

  out=$(http GET "/v1/reports?task_id=$tid" "" "$ACCESS")
  check "任务报告已生成（reports 按 task 过滤含该任务）" contains "$(echo "$out" | tail -n +2)" "$tid"

  # verdict:batch 批量裁决路由（2026-09-11 审计：暴露面无消费方无断言=腐化面）——
  # 契约形态 proto={finding_ids[],direction,confidence}（两轴重构 2026-10-10：verdict 枚举
  # →direction；tier 服务端钉 DEFINITE。无 updates[]/reasoning——错误形态会被 protojson
  # DiscardUnknown 静默丢弃成空请求 200 no-op，首版断言即踩此坑）
  local fid; fid=$(jsonq "$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS" | tail -n +2)" "d['findings'][0]['finding_id']")
  check "取首条 finding_id（verdict:batch 用例锚）" nonempty "$fid"
  out=$(http POST "/v1/findings/verdict:batch" "{\"finding_ids\":[\"$fid\"],\"direction\":\"VERDICT_DIRECTION_FALSE\"}" "$ACCESS")
  check "verdict:batch 提交 200且回执 updated_count≥1" test "$(jsonq "$(echo "$out" | tail -n +2)" "int(d.get('updated_count',0))")" -ge 1
  out=$(http GET "/v1/findings/$fid" "" "$ACCESS")
  check "verdict:batch 读回生效（direction=FALSE 且 tier=DEFINITE——批量裁决即结构化裁决）" test "$(jsonq "$(echo "$out" | tail -n +2)" "str((d.get('finding') or d).get('verdict_direction',''))+'|'+str((d.get('finding') or d).get('verdict_tier',''))")" = "VERDICT_DIRECTION_FALSE|VERDICT_TIER_DEFINITE"
  echo "$tid" > /tmp/sim-e2e-task-id   # 供 06 通知用例复用
  rm -rf "$work"
}

# ---------- 05 控制台 ----------
c05_web() {
  echo "[05] 前端控制台（容器内 nginx）"
  local out; out=$(curl -sS -m 10 -o /tmp/sim-web-body -w '%{http_code}' "$WEB_URL/")
  check "web 首页 200" eq "$out" "200"
  check "返回 SPA 宿主页（含根挂载点）" contains "$(cat /tmp/sim-web-body)" 'id="root"'
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$WEB_URL/tasks")
  check "SPA 路由回退（/tasks → 200）" eq "$out" "200"
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$WEB_URL/v1/tools")
  check "web 的 /v1 反代连通网关（401 透传）" eq "$out" "401"
}

# ---------- 06 通知链路 ----------
c06_notifications() {
  echo "[06] 通知链路（Kafka→通知）"
  local tid; tid=$(cat /tmp/sim-e2e-task-id 2>/dev/null)
  [ -z "$tid" ] && { echo "  -（无前置任务，跳过）"; return 0; }
  # 通知按用户 ID（JWT sub）投递，非用户名（proto ListNotificationsRequest 契约）
  local uid; uid=$(echo "$ACCESS" | cut -d. -f2 | python3 -c 'import sys,base64,json;s=sys.stdin.read().strip();s+="="*(-len(s)%4);print(json.loads(base64.urlsafe_b64decode(s))["sub"])')
  local out; out=$(http GET "/v1/notifications?user_id=$uid" "" "$ACCESS")
  local ncount; ncount=$(jsonq "$(echo "$out" | tail -n +2)" "len(d.get('notifications',[]))")
  check "通知列表可达（非空，实际=$ncount）" test "${ncount:-0}" -ge 1
  # ADR-222 一键全部已读：未读数 N → read-all 计数=N → 未读归零
  local uout; uout=$(http GET "/v1/notifications?unread_only=true" "" "$ACCESS")
  local ucount; ucount=$(jsonq "$(echo "$uout" | tail -n +2)" "len(d.get('notifications',[]))")
  local rall; rall=$(http POST "/v1/notifications/read-all" "" "$ACCESS")
  check "read-all 可达（200）" eq "$(echo "$rall" | head -1)" "200"
  local marked; marked=$(jsonq "$(echo "$rall" | tail -n +2)" "d.get('marked',-1)")
  check "read-all 计数=未读数（未读=${ucount:-0}，标记=${marked:--1}）" eq "$marked" "${ucount:-0}"
  uout=$(http GET "/v1/notifications?unread_only=true" "" "$ACCESS")
  ucount=$(jsonq "$(echo "$uout" | tail -n +2)" "len(d.get('notifications',[]))")
  check "read-all 后未读归零（实际=${ucount:-?}）" eq "${ucount:--1}" "0"
}

# ai_log_has_completed_round <task_id> — AI 交互日志内容级甄别（2026-10-03 反例修正）：
# 分页拉全量 ai-log、base64 解码，检查是否含真实模型完成回合标记「── 回合结束: completed ──」。
# 反例实录（2026-10-03 gw-59b84773）：LLM SSE 三轮立断 STREAM_CLOSED 的降级任务日志
# 非空（3478B，任务下发帧+失败回合帧）但全为 ❌ 回合收尾——旧判据"非空=真全链"会被其击穿。
ai_log_has_completed_round() {
  python3 - "$1" "$ACCESS" "$BASE_URL" <<'PYEOF' 2>/dev/null
import sys, json, base64, urllib.request
tid, tok, base = sys.argv[1], sys.argv[2], sys.argv[3]
cur, buf = 0, b''
while True:
    req = urllib.request.Request(f"{base}/v1/tasks/{tid}/ai-log?cursor={cur}&max_bytes=60000",
                                 headers={"Authorization": f"Bearer {tok}"})
    d = json.load(urllib.request.urlopen(req, timeout=30))
    chunk = base64.b64decode(d.get('chunk') or '')
    buf += chunk
    if d.get('complete') or not chunk:
        break
    cur = d.get('next_cursor', cur + len(chunk))
    if len(buf) > 1_000_000:
        break
sys.exit(0 if '回合结束: completed' in buf.decode('utf-8', 'replace') else 1)
PYEOF
}

# ---------- 07 AI 链路（上传型项目；环境相关三形态）----------
# 2026-09-07 重验证修正：原实现挂 03 用例的假仓库项目（repo_url=demo.example），
# prepare 的 git clone 必然失败 → 恒走 DEAD 分支，manager/沙箱/LLM 全链从未被真实
# 行使（还把 R-29 task 镜像缺 git 掩成"预期内诚实失败"）。改挂上传型项目后：
#   a) manager+沙箱+LLM 可达 → COMPLETED 且 AI 交互日志非空（真全链）；
#   b) 沙箱不可达 → COMPLETED 走 RuleScan 兜底，发现标 NEEDS_MANUAL（设计行为，
#      manual-test-guide §2 口径：降级非缺陷）；
#   c) 崩坏 → FAILED/DEAD 且 error_message 非空（诚实失败，不允许静默挂死/空原因）。
c07_ai() {
  echo "[07] AI 链路（沙箱模式，环境相关）"
  local work; work=$(mktemp -d)
  cat > "$work/app.py" <<'PY'
import sqlite3
def get_user(uid):
    conn = sqlite3.connect("app.db")
    cur = conn.cursor()
    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)  # SQL 注入
    return cur.fetchone()
API_TOKEN = "hunter2-hardcoded-secret"  # 硬编码凭据
PY
  make_zip "$work/sample.zip" "$work/app.py" app.py
  local up; up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" \
    -H "Authorization: Bearer $ACCESS" -F "file=@$work/sample.zip;type=application/zip")
  local fid; fid=$(jsonq "$up" "d['file_id']")
  check "上传压缩包取得 file_id" nonempty "$fid"
  local out id
  out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-ai链路","default_branch":"main","default_scan_mode":"SCAN_MODE_AI_ONLY"}}' "$ACCESS")
  id=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "创建 AI 型上传项目" nonempty "$id"
  out=$(http PUT "/v1/projects/$id/config" '{"config":{"project_id":"'"$id"'","config":{"upload_file_id":"'"$fid"'"}}}' "$ACCESS")
  check "项目 config 关联 upload_file_id（200）" eq "$(echo "$out" | head -1)" "200"
  out=$(http POST /v1/tasks "{\"project_id\":\"$id\",\"scan_mode\":\"SCAN_MODE_AI_ONLY\",\"sast_tools\":[],\"config\":{}}" "$ACCESS")
  local tid; tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  check "创建 AI 任务" nonempty "$tid"
  http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null
  local res; res=$(poll_task "$tid" "${TASK_TIMEOUT_AI:-900}")
  local status="${res%%|*}" emsg="${res#*|}"
  case "$status" in
    TASK_STATUS_COMPLETED)
      # 形态甄别（2026-10-03 反例修正）：非空≠真全链——LLM 不可达但沙箱链路活着的
      # 任务（三轮 SSE 立断 STREAM_CLOSED）也会写非空 AI 日志（任务下发帧+失败回合帧，
      # 实测反例 3478B）→ 判据升级为内容级：须含"回合结束: completed"（真实模型回合
      # 完成标记；纯降级日志只有 ❌ 错误回合收尾）。纯空日志（连失败回合都无）同归降级分支。
      out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
      local ai_bytes; ai_bytes=$(jsonq "$(echo "$out" | tail -n +2)" "int((d.get('ai') or {}).get('total_bytes') or 0)")
      if [ "${ai_bytes:-0}" -gt 0 ] && ai_log_has_completed_round "$tid"; then
        check "AI 任务 COMPLETED 且交互日志非空=${ai_bytes}B 含真实完成回合（manager/沙箱/LLM 全链真实走通）" test "${ai_bytes:-0}" -gt 0
      else
        # 两轴重构（2026-10-10）：原 NEEDS_MANUAL 归未判定（处置语义不变——
        # needs_review 谓词；"为什么没结论"在 ai_reasoning 的 [降级] 前缀）
        check "AI 任务 COMPLETED 走 RuleScan 降级（交互日志空 → 发现须为未判定：direction=UNSPECIFIED）" contains "$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS" | tail -n +2)" '"verdict_direction":"VERDICT_DIRECTION_UNSPECIFIED"'
        check "降级发现 ai_reasoning 带 [降级] provenance 前缀" contains "$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS" | tail -n +2)" "[降级]"
        # R56/R57 降级可感知：阶段看板须带降级标志（此前 success 返回无标志→阶段照样绿勾）
        local snap; snap=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS" | tail -n +2)
        check "降级可感知：快照阶段 metadata 带 degraded=true（R56）" contains "$snap" '"degraded":"true"'
      fi ;;
    TASK_STATUS_FAILED|TASK_STATUS_DEAD)
      # 崩坏时的**诚实失败**也是被测行为：终态 + 完整错误信息（不允许静默挂死/空原因）
      check "AI 任务诚实失败（终态=$status，error_message 非空）" nonempty "$emsg" ;;
    *)
      check "AI 任务到达终态（实际=$status）" eq "$status" "TASK_STATUS_COMPLETED" ;;
  esac
  rm -rf "$work"
}

# ---------- 08 项目级上传→自动任务全链（GUI 用户路径回归）----------
# 与 04 的关键差异：任务 config 不带 upload_file_id——完全复刻 GUI「新建项目（上传
# 压缩包）→ 自动创建扫描任务 → 自动启动」的请求序列。回归锚点（2026-09-05 生产栈
# GUI 实测暴露，引擎 8de1a4d9/c24fb917）：
#   a) start 不得 409 FailedPrecondition project_path 未配置——源码来源必须经
#      项目 config 兜底链（task→project/storage 服务间地址，engine compose env 全覆盖）；
#   b) COMPLETED 且发现 ≥1——任务源共享卷 agent_repos 生效（否则扫的是空目录）。
c08_project_upload_autotask() {
  echo "[08] 项目级上传→自动任务全链（GUI 用户路径）"
  local work; work=$(mktemp -d)
  cat > "$work/app.py" <<'PY'
import sqlite3
def get_user(uid):
    conn = sqlite3.connect("app.db")
    cur = conn.cursor()
    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)  # SQL 注入
    return cur.fetchone()
API_TOKEN = "hunter2-hardcoded-secret"  # 硬编码凭据
PY
  make_zip "$work/sample.zip" "$work/app.py" app.py
  local up; up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" \
    -H "Authorization: Bearer $ACCESS" -F "file=@$work/sample.zip;type=application/zip")
  local fid; fid=$(jsonq "$up" "d['file_id']")
  check "上传压缩包取得 file_id" nonempty "$fid"
  local out id
  out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-项目级上传","default_branch":"main","default_scan_mode":"SCAN_MODE_PARALLEL"}}' "$ACCESS")
  id=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "创建上传型项目（repo_url 留空）" nonempty "$id"
  out=$(http PUT "/v1/projects/$id/config" '{"config":{"project_id":"'"$id"'","config":{"upload_file_id":"'"$fid"'"}}}' "$ACCESS")
  check "项目 config 关联 upload_file_id（200）" eq "$(echo "$out" | head -1)" "200"
  out=$(http POST /v1/tasks "{\"project_id\":\"$id\",\"scan_mode\":\"SCAN_MODE_SAST_ONLY\",\"sast_tools\":[\"bandit\"],\"config\":{}}" "$ACCESS")
  local tid; tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  check "自动建任务（空 config，源码来源留待兜底链解析）" nonempty "$tid"
  out=$(http POST "/v1/tasks/$tid/start" "" "$ACCESS")
  check "start=200（回归：project_path 未配置 409 不得复现）" eq "$(echo "$out" | head -1)" "200"
  local res; res=$(poll_task "$tid")
  local status="${res%%|*}" emsg="${res#*|}"
  check "任务到达 COMPLETED（实际=$status ${emsg:+错误=$emsg}）" eq "$status" "TASK_STATUS_COMPLETED"
  out=$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS")
  local fcount; fcount=$(jsonq "$(echo "$out" | tail -n +2)" "len(d.get('findings',[]))")
  check "发现数 ≥1（回归：共享卷生效，非空目录扫描，实际=$fcount）" test "${fcount:-0}" -ge 1
  echo "$tid" > /tmp/sim-e2e-task-id
  rm -rf "$work"
}

# ---------- 09 可观测面（快照聚合：执行日志/AI 交互日志/通知）----------
# 回归锚点（2026-09-05 GUI 实测暴露）：runtime→task 的 AppendTaskLog 地址缺口
# （执行日志静默丢失）与 storage 通知 memory 降级档（通知恒空）——可观测通道必须
# 真实可达，不许静默吞错。
c09_observability() {
  echo "[09] 可观测面（快照聚合 + AI 交互日志 + 通知）"
  local tid; tid=$(cat /tmp/sim-e2e-task-id 2>/dev/null)
  [ -z "$tid" ] && { echo "  -（无前置任务，先跑 08）"; return 0; }
  local out; out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
  check "详情快照可达（200，ADR-170 聚合口）" eq "$(echo "$out" | head -1)" "200"
  check "快照含执行日志（task 状态流转行）" contains "$(echo "$out" | tail -n +2)" "状态流转"
  local uid; uid=$(echo "$ACCESS" | cut -d. -f2 | python3 -c "import sys,base64,json;s=sys.stdin.read().strip();s+='='*(-len(s)%4);print(json.loads(base64.urlsafe_b64decode(s))['sub'])")
  local nout; nout=$(http GET "/v1/notifications?user_id=$uid" "" "$ACCESS")
  local ncount; ncount=$(jsonq "$(echo "$nout" | tail -n +2)" "len(d.get('notifications',[]))")
  check "通知列表非空（回归：storage 生产档位，实际=$ncount）" test "${ncount:-0}" -ge 1
}

# ---------- 10 推理 provider/路由管理面（ADR-217）----------
# 经 gateway → runtime → manager → OpenShell 网关的真实链路（gateway.db 权威存储）。
# fixture 用假凭据+不可达 base_url，全程不触真实 LLM egress；现役路由先存后恢复。
c10_inference_admin() {
  echo "[10] 推理 provider/路由管理面（ADR-217 透传链）"
  check "登录 admin" login
  local out name="e2e-prov-$(date +%s)"

  # 未认证 → 401（admin 门禁由引擎单测锁定 403，此处锁传输面）
  out=$(http GET /v1/inference/providers)
  check "未认证访问 → 401" eq "$(echo "$out" | head -1)" "401"

  # 增（upsert create 路径）
  out=$(http POST /v1/inference/providers \
    "{\"name\":\"$name\",\"type\":\"openai\",\"credentials\":{\"OPENAI_API_KEY\":\"sk-e2e-fixture\"},\"config\":{\"OPENAI_BASE_URL\":\"http://127.0.0.1:9/v1\"}}" "$ACCESS")
  check "创建 fixture provider（200）" eq "$(echo "$out" | head -1)" "200"
  check "回执 created=true（走网关 CreateProvider）" contains "$(echo "$out" | tail -n +2)" '"created":true'

  # 查（list/detail，凭据不回流）
  out=$(http GET /v1/inference/providers "" "$ACCESS")
  check "清单含 fixture" contains "$(echo "$out" | tail -n +2)" "$name"
  check "清单/详情永不回显凭据" test -z "$(echo "$out" | grep 'sk-e2e-fixture')"
  out=$(http GET "/v1/inference/providers/$name" "" "$ACCESS")
  check "详情 200 且含 type" contains "$(echo "$out" | tail -n +2)" '"type":"openai"'

  # 改（upsert update 路径：created=false）
  out=$(http POST /v1/inference/providers \
    "{\"name\":\"$name\",\"type\":\"openai\",\"credentials\":{\"OPENAI_API_KEY\":\"sk-e2e-fixture-2\"},\"config\":{\"OPENAI_BASE_URL\":\"http://127.0.0.1:9/v2\"}}" "$ACCESS")
  check "更新走 UpdateProvider（created=false）" contains "$(echo "$out" | tail -n +2)" '"created":false'

  # R55 键名守卫（2026-09-11 报障"网关只认大写"；2026-09-18 口径反转 ADR-242 同步）：
  # 小写别名键必须 400 并指路该类型约定键——守卫按 map 序随机报首个命中键，
  # 断言 grep 具体约定键名（OPENAI_BASE_URL/OPENAI_API_KEY 其一，openai 型指路键）
  out=$(http POST /v1/inference/providers \
    "{\"name\":\"$name-alias\",\"type\":\"openai\",\"credentials\":{\"api_key\":\"sk\"},\"config\":{\"base_url\":\"http://127.0.0.1:9/v1\"}}" "$ACCESS")
  check "小写别名键 → 400 且指路约定键名（R55/ADR-242）" test "$(echo "$out" | head -1)$(echo "$out" | tail -n +2)" != "" -a "$(echo "$out" | head -1)" = "400" -a -n "$(echo "$out" | tail -n +2 | grep -E 'OPENAI_BASE_URL|OPENAI_API_KEY')"

  # 路由：存现场 → 切 fixture（no_verify 避开 LLM egress）→ 读回生效
  local orig_prov="" orig_model=""
  out=$(http GET /v1/inference/route "" "$ACCESS")
  # 全新部署路由未配置=404（manager R30 契约口径，dind 七战实证裸 500 缺陷已修）；
  # 200=已有路由。两态都算"读现场"成功
  check "读当前路由（200=已配置/404=未配置，实际=$(echo "$out" | head -1)）" \
    test "$(echo "$out" | head -1)" = "200" -o "$(echo "$out" | head -1)" = "404"
  orig_prov=$(jsonq "$(echo "$out" | tail -n +2)" "d.get('provider','')")
  orig_model=$(jsonq "$(echo "$out" | tail -n +2)" "d.get('model','')")
  out=$(http PUT /v1/inference/route "{\"provider\":\"$name\",\"model\":\"e2e-model\",\"no_verify\":true}" "$ACCESS")
  check "切路由 no_verify=true（200）" eq "$(echo "$out" | head -1)" "200"
  check "回执 validation_performed=false（no_verify 语义）" contains "$(echo "$out" | tail -n +2)" '"validation_performed":false'
  out=$(http GET /v1/inference/route "" "$ACCESS")
  check "路由生效指向 fixture" contains "$(echo "$out" | tail -n +2)" "$name"

  # 真实验证路径（2026-09-12 待办收尾，补"验证连通性"覆盖为零的盲区）：本地 stub 起
  # openai 兼容端点，no_verify=false 切路由 → 网关实测端点。stub 不可用（端口占用/
  # python 缺失）则如实 SKIP。引擎容器经所在 compose 网络的网桥网关回宿主 stub——
  # 网关 IP 从运行栈事实推导（模拟栈=192.0.2.210、生产=172.28.110.1，不钉死；
  # docker 不可用或无栈时回退模拟值，可用 CODEAUDIT_E2E_STUB_HOST 显式覆盖）。
  local stub_pid="" stub_port=19419 stub_host="${CODEAUDIT_E2E_STUB_HOST:-}"
  if [ -z "$stub_host" ]; then
    local stub_container stub_net
    stub_container=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 -E '^codeaudit(-sim)?-runtime$' \
      || docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 '^codeaudit' || true)
    if [ -n "$stub_container" ]; then
      # 七战实测：compose 只显式钉 Subnet 未钉 Gateway 时 IPAM.Config[0].Gateway
      # 为空串——首选容器自身缺省路由（default via，运行态事实源），network inspect
      # 退居兜底
      stub_host=$(docker exec "$stub_container" sh -c 'ip route' 2>/dev/null | awk '$1=="default"{print $3; exit}')
      if [ -z "$stub_host" ]; then
        stub_net=$(docker inspect --format '{{range $k,$_ := .NetworkSettings.Networks}}{{$k}} {{end}}' "$stub_container" 2>/dev/null | awk '{print $1}')
        [ -n "$stub_net" ] && stub_host=$(docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}' "$stub_net" 2>/dev/null || true)
      fi
    fi
  fi
  stub_host="${stub_host:-192.0.2.210}"
  python3 - <<'PYSTUB' >/dev/null 2>&1 &
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0)); self.rfile.read(n)
        body = json.dumps({"id": "stub", "object": "chat.completion",
                           "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}).encode()
        self.send_response(200); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 19419), H).serve_forever()
PYSTUB
  stub_pid=$!
  sleep 1
  if curl -sS -m 3 -o /dev/null -X POST "http://127.0.0.1:$stub_port/v1/chat/completions" 2>/dev/null; then
    out=$(http POST /v1/inference/providers \
      "{\"name\":\"$name-stub\",\"type\":\"openai\",\"credentials\":{\"OPENAI_API_KEY\":\"sk-stub\"},\"config\":{\"OPENAI_BASE_URL\":\"http://$stub_host:$stub_port\"}}" "$ACCESS")
    check "创建 stub provider（真实端点，大写约定键）" eq "$(echo "$out" | head -1)" "200"
    out=$(http PUT /v1/inference/route "{\"provider\":\"$name-stub\",\"model\":\"stub-model\",\"no_verify\":false}" "$ACCESS")
    check "真实验证路径：no_verify=false 切路由 200" eq "$(echo "$out" | head -1)" "200"
    check "回执 validation_performed=true（网关实测端点）" contains "$(echo "$out" | tail -n +2)" '"validation_performed":true'
    check "validated_endpoints 非空（端点可达被记录）" test -n "$(echo "$out" | tail -n +2 | grep -o '"validated_endpoints":\[[^]]\+\]')"
    http DELETE "/v1/inference/providers/$name-stub" "" "$ACCESS" >/dev/null
  else
    check_skip "真实验证路径：本地 stub 未就绪（端口 $stub_port）"
  fi
  [ -n "$stub_pid" ] && kill "$stub_pid" 2>/dev/null

  # 上游语义（2026-09-07 实测钉死）：网关允许删除"在用" provider（deleted:true），
  # 且路由悬空存活——破坏不在删除时暴露，而在下一次 AI 阶段路由解析时；
  # 前端"在用禁删"守卫是必要保护而非后端强制。
  out=$(http DELETE "/v1/inference/providers/$name" "" "$ACCESS")
  check "删除在用 provider 被网关接受（deleted:true，ADR-217 语义）" \
    contains "$(echo "$out" | tail -n +2)" '"deleted":true'

  # 恢复现场（2026-10-10 GUI 遍历判例升级契约）：原路由指向的 provider 仍存活 → 恢复；
  # 否则收尾必须 DELETE 路由。原实现"只记录不处置"会留悬空路由（指向已删 provider）——
  # 破坏不在删除时暴露，而在下一次 CreateSandbox 以 FAILED_PRECONDITION 500 硬失败
  # （共享 manager 被毒化后所有后续任务沙箱全灭，降级 RuleScan；122 生产栈实报判例）。
  # 通道说明：网关 REST /v1/inference/route 仅 GET|PUT（无 DELETE），走 manager 管理
  # API（同 c12 通道：env.sim token + 18800）；c07 全程 workspace 缺省=default（ADR-247 回落档）。
  # 注：GET /v1/inference/route 契约不透出 no_verify（proto InferenceRouteInfo 仅 provider/
  # model/version），原值不可知——恢复恒 no_verify=true（保守跳验证，2026-09-13 审计登记局限）
  local route_disposition=""
  if [ -n "$orig_prov" ] && [ "$orig_prov" != "$name" ]; then
    prov_st=$(http GET "/v1/inference/providers/$orig_prov" "" "$ACCESS" | head -1)
    if [ "$prov_st" = "200" ]; then
      out=$(http PUT /v1/inference/route "{\"provider\":\"$orig_prov\",\"model\":\"$orig_model\",\"no_verify\":true}" "$ACCESS")
      check "恢复原路由（$orig_prov 存活，PUT 恢复）" eq "$(echo "$out" | head -1)" "200"
      route_disposition="restored:$orig_prov"
    fi
  fi
  if [ -z "$route_disposition" ]; then
    local mgr_tok; mgr_tok=$(grep -m1 '^OPENSHELL_MANAGER_TOKEN=' "$SCRIPT_DIR/../env.sim" | cut -d= -f2-)
    local del_code; del_code=$(curl -sS -m 20 -o /dev/null -w '%{http_code}' -X DELETE \
      "http://192.0.2.122:18800/api/v1/inference/route?workspace=default" \
      -H "Authorization: Bearer $mgr_tok")
    # 200=已删 404=本就不在——两者皆"无悬空残留"（此处路由若存在必指向已删 fixture）
    check "清理悬空路由（orig=[$orig_prov/$orig_model] 空或已失效，DELETE default 路由，HTTP $del_code）" \
      test "$del_code" = "200" -o "$del_code" = "404"
    route_disposition="deleted"
  fi

  # 删除即成事实：详情 404 + 幂等重删 false（本步兼作 fixture 清理兜底）
  out=$(http GET "/v1/inference/providers/$name" "" "$ACCESS")
  check "删除后详情 404" eq "$(echo "$out" | head -1)" "404"
  out=$(http DELETE "/v1/inference/providers/$name" "" "$ACCESS")
  check "幂等重删 deleted=false" contains "$(echo "$out" | tail -n +2)" '"deleted":false'
}

# ---------- 11 增量扫描两连扫（ADR-225；设计=伞仓 docs/designs/incremental-scan.md） ----------
# 链路：首扫全量（基线）→ 改 1 增 1 删 1 后二扫增量 → 断言 changed/deleted 快照、
# findings 继承（未变更文件终态复制+继承标记）、变更文件旧 findings 不继承、
# 删除文件 findings 不复活 → 三扫零变更全继承 → 新项目无基线诚实降级全量。
# 注：AI 任务卡的增量段断言由 runtime 单测承担（sandbox_incremental_test.go）——
# 本用例锁定 SAST_ONLY 确定性路径，不引 LLM 依赖。
c11_incremental_two_scan() {
  echo "[11] 增量扫描两连扫（服务端内容对比+继承物化）"
  check "登录 admin" login
  local work; work=$(mktemp -d)
  local pid tid1 out body

  # 基线树：mod.py(将改) + keep.py(不动) + del.py(将删)
  cat > "$work/mod.py" <<'PY'
import sqlite3
def q(uid):
    cur = sqlite3.connect("a.db").cursor()
    cur.execute("SELECT * FROM t WHERE id = '%s'" % uid)  # B608
    return cur.fetchone()
PY
  cat > "$work/keep.py" <<'PY'
TOKEN = "hunter2-keep-secret"  # B105（继承锚点：二扫后此 finding 必须带继承标记）
PY
  cat > "$work/del.py" <<'PY'
PWD = "hunter2-del-secret"  # B105（删除锚点：二扫后不得复活）
PY
  make_zip_multi "$work/v1.zip" "$work" mod.py keep.py del.py

  out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-增量两连扫","default_branch":"main","default_scan_mode":"SCAN_MODE_SAST_ONLY"}}' "$ACCESS")
  pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "创建项目（上传型）" nonempty "$pid"

  tid1=$(scan_once "$work/v1.zip" "$pid" "")
  T0=$SECONDS
  check "首扫（全量基线）COMPLETED" eq "$(poll_task "$tid1" | cut -d'|' -f1)" "TASK_STATUS_COMPLETED"
  FULL_T=$((SECONDS - T0))
  out=$(http GET "/v1/findings?task_id=$tid1" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  local base_n; base_n=$(jsonq "$body" "len(d.get('findings',[]))")
  check "基线发现 ≥3（mod/keep/del 各至少 1，实际=$base_n）" test "${base_n:-0}" -ge 3
  local keep_id; keep_id=$(jsonq "$body" "[f for f in d['findings'] if f['location']['file_path'].endswith('keep.py')][0]['finding_id']")
  check "取 keep.py 基线 finding_id（继承对照锚）" nonempty "$keep_id"

  # 二扫树：mod.py 改内容 + new.py 新增 + del.py 删除（keep.py 原样）
  cat > "$work/mod.py" <<'PY'
import sqlite3
def q(uid, name):
    cur = sqlite3.connect("a.db").cursor()
    cur.execute("SELECT * FROM t WHERE id = '%s' AND n = '%s'" % (uid, name))  # B608（内容已变）
    return cur.fetchone()
PY
  cat > "$work/new.py" <<'PY'
API_SECRET = "hunter2-new-secret"  # B105（新发现锚点；注意 B105 默认词表=password/passwd/pwd/secret/token/secrete——不含 key，API_KEY 不会触发）
PY
  rm -f "$work/del.py" "$work/v1.zip"
  make_zip_multi "$work/v2.zip" "$work" mod.py keep.py new.py

  local tid2; tid2=$(scan_once "$work/v2.zip" "$pid" "true")
  local T1; T1=$SECONDS
  local res2; res2=$(poll_task "$tid2")
  echo "  [观测] 时长（A23.4，只观测不设阈值）：全量基线 ${FULL_T}s vs 增量二扫 $((SECONDS - T1))s"
  check "二扫（增量）COMPLETED（实际=$res2）" eq "$(echo "$res2" | cut -d'|' -f1)" "TASK_STATUS_COMPLETED"

  out=$(http GET "/v1/tasks/$tid2" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  check "快照：baseline=首扫任务" eq "$(jsonq "$body" "d.get('baseline_task_id','')")" "$tid1"
  check "快照：diff_source=content" eq "$(jsonq "$body" "d.get('diff_source','')")" "content"
  check "快照：changed=2（mod.py+new.py）" eq "$(jsonq "$body" "len(d.get('changed_files',[]))")" "2"
  check "快照：deleted=1（del.py）" eq "$(jsonq "$body" "len(d.get('deleted_files',[]))")" "1"
  check "快照：changed 含 mod.py" contains "$(jsonq "$body" "','.join(d.get('changed_files',[]))")" "mod.py"
  check "快照：deleted 为 del.py" contains "$(jsonq "$body" "','.join(d.get('deleted_files',[]))")" "del.py"

  out=$(http GET "/v1/findings?task_id=$tid2" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  local inh_n fresh_n del_n keep_inh
  inh_n=$(jsonq "$body" "len([f for f in d['findings'] if f.get('inherited_from_task_id')])")
  fresh_n=$(jsonq "$body" "len([f for f in d['findings'] if not f.get('inherited_from_task_id')])")
  del_n=$(jsonq "$body" "len([f for f in d['findings'] if f['location']['file_path'].endswith('del.py')])")
  keep_inh=$(jsonq "$body" "len([f for f in d['findings'] if f['location']['file_path'].endswith('keep.py') and f.get('inherited_from_task_id')])")
  check "继承 ≥1（keep.py 终态复制，实际=$inh_n）" test "${inh_n:-0}" -ge 1
  check "keep.py 继承项带 inherited 标记（实际=$keep_inh）" test "${keep_inh:-0}" -ge 1
  check "新发现 ≥2（mod.py 重扫+new.py，实际=$fresh_n）" test "${fresh_n:-0}" -ge 2
  check "del.py findings 不复活（实际=$del_n）" eq "${del_n:-0}" "0"
  check "继承标记指向基线任务" contains "$(jsonq "$body" "set(f.get('inherited_from_task_id','') for f in d['findings'])")" "$tid1"

  # A23.1 补盲（2026-09-11 review）：verdict 复制比对（"终态复制"的实证断言）+ 报告生成
  # 两轴重构（2026-10-10）：继承比对改 direction+tier 拼接（终态复制的完整两轴）
  local base_verdict; base_verdict=$(jsonq "$(http GET "/v1/findings/$keep_id" "" "$ACCESS" | tail -n +2)" "str((d.get('finding') or d).get('verdict_direction',''))+'|'+str((d.get('finding') or d).get('verdict_tier',''))")
  local inh_verdict; inh_verdict=$(jsonq "$body" "[f for f in d['findings'] if f['location']['file_path'].endswith('keep.py') and f.get('inherited_from_task_id')][0].get('verdict_direction','')+'|'+[f for f in d['findings'] if f['location']['file_path'].endswith('keep.py') and f.get('inherited_from_task_id')][0].get('verdict_tier','')")
  check "keep.py 基线判定两轴读回非空" nonempty "$base_verdict"
  check "keep.py 继承项判定两轴与基线一致（终态复制实证）" eq "$inh_verdict" "$base_verdict"
  local rep_n; rep_n=$(jsonq "$(http GET "/v1/reports?task_id=$tid2" "" "$ACCESS" | tail -n +2)" "len(d.get('reports',[]))")
  check "二扫报告已生成（完整视图产物，实际=$rep_n）" test "${rep_n:-0}" -ge 1

  # 三扫：零变更（同一 zip 重传）→ 全继承
  local tid3; tid3=$(scan_once "$work/v2.zip" "$pid" "true")
  check "三扫（零变更）COMPLETED" eq "$(poll_task "$tid3" | cut -d'|' -f1)" "TASK_STATUS_COMPLETED"
  out=$(http GET "/v1/tasks/$tid3" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  check "三扫快照：changed=0（零变更语义）" eq "$(jsonq "$body" "len(d.get('changed_files',[]))")" "0"
  out=$(http GET "/v1/findings?task_id=$tid3" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  local all_inh; all_inh=$(jsonq "$body" "len([f for f in d['findings'] if not f.get('inherited_from_task_id')])")
  check "三扫全继承（新发现=0，实际=$all_inh）" eq "${all_inh:-0}" "0"

  # 降级链：全新项目无基线 → 增量请求自动降级全量 + 原因可见（诚实降级）
  out=$(http POST /v1/projects '{"project":{"name":"sim-e2e-增量降级","default_branch":"main","default_scan_mode":"SCAN_MODE_SAST_ONLY"}}' "$ACCESS")
  local pid2; pid2=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  make_zip_multi "$work/d.zip" "$work" keep.py
  local tid4; tid4=$(scan_once "$work/d.zip" "$pid2" "true")
  check "无基线项目增量请求 → 降级后仍 COMPLETED" eq "$(poll_task "$tid4" | cut -d'|' -f1)" "TASK_STATUS_COMPLETED"
  out=$(http GET "/v1/tasks/$tid4" "" "$ACCESS"); body=$(echo "$out" | tail -n +2)
  check "降级原因可见（no_baseline，不静默）" contains "$(jsonq "$body" "d.get('config',{}).get('incremental_degraded_reason','')")" "no_baseline"
  check "降级任务无增量快照（baseline 空）" eq "$(jsonq "$body" "d.get('baseline_task_id','')")" ""

  echo "$tid2" > /tmp/sim-e2e-task-id
  rm -rf "$work"
}

# scan_once — 上传 zip + 建任务（incremental 空串=全量）+ start，输出 task_id
scan_once() { # scan_once <zip> <project_id> <incremental>
  local zip="$1" pid="$2" inc="$3" out tid fid
  local up; up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" \
    -H "Authorization: Bearer $ACCESS" -F "file=@$zip;type=application/zip")
  fid=$(jsonq "$up" "d['file_id']")
  out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_SAST_ONLY\",\"sast_tools\":[\"bandit\"],\"config\":{\"upload_file_id\":\"$fid\"},\"incremental\":${inc:-false},\"git_anchor\":{\"commit\":\"e2e0000000000000000000000000000000000000\",\"branch\":\"main\",\"dirty\":false,\"remote\":\"git://e2e\"}}" "$ACCESS")
  tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null
  echo "$tid"
}

# make_zip_multi — 多文件 zip（lib.sh make_zip 仅单文件）
make_zip_multi() { # make_zip_multi <输出.zip> <目录> <文件...>
  python3 - "$@" <<'PY'
import sys, zipfile
out, src, names = sys.argv[1], sys.argv[2], sys.argv[3:]
with zipfile.ZipFile(out, 'w') as z:
    for n in names:
        z.write(f"{src}/{n}", n)
PY
}

# ---------- 主流程 ----------
run_case() {
  echo ""; echo "======== 用例 $1 ========"
  case "$1" in
    01) c01_health ;; 02) c02_auth ;; 03) c03_projects ;;
    04) c04_sast_fullchain ;; 05) c05_web ;; 06) c06_notifications ;;
    07) c07_ai ;; 08) c08_project_upload_autotask ;; 09) c09_observability ;;
    10) c10_inference_admin ;; 11) c11_incremental_two_scan ;;
    12) c12_team_workspace_path ;; 13) c13_pack_engines ;;
    *) echo "未知用例 $1"; exit 2 ;;
  esac
}


# ---------- 12 team 工作区路径（切片3b team 上车；设计=伞仓 docs/designs/platform-slice3b-team-onboarding.md） ----------
# 链路：manager 建区 → engine /v1/workspaces 透传（新 RPC 全链）→ team 区注册
# mock provider+route（PUT 实核回执=L7 按 team 路由转发的证明）→ project.team 任务
# （沙箱建于 team 区、DSH 握手经 team 路由打 mock）→ usage 端点形状 → 清理。
# 诚实口径：mock 非 LLM，任务终态 FAILED/TIMEOUT+error 非空=被测行为（c07 同型）；
# 运行中且 mock 已被 DSH 打过（握手请求）同样证明全链。
c12_team_workspace_path() {
  echo "[12] team 工作区路径（workspace 供给+per-team 推理路由）"
  check "登录 admin" login
  local WS=sim-e2e-team
  local MTOK; MTOK=$(grep -m1 '^OPENSHELL_MANAGER_TOKEN=' "$SCRIPT_DIR/../env.sim" | cut -d= -f2-)
  check "取得 manager token（env.sim）" nonempty "$MTOK"
  local MGR=http://192.0.2.122:18800
  mgr() { curl -sS -m 20 -X "$1" "$MGR$2" -H "Authorization: Bearer $MTOK" -H 'content-type: application/json' ${3:+-d "$3"}; }

  # mock LLM（107 本机；gateway 容器可达 LXC IP）——路由证明的接收端
  local MD; MD=$(mktemp -d)
  cat > "$MD/mockllm.py" <<'MOCKEOF'
import json, datetime
from http.server import BaseHTTPRequestHandler, HTTPServer
LOG = __file__ + '.hits'
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('content-length', 0)); b = self.rfile.read(n)
        open(LOG, 'a').write(json.dumps({'t': datetime.datetime.now().isoformat(), 'p': self.path, 'b': b[:200].decode('utf-8','replace')}) + '\n')
        r = json.dumps({"id":"mock","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"2"},"finish_reason":"stop"}]}).encode()
        self.send_response(200); self.send_header('content-type','application/json'); self.send_header('content-length',str(len(r))); self.end_headers(); self.wfile.write(r)
    def do_GET(self):
        self.send_response(200); self.send_header('content-length','2'); self.end_headers(); self.wfile.write(b'ok')
    def log_message(self, *a): pass
HTTPServer(('0.0.0.0', 18099), H).serve_forever()
MOCKEOF
  nohup python3 "$MD/mockllm.py" >/dev/null 2>&1 &
  local MOCK_PID=$!

  # ① 建区（幂等：ALREADY_EXISTS 的 409 也含区名，视为在位）
  local out; out=$(mgr POST /api/v1/workspaces "{\"name\":\"$WS\",\"labels\":{\"e2e\":\"c12\"}}")
  check "建区 $WS（200 或已存在）" contains "$out" "$WS"
  # ② engine 透传：GET /v1/workspaces 经 sim gateway（admin）
  out=$(http GET /v1/workspaces "" "$ACCESS")
  check "engine /v1/workspaces 透传含 $WS" contains "$(echo "$out" | tail -n +2)" "$WS"
  # ③ team 区 mock provider + route（PUT 实核=L7 按 team 路由转发证明）
  out=$(mgr PUT /api/v1/inference/providers "{\"workspace\":\"$WS\",\"name\":\"mockz\",\"type\":\"openai\",\"credentials\":{\"OPENAI_API_KEY\":\"e2e-mock\"},\"config\":{\"OPENAI_BASE_URL\":\"http://192.0.2.122:18099/v1\"}}")
  check "team 区注册 mock provider" contains "$out" 'mockz'
  out=$(mgr PUT /api/v1/inference/route "{\"workspace\":\"$WS\",\"provider\":\"mockz\",\"model\":\"e2e-m\"}")
  check "team 区 route 实核通过（validated_endpoints 含 mock）" contains "$out" '18099'

  # ④ project.team 任务：沙箱建于 team 区、DSH 经 team 路由
  local work; work=$(mktemp -d)
  printf 'TOKEN = "team-e2e-secret"  # B105\n' > "$work/app.py"
  make_zip "$work/app.zip" "$work/app.py" app.py
  local up fid pid tid
  up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" -H "Authorization: Bearer $ACCESS" -F "file=@$work/app.zip;type=application/zip")
  fid=$(jsonq "$up" "d['file_id']")
  out=$(http POST /v1/projects "{\"project\":{\"name\":\"sim-e2e-team路径\",\"team\":\"$WS\",\"default_branch\":\"main\",\"default_scan_mode\":\"SCAN_MODE_AI_ONLY\"}}" "$ACCESS")
  pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "创建带 team 的项目" nonempty "$pid"
  http PUT "/v1/projects/$pid/config" "{\"config\":{\"project_id\":\"$pid\",\"config\":{\"upload_file_id\":\"$fid\"}}}" "$ACCESS" >/dev/null
  out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_AI_ONLY\",\"sast_tools\":[],\"config\":{}}" "$ACCESS")
  tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
  check "创建 AI 任务（team=$WS）" nonempty "$tid"
  http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null

  # 有界等待：终态（FAILED/TIMEOUT/DEAD 之一）或 mock 被 DSH 打过（握手/推理请求）
  local res="" i hits=0
  for i in $(seq 1 36); do
    sleep 5
    hits=$(grep -c 'dsh_plugin_packages\|"messages"' "$MD/mockllm.py.hits" 2>/dev/null); hits=${hits:-0}
    res=$(http GET "/v1/tasks/$tid" "" "$ACCESS" | tail -n +2 | jsonq - "d.get('task',{}).get('status','')" 2>/dev/null || echo "")
    case "$res" in
      TASK_STATUS_COMPLETED|TASK_STATUS_FAILED|TASK_STATUS_TIMEOUT|TASK_STATUS_DEAD) break ;;
    esac
    [ "${hits:-0}" -ge 3 ] && break   # DSH 握手已到 mock，全链已证
  done
  check "任务到终态或 mock 已被 DSH 打过（status=${res:-运行中} hits=${hits:-0}）" test -n "$res" -o "${hits:-0}" -ge 3
  hits=$(grep -c 'dsh_plugin_packages\|"messages"' "$MD/mockllm.py.hits" 2>/dev/null); hits=${hits:-0}
  check "team 路由转发实证：mock 收到 LLM 请求（hits=${hits:-0}）" test "${hits:-0}" -ge 2

  # ⑤ usage 端点形状（终态钩子归集；失败任务=最需要计量的面，ADR-247）
  out=$(http GET "/v1/tasks/$tid/usage" "" "$ACCESS")
  check "GET /v1/tasks/{id}/usage → 200" eq "$(echo "$out" | head -1)" "200"

  # ⑥ 清理（逆序：取消在跑任务 → route → provider → 空区删除；mock 收尾）
  case "$res" in
    TASK_STATUS_RUNNING|"") http POST "/v1/tasks/$tid/cancel" "" "$ACCESS" >/dev/null 2>&1; sleep 8 ;;
  esac
  mgr DELETE "/api/v1/inference/route?workspace=$WS" >/dev/null
  mgr DELETE "/api/v1/inference/providers/mockz?workspace=$WS" >/dev/null
  local del; del=$(mgr DELETE "/api/v1/workspaces/$WS")
  check "清理：空区删除成功（资源守卫放行）" contains "$del" '"deleted":true'
  kill "$MOCK_PID" 2>/dev/null
  rm -rf "$MD" "$work"
}

# ---------- 13 pack 双引擎（audit-sample 会话轨 / audit-graph 图编排轨） ----------
# 2026-10-10 立（用户验收反馈：发布验证未覆盖两执行引擎）。断言口径=平台诚实哲学：
# 引擎被真实驱动到终态（COMPLETED=全链走通，或诚实失败/降级=链路面真实演练过），
# 不要求 mock LLM 下产出语义正确的审计结论。
c13_pack_engines() {
  echo "[13] pack 双引擎（audit-sample / audit-graph）"
  check "登录 admin" login
  # 靶标样本（与 c07 同款双漏洞）
  local work; work=$(mktemp -d)
  cat > "$work/app.py" <<'PY'
import sqlite3
def get_user(uid):
    conn = sqlite3.connect("app.db")
    cur = conn.cursor()
    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)
    return cur.fetchone()
API_TOKEN = "hunter2-hardcoded-secret"
PY
  make_zip "$work/sample.zip" "$work/app.py" app.py
  local up fid; up=$(curl -sS -m 60 -X POST "$BASE_URL/v1/uploads/archive" -H "Authorization: Bearer $ACCESS" -F "file=@$work/sample.zip;type=application/zip")
  fid=$(jsonq "$up" "d['file_id']")
  check "上传压缩包取得 file_id" nonempty "$fid"

  # pack 实质断言内核：COMPLETED ⇒ AI 面有真内容（total_bytes>0 且含真实完成回合——
  # 纯降级日志只有失败回合收尾，c07 判例同款 ai_log_has_completed_round）
  pack_assert() { # pack_assert <label> <pack> <timeout>
    local label="$1" pack="$2" tmo="$3" out pid tid res status emsg body ai_bytes
    out=$(http POST /v1/projects "{\"project\":{\"name\":\"sim-e2e-pack-$pack\",\"default_branch\":\"main\",\"default_scan_mode\":\"SCAN_MODE_AI_ONLY\"}}" "$ACCESS")
    pid=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
    check "创建 $label 项目" nonempty "$pid"
    http PUT "/v1/projects/$pid/config" "{\"config\":{\"project_id\":\"$pid\",\"config\":{\"upload_file_id\":\"$fid\",\"pack\":\"$pack\"}}}" "$ACCESS" >/dev/null
    out=$(http POST /v1/tasks "{\"project_id\":\"$pid\",\"scan_mode\":\"SCAN_MODE_AI_ONLY\",\"sast_tools\":[],\"config\":{\"pack\":\"$pack\"}}" "$ACCESS")
    tid=$(jsonq "$(echo "$out" | tail -n +2)" "d['task_id']")
    check "创建 $label pack 任务（pack 解析+任务受理）" nonempty "$tid"
    http POST "/v1/tasks/$tid/start" "" "$ACCESS" >/dev/null
    [ "$pack" = audit-graph ] && { out=$(http GET "/v1/tasks/$tid/graph" "" "$ACCESS"); check "$label graph 端点可达（200）" eq "$(echo "$out" | head -1)" "200"; }
    res=$(poll_task "$tid" "$tmo")
    status="${res%%|*}"; emsg="${res#*|}"
    case "$status" in
      TASK_STATUS_COMPLETED)
        check "$label 任务 COMPLETED（引擎全链）" eq "$status" "TASK_STATUS_COMPLETED"
        body=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS" | tail -n +2)
        ai_bytes=$(jsonq "$body" "int((d.get('ai') or {}).get('total_bytes') or 0)")
        if [ "${ai_bytes:-0}" -gt 0 ] && ai_log_has_completed_round "$tid"; then
          check "$label AI 面真内容（${ai_bytes}B 含真实完成回合——沙箱×DSH×推理全链实证）" test "${ai_bytes:-0}" -gt 0
          # v0.1.2 持久化补齐（AI 路径）：title 由 DSH submit_findings 必填提交——锁非空；
          # cwe_id 模板允许"或空"（模型自报合法为空），只锁字段在位不锁非空
          local fbody; fbody=$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS" | tail -n +2)
          check "$label 发现 title 持久化（首条非空）" test -n "$(jsonq "$fbody" "str((d.get('findings') or [{}])[0].get('title',''))")"
          check "$label 发现 cwe_id 字段在位" contains "$fbody" '"cwe_id"'
        else
          check "$label COMPLETED 走降级（AI 面无真实完成回合 → 诚实降级语义，阶段面核验）" contains "$body" '"status":"STAGE_STATUS_COMPLETED"'
        fi ;;
      TASK_STATUS_FAILED|TASK_STATUS_DEAD|TASK_STATUS_TIMEOUT)
        check "$label 任务诚实终态=$status（error_message 非空）" nonempty "$emsg" ;;
      *) check "$label 任务到达终态（实际=$status）" eq "$status" "TASK_STATUS_COMPLETED" ;;
    esac
  }

  pack_assert audit-sample audit-sample "${TASK_TIMEOUT_AI:-900}"
  pack_assert audit-graph audit-graph "${TASK_TIMEOUT_AI:-1200}"
  rm -rf "$work"
}

login || { echo "登录失败——模拟栈未就绪或凭据不符（BASE_URL=$BASE_URL）" >&2; exit 1; }

if [ $# -gt 0 ]; then
  for c in "$@"; do run_case "$c"; done
else
  for c in 01 02 03 04 05 06 07 08 09 10 11 12 13; do run_case "$c"; done
fi

echo ""
echo "================ e2e 结果 ================"
echo "通过=$PASS 失败=$FAIL 跳过=$SKIP"
if [ "$FAIL" -gt 0 ]; then
  printf '失败项:\n'; printf '  - %s\n' "${FAILED_CASES[@]}"
  exit 1
fi
exit 0
