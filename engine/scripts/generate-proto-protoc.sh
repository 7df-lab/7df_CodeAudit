#!/bin/bash
# Proto 代码生成脚本（使用 protoc）
# 用途：从 codeaudit_common.proto 生成 Go 和 Python 代码
# 遵循 R1：proto 是唯一数据契约，生成代码必须由 proto 派生

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
PROTO_DIR="$PROJECT_ROOT/proto"
OUTPUT_DIR="$PROJECT_ROOT/libs/proto-gen"

echo "=== Proto 代码生成 (protoc) ==="
echo "源目录: $PROTO_DIR"
echo "输出目录: $OUTPUT_DIR"

# 检查 protoc 是否安装
if ! command -v protoc &> /dev/null; then
    echo "错误: protoc 未安装"
    echo "请运行: bash scripts/setup-proto-tools.sh"
    exit 1
fi

# 检查 Go 插件
if ! command -v protoc-gen-go &> /dev/null; then
    echo "警告: protoc-gen-go 未安装，将跳过 Go 代码生成"
    GENERATE_GO=false
else
    GENERATE_GO=true
fi

if ! command -v protoc-gen-go-grpc &> /dev/null; then
    echo "警告: protoc-gen-go-grpc 未安装，将跳过 Go gRPC 代码生成"
    GENERATE_GO_GRPC=false
else
    GENERATE_GO_GRPC=true
fi

# 清理旧的生成代码
rm -rf "$OUTPUT_DIR/go" "$OUTPUT_DIR/python"
mkdir -p "$OUTPUT_DIR/go" "$OUTPUT_DIR/python"

# 生成 Go 代码
if [ "$GENERATE_GO" = true ]; then
    echo "生成 Go 代码..."
    GO_OUT="$OUTPUT_DIR/go"
    mkdir -p "$GO_OUT"

    PROTOC_GO_OPT="paths=source_relative"
    PROTOC_GO_GRPC_OPT="paths=source_relative"

    PROTOC_CMD="protoc \
        --proto_path=$PROTO_DIR \
        --proto_path=/usr/local/include"

    if [ "$GENERATE_GO" = true ]; then
        PROTOC_CMD="$PROTOC_CMD --go_out=$GO_OUT --go_opt=$PROTOC_GO_OPT"
    fi

    if [ "$GENERATE_GO_GRPC" = true ]; then
        PROTOC_CMD="$PROTOC_CMD --go-grpc_out=$GO_OUT --go-grpc_opt=$PROTOC_GO_GRPC_OPT"
    fi

    PROTOC_CMD="$PROTOC_CMD $PROTO_DIR/codeaudit_common.proto"
    eval $PROTOC_CMD
fi

# 生成 Python 代码
echo "生成 Python 代码..."
PYTHON_OUT="$OUTPUT_DIR/python"
mkdir -p "$PYTHON_OUT"

python3 -m grpc_tools.protoc \
    --proto_path=$PROTO_DIR \
    --python_out=$PYTHON_OUT \
    --grpc_python_out=$PYTHON_OUT \
    $PROTO_DIR/codeaudit_common.proto 2>/dev/null || {
    echo "警告: Python grpc_tools 未安装，使用 protoc 直接生成"
    protoc \
        --proto_path=$PROTO_DIR \
        --python_out=$PYTHON_OUT \
        $PROTO_DIR/codeaudit_common.proto
}

# 验证生成结果
echo ""
echo "=== 生成结果 ==="
echo "Go 代码:"
find "$OUTPUT_DIR/go" -name "*.go" 2>/dev/null | head -10 || echo "(无)"
echo ""
echo "Python 代码:"
find "$OUTPUT_DIR/python" -name "*.py" 2>/dev/null | head -10 || echo "(无)"

echo ""
echo "=== 生成完成 ==="
echo "生成物位于: $OUTPUT_DIR"
