#!/bin/bash
# ============================================================
# SMK-6 冒烟测试启动脚本（TP11-T1）
# 依据: test-gates.md §5 SMK-6 + 04 §3.2 模式B全流程
# ============================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT_DIR"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# ============================================================
# 环境检查
# ============================================================
check_dependencies() {
    log_info "检查依赖..."
    
    # 检查 docker
    if ! command -v docker &> /dev/null; then
        log_error "docker 未安装"
        return 1
    fi
    
    # 检查 docker compose
    if ! docker compose version &> /dev/null; then
        log_error "docker compose 不可用"
        return 1
    fi
    
    # 检查 python3
    if ! command -v python3 &> /dev/null; then
        log_error "python3 未安装"
        return 1
    fi
    
    # 检查 pytest
    if ! python3 -m pytest --version &> /dev/null; then
        log_warn "pytest 未安装，尝试安装..."
        pip install pytest requests -i https://pypi.tuna.tsinghua.edu.cn/simple -q
    fi
    
    log_info "依赖检查通过"
    return 0
}

# ============================================================
# 环境变量设置（依据: ADR-113 端口分配）
# ============================================================
setup_environment() {
    log_info "设置环境变量..."
    
    # 动态端口分配（根治端口冲突）
    pick_free() {
        python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()"
    }
    
    export CODEAUDIT_HOST_PG=$(pick_free)
    export CODEAUDIT_HOST_REDIS=$(pick_free)
    export CODEAUDIT_HOST_MINIO_API=$(pick_free)
    export CODEAUDIT_HOST_MINIO_CONSOLE=$(pick_free)
    export CODEAUDIT_HOST_KAFKA=$(pick_free)
    export CODEAUDIT_HOST_GATEWAY=$(pick_free)
    export CODEAUDIT_HOST_PROJECT=$(pick_free)
    export CODEAUDIT_HOST_TASK=$(pick_free)
    export CODEAUDIT_HOST_STORAGE=$(pick_free)
    export CODEAUDIT_HOST_RESULT=$(pick_free)
    
    # 设置 Gateway URL
    export GATEWAY_URL="http://localhost:${CODEAUDIT_HOST_GATEWAY}"
    export PROJECT_SERVICE_URL="http://localhost:${CODEAUDIT_HOST_PROJECT}"
    export TASK_SERVICE_URL="http://localhost:${CODEAUDIT_HOST_TASK}"
    export RESULT_SERVICE_URL="http://localhost:${CODEAUDIT_HOST_RESULT}"  # ADR-136 修复: 此前误指 storage 端口
    export STORAGE_SERVICE_URL="http://localhost:${CODEAUDIT_HOST_STORAGE}"
    
    log_info "端口分配: GW=$CODEAUDIT_HOST_GATEWAY PROJ=$CODEAUDIT_HOST_PROJECT TASK=$CODEAUDIT_HOST_TASK STORE=$CODEAUDIT_HOST_STORAGE RESULT=$CODEAUDIT_HOST_RESULT"
}

# ============================================================
# 启动服务
# ============================================================
start_services() {
    log_info "启动服务..."
    
    # 清理残留容器
    docker rm -f codeaudit-postgres codeaudit-redis codeaudit-neo4j codeaudit-milvus \
                 codeaudit-minio codeaudit-kafka codeaudit-gateway codeaudit-project \
                 codeaudit-task codeaudit-storage 2>/dev/null || true
    
    # 启动基础服务
    log_info "启动基础服务（postgres, redis, minio, kafka）..."
    docker compose up -d postgres redis minio kafka
    
    log_info "等待基础服务健康检查（30s）..."
    sleep 30
    
    # 启动应用服务
    log_info "启动应用服务（project, task, storage, gateway）..."
    docker compose up -d project task storage gateway || true
    
    log_info "等待应用服务启动（10s）..."
    sleep 10
    
    # 检查服务状态
    docker compose ps
    
    # 等待健康检查
    log_info "等待服务健康检查（30s）..."
    sleep 30
    
    # 验证服务可用
    log_info "验证服务可用性..."
    
    # Gateway
    if ! curl -s "$GATEWAY_URL/health" > /dev/null 2>&1; then
        log_warn "Gateway 健康检查失败，等待更长时间..."
        sleep 20
        if ! curl -s "$GATEWAY_URL/health" > /dev/null 2>&1; then
            log_error "Gateway 不可用"
            docker compose logs gateway | tail -20
            return 1
        fi
    fi
    
    log_info "服务启动完成"
    return 0
}

# ============================================================
# 运行冒烟测试
# ============================================================
run_smoke_test() {
    log_info "运行 SMK-6 冒烟测试..."
    
    # 保存环境变量到文件（供 pytest 读取）
    cat > .env.smoke <<EOF
GATEWAY_URL=${GATEWAY_URL}
PROJECT_SERVICE_URL=${PROJECT_SERVICE_URL}
TASK_SERVICE_URL=${TASK_SERVICE_URL}
RESULT_SERVICE_URL=${RESULT_SERVICE_URL}
STORAGE_SERVICE_URL=${STORAGE_SERVICE_URL}
JWT_SECRET=${JWT_SECRET:-ci-test-secret}
EOF
    
    # 运行 pytest
    python3 -m pytest tests/smoke/test_smk6_mode_b_e2e.py -v --tb=short 2>&1 | tee .agent/evidence/SMK6_smoke_output.txt
    
    local exit_code=$?
    
    if [ $exit_code -eq 0 ]; then
        log_info "冒烟测试通过"
    else
        log_error "冒烟测试失败"
    fi
    
    return $exit_code
}

# ============================================================
# 清理
# ============================================================
cleanup() {
    log_info "清理环境..."
    docker compose down -v --timeout 10 2>/dev/null || true
    rm -f .env.smoke
}

# ============================================================
# 主流程
# ============================================================
main() {
    local start_time=$(date +%s)
    
    echo "============================================================"
    echo "SMK-6: 模式B端到端冒烟测试（TP11-T1）"
    echo "依据: test-gates.md §5 SMK-6 + 04 §3.2 模式B全流程"
    echo "============================================================"
    echo "开始时间: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    
    # 检查依赖
    if ! check_dependencies; then
        log_error "依赖检查失败"
        return 1
    fi
    
    # 设置环境
    setup_environment
    
    # 启动服务
    if ! start_services; then
        log_error "服务启动失败"
        cleanup
        return 1
    fi
    
    # 运行测试
    local test_result=0
    if ! run_smoke_test; then
        test_result=1
    fi
    
    # 清理（CI 环境中由 after_script 处理）
    if [ "${CI:-}" != "true" ]; then
        cleanup
    fi
    
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    echo ""
    echo "============================================================"
    echo "SMK-6 测试完成"
    echo "结果: $(if [ $test_result -eq 0 ]; then echo 'PASS'; else echo 'FAIL'; fi)"
    echo "耗时: ${duration}s"
    echo "结束时间: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
    echo "============================================================"
    
    return $test_result
}

# 运行主流程
main "$@"
