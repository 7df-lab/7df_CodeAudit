#!/usr/bin/env bash
# =============================================================================
# run.sh — CodeAudit 生产模拟环境功能测试套
#
# 对"生产模拟栈"（deploy/sim.sh up 拉起）做黑盒功能验证：只经 gateway/console
# 的 HTTP 面驱动真实链路（上传→storage→task 拉包解包→SAST→result→报告/通知）。
#
# 用法:  bash deploy/tests/run.sh [用例编号，如 04]
# 退出码: 0=全部通过；非零=存在失败（失败明细见输出）
# =============================================================================
set -u
cd "$(dirname "$0")" && . ./lib.sh

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
  check "发现标注 source_tool=bandit" contains "$(echo "$out" | tail -n +2)" '"bandit"'

  # 源码全文端点冒烟（gw-f6a3523 实证防回归：ADR-200 拉包流布局迁移后 source-file
  # 解析链四流全死 → 发现详情"源码全文不可用/Sink 链路不可用"。上传流任务的源根
  # 必须 经 ①b uploads-<task_id>/unpacked 流解析命中）。
  out=$(http GET "/v1/tasks/$tid/source-file?path=app.py" "" "$ACCESS")
  check "source-file 对上传流任务返回 200（源根解析链存活）" eq "$(echo "$out" | head -1)" "200"
  check "source-file 内容即样本源码" contains "$(echo "$out" | tail -n +2)" "SQLITE_INJECTION"

  out=$(http GET "/v1/reports?task_id=$tid" "" "$ACCESS")
  check "任务报告已生成（reports 按 task 过滤含该任务）" contains "$(echo "$out" | tail -n +2)" "$tid"
  echo "$tid" > /tmp/sim-e2e-task-id   # 供 06 通知用例复用
  rm -rf "$work"
}

# ---------- 05 控制台 ----------
c05_console() {
  echo "[05] 前端控制台（容器内 nginx）"
  local out; out=$(curl -sS -m 10 -o /tmp/sim-console-body -w '%{http_code}' "$CONSOLE_URL/")
  check "console 首页 200" eq "$out" "200"
  check "返回 SPA 宿主页（含根挂载点）" contains "$(cat /tmp/sim-console-body)" 'id="root"'
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$CONSOLE_URL/tasks")
  check "SPA 路由回退（/tasks → 200）" eq "$out" "200"
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$CONSOLE_URL/v1/tools")
  check "console 的 /v1 反代连通网关（401 透传）" eq "$out" "401"
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
      # 形态甄别：AI 交互日志非空=真全链；否则须为 RuleScan 降级（ai_verdict=NEEDS_MANUAL）
      out=$(http GET "/v1/tasks/$tid/snapshot" "" "$ACCESS")
      local ai_bytes; ai_bytes=$(jsonq "$(echo "$out" | tail -n +2)" "int((d.get('ai') or {}).get('total_bytes') or 0)")
      if [ "${ai_bytes:-0}" -gt 0 ]; then
        check "AI 任务 COMPLETED 且交互日志非空=${ai_bytes}B（manager/沙箱/LLM 全链真实走通）" test "${ai_bytes:-0}" -gt 0
      else
        check "AI 任务 COMPLETED 走 RuleScan 降级（交互日志空 → 发现须标 NEEDS_MANUAL）" contains "$(http GET "/v1/findings?task_id=$tid" "" "$ACCESS" | tail -n +2)" "NEEDS_MANUAL"
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
# 回归锚点（2026-09-05 GUI 实测暴露）：dsh-runtime→task 的 AppendTaskLog 地址缺口
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
# 经 gateway → dsh-runtime → manager → OpenShell 网关的真实链路（gateway.db 权威存储）。
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
    "{\"name\":\"$name\",\"type\":\"openai\",\"credentials\":{\"api_key\":\"sk-e2e-fixture\"},\"config\":{\"base_url\":\"http://127.0.0.1:9/v1\"}}" "$ACCESS")
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
    "{\"name\":\"$name\",\"type\":\"openai\",\"credentials\":{\"api_key\":\"sk-e2e-fixture-2\"},\"config\":{\"base_url\":\"http://127.0.0.1:9/v2\"}}" "$ACCESS")
  check "更新走 UpdateProvider（created=false）" contains "$(echo "$out" | tail -n +2)" '"created":false'

  # 路由：存现场 → 切 fixture（no_verify 避开 LLM egress）→ 读回生效
  local orig_prov="" orig_model=""
  out=$(http GET /v1/inference/route "" "$ACCESS")
  check "读当前路由（200）" eq "$(echo "$out" | head -1)" "200"
  orig_prov=$(jsonq "$(echo "$out" | tail -n +2)" "d.get('provider','')")
  orig_model=$(jsonq "$(echo "$out" | tail -n +2)" "d.get('model','')")
  out=$(http PUT /v1/inference/route "{\"provider\":\"$name\",\"model\":\"e2e-model\",\"no_verify\":true}" "$ACCESS")
  check "切路由 no_verify=true（200）" eq "$(echo "$out" | head -1)" "200"
  check "回执 validation_performed=false（no_verify 语义）" contains "$(echo "$out" | tail -n +2)" '"validation_performed":false'
  out=$(http GET /v1/inference/route "" "$ACCESS")
  check "路由生效指向 fixture" contains "$(echo "$out" | tail -n +2)" "$name"

  # 上游语义（2026-09-07 实测钉死）：网关允许删除"在用" provider（deleted:true），
  # 且路由悬空存活——破坏不在删除时暴露，而在下一次 AI 阶段路由解析时；
  # 前端"在用禁删"守卫是必要保护而非后端强制。
  out=$(http DELETE "/v1/inference/providers/$name" "" "$ACCESS")
  check "删除在用 provider 被网关接受（deleted:true，ADR-217 语义）" \
    contains "$(echo "$out" | tail -n +2)" '"deleted":true'

  # 恢复现场：原路由存在且非 fixture → 先恢复再清理；否则路由已被上一步牵动，如实记录
  if [ -n "$orig_prov" ] && [ "$orig_prov" != "$name" ]; then
    out=$(http PUT /v1/inference/route "{\"provider\":\"$orig_prov\",\"model\":\"$orig_model\",\"no_verify\":true}" "$ACCESS")
    check "恢复原路由（$orig_prov）" eq "$(echo "$out" | head -1)" "200"
  else
    echo "  -（原路由为空或即 fixture：路由现场未恢复，记录 orig=[$orig_prov/$orig_model]）"
  fi

  # 删除即成事实：详情 404 + 幂等重删 false（本步兼作 fixture 清理兜底）
  out=$(http GET "/v1/inference/providers/$name" "" "$ACCESS")
  check "删除后详情 404" eq "$(echo "$out" | head -1)" "404"
  out=$(http DELETE "/v1/inference/providers/$name" "" "$ACCESS")
  check "幂等重删 deleted=false" contains "$(echo "$out" | tail -n +2)" '"deleted":false'
}

# ---------- 主流程 ----------
run_case() {
  echo ""; echo "======== 用例 $1 ========"
  case "$1" in
    01) c01_health ;; 02) c02_auth ;; 03) c03_projects ;;
    04) c04_sast_fullchain ;; 05) c05_console ;; 06) c06_notifications ;;
    07) c07_ai ;; 08) c08_project_upload_autotask ;; 09) c09_observability ;;
    10) c10_inference_admin ;;
    *) echo "未知用例 $1"; exit 2 ;;
  esac
}

login || { echo "登录失败——模拟栈未就绪或凭据不符（BASE_URL=$BASE_URL）" >&2; exit 1; }

if [ $# -gt 0 ]; then
  for c in "$@"; do run_case "$c"; done
else
  for c in 01 02 03 04 05 06 07 08 09 10; do run_case "$c"; done
fi

echo ""
echo "================ e2e 结果 ================"
echo "通过=$PASS 失败=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '失败项:\n'; printf '  - %s\n' "${FAILED_CASES[@]}"
  exit 1
fi
exit 0
