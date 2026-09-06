#!/usr/bin/env bash
# =============================================================================
# sim.sh — CodeAudit 生产模拟栈统一入口（up/down/status/logs/wait/seed）
#
# 拓扑同构于 CD 现役生产（base compose + overlay + env），但独立 project name /
# 网段 / 端口段，与生产栈可在同一 docker daemon 上共存互不干扰。
#
# 用法:
#   ./sim.sh up      # 构建+启动全栈，等待 gateway 健康，初始化 PG 库表
#   ./sim.sh down    # 停止并移除（保留 named volumes，数据可复用）
#   ./sim.sh destroy # 停止并清除数据卷（完全重置）
#   ./sim.sh status | logs [service] [N] | wait | seed
# 环境变量: PLATFORM_DIR（engine 仓检出路径，默认 ../engine 相对本文件；变量名沿用历史接口）
# =============================================================================
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")" && pwd)"
PLATFORM_DIR="${PLATFORM_DIR:-$(cd "$DEPLOY_DIR/../engine" && pwd)}"
PROJECT=codeaudit-sim

for f in "$PLATFORM_DIR/docker-compose.yml" "$DEPLOY_DIR/docker-compose.sim.yml"; do
  [ -f "$f" ] || { echo "缺少 $f（伞仓须含 engine 子模块检出）" >&2; exit 1; }
done

# env.sim（真实值，gitignore）存在则加载
[ -f "$DEPLOY_DIR/env.sim" ] && set -a && . "$DEPLOY_DIR/env.sim" && set +a

# 中间件宿主端口钉到 1xxxx 段（基础 compose 缺省发布 5432/6379/9000/9001/9092，
# 与同一 docker daemon 上的生产栈共存时必撞——sim 栈整体走 1xxxx 端口段口径）
export CODEAUDIT_HOST_PG="${CODEAUDIT_HOST_PG:-15432}"
export CODEAUDIT_HOST_REDIS="${CODEAUDIT_HOST_REDIS:-16379}"
export CODEAUDIT_HOST_MINIO_API="${CODEAUDIT_HOST_MINIO_API:-19000}"
export CODEAUDIT_HOST_MINIO_CONSOLE="${CODEAUDIT_HOST_MINIO_CONSOLE:-19001}"
export CODEAUDIT_HOST_KAFKA="${CODEAUDIT_HOST_KAFKA:-19092}"
# gRPC 服务宿主端口同理钉 15xxx 段（服务间互访走容器网络 service:50xxx 不受影响）
export CODEAUDIT_HOST_PROJECT="${CODEAUDIT_HOST_PROJECT:-15052}"
export CODEAUDIT_HOST_STORAGE="${CODEAUDIT_HOST_STORAGE:-15055}"
export CODEAUDIT_HOST_TASK="${CODEAUDIT_HOST_TASK:-15054}"
export CODEAUDIT_HOST_SAST_ADAPTER="${CODEAUDIT_HOST_SAST_ADAPTER:-15051}"
export CODEAUDIT_HOST_RESULT="${CODEAUDIT_HOST_RESULT:-15058}"
export CODEAUDIT_HOST_DSH="${CODEAUDIT_HOST_DSH_RUNTIME:-15057}"

dc() { docker compose -p "$PROJECT" --project-directory "$PLATFORM_DIR" \
    -f "$PLATFORM_DIR/docker-compose.yml" -f "$DEPLOY_DIR/docker-compose.sim.yml" "$@"; }

gw_url() { echo "http://localhost:${CODEAUDIT_HOST_GATEWAY:-18080}"; }

wait_gateway() {
  local deadline=$(( $(date +%s) + ${HEALTH_TIMEOUT:-420} ))
  echo "等待模拟栈 gateway 就绪（${HEALTH_TIMEOUT:-420}s 上限，首次构建较慢）..."
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if dc ps --format '{{.Name}} {{.Health}}' 2>/dev/null | grep -q unhealthy; then :; fi
    if curl -fsS --max-time 3 "$(gw_url)/health" >/dev/null 2>&1; then
      echo "gateway 已就绪: $(gw_url)"; return 0
    fi
    sleep 5
  done
  echo "超时：gateway 未就绪（查看 ./sim.sh logs gateway 200）" >&2
  return 1
}

seed_db() {
  # PG 库表初始化（ADR-111：init-db.sql 幂等 CREATE TABLE IF NOT EXISTS）
  dc exec -T postgres psql -U postgres -d postgres < "$PLATFORM_DIR/scripts/init-db.sql" >/dev/null 2>&1 \
    && echo "[seed] PG 库表就绪（codeaudit_project/task/result）" \
    || echo "[seed][警告] init-db.sql 执行有误——若库表已存在可忽略" >&2
}

cmd="${1:-up}"; shift || true
case "$cmd" in
  up)
    dc up -d --build "$@"
    wait_gateway
    seed_db
    echo "--- 模拟栈入口 ---"
    echo "  gateway : $(gw_url)  （宿主 ${CODEAUDIT_HOST_GATEWAY:-18080}）"
    echo "  console : http://localhost:${CODEAUDIT_SIM_CONSOLE_PORT:-18088}"
    echo "  功能测试: bash $DEPLOY_DIR/tests/run.sh"
    ;;
  down)   dc down ;;
  destroy) dc down -v ;;
  status) dc ps ;;
  logs)   local svc="${1:-}"; if [ -n "$svc" ]; then dc logs --tail "${2:-200}" "$svc"; else dc logs --tail "${2:-200}"; fi ;;
  wait)   wait_gateway ;;
  seed)   seed_db ;;
  *) echo "未知命令: $cmd（up|down|destroy|status|logs|wait|seed）" >&2; exit 1 ;;
esac
