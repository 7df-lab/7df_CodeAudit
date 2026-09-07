#!/usr/bin/env bash
# git_fixture_107.sh — 仓库拉取模式(git clone)回归的匿名 git-daemon fixture（跑在 LXC 107）
#
# 背景：真实 GitLab 是私有 CA + 鉴权（ADR-163 V1 不做凭据管理），e2e 无法打真源；
#   2026-09-07 用本 fixture 真跑通仓库模式全链（任务 gw-ec58356：clone→bandit→
#   COMPLETED，发现=3：B608×2 + B105）。按“手工应急必须固化为脚本”纪律固化。
# 网络口径：任务容器经 sim 网络默认网关回 107 宿主——repo_url 用
#   git://10.10.210.1:19418/sample-sast.git（10.10.210.1 = docker 网桥网关，
#   若 sim 网段变更需在容器内实测网关后替换）。
# 用法：bash deploy/tests/git_fixture_107.sh up|down|status   （在伞仓根目录执行）
set -euo pipefail

LXC=107
FIXDIR=/root/codeaudit-sim-fixtures
PORT=19418
REPO_URL="git://10.10.210.1:${PORT}/sample-sast.git"

on107() { pct exec "$LXC" -- sh -c "$1"; }

do_up() {
  # 1) bare repo（幂等：已在则跳过）
  on107 "test -d $FIXDIR/sample-sast.git" && echo "fixture 仓库已存在：$FIXDIR/sample-sast.git" || {
    on107 "mkdir -p $FIXDIR/sample-sast.git && git init --bare -b main $FIXDIR/sample-sast.git >/dev/null"
    on107 "rm -rf /tmp/sast-seed && mkdir -p /tmp/sast-seed && cd /tmp/sast-seed && \
printf '%s\n' \
'import sqlite3' \
'def get_user(uid):' \
'    conn = sqlite3.connect(\"app.db\")' \
'    cur = conn.cursor()' \
'    cur.execute(\"SELECT * FROM users WHERE id = \\\"%s\\\"\" % uid)' \
'    return cur.fetchone()' \
'API_TOKEN = \"hunter2-hardcoded-secret\"' \
'def run_report(q):' \
'    return conn.execute(\"SELECT * FROM reports WHERE title LIKE \\\"%%%s%%\\\"\" % q).fetchall()' \
> app.py && \
git -c user.email=fixture@local -c user.name=fixture init -b main . >/dev/null && \
git add app.py && git -c user.email=fixture@local -c user.name=fixture commit -m 'vulnerable sample' >/dev/null && \
git push $FIXDIR/sample-sast.git main 2>/dev/null && rm -rf /tmp/sast-seed"
    echo "fixture 仓库已重建（app.py：bandit B608×2 + B105）"
  }
  # 2) git daemon（幂等：端口在听则跳过）
  if on107 "ss -tln | grep -q ':$PORT '"; then
    echo "git-daemon 已在监听 :$PORT"
  else
    on107 "git daemon --base-path=$FIXDIR --export-all --port=$PORT --detach"
    sleep 1
    on107 "ss -tln | grep ':$PORT '" >/dev/null && echo "git-daemon 已启动 :$PORT"
  fi
  echo "repo_url = $REPO_URL   （分支 main；建 SAST_ONLY 任务用）"
}

do_down() {
  on107 "pgrep -f 'git.*daemon.*$PORT' | xargs -r kill" || true
  sleep 1
  on107 "ss -tln | grep ':$PORT '" >/dev/null 2>&1 && echo "⚠ 端口 $PORT 仍在监听" || echo "git-daemon 已停止（fixture 目录保留：$FIXDIR）"
}

do_status() {
  echo "== 监听 =="; on107 "ss -tlnp 2>/dev/null | grep ':$PORT ' || echo '未监听'"
  echo "== fixture =="; on107 "ls $FIXDIR 2>/dev/null || echo '目录不存在'"
  echo "repo_url = $REPO_URL"
}

case "${1:-}" in
  up) do_up ;;
  down) do_down ;;
  status) do_status ;;
  *) echo "用法：$0 up|down|status"; exit 2 ;;
esac
