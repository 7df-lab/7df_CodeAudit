#!/bin/bash
# Proto 同步检查脚本
# 用途：验证生成代码与 proto 源文件一致（R1 红线检查）
# 用法：bash scripts/check-proto-sync.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
PROTO_DIR="$PROJECT_ROOT/proto"
OUTPUT_DIR="$PROJECT_ROOT/libs/proto-gen"

echo "=== Proto 同步检查 ==="

# 检查 proto 文件是否存在
if [ ! -f "$PROTO_DIR/codeaudit_common.proto" ]; then
    echo "错误: proto 文件不存在: $PROTO_DIR/codeaudit_common.proto"
    exit 1
fi

# 创建临时目录
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# 工具链预检(AGENTS.md §2c): 必须在任何删除动作之前执行——
# 否则"先rm后校验"会在工具缺失时把已入库生成物删掉(R8工作区被弄脏,且违反完成本质判定)
if ! command -v buf >/dev/null 2>&1 && ! command -v protoc >/dev/null 2>&1; then
    echo "警告: buf/protoc 均不可用，跳过同步检查（不破坏现有生成物）"
    exit 0
fi

# 备份当前生成的代码
if [ -d "$OUTPUT_DIR" ]; then
    cp -r "$OUTPUT_DIR" "$TEMP_DIR/current"
fi

# 重新生成代码
echo "重新生成代码..."
# 失败恢复: buf 依赖(BSR googleapis)拉取可能因网络抖动失败, 而 generate-proto.sh
# 已 rm 生成物目录——必须立即用 git 恢复, 否则 R8 检查会把"网络故障"误判为"工作区脏"
# ADR-112后只有一条本地工具链权威路径; 失败即恢复生成物并以失败退出(不静默跳过)
if ! bash "$SCRIPT_DIR/generate-proto.sh"; then
    echo "错误: 代码生成失败, 恢复已入库生成物"
    git -C "$PROJECT_ROOT" checkout -- libs/proto-gen 2>/dev/null || true
    exit 1
fi

# 恢复 go.mod/go.sum（generate-proto.sh 会清理整个目录，但这些是独立入库的模块文件）
git -C "$PROJECT_ROOT" checkout -- libs/proto-gen/go/go.mod libs/proto-gen/go/go.sum 2>/dev/null || true

# 比较差异
if [ -d "$TEMP_DIR/current" ] && [ -d "$OUTPUT_DIR" ]; then
    echo "比较生成代码差异..."
    DIFF=$(diff -r --exclude='go.mod' --exclude='go.sum' --exclude='__pycache__' "$TEMP_DIR/current" "$OUTPUT_DIR" 2>/dev/null || true)

    if [ -n "$DIFF" ]; then
        echo "错误: 生成代码与 proto 源文件不一致!"
        echo ""
        echo "差异:"
        echo "$DIFF"
        echo ""
        echo "请运行 'make proto' 重新生成代码"
        exit 1
    else
        echo "✓ 生成代码与 proto 源文件一致"
    fi
else
    echo "✓ 首次生成代码"
fi

echo ""
echo "=== Proto 同步检查完成 ==="
