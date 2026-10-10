#!/usr/bin/env bash
# =============================================================================
# preprod_run.sh — 现役预生产栈直测套（2026-10-09 立；基于 tests/run.sh 资产派生）
#
# 语义=**部署验证面**：对"已在运行"的预生产栈做黑盒断言（健康/认证/只读面/可自清
# CRUD），不起栈、不 docker、零栈操作——适用于"真实预生产环境常驻"拓扑：sim/prod
# 互斥立制下 CI 不得 deploy-sim，完整提交级回归由 sim-e2e 作业承担（when: manual，
# 需无生产栈窗口）。测试资产（http/check/login 原语、端点口径）与 run.sh 同源
# （lib.sh），断言风格一致。
#
# 用法: CODEAUDIT_SIM_URL=http://<gw>:8090 CODEAUDIT_SIM_CONSOLE_URL=http://<web>:8088 \
#       bash deploy/internal/tests/preprod_run.sh
# 凭据: CODEAUDIT_SIM_USER/PASS（缺省 admin/admin）。登录失败（口令已改）≠栈缺陷
#       →授权面用例 SKIP 不计失败。
# 污染纪律: 只读为主；唯一写面=命名空间化项目 CRUD（ci-preprod-<ts>）且 DeleteProject
#       自清——对共享预生产库零残留。
# 退出码: 0=全部通过（SKIP 不计）；1=存在失败。
# =============================================================================
set -u
cd "$(dirname "$0")" && . ./lib.sh

PP_TAG="ci-preprod-$(date +%s)"
LOGIN_OK=0

# ---------- P1 健康与边界 ----------
p01_health() {
  echo "[P1] 健康与边界"
  local out
  out=$(http GET /health)
  check "gateway /health → 200" eq "$(echo "$out" | head -1)" "200"
  check "健康响应含 status=ok" contains "$(echo "$out" | tail -n +2)" '"ok"'
  out=$(http GET /v1/tools)
  check "未认证 /v1/tools 被拒（401）" eq "$(echo "$out" | head -1)" "401"
}

# ---------- P2 前端接入面 ----------
p02_web() {
  echo "[P2] 前端接入面"
  local out
  out=$(curl -sS -m 10 -o /tmp/pp-web-body -w '%{http_code}' "$WEB_URL/")
  check "web 首页 200" eq "$out" "200"
  check "SPA 宿主页（根挂载点）" contains "$(cat /tmp/pp-web-body)" 'id="root"'
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$WEB_URL/tasks")
  check "SPA 路由回退（/tasks → 200）" eq "$out" "200"
  out=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$WEB_URL/v1/tools")
  check "web /v1 反代透传（401）" eq "$out" "401"
}

# ---------- P3 认证面 ----------
p03_auth() {
  echo "[P3] 认证面"
  local out
  out=$(http POST /v1/auth/login "{\"username\":\"$USERNAME\",\"password\":\"wrong-password\"}")
  check "错误口令被拒（非200）" test "$(echo "$out" | head -1)" != "200"
  if login; then
    LOGIN_OK=1; echo "  ✓ 正确口令登录成功（授权面用例启用）"
  else
    echo "  - 登录失败（预生产口令已改？CODEAUDIT_SIM_PASS 可覆写）→授权面用例跳过（非栈缺陷）"
  fi
}

# ---------- P4 授权只读面（登录成功时）----------
p04_readonly() {
  echo "[P4] 授权只读面"
  if [ "$LOGIN_OK" != 1 ]; then echo "  -（SKIP）"; return 0; fi
  local out
  out=$(http GET /v1/projects "" "$ACCESS")
  check "项目列表 → 200" eq "$(echo "$out" | head -1)" "200"
  out=$(http GET /v1/tasks "" "$ACCESS")
  check "任务列表 → 200" eq "$(echo "$out" | head -1)" "200"
  out=$(http GET /v1/notifications?unread_only=true "" "$ACCESS")
  check "通知面可达（200）" eq "$(echo "$out" | head -1)" "200"
  out=$(http GET /v1/inference/providers "" "$ACCESS")
  check "推理 provider 面（admin 门禁 200）" eq "$(echo "$out" | head -1)" "200"
  out=$(http GET /v1/inference/route "" "$ACCESS")
  check "推理路由面 → 200" eq "$(echo "$out" | head -1)" "200"
}

# ---------- P5 可自清 CRUD（唯一写面；命名空间化+删除回收）----------
p05_crud_clean() {
  echo "[P5] 项目 CRUD（写面·自清）"
  if [ "$LOGIN_OK" != 1 ]; then echo "  -（SKIP）"; return 0; fi
  local out id
  out=$(http POST /v1/projects "{\"project\":{\"name\":\"$PP_TAG\",\"repo_url\":\"https://demo.example/preprod.git\",\"default_branch\":\"main\",\"default_scan_mode\":\"SCAN_MODE_SAST_ONLY\"}}" "$ACCESS")
  check "创建命名空间项目（200/201）" test "$(echo "$out" | head -1)" = "200" -o "$(echo "$out" | head -1)" = "201"
  id=$(jsonq "$(echo "$out" | tail -n +2)" "(d.get('project') or d)['project_id']")
  check "响应含 project_id" nonempty "$id"
  if [ -n "$id" ]; then
    out=$(http PUT "/v1/projects/$id/config" '{"config":{"project_id":"'"$id"'","config":{"ci_marker":"preprod"}}}' "$ACCESS")
    check "写入项目 config（200）" eq "$(echo "$out" | head -1)" "200"
    out=$(http GET "/v1/projects/$id/config" "" "$ACCESS")
    check "读回 config 含 ci_marker" contains "$(echo "$out" | tail -n +2)" "preprod"
    out=$(http DELETE "/v1/projects/$id" "" "$ACCESS")
    check "删除测试项目（自清，2xx）" test "$(echo "$out" | head -1)" = "200" -o "$(echo "$out" | head -1)" = "204"
    out=$(http GET /v1/projects "" "$ACCESS")
    check "列表不再含测试项目（零残留）" test ! "$(echo "$out" | tail -n +2)" = *"$PP_TAG"*
  fi
}

for c in p01_health p02_web p03_auth p04_readonly p05_crud_clean; do $c; done

echo
echo "结果：PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf '失败项: %s\n' "${FAILED_CASES[@]}"
  exit 1
fi
exit 0
