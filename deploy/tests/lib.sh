#!/usr/bin/env bash
# lib.sh — 生产模拟 e2e 公共库：HTTP 工具 / 断言 / 轮询
# 依赖: curl, python3（仅 JSON 字段提取）

BASE_URL="${CODEAUDIT_SIM_URL:-http://localhost:18080}"
CONSOLE_URL="${CODEAUDIT_SIM_CONSOLE_URL:-http://localhost:18088}"
USERNAME="${CODEAUDIT_SIM_USER:-admin}"
PASSWORD="${CODEAUDIT_SIM_PASS:-admin}"
TASK_TIMEOUT="${TASK_TIMEOUT:-600}"

PASS=0; FAIL=0; FAILED_CASES=()

check() { # check <说明> <退出码式命令>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ✓ $desc"; PASS=$((PASS+1)); return 0
  else
    echo "  ✗ $desc"; FAIL=$((FAIL+1)); FAILED_CASES+=("$desc"); return 1
  fi
}

eq() { [ "$1" = "$2" ]; }                    # eq <实际> <期望>
contains() { echo "$1" | grep -qF "$2"; }    # contains <文本> <子串>
nonempty() { [ -n "$1" ]; }

make_zip() { # make_zip <输出.zip> <文件...> —— python3 造 zip(宿主可能无 zip 二进制,python3 是已声明依赖)
  python3 -c "import sys,zipfile;zipfile.ZipFile(sys.argv[1],'w').write(sys.argv[2],sys.argv[3].rsplit('/',1)[-1])" "$@"
}

jsonq() { # jsonq <json文本> <python表达式，d=对象> —— 提取字段
  echo "$1" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
expr=sys.argv[1]
try:
    r=eval(expr, {'d': d})
except Exception:
    sys.exit(1)
print(r if r is not None else '')
" "$2"
}

http() { # http <method> <path> [json-body] [token] → 输出 "状态码\n响应体"
  local method="$1" path="$2" body="${3:-}" token="${4:-}"
  local args=(-sS -m 30 -X "$method" "$BASE_URL$path" -o /tmp/sim-http-body -w '%{http_code}')
  [ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  local code; code=$(curl "${args[@]}")
  echo "$code"
  cat /tmp/sim-http-body
}

login() { # login → 设置全局 ACCESS/REFRESH；失败返回非零
  local out
  out=$(http POST /v1/auth/login "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}") || return 1
  local code; code=$(echo "$out" | head -1)
  [ "$code" = "200" ] || return 1
  local body; body=$(echo "$out" | tail -n +2)
  ACCESS=$(jsonq "$body" "d['access_token']")
  REFRESH=$(jsonq "$body" "d.get('refresh_token','')")
  [ -n "$ACCESS" ]
}

# 轮询任务至终态（COMPLETED/FAILED/TIMEOUT/DEAD），输出终态与 error_message
poll_task() { # poll_task <task_id> [timeout_s]
  local id="$1" timeout="${2:-$TASK_TIMEOUT}"
  local deadline=$(( $(date +%s) + timeout )) status=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local out; out=$(http GET "/v1/tasks/$id/snapshot" "" "$ACCESS")
    local body; body=$(echo "$out" | tail -n +2)
    status=$(jsonq "$body" "d['task']['status']" 2>/dev/null)
    case "$status" in
      TASK_STATUS_COMPLETED|TASK_STATUS_FAILED|TASK_STATUS_TIMEOUT|TASK_STATUS_DEAD)
        local emsg; emsg=$(jsonq "$body" "d['task'].get('error_message','')" 2>/dev/null)
        echo "$status|$emsg"; return 0 ;;
    esac
    sleep 5
  done
  echo "POLL_TIMEOUT|"; return 1
}
